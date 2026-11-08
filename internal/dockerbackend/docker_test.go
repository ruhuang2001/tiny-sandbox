//go:build docker

// Package dockerbackend's real-Docker tests. They need a running Docker daemon
// and are excluded from ordinary runs:
//
//	go test -tags docker ./internal/dockerbackend/ -run TestReal -v
//
// Each test cleans up its own containers and asserts the daemon's container
// list is empty afterwards.
package dockerbackend

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/dockerapi"
)

// dockerSocket returns the daemon socket path, honouring DOCKER_HOST.
func dockerSocket(t *testing.T) string {
	t.Helper()
	if host := os.Getenv("DOCKER_HOST"); strings.HasPrefix(host, "unix://") {
		return strings.TrimPrefix(host, "unix://")
	}
	return "/var/run/docker.sock"
}

// realHarness is a daemon store over the real Docker backend, with an owner
// label unique to the test so cleanup can find everything it created.
type realHarness struct {
	t     *testing.T
	owner string
	api   *dockerapi.Client
	be    *Backend
	s     *daemon.Sandboxes
}

func newRealHarness(t *testing.T) *realHarness {
	t.Helper()
	sock := dockerSocket(t)
	if _, err := os.Stat(sock); err != nil {
		t.Skipf("docker socket %s: %v", sock, err)
	}
	api, err := dockerapi.Dial(context.Background(), sock)
	if err != nil {
		t.Skipf("docker daemon not reachable: %v", err)
	}
	owner := "test-" + t.Name()
	be := New(api)
	h := &realHarness{
		t:     t,
		owner: owner,
		api:   api,
		be:    be,
		s:     daemon.New(be, daemon.WithOwnerLabel(owner)),
	}
	t.Cleanup(func() {
		n, err := be.RemoveOwned(context.Background(), daemon.OwnerLabelKey, owner)
		if err != nil {
			t.Errorf("cleanup: %v", err)
		}
		t.Logf("cleanup removed %d containers", n)
		left, err := api.ContainerList(context.Background(), true, map[string][]string{
			"label": {daemon.OwnerLabelKey + "=" + owner},
		})
		if err != nil {
			t.Errorf("list after cleanup: %v", err)
		}
		if len(left) != 0 {
			t.Errorf("%d containers remain after cleanup", len(left))
		}
	})
	return h
}

func (h *realHarness) create() daemon.Sandbox {
	h.t.Helper()
	sb, err := h.s.Create(context.Background(), daemon.CreateRequest{
		Image:          ptr("alpine:3.20"),
		MemoryMB:       ptr(64),
		TimeoutSeconds: ptr(300),
	})
	if err != nil {
		h.t.Fatalf("create: %v", err)
	}
	return sb
}

func ptr[T any](v T) *T { return &v }

// exec runs a command and returns its execution ID.
func (h *realHarness) exec(sandboxID string, command ...string) string {
	h.t.Helper()
	ex, _, err := h.s.StartExecution(context.Background(), sandboxID, daemon.ExecuteRequest{Command: command})
	if err != nil {
		h.t.Fatalf("exec %v: %v", command, err)
	}
	return ex.ID
}

// output returns the retained stdout of an execution.
func (h *realHarness) output(sandboxID, executionID string) string {
	h.t.Helper()
	ex, err := h.s.GetExecution(sandboxID, executionID)
	if err != nil {
		h.t.Fatalf("get execution: %v", err)
	}
	return ex.Stdout
}

// waitFor polls cond until it holds, failing the test on timeout.
func (h *realHarness) waitFor(what string, timeout time.Duration, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s", what)
}

