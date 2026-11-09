// Package dockerbackend implements daemon.Backend on top of the Docker Engine
// API. It knows only internal/dockerapi; nothing in it knows about HTTP or the
// sandbox state machine.
//
// One container runs per sandbox. The container's init process is
// `sleep infinity`; every execution is a Docker exec inside it. Two details
// are load-bearing:
//
//   - An exec starts in its own process group, so signalling the group ends
//     grandchildren too. The exec's PID is only visible inside the container,
//     so the command is wrapped: a tiny shell writes $$ to a tmpfs file and
//     execs the real command, keeping the PID. Signal reads that file and
//     kills the group from inside the container.
//   - Exec streams must outlive the request that started them. The engine
//     hands Exec the HTTP request's context, which net/http cancels when the
//     handler returns, so ExecStart detaches from it.
package dockerbackend

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/dockerapi"
)

// PidDir is a tmpfs mounted in every sandbox container. PID files live there
// so signalling never touches the container's writable layer and cannot
// collide with files the user writes.
const PidDir = "/run/tiny-sandbox"

// initCommand keeps the container alive between executions. Every image the
// backend supports has sh and sleep; `sleep infinity` needs no shell.
var initCommand = []string{"sleep", "infinity"}

// Backend is a daemon.Backend over a Docker daemon.
type Backend struct {
	api *dockerapi.Client

	mu  sync.Mutex
	seq uint64
}

var _ daemon.Backend = (*Backend)(nil)

// New returns a backend that sends requests through api. The client must have
// negotiated its API version, which Dial does.
func New(api *dockerapi.Client) *Backend { return &Backend{api: api} }

// Dial connects to the Docker socket at socketPath and returns a backend.
func Dial(ctx context.Context, socketPath string) (*Backend, error) {
	api, err := dockerapi.Dial(ctx, socketPath)
	if err != nil {
		return nil, err
	}
	return New(api), nil
}

// ---------------------------------------------------------------- containers

// Create creates and starts the sandbox container. It returns only once the
// container is running, as the Backend contract requires.
func (b *Backend) Create(ctx context.Context, spec daemon.ContainerSpec) (string, error) {
	limit := int64(spec.MemoryMB) << 20
	cfg := dockerapi.ContainerConfig{
		Image:  spec.Image,
		Cmd:    initCommand,
		Labels: spec.Labels,
		HostConfig: dockerapi.HostConfig{
			Memory:     limit,
			MemorySwap: limit, // no swap: the limit the user asked for is the limit
			Tmpfs:      map[string]string{PidDir: "rw,size=1m,mode=1777"},
		},
	}

	id, err := b.api.ContainerCreate(ctx, spec.Name, cfg)
	if err != nil && dockerapi.IsNoSuchImage(err) {
		// The Engine API's create does not pull, unlike `docker run`. A clean
		// host therefore fails every create until the image is fetched, which
		// is exactly the difference between a developer machine and CI.
		if pullErr := b.api.PullImage(ctx, spec.Image); pullErr != nil {
			return "", fmt.Errorf("dockerbackend: pull %s: %w", spec.Image, pullErr)
		}
		id, err = b.api.ContainerCreate(ctx, spec.Name, cfg)
	}
	if err != nil && dockerapi.IsConflict(err) && spec.Name != "" {
		// A leftover container still owns the name. The sandbox ID is unique,
		// so this is a restart artefact; remove it and retry once.
		if rmErr := b.api.ContainerRemove(ctx, spec.Name); rmErr == nil {
			id, err = b.api.ContainerCreate(ctx, spec.Name, cfg)
		}
	}
	if err != nil {
		return "", fmt.Errorf("dockerbackend: create %s: %w", spec.Image, err)
	}

	if err := b.api.ContainerStart(ctx, id); err != nil {
		// A container that exists but never started would leak until the next
		// startup cleanup; remove it now.
		_ = b.api.ContainerRemove(context.WithoutCancel(ctx), id)
		return "", fmt.Errorf("dockerbackend: start container: %w", err)
	}
	return id, nil
}

// Remove force-removes the container and everything running in it.
func (b *Backend) Remove(ctx context.Context, containerID string) error {
	err := b.api.ContainerRemove(ctx, containerID)
	return notFound(err, "container %s", containerID)
}

// Pause freezes the container.
func (b *Backend) Pause(ctx context.Context, containerID string) error {
	err := b.api.ContainerPause(ctx, containerID)
	return notFound(err, "container %s", containerID)
}

