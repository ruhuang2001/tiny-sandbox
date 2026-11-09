package dockerbackend

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
)

func TestCreateSendsLimitsAndLabels(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)

	id, err := b.Create(context.Background(), daemon.ContainerSpec{
		Image:    "python:3.12-alpine",
		MemoryMB: 256,
		Name:     "tiny-sandbox-sbx_1",
		Labels:   map[string]string{daemon.OwnerLabelKey: "127.0.0.1:8080"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == "" {
		t.Fatal("create returned an empty container id")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	cfg := f.creates[0]
	if cfg.Image != "python:3.12-alpine" {
		t.Errorf("image = %q", cfg.Image)
	}
	if want := int64(256 << 20); cfg.HostConfig.Memory != want {
		t.Errorf("memory = %d, want %d", cfg.HostConfig.Memory, want)
	}
	// Swap must equal the memory limit, or the sandbox exceeds the limit the
	// user declared by swapping.
	if cfg.HostConfig.MemorySwap != cfg.HostConfig.Memory {
		t.Errorf("memory_swap = %d, want %d", cfg.HostConfig.MemorySwap, cfg.HostConfig.Memory)
	}
	if _, ok := cfg.HostConfig.Tmpfs[PidDir]; !ok {
		t.Errorf("tmpfs %s is not mounted: %v", PidDir, cfg.HostConfig.Tmpfs)
	}
	if cfg.HostConfig.PidsLimit == nil {
		t.Fatal("pids limit is unset: an unchecked fork bomb reaches the host")
	}
	if *cfg.HostConfig.PidsLimit != DefaultPidsLimit {
		t.Errorf("pids_limit = %d, want %d", *cfg.HostConfig.PidsLimit, DefaultPidsLimit)
	}
	if !slices.Contains(cfg.HostConfig.CapDrop, "NET_RAW") {
		t.Errorf("cap_drop = %v, want NET_RAW among them", cfg.HostConfig.CapDrop)
	}
	// Device-node creation is the other capability a sandbox must not have:
	// MKNOD would let a command reach host devices.
	if !slices.Contains(cfg.HostConfig.CapDrop, "MKNOD") {
		t.Errorf("cap_drop = %v, want MKNOD among them", cfg.HostConfig.CapDrop)
	}
	if !slices.Contains(cfg.HostConfig.SecurityOpt, "no-new-privileges") {
		t.Errorf("security_opt = %v, want no-new-privileges", cfg.HostConfig.SecurityOpt)
	}
	// PID 1 has to reap orphans. Without an init process, a child whose
	// parent exits stays a zombie and keeps its pids slot forever, so a
	// sandbox that forked once eventually cannot fork again.
	if cfg.HostConfig.Init == nil || !*cfg.HostConfig.Init {
		t.Errorf("init = %v, want an init process as PID 1 so orphans are reaped", cfg.HostConfig.Init)
	}
	// An unset network means bridge: the default must not silently isolate a
	// sandbox whose request never mentioned networking.
	if cfg.HostConfig.NetworkMode != string(daemon.NetworkBridge) {
		t.Errorf("network_mode = %q, want %q", cfg.HostConfig.NetworkMode, daemon.NetworkBridge)
	}
	if cfg.Labels[daemon.OwnerLabelKey] != "127.0.0.1:8080" {
		t.Errorf("labels = %v", cfg.Labels)
	}
	if f.createNames[0] != "tiny-sandbox-sbx_1" {
		t.Errorf("name = %q", f.createNames[0])
	}
}

// TestCreateAppliesSpecLimits pins the request-driven values, which the
// defaults test cannot see: a limit the caller asked for must reach Docker,
// and an explicit network mode must override the bridge default.
func TestCreateAppliesSpecLimits(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)

	if _, err := b.Create(context.Background(), daemon.ContainerSpec{
		Image:     "alpine:3.20",
		MemoryMB:  64,
		PidsLimit: 256,
		NanoCPUs:  1_500_000_000,
		Network:   daemon.NetworkNone,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	host := f.creates[0].HostConfig
	if host.PidsLimit == nil || *host.PidsLimit != 256 {
		t.Errorf("pids_limit = %v, want 256", host.PidsLimit)
	}
	if host.NanoCpus != 1_500_000_000 {
		t.Errorf("nano_cpus = %d, want 1500000000", host.NanoCpus)
	}
	if host.NetworkMode != string(daemon.NetworkNone) {
		t.Errorf("network_mode = %q, want %q", host.NetworkMode, daemon.NetworkNone)
	}
}

// A spec without a pids limit still gets the default. The request layer
// resolves it, but a backend used directly must not be able to create an
// unbounded container by omission.
func TestCreateDefaultsPidsLimitWhenUnset(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)

	if _, err := b.Create(context.Background(), daemon.ContainerSpec{Image: "alpine:3.20", MemoryMB: 64}); err != nil {
		t.Fatalf("create: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	host := f.creates[0].HostConfig
	if host.PidsLimit == nil || *host.PidsLimit != DefaultPidsLimit {
		t.Fatalf("pids_limit = %v, want the default %d", host.PidsLimit, DefaultPidsLimit)
	}
}

func TestCreateRemovesContainerWhenStartFails(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	f.failStart = true

	_, err := b.Create(context.Background(), daemon.ContainerSpec{Image: "alpine:3.20", MemoryMB: 64})
	if err == nil {
		t.Fatal("create succeeded, want a start failure")
	}
	if got := f.removedContainers(); len(got) != 1 {
		t.Fatalf("removed %v, want the started-then-failed container", got)
	}
}

// A clean host has no image, and the Engine API's create does not pull one.
// Without this the server works on a developer machine and fails everywhere
// else; CI caught exactly that.
func TestCreatePullsMissingImage(t *testing.T) {
	f := newFakeDaemon(t)
	f.mu.Lock()
	f.missingImages["python:3.12-alpine"] = true
	f.mu.Unlock()

	b := f.backend(t)
	id, err := b.Create(context.Background(), daemon.ContainerSpec{Image: "python:3.12-alpine", MemoryMB: 64})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == "" {
		t.Fatal("create returned no container id")
	}
	f.mu.Lock()
	pulled := append([]string(nil), f.pulled...)
	f.mu.Unlock()
	if len(pulled) != 1 || pulled[0] != "python:3.12-alpine" {
		t.Fatalf("pulled %v, want [python:3.12-alpine]", pulled)
	}
}

func TestCreateReportsPullFailure(t *testing.T) {
	f := newFakeDaemon(t)
	f.mu.Lock()
	f.missingImages["private/nope:1"] = true
	f.pullError = "pull access denied for private/nope"
	f.mu.Unlock()

	b := f.backend(t)
	if _, err := b.Create(context.Background(), daemon.ContainerSpec{Image: "private/nope:1", MemoryMB: 64}); err == nil {
		t.Fatal("create succeeded, want the pull failure")
	}
}

func TestCreateDoesNotPullAnImageThatExists(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	if _, err := b.Create(context.Background(), daemon.ContainerSpec{Image: "alpine:3.20", MemoryMB: 64}); err != nil {
		t.Fatalf("create: %v", err)
	}
	f.mu.Lock()
	pulled := len(f.pulled)
	f.mu.Unlock()
	if pulled != 0 {
		t.Errorf("%d pulls for a present image, want 0", pulled)
	}
}

func TestCreateRetriesAfterNameConflict(t *testing.T) {
	f := newFakeDaemon(t)
	// A leftover container already owns the name.
	f.mu.Lock()
	f.nameTaken = "tiny-sandbox-sbx_1"
	f.mu.Unlock()

	b := f.backend(t)
	id, err := b.Create(context.Background(), daemon.ContainerSpec{
		Image: "alpine:3.20", MemoryMB: 64, Name: "tiny-sandbox-sbx_1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == "" || id == "ctr_leftover" {
		t.Fatalf("id = %q, want a fresh container", id)
	}
	// The backend removes the leftover by name; the daemon resolves that to
	// the container's ID, which is what the record shows.
	removed := f.removedContainers()
	if len(removed) != 1 || removed[0] != "ctr_leftover" {
		t.Errorf("removed %v, want the leftover that held the name", removed)
	}
}

func TestLivenessMatchesExactID(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)

	running, _ := b.Create(context.Background(), daemon.ContainerSpec{Image: "alpine:3.20", MemoryMB: 64})
	paused, _ := b.Create(context.Background(), daemon.ContainerSpec{Image: "alpine:3.20", MemoryMB: 64})
	stopped, _ := b.Create(context.Background(), daemon.ContainerSpec{Image: "alpine:3.20", MemoryMB: 64})

	if err := b.Pause(context.Background(), paused); err != nil {
		t.Fatalf("pause: %v", err)
	}
	f.mu.Lock()
	f.containers[stopped].state = "exited"
	f.mu.Unlock()

	// A decoy whose ID extends another container's ID must not be confused
	// with it: Docker's id filter matches on prefix.
	f.mu.Lock()
	f.containers[running+"0"] = &fakeContainer{id: running + "0", state: "running"}
	f.mu.Unlock()

	cases := []struct {
		container string
		want      daemon.Liveness
	}{
		{running, daemon.LivenessRunning},
		{paused, daemon.LivenessPaused},
		{stopped, daemon.LivenessStopped},
		{"ctr_does_not_exist", daemon.LivenessMissing},
	}
	for _, tc := range cases {
		got, err := b.Liveness(context.Background(), tc.container)
		if err != nil {
			t.Fatalf("liveness %s: %v", tc.container, err)
		}
		if got != tc.want {
			t.Errorf("liveness %s = %s, want %s", tc.container, got, tc.want)
		}
	}
}

func TestExecRunsWrappedCommandAndDemuxes(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	id, err := b.Create(context.Background(), daemon.ContainerSpec{Image: "alpine:3.20", MemoryMB: 64})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	f.setProgram(func(string, []string) (string, string, int) {
		return "out\n", "err\n", 3
	})

	proc, err := b.Exec(context.Background(), id, []string{"python3", "-c", "print(1)"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	stdout, _ := io.ReadAll(proc.Stdout())
	stderr, _ := io.ReadAll(proc.Stderr())
	code, err := proc.Wait()
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if string(stdout) != "out\n" || string(stderr) != "err\n" {
		t.Errorf("stdout=%q stderr=%q", stdout, stderr)
	}
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}

	cmd := f.lastExecCommand()
	pidPath, actual, ok := unwrap(cmd)
	if !ok {
		t.Fatalf("command was not wrapped: %v", cmd)
	}
	if !strings.HasPrefix(pidPath, PidDir+"/") {
		t.Errorf("pid file %q is outside %s", pidPath, PidDir)
	}
	if strings.Join(actual, " ") != "python3 -c print(1)" {
		t.Errorf("real command = %v", actual)
	}
}

func TestExecOutputSurvivesRequestContextCancel(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	id, err := b.Create(context.Background(), daemon.ContainerSpec{Image: "alpine:3.20", MemoryMB: 64})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	f.setProgram(func(string, []string) (string, string, int) {
		time.Sleep(100 * time.Millisecond) // let the caller cancel first
		return "slow output", "", 0
	})

	ctx, cancel := context.WithCancel(context.Background())
	proc, err := b.Exec(ctx, id, []string{"slow"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	cancel() // models net/http cancelling the handler's context on return

	out, _ := io.ReadAll(proc.Stdout())
	if string(out) != "slow output" {
		t.Errorf("stdout = %q, want the output after the caller's context ended", out)
	}
}

func TestSignalKillsProcessGroup(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	id, err := b.Create(context.Background(), daemon.ContainerSpec{Image: "alpine:3.20", MemoryMB: 64})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	proc, err := b.Exec(context.Background(), id, []string{"sleep", "100"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}

	for _, sig := range []daemon.Signal{daemon.SignalTerm, daemon.SignalKill} {
		if err := proc.Signal(sig); err != nil {
			t.Fatalf("signal %s: %v", sig, err)
		}
		want := "kill -TERM -4242"
		if sig == daemon.SignalKill {
			want = "kill -KILL -4242"
		}
		got := f.lastExecCommand()
		if len(got) != 3 || got[2] != want {
			t.Errorf("signal command = %v, want %q", got, want)
		}
	}
}

func TestSignalWaitsForPidFile(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	id, err := b.Create(context.Background(), daemon.ContainerSpec{Image: "alpine:3.20", MemoryMB: 64})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	f.noPidFile = true
	proc, err := b.Exec(context.Background(), id, []string{"sleep", "100"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}

	// The wrapper has not written its file yet. Signal must keep asking
	// instead of reporting a missing process.
	go func() {
		time.Sleep(30 * time.Millisecond)
		f.mu.Lock()
		f.noPidFile = false
		c := f.containers[id]
		for p := range c.files {
			if strings.HasPrefix(p, PidDir+"/") {
				c.files[p] = []byte("4242\n")
			}
		}
		f.mu.Unlock()
	}()
	if err := proc.Signal(daemon.SignalTerm); err != nil {
		t.Fatalf("signal: %v", err)
	}
	if got := f.lastExecCommand(); len(got) != 3 || got[2] != "kill -TERM -4242" {
		t.Errorf("signal command = %v", got)
	}
}

func TestOOMKillCount(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	id, err := b.Create(context.Background(), daemon.ContainerSpec{Image: "alpine:3.20", MemoryMB: 64})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	n, err := b.OOMKillCount(context.Background(), id)
	if err != nil {
		t.Fatalf("oom count: %v", err)
	}
	if n != 5 {
		t.Errorf("oom_kill = %d, want 5", n)
	}
}

func TestOOMKillCountFailsOnBadCgroup(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	id, err := b.Create(context.Background(), daemon.ContainerSpec{Image: "alpine:3.20", MemoryMB: 64})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	f.setProgram(func(string, []string) (string, string, int) { return "", "no cgroup", 3 })
	if _, err := b.OOMKillCount(context.Background(), id); err == nil {
		t.Fatal("oom count succeeded, want the read failure")
	}
}

func TestFilesRoundTrip(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	id, err := b.Create(context.Background(), daemon.ContainerSpec{Image: "alpine:3.20", MemoryMB: 64})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := b.WriteFile(context.Background(), id, "/home/user/deep/dir/f.txt", []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := f.file(id, "/home/user/deep/dir/f.txt"); string(got) != "hello" {
		t.Fatalf("stored %q, want hello", got)
	}
	data, err := b.ReadFile(context.Background(), id, "/home/user/deep/dir/f.txt")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("read %q, want hello", data)
	}
}

func TestMissingFileIsNotFound(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	id, err := b.Create(context.Background(), daemon.ContainerSpec{Image: "alpine:3.20", MemoryMB: 64})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = b.ReadFile(context.Background(), id, "/nope.txt")
	if !errors.Is(err, daemon.ErrNotFound) {
		t.Fatalf("err = %v, want daemon.ErrNotFound", err)
	}
}

func TestWriteFileRequiresAbsolutePath(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	id, err := b.Create(context.Background(), daemon.ContainerSpec{Image: "alpine:3.20", MemoryMB: 64})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := b.WriteFile(context.Background(), id, "relative.txt", []byte("x")); err == nil {
		t.Fatal("write succeeded, want a path error")
	}
}

func TestPauseUnpauseIdempotent(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	id, err := b.Create(context.Background(), daemon.ContainerSpec{Image: "alpine:3.20", MemoryMB: 64})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for range 2 {
		if err := b.Pause(context.Background(), id); err != nil {
			t.Fatalf("pause: %v", err)
		}
		if err := b.Unpause(context.Background(), id); err != nil {
			t.Fatalf("unpause: %v", err)
		}
	}
}

func TestRemoveMissingContainerIsNotFound(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	if err := b.Remove(context.Background(), "ctr_gone"); !errors.Is(err, daemon.ErrNotFound) {
		t.Fatalf("err = %v, want daemon.ErrNotFound", err)
	}
}

func TestRemoveOwnedRemovesOnlyItsLabel(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	for range 2 {
		if _, err := b.Create(context.Background(), daemon.ContainerSpec{
			Image: "alpine:3.20", MemoryMB: 64,
			Labels: map[string]string{daemon.OwnerLabelKey: "127.0.0.1:8080"},
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	if _, err := b.Create(context.Background(), daemon.ContainerSpec{
		Image: "alpine:3.20", MemoryMB: 64,
		Labels: map[string]string{daemon.OwnerLabelKey: "127.0.0.1:9999"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	n, err := b.RemoveOwned(context.Background(), daemon.OwnerLabelKey, "127.0.0.1:8080")
	if err != nil {
		t.Fatalf("remove owned: %v", err)
	}
	if n != 2 {
		t.Errorf("removed %d, want 2", n)
	}
}

func TestParsePidRejectsNonNumeric(t *testing.T) {
	for _, in := range []string{"", "  ", "abc", "12; rm -rf /", "-5", "12 34"} {
		if _, err := parsePid([]byte(in)); err == nil {
			t.Errorf("parsePid(%q) succeeded, want an error", in)
		}
	}
	if pid, err := parsePid([]byte(" 4242\n")); err != nil || pid != "4242" {
		t.Errorf("parsePid(4242) = %q, %v", pid, err)
	}
}

// waitFor is a small helper for tests that wait on a condition.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

var (
	_ = bytes.Equal
	_ = waitFor
)
