package daemon_test

import (
	"context"
	"testing"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
)

// TestLifetimeTimeout covers the lifetime timeout rules in api.md: count from
// creation, keep counting while paused with a kill action, count only while
// running with a pause action, and arm a fresh interval on every resume.
func TestLifetimeTimeout(t *testing.T) {
	ctx := context.Background()

	t.Run("kill", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(10)})
		h.clock.Advance(9 * time.Second)
		h.s.Reap(ctx)
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateRunning {
			t.Fatalf("state before the deadline = %s, want running", got.State)
		}

		h.clock.Advance(time.Second)
		h.s.Reap(ctx)
		got, err = h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateKilled || got.KillReason == nil || *got.KillReason != daemon.KillTimeout {
			t.Fatalf("state = %s, kill_reason = %v; want killed/timeout", got.State, got.KillReason)
		}
		if got.ExpiresAt != nil {
			t.Fatalf("expires_at = %v, want null", got.ExpiresAt)
		}
	})

	t.Run("kill keeps counting while paused", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(10)})
		if _, err := h.s.Pause(ctx, sb.ID); err != nil {
			t.Fatalf("pause: %v", err)
		}
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.ExpiresAt == nil {
			t.Fatal("expires_at is null while paused under a kill action; the deadline keeps counting")
		}

		h.clock.Advance(10 * time.Second)
		h.s.Reap(ctx)
		got, err = h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateKilled || *got.KillReason != daemon.KillTimeout {
			t.Fatalf("state = %s, kill_reason = %v; want killed/timeout from paused", got.State, got.KillReason)
		}
	})

	t.Run("pause does not count while paused", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{
			TimeoutSeconds: ptr(10),
			TimeoutAction:  ptr(daemon.ActionPause),
		})
		if _, err := h.s.Pause(ctx, sb.ID); err != nil {
			t.Fatalf("pause: %v", err)
		}
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.ExpiresAt != nil {
			t.Fatalf("expires_at = %v while paused under a pause action, want null", got.ExpiresAt)
		}

		// Well past the original deadline: a paused pause-action sandbox is
		// not counting, so the reaper must leave it alone.
		h.clock.Advance(time.Hour)
		h.s.Reap(ctx)
		got, err = h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StatePaused {
			t.Fatalf("state = %s, want paused", got.State)
		}
	})

	t.Run("pause fires at the deadline", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{
			TimeoutSeconds: ptr(10),
			TimeoutAction:  ptr(daemon.ActionPause),
		})
		h.clock.Advance(10 * time.Second)
		h.s.Reap(ctx)
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StatePaused {
			t.Fatalf("state = %s, want paused", got.State)
		}
		if got.ExpiresAt != nil {
			t.Fatalf("expires_at = %v, want null after a timeout pause", got.ExpiresAt)
		}
	})

	t.Run("resume after a timeout pause starts a fresh interval", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{
			TimeoutSeconds: ptr(10),
			TimeoutAction:  ptr(daemon.ActionPause),
		})
		h.clock.Advance(10 * time.Second)
		h.s.Reap(ctx)

		h.clock.Advance(5 * time.Second) // still paused, not counting
		got, err := h.s.Resume(ctx, sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateRunning {
			t.Fatalf("state = %s, want running", got.State)
		}
		wantDeadline := h.clock.Now().Add(10 * time.Second)
		if got.ExpiresAt == nil || !got.ExpiresAt.Equal(wantDeadline) {
			t.Fatalf("expires_at = %v, want %v (now + timeout)", got.ExpiresAt, wantDeadline)
		}

		// An automatic resume (starting a command) arms a fresh deadline too,
		// otherwise it would expire again immediately.
		if _, err := h.s.Pause(ctx, sb.ID); err != nil {
			t.Fatalf("pause: %v", err)
		}
		h.clock.Advance(time.Hour)
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"true"}})
		wantNoError(t, err)
		after, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if after.ExpiresAt == nil || !after.ExpiresAt.After(h.clock.Now()) {
			t.Fatalf("expires_at = %v after an automatic resume, want a future deadline", after.ExpiresAt)
		}
		h.waitCompleted(sb.ID, ex.ID)
	})

	t.Run("zero disables the deadline", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})
		h.clock.Advance(1000 * time.Hour)
		h.s.Reap(ctx)
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateRunning {
			t.Fatalf("state = %s, want running with the timeout disabled", got.State)
		}
	})

	t.Run("a running execution times out with the sandbox", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["block"] = memBlock
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(10)})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"block"}})
		wantNoError(t, err)

		h.clock.Advance(10 * time.Second)
		h.s.Reap(ctx)

		done := h.waitCompleted(sb.ID, ex.ID)
		if *done.TerminationReason != daemon.ReasonTimeout {
			t.Fatalf("reason = %s, want timeout", *done.TerminationReason)
		}
		sandbox, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if sandbox.State != daemon.StateKilled || *sandbox.KillReason != daemon.KillTimeout {
			t.Fatalf("sandbox = %+v, want killed/timeout", sandbox)
		}
	})

	t.Run("reads never change state", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(10)})
		h.clock.Advance(time.Hour)
		for range 5 {
			got, err := h.s.Get(sb.ID)
			wantNoError(t, err)
			if got.State != daemon.StateRunning {
				t.Fatalf("a read changed the state to %s", got.State)
			}
		}
	})
}