// TestRealCounterAdvancesAcrossPause proves pause freezes processes rather
// than only flipping a state field: a counter that prints while it runs must
// stop advancing while the sandbox is paused and continue after resume.
func TestRealCounterAdvancesAcrossPause(t *testing.T) {
	h := newRealHarness(t)
	sb := h.create()
	id := h.exec(sb.ID, "sh", "-c", "i=0; while true; do i=$((i+1)); echo $i; sleep 0.05; done")

	// Let a few lines accumulate.
	h.waitFor("initial output", 10*time.Second, func() bool {
		return strings.Count(h.output(sb.ID, id), "\n") >= 3
	})

	if _, err := h.s.Pause(context.Background(), sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // let an in-flight write land
	frozen := h.output(sb.ID, id)
	time.Sleep(700 * time.Millisecond)
	if after := h.output(sb.ID, id); after != frozen {
		t.Fatalf("output advanced while paused: %d -> %d bytes", len(frozen), len(after))
	}

	if _, err := h.s.Resume(context.Background(), sb.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	h.waitFor("output after resume", 10*time.Second, func() bool {
		return len(h.output(sb.ID, id)) > len(frozen)
	})

	if _, err := h.s.Delete(context.Background(), sb.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

// TestRealExternalKillIsEvicted stops the container behind the daemon's back
// with kill -9. The process exits 137, which must classify as evicted, never
// as oom: no intent was recorded and the cgroup counter did not move.
func TestRealExternalKillIsEvicted(t *testing.T) {
	h := newRealHarness(t)
	sb := h.create()
	id := h.exec(sb.ID, "sleep", "100")

	// The sandbox's container is the one carrying this test's owner label;
	// the daemon's in-memory record is not reachable from another package.
	list, err := h.api.ContainerList(context.Background(), true, map[string][]string{
		"label": {daemon.OwnerLabelKey + "=" + h.owner},
	})
	if err != nil || len(list) != 1 {
		t.Fatalf("find container: %v, %d containers", err, len(list))
	}
	container := list[0].ID
	// Externally kill the container's init process. This is the `docker kill`
	// an operator would type, not a daemon request, so nothing records intent.
	if err := h.api.ContainerKill(context.Background(), container, "KILL"); err != nil {
		t.Fatalf("external kill: %v", err)
	}

	h.waitFor("execution to complete", 10*time.Second, func() bool {
		ex, err := h.s.GetExecution(sb.ID, id)
		return err == nil && ex.State == daemon.ExecCompleted
	})
	ex, err := h.s.GetExecution(sb.ID, id)
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}
	if ex.TerminationReason == nil || *ex.TerminationReason != daemon.ReasonEvicted {
		t.Fatalf("termination_reason = %v, want evicted", ex.TerminationReason)
	}
	if ex.ExitCode == nil || *ex.ExitCode != 137 {
		t.Logf("exit code = %v (137 expected but not required for eviction)", ex.ExitCode)
	}

	// The eviction sweep must also kill the sandbox itself.
	h.waitFor("sandbox to be killed", 10*time.Second, func() bool {
		got, err := h.s.Get(sb.ID)
		return err == nil && got.State == daemon.StateKilled
	})
	got, err := h.s.Get(sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if got.KillReason == nil || *got.KillReason != daemon.KillEvicted {
		t.Fatalf("kill_reason = %v, want evicted", got.KillReason)
	}
}

// TestRealOOMIsOOM proves the cgroup counter distinguishes the kernel OOM
// killer from every other 137: a process that exceeds the container limit is
// killed by the kernel, and the delta attributes it to the execution.
func TestRealOOMIsOOM(t *testing.T) {
	h := newRealHarness(t)
	sb, err := h.s.Create(context.Background(), daemon.CreateRequest{
		Image:          ptr("python:3.12-alpine"),
		MemoryMB:       ptr(64),
		TimeoutSeconds: ptr(120),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Allocate 256 MiB of anonymous memory in a 64 MiB container. Anonymous
	// pages cannot be reclaimed the way page cache can, so the kernel kills
	// the process instead of dropping data. A file the page cache owns would
	// be reclaimed and the command would exit 0.
	id := h.exec(sb.ID, "python3", "-c", "x = bytearray(256 * 1024 * 1024)")

	h.waitFor("execution to complete", 30*time.Second, func() bool {
		ex, err := h.s.GetExecution(sb.ID, id)
		return err == nil && ex.State == daemon.ExecCompleted
	})
	ex, err := h.s.GetExecution(sb.ID, id)
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}
	if ex.TerminationReason == nil || *ex.TerminationReason != daemon.ReasonOOM {
		reason := "nil"
		if ex.TerminationReason != nil {
			reason = string(*ex.TerminationReason)
		}
		exit := -1
		if ex.ExitCode != nil {
			exit = *ex.ExitCode
		}
		t.Fatalf("termination_reason = %s, want oom (exit %d, stderr %q)", reason, exit, ex.Stderr)
	}
}
