package daemon_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
)

// TestLiveSandboxLimit pins admission: a create beyond MaxSandboxes is
// rate_limited and starts no container, so an agent loop that never deletes
// cannot exhaust the host one sandbox at a time.
func TestLiveSandboxLimit(t *testing.T) {
	h := newHarness(t)
	h.s = daemon.New(h.be, daemon.WithClock(h.clock), daemon.WithMaxSandboxes(2))

	h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})
	h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})

	before := len(h.be.IDs())
	_, err := h.s.Create(context.Background(), daemon.CreateRequest{TimeoutSeconds: ptr(0)})
	wantCode(t, err, daemon.CodeRateLimited)
	if after := len(h.be.IDs()); after != before {
		t.Fatalf("a rejected create started a container: %d containers, want %d", after, before)
	}

	// A killed sandbox holds no container, so its slot comes back.
	if _, err := h.s.Delete(context.Background(), h.s.List()[0].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := h.s.Create(context.Background(), daemon.CreateRequest{TimeoutSeconds: ptr(0)}); err != nil {
		t.Fatalf("create after delete: %v", err)
	}
}

// TestKilledRecordsArePruned pins MaxRetainedSandboxes. Killed records are kept
// for idempotent delete and history, but they are dropped oldest first once the
// bound is exceeded, so a run of one-task sandboxes cannot hold the process's
// memory forever.
func TestKilledRecordsArePruned(t *testing.T) {
	h := newHarness(t)
	h.s = daemon.New(h.be, daemon.WithClock(h.clock), daemon.WithMaxRetainedSandboxes(2))

	var ids []string
	for i := 0; i < 5; i++ {
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})
		ids = append(ids, sb.ID)
		if _, err := h.s.Delete(context.Background(), sb.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		h.s.RetentionForTest()
	}

	list := h.s.List()
	if len(list) != 2 {
		t.Fatalf("retained %d killed records, want 2", len(list))
	}
	// The survivors are the newest two.
	if list[0].ID != ids[4] || list[1].ID != ids[3] {
		t.Fatalf("retained %s and %s, want the two newest", list[0].ID, list[1].ID)
	}
	// The oldest records are gone, not merely invisible.
	if _, err := h.s.Get(ids[0]); err == nil {
		t.Fatal("a pruned record is still readable")
	} else {
		wantCode(t, err, daemon.CodeNotFound)
	}
}

// TestOutputBudgetIsTotal pins MaxRetainedOutputBytes: per-execution limits
// multiply by history, so the process also bounds the total. Dropping output
// drops the whole execution, oldest first, and keeps the newest result.
func TestOutputBudgetIsTotal(t *testing.T) {
	h := newHarness(t)
	// Two executions of 32 KiB fit; 20 do not.
	h.s = daemon.New(h.be, daemon.WithClock(h.clock), daemon.WithMaxRetainedOutputBytes(64<<10))
	h.be.Streamers["spew"] = func(_ context.Context, write func([]byte, []byte) error) int {
		chunk := []byte(strings.Repeat("x", daemon.MaxEventDataBytes))
		if err := write(chunk, nil); err != nil {
			return 1
		}
		return 0
	}
	sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})

	newestID := ""
	for i := 0; i < 20; i++ {
		ex, _, err := h.s.StartExecution(context.Background(), sb.ID, daemon.ExecuteRequest{Command: []string{"spew"}})
		wantNoError(t, err)
		newestID = ex.ID
		h.waitCompleted(sb.ID, ex.ID)
		h.s.RetentionForTest()
	}

	if got := h.s.RetainedBytesForTest(); got > 64<<10 {
		t.Fatalf("retained %d bytes, budget is %d", got, 64<<10)
	}
	// The newest execution is never dropped: a caller polling for its result
	// must find it.
	got, err := h.s.GetExecution(sb.ID, newestID)
	wantNoError(t, err)
	if len(got.Stdout) != daemon.MaxEventDataBytes {
		t.Fatalf("newest execution kept %d bytes, want %d", len(got.Stdout), daemon.MaxEventDataBytes)
	}
	// And what is retained is still complete and readable.
	if len(got.Stdout) > 0 && got.Stdout[0] != 'x' {
		t.Fatalf("retained output is corrupt: %q", got.Stdout[:1])
	}
}

// TestShutdownRemovesContainers pins graceful shutdown: containers belong to
// this process's in-memory state, so an ordinary stop removes them rather than
// leaving them for a later owner-label sweep that a changed port would miss.
func TestShutdownRemovesContainers(t *testing.T) {
	h := newHarness(t)
	first := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})
	second := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})
	containers := []string{h.containerOf(first.ID), h.containerOf(second.ID)}

	h.s.Shutdown(context.Background())

	for _, id := range containers {
		if state := h.be.State(id); state.Alive() {
			t.Fatalf("container %s is %s after shutdown, want removed", id, state)
		}
	}
	if list := h.s.List(); len(list) != 0 {
		t.Fatalf("shutdown left %d records", len(list))
	}
}