// Unpause resumes the container. Docker answers 204 when it is already
// running, which matches the contract's idempotent resume.
func (b *Backend) Unpause(ctx context.Context, containerID string) error {
	err := b.api.ContainerUnpause(ctx, containerID)
	return notFound(err, "container %s", containerID)
}

// Liveness reports the exact container's state.
//
// It uses the container list, not inspect. An earlier build lost this race:
// inspect intermittently answered "no such object" for a container that was
// still there. The list with all=1 distinguishes "exists and stopped" from
// "does not exist" without that window, and the ID filter is compared exactly
// because Docker's id filter matches a prefix.
func (b *Backend) Liveness(ctx context.Context, containerID string) (daemon.Liveness, error) {
	list, err := b.api.ContainerList(ctx, true, map[string][]string{"id": {containerID}})
	if err != nil {
		return daemon.LivenessMissing, fmt.Errorf("dockerbackend: list containers: %w", err)
	}
	for _, c := range list {
		if c.ID != containerID {
			continue
		}
		switch c.State {
		case "running":
			return daemon.LivenessRunning, nil
		case "paused":
			return daemon.LivenessPaused, nil
		default:
			return daemon.LivenessStopped, nil
		}
	}
	return daemon.LivenessMissing, nil
}

// RemoveOwned removes every container carrying labelKey=owner and returns how
// many it removed. The server runs it at startup so a restart does not leak
// containers from the previous process.
func (b *Backend) RemoveOwned(ctx context.Context, labelKey, owner string) (int, error) {
	filters := map[string][]string{"label": {labelKey + "=" + owner}}
	list, err := b.api.ContainerList(ctx, true, filters)
	if err != nil {
		return 0, fmt.Errorf("dockerbackend: list owned containers: %w", err)
	}
	removed := 0
	var errs []error
	for _, c := range list {
		if err := b.api.ContainerRemove(ctx, c.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// ---------------------------------------------------------------- executions

// Exec starts command inside the container. The command is wrapped in a shell
// that records its PID and then execs the real argv, so the PID file names the
// process group leader and the command itself still runs without a shell.
func (b *Backend) Exec(ctx context.Context, containerID string, command []string) (daemon.Process, error) {
	if len(command) == 0 {
		return nil, errors.New("dockerbackend: empty command")
	}
	pidPath := b.nextPidPath()
	wrapped := wrapCommand(pidPath, command)

	execID, err := b.api.ExecCreate(ctx, containerID, wrapped)
	if err != nil {
		return nil, fmt.Errorf("dockerbackend: exec create: %w", err)
	}
	stream, err := b.api.ExecStart(context.WithoutCancel(ctx), execID)
	if err != nil {
		return nil, fmt.Errorf("dockerbackend: exec start: %w", err)
	}

	outW := newBufferedStream()
	errW := newBufferedStream()
	p := &process{
		b:           b,
		containerID: containerID,
		execID:      execID,
		pidPath:     pidPath,
		stdout:      outW,
		stderr:      errW,
		done:        make(chan struct{}),
	}
	go func() {
		defer close(p.done)
		err := dockerapi.Demux(stream, outW, errW)
		_ = stream.Close()
		_ = outW.Close()
		_ = errW.Close()
		p.mu.Lock()
		p.demuxErr = err
		p.mu.Unlock()
	}()
	return p, nil
}

// OOMKillCount reads the container cgroup's cumulative oom_kill counter. The
// file is read from inside the container because the cgroup layout is the
// container's, and a host path would differ across cgroups versions and
// remote daemons.
//
// When the cgroup read fails, inspect decides: a container that has run out
// of memory cannot start another process, so exec itself fails right after
// the kill that matters most. The daemon's OOMKilled flag carries the same
// fact and survives that state. It is reported as 1 rather than a count: the
// caller only compares it with a baseline, and a value above any real
// baseline is all the comparison needs.
func (b *Backend) OOMKillCount(ctx context.Context, containerID string) (int64, error) {
	out, err := b.capture(ctx, containerID, []string{"/bin/sh", "-c", dockerapi.OOMScript})
	if err == nil {
		return dockerapi.ParseOOMKill(out)
	}

	inspect, inspectErr := b.api.ContainerInspect(ctx, containerID)
	if inspectErr != nil || !inspect.State.OOMKilled {
		return 0, fmt.Errorf("dockerbackend: read oom_kill: %w", err)
	}
	return 1, nil
}

// ---------------------------------------------------------------- files

// ReadFile reads a file through the archive API. Docker tar-wraps the bytes,
// so the single entry is unwrapped here.
func (b *Backend) ReadFile(ctx context.Context, containerID, path string) ([]byte, error) {
	rc, err := b.api.ArchiveGet(ctx, containerID, path)
	if err != nil {
		return nil, notFound(err, "%s", path)
	}
	defer func() { _ = rc.Close() }()
	data, err := dockerapi.ExtractFile(rc)
	if err != nil {
		return nil, fmt.Errorf("dockerbackend: read %s: %w", path, err)
	}
	return data, nil
}

// WriteFile writes a file through the archive API. The archive is extracted at
// "/" with the full relative path as its entry name, which makes Docker create
// every missing parent directory itself.
func (b *Backend) WriteFile(ctx context.Context, containerID, path string, data []byte) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("dockerbackend: path %q must be absolute", path)
	}
	tarBody, err := dockerapi.BuildFileTar(strings.TrimPrefix(path, "/"), data)
	if err != nil {
		return err
	}
	if err := b.api.ArchivePut(ctx, containerID, "/", tarBody); err != nil {
		return notFound(err, "%s", path)
	}
	return nil
}

// ---------------------------------------------------------------- helpers

// capture runs a command and returns its stdout, for the cgroup read.
func (b *Backend) capture(ctx context.Context, containerID string, command []string) ([]byte, error) {
	execID, err := b.api.ExecCreate(ctx, containerID, command)
	if err != nil {
		return nil, err
	}
	stream, err := b.api.ExecStart(ctx, execID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()

	var stdout, stderr bytes.Buffer
	if err := dockerapi.Demux(stream, &stdout, &stderr); err != nil {
		return nil, err
	}
	inspect, err := b.waitInspect(ctx, execID)
	if err != nil {
		return nil, err
	}
	if inspect.ExitCode != nil && *inspect.ExitCode != 0 {
		return nil, fmt.Errorf("cgroup read exited %d: %s", *inspect.ExitCode, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// waitInspect polls until the exec is no longer running and returns its final
// state.
func (b *Backend) waitInspect(ctx context.Context, execID string) (dockerapi.ExecInspect, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		inspect, err := b.api.ExecInspect(ctx, execID)
		if err == nil && !inspect.Running {
			return inspect, nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return dockerapi.ExecInspect{}, fmt.Errorf("dockerbackend: inspect exec %s: %w", execID, err)
			}
			return dockerapi.ExecInspect{}, fmt.Errorf("dockerbackend: exec %s still running after 10s", execID)
		}
		select {
		case <-ctx.Done():
			return dockerapi.ExecInspect{}, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// execWrapper records the process group leader's PID and then becomes the
// command, without a shell around the command itself: $0 is the PID file and
// "$@" is the argv passed after it, so no argument is ever parsed as shell
// syntax.
const execWrapper = `printf %s $$ > "$0" || exit 125; exec "$@"`

// wrapCommand builds the argv an exec runs for command. Writing the PID before
// the exec matters: the PID of the shell equals the PID of the process group
// that Docker created for the exec, and the exec keeps that PID and group.
func wrapCommand(pidPath string, command []string) []string {
	return append([]string{"/bin/sh", "-c", execWrapper, pidPath}, command...)
}

// nextPidPath returns a unique PID-file path. The counter keeps it unique per
// process; the random suffix keeps it unique per execution even if the process
// restarts while a container is reused from cleanup.
func (b *Backend) nextPidPath() string {
	b.mu.Lock()
	b.seq++
	n := b.seq
	b.mu.Unlock()
	var r [6]byte
	if _, err := rand.Read(r[:]); err != nil {
		return fmt.Sprintf("%s/%d-%d", PidDir, n, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s/%d-%s", PidDir, n, hex.EncodeToString(r[:]))
}

// notFound maps a Docker 404 to the daemon's sentinel so the contract answers
// not_found rather than backend_error.
func notFound(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	if dockerapi.IsNotFound(err) {
		return fmt.Errorf("dockerbackend: %s: %w", fmt.Sprintf(format, args...), daemon.ErrNotFound)
	}
	return fmt.Errorf("dockerbackend: %s: %w", fmt.Sprintf(format, args...), err)
}

// parsePid validates the PID file contents: decimal digits only, so a changed
// file cannot turn into shell syntax.
func parsePid(data []byte) (string, error) {
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "", errors.New("dockerbackend: pid file is empty")
	}
	if _, err := strconv.ParseUint(s, 10, 32); err != nil {
		return "", fmt.Errorf("dockerbackend: pid file %q is not a pid", s)
	}
	return s, nil
}