// TestIdleTimeout covers the idle rules: idle while running with no execution
// and no stream, measured from the latest activity.
func TestIdleTimeout(t *testing.T) {
	ctx := context.Background()

	t.Run("kill", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{IdleTimeoutSeconds: ptr(10)})
		h.clock.Advance(9 * time.Second)
		h.s.Reap(ctx)
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateRunning {
			t.Fatalf("state = %s before the idle deadline, want running", got.State)
		}

		h.clock.Advance(time.Second)
		h.s.Reap(ctx)
		got, err = h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateKilled || *got.KillReason != daemon.KillIdleTimeout {
			t.Fatalf("state = %s, kill_reason = %v; want killed/idle_timeout", got.State, got.KillReason)
		}
	})

	t.Run("pause", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{
			IdleTimeoutSeconds: ptr(10),
			IdleTimeoutAction:  ptr(daemon.ActionPause),
		})
		h.clock.Advance(10 * time.Second)
		h.s.Reap(ctx)
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StatePaused {
			t.Fatalf("state = %s, want paused", got.State)
		}
	})

	t.Run("requests reset the idle clock", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{IdleTimeoutSeconds: ptr(10)})
		h.clock.Advance(9 * time.Second)
		h.s.Touch(sb.ID)
		h.clock.Advance(9 * time.Second)
		h.s.Reap(ctx)
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateRunning {
			t.Fatalf("state = %s after activity, want running", got.State)
		}
	})

	t.Run("an open stream is activity", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{IdleTimeoutSeconds: ptr(10), TimeoutSeconds: ptr(0)})
		closeStream, err := h.s.OpenStream(sb.ID)
		wantNoError(t, err)
		h.clock.Advance(time.Hour)
		h.s.Reap(ctx)
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateRunning {
			t.Fatalf("state = %s with an open stream, want running", got.State)
		}

		closeStream()
		h.clock.Advance(10 * time.Second)
		h.s.Reap(ctx)
		got, err = h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateKilled {
			t.Fatalf("state = %s after the stream closed and idled, want killed", got.State)
		}
	})

	t.Run("a running execution is not idle", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["block"] = memLongBlock
		sb := h.create(daemon.CreateRequest{IdleTimeoutSeconds: ptr(10), TimeoutSeconds: ptr(0)})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"block"}})
		wantNoError(t, err)

		h.clock.Advance(time.Hour)
		h.s.Reap(ctx)
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateRunning {
			t.Fatalf("state = %s while an execution runs, want running", got.State)
		}
		if _, err := h.s.CancelExecution(ctx, sb.ID, ex.ID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
	})

	t.Run("idle time runs from the end of the last execution", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["exit"] = memExit
		sb := h.create(daemon.CreateRequest{IdleTimeoutSeconds: ptr(10)})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"exit", "0"}})
		wantNoError(t, err)
		h.waitCompleted(sb.ID, ex.ID)

		h.clock.Advance(9 * time.Second)
		h.s.Reap(ctx)
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateRunning {
			t.Fatalf("state = %s, want running", got.State)
		}
		h.clock.Advance(2 * time.Second)
		h.s.Reap(ctx)
		got, err = h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateKilled {
			t.Fatalf("state = %s, want killed", got.State)
		}
	})

	t.Run("disabled by default", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})
		h.clock.Advance(1000 * time.Hour)
		h.s.Reap(ctx)
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateRunning {
			t.Fatalf("state = %s, want running with idle_timeout disabled", got.State)
		}
	})
}

// TestEvictSweep covers detection of a container stopped externally, which
// must surface within the five-second budget without a request arriving.
func TestEvictSweep(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	sb := h.create(daemon.CreateRequest{})
	h.be.Kill(h.containerOf(sb.ID))

	h.s.EvictSweep(ctx)

	got, err := h.s.Get(sb.ID)
	wantNoError(t, err)
	if got.State != daemon.StateKilled || got.KillReason == nil || *got.KillReason != daemon.KillEvicted {
		t.Fatalf("sandbox = %+v, want killed/evicted", got)
	}
}

// TestReaperRun drives the periodic loop, which is what applies timeouts in
// the running server.
func TestReaperRun(t *testing.T) {
	h := newHarness(t)
	sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(1)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.s.Run(ctx)

	h.clock.Advance(time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State == daemon.StateKilled {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the reaper did not apply the lifetime timeout")
}
