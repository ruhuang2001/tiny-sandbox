package dockerbackend

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/dockerapi"
)

// fakeDaemon speaks the subset of the Engine API the backend uses, over a
// real unix socket. Tests configure its answers per scenario.
type fakeDaemon struct {
	t    *testing.T
	sock string

	mu         sync.Mutex
	nextCtr    int
	nextExec   int
	containers map[string]*fakeContainer
	byName     map[string]string // container name -> id, as Docker accepts names
	execs      map[string]*fakeExec

	creates      []dockerapi.ContainerConfig
	createNames  []string
	removed      []string
	kills        []string
	execCommands [][]string

	// Scenario switches.
	failStart bool
	nameTaken string
	noPidFile bool
	programFn func(containerID string, cmd []string) (stdout, stderr string, exitCode int)
}

type fakeContainer struct {
	id     string
	spec   dockerapi.ContainerConfig
	state  string // running, paused, exited
	labels map[string]string
	files  map[string][]byte
}

type fakeExec struct {
	id          string
	containerID string
	command     []string
	stdout      string
	stderr      string
	exitCode    int
	running     bool
	started     bool
}

// newFakeDaemon starts a daemon on a temporary unix socket.
func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	// A unix socket path is limited to ~104 bytes on macOS, and t.TempDir()
	// alone can exceed that. A short directory under the system temp root
	// keeps the path well inside the limit.
	dir, err := os.MkdirTemp("", "ts-docker")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := path.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeDaemon{
		t:          t,
		sock:       sock,
		containers: make(map[string]*fakeContainer),
		byName:     make(map[string]string),
		execs:      make(map[string]*fakeExec),
	}
	srv := &http.Server{Handler: f, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

// client dials the fake and negotiates the version, as production does.
func (f *fakeDaemon) client(t *testing.T) *dockerapi.Client {
	t.Helper()
	c, err := dockerapi.Dial(t.Context(), f.sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func (f *fakeDaemon) backend(t *testing.T) *Backend {
	t.Helper()
	return New(f.client(t))
}

// ServeHTTP routes the Engine API paths the backend uses.
func (f *fakeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/version":
		writeJSON(w, 200, map[string]string{"ApiVersion": "1.52", "Version": "29.0.0"})
	case strings.HasPrefix(r.URL.Path, "/v1.52/"):
		f.serveV152(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeDaemon) serveV152(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/v1.52")

	switch {
	case p == "/containers/create" && r.Method == http.MethodPost:
		f.create(w, r)
	case p == "/containers/json" && r.Method == http.MethodGet:
		f.list(w, r)
	case strings.HasSuffix(p, "/exec") && r.Method == http.MethodPost:
		f.execCreate(w, r, strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/exec"))
	case strings.HasPrefix(p, "/exec/") && strings.HasSuffix(p, "/start"):
		f.execStart(w, r, strings.TrimSuffix(strings.TrimPrefix(p, "/exec/"), "/start"))
	case strings.HasPrefix(p, "/exec/") && strings.HasSuffix(p, "/json"):
		f.execInspect(w, r, strings.TrimSuffix(strings.TrimPrefix(p, "/exec/"), "/json"))
	case strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/archive"):
		f.archive(w, r, strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/archive"))
	case strings.HasPrefix(p, "/containers/"):
		f.containerOp(w, r, strings.TrimPrefix(p, "/containers/"))
	default:
		http.NotFound(w, r)
	}
}

// containerOp serves start, pause, unpause, kill and delete.
func (f *fakeDaemon) containerOp(w http.ResponseWriter, r *http.Request, tail string) {
	action := ""
	id := tail
	if i := strings.LastIndex(tail, "/"); i >= 0 {
		id, action = tail[:i], tail[i+1:]
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.container(w, id)
	if !ok {
		return
	}
	id = c.id // a name resolves to the ID a later delete must key on
	switch {
	case action == "start" && r.Method == http.MethodPost:
		if f.failStart {
			writeError(w, 500, "cannot start: simulated failure")
			return
		}
		w.WriteHeader(204)
	case action == "pause" && r.Method == http.MethodPost:
		// Docker answers 204 for an already paused container.
		if c.state == "running" {
			c.state = "paused"
		}
		w.WriteHeader(204)
	case action == "unpause" && r.Method == http.MethodPost:
		if c.state == "paused" {
			c.state = "running"
		}
		w.WriteHeader(204)
	case action == "kill" && r.Method == http.MethodPost:
		f.kills = append(f.kills, id)
		c.state = "exited"
		w.WriteHeader(204)
	case action == "" && r.Method == http.MethodDelete:
		f.removed = append(f.removed, id)
		for name, owner := range f.byName {
			if owner == id {
				delete(f.byName, name)
				if name == f.nameTaken {
					f.nameTaken = ""
				}
			}
		}
		delete(f.containers, id)
		w.WriteHeader(204)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeDaemon) create(w http.ResponseWriter, r *http.Request) {
	var cfg dockerapi.ContainerConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name := r.URL.Query().Get("name")
	if f.nameTaken != "" && name == f.nameTaken {
		if _, held := f.byName[name]; !held {
			// The first conflict creates the leftover that holds the name, so
			// the caller can remove it and retry, as a real daemon allows.
			id := "ctr_leftover"
			f.containers[id] = &fakeContainer{id: id, state: "exited", labels: map[string]string{}}
			f.byName[name] = id
		}
		writeError(w, 409, "Conflict. The container name "+name+" is already in use")
		return
	}
	f.creates = append(f.creates, cfg)
	f.createNames = append(f.createNames, name)
	f.nextCtr++
	id := fmt.Sprintf("ctr_%05d", f.nextCtr)
	f.containers[id] = &fakeContainer{
		id:     id,
		spec:   cfg,
		state:  "running",
		labels: cfg.Labels,
		files:  make(map[string][]byte),
	}
	if name != "" {
		f.byName[name] = id
	}
	writeJSON(w, 201, map[string]string{"Id": id})
}

// container resolves a name or a full ID. Docker accepts a unique name or an
// ID prefix; resolving names here is enough for the backend, which always
// passes an ID except when removing a leftover by name.
// resolveContainer maps a name to the ID it owns, if any.
func (f *fakeDaemon) resolveContainer(id string) string {
	if mapped, ok := f.byName[id]; ok {
		return mapped
	}
	return id
}

func (f *fakeDaemon) container(w http.ResponseWriter, id string) (*fakeContainer, bool) {
	id = f.resolveContainer(id)
	c := f.containers[id]
	if c == nil {
		writeError(w, 404, "No such container: "+id)
		return nil, false
	}
	return c, true
}

func (f *fakeDaemon) list(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var filters map[string][]string
	if raw := r.URL.Query().Get("filters"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &filters)
	}
	out := make([]map[string]any, 0, len(f.containers))
	for _, c := range f.containers {
		if !matchesID(c.id, filters["id"]) || !matchesLabel(c.labels, filters["label"]) {
			continue
		}
		out = append(out, map[string]any{
			"Id": c.id, "State": c.state, "Image": c.spec.Image, "Labels": c.labels,
		})
	}
	writeJSON(w, 200, out)
}

// matchesID applies Docker's rule: an id filter matches a prefix.
func matchesID(id string, want []string) bool {
	if len(want) == 0 {
		return true
	}
	for _, w := range want {
		if strings.HasPrefix(id, w) {
			return true
		}
	}
	return false
}

// matchesLabel applies Docker's rule: every filter must match, and a filter
// without "=" matches any value.
func matchesLabel(labels map[string]string, want []string) bool {
	for _, pair := range want {
		key, value, hasValue := strings.Cut(pair, "=")
		got, ok := labels[key]
		if !ok || (hasValue && got != value) {
			return false
		}
	}
	return true
}

// execCreate records the command and, for the PID-file wrapper, writes the PID
// file the command would have written.
func (f *fakeDaemon) execCreate(w http.ResponseWriter, r *http.Request, containerID string) {
	var cfg struct {
		Cmd []string `json:"Cmd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.container(w, containerID)
	if !ok {
		return
	}
	f.nextExec++
	ex := &fakeExec{
		id:          fmt.Sprintf("exe_%05d", f.nextExec),
		containerID: containerID,
		command:     cfg.Cmd,
	}
	real := cfg.Cmd
	if pidPath, realCmd, wrapped := unwrap(cfg.Cmd); wrapped {
		c.files[pidPath] = []byte("4242\n")
		ex.command = realCmd
		real = realCmd
	}
	// programFn runs while the lock is held: it is a test-supplied decision
	// with no reason to call back into the fake.
	ex.stdout, ex.stderr, ex.exitCode = f.programLocked(containerID, real)
	f.execs[ex.id] = ex
	f.execCommands = append(f.execCommands, cfg.Cmd)
	writeJSON(w, 201, map[string]string{"Id": ex.id})
}

// programLocked decides what an exec does. Callers hold f.mu. The default
// reads the cgroup files for the OOM script and succeeds silently otherwise.
func (f *fakeDaemon) programLocked(containerID string, cmd []string) (string, string, int) {
	if f.programFn != nil {
		return f.programFn(containerID, cmd)
	}
	if len(cmd) == 3 && cmd[0] == "/bin/sh" && cmd[2] == dockerapi.OOMScript {
		return "low 0\nhigh 0\nmax 0\noom 0\noom_kill 5\n", "", 0
	}
	return "", "", 0
}

// setProgram replaces what every exec does.
func (f *fakeDaemon) setProgram(fn func(containerID string, cmd []string) (stdout, stderr string, exitCode int)) {
	f.mu.Lock()
	f.programFn = fn
	f.mu.Unlock()
}

func (f *fakeDaemon) execStart(w http.ResponseWriter, _ *http.Request, execID string) {
	f.mu.Lock()
	ex := f.execs[execID]
	f.mu.Unlock()
	if ex == nil {
		writeError(w, 404, "No such exec: "+execID)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	w.WriteHeader(200)
	if ex.stdout != "" {
		writeFrame(w, 1, ex.stdout)
	}
	if ex.stderr != "" {
		writeFrame(w, 2, ex.stderr)
	}
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
	f.mu.Lock()
	ex.running = false
	ex.started = true
	f.mu.Unlock()
}

func (f *fakeDaemon) execInspect(w http.ResponseWriter, _ *http.Request, execID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ex := f.execs[execID]
	if ex == nil {
		writeError(w, 404, "No such exec: "+execID)
		return
	}
	writeJSON(w, 200, map[string]any{
		"ID": ex.id, "Running": ex.running, "ExitCode": ex.exitCode, "Pid": 4242,
	})
}

func (f *fakeDaemon) archive(w http.ResponseWriter, r *http.Request, containerID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.container(w, containerID)
	if !ok {
		return
	}
	p := r.URL.Query().Get("path")
	switch r.Method {
	case http.MethodGet:
		data, exists := c.files[p]
		if !exists {
			writeError(w, 404, "Could not find the file "+p+" in container "+containerID)
			return
		}
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg, Name: path.Base(p), Mode: 0o644, Size: int64(len(data)),
		})
		_, _ = tw.Write(data)
		_ = tw.Close()
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(buf.Bytes())
	case http.MethodPut:
		tr := tar.NewReader(r.Body)
		for {
			hdr, err := tr.Next()
			if err != nil {
				break
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				writeError(w, 400, err.Error())
				return
			}
			c.files[path.Join(p, hdr.Name)] = data
		}
		w.WriteHeader(200)
	}
}

func writeFrame(w io.Writer, stream byte, payload string) {
	var hdr [8]byte
	hdr[0] = stream
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
	_, _ = w.Write(hdr[:])
	_, _ = io.WriteString(w, payload)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

// unwrap recognizes the PID-file wrapper and returns the PID file path and the
// real command.
func unwrap(cmd []string) (pidPath string, real []string, ok bool) {
	if len(cmd) < 5 || cmd[0] != "/bin/sh" || cmd[1] != "-c" || cmd[2] != execWrapper {
		return "", nil, false
	}
	return cmd[3], cmd[4:], true
}

// setFile seeds a file in a container, for archive tests.
func (f *fakeDaemon) setFile(containerID, path string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.containers[containerID]; c != nil {
		c.files[path] = data
	}
}

// file returns a container's stored file.
func (f *fakeDaemon) file(containerID, path string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.containers[containerID]; c != nil {
		return c.files[path]
	}
	return nil
}

// lastExecCommand returns the most recent exec argv.
func (f *fakeDaemon) lastExecCommand() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.execCommands) == 0 {
		return nil
	}
	return f.execCommands[len(f.execCommands)-1]
}

// countExecCommands returns how many execs of a shape were created.
func (f *fakeDaemon) countExecCommands() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.execCommands)
}

// removedContainers returns every removed container ID.
func (f *fakeDaemon) removedContainers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}

var _ = strconv.Itoa
