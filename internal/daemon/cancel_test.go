package daemon_test

import (
	"context"
	"testing"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
)

// TestCancel covers the cancellation rules from api.md: terminate the process
// group with SIGTERM then SIGKILL, respond only once the execution is
// completed, leave a completed execution unchanged, and remove the container
// when the processes cannot be confirmed gone.
func TestCancel(t *testing.T) {
	ctx := context.Background()

	t.Run("running execution", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["block"] = memLongBlock
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"block"}})
		wantNoError(t, err)

		done, err := h.s.CancelExecution(ctx, sb.ID, ex.ID)
		wantNoError(t, err)
		if done.State != daemon.ExecCompleted {
			t.Fatalf("state = %s, want completed", done.State)
		}
		if done.TerminationReason == nil || *done.TerminationReason != daemon.ReasonCanceled {
			t.Fatalf("reason = %v, want canceled", done.TerminationReason)
		}
		sandbox, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if sandbox.State != daemon.StateRunning {
			t.Fatalf("sandbox state = %s, a cancel must not kill it", sandbox.State)
		}
	})

	t.Run("completed execution is returned unchanged", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["echo"] = memEcho
		sb := h.create(daemon.CreateRequest{})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"echo", "done"}})
		wantNoError(t, err)
		first := h.waitCompleted(sb.ID, ex.ID)

		again, err := h.s.CancelExecution(ctx, sb.ID, ex.ID)
		wantNoError(t, err)
		if again.State != daemon.ExecCompleted ||
			*again.TerminationReason != daemon.ReasonExited ||
			again.FinishedAt == nil || !again.FinishedAt.Equal(*first.FinishedAt) {
			t.Fatalf("cancel changed a completed execution: %+v", again)
		}
	})

	t.Run("paused sandbox is resumed first", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["block"] = memLongBlock
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"block"}})
		wantNoError(t, err)
		if _, err := h.s.Pause(ctx, sb.ID); err != nil {
			t.Fatalf("pause: %v", err)
		}

		done, err := h.s.CancelExecution(ctx, sb.ID, ex.ID)
		wantNoError(t, err)
		if *done.TerminationReason != daemon.ReasonCanceled {
			t.Fatalf("reason = %s, want canceled", *done.TerminationReason)
		}
		sandbox, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if sandbox.State != daemon.StateRunning {
			t.Fatalf("sandbox state = %s, want running after the resume", sandbox.State)
		}
	})

	t.Run("SIGKILL follows an ignored SIGTERM", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["block"] = memLongBlock
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"block"}})
		wantNoError(t, err)
		h.be.IgnoreTerm(h.containerOf(sb.ID))

		done, err := h.s.CancelExecution(ctx, sb.ID, ex.ID)
		wantNoError(t, err)
		if *done.TerminationReason != daemon.ReasonCanceled {
			t.Fatalf("reason = %s, want canceled", *done.TerminationReason)
		}
	})

	t.Run("unconfirmed cancel removes the container", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["block"] = memLongBlock
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"block"}})
		wantNoError(t, err)
		h.be.IgnoreSignals(h.containerOf(sb.ID))

		if _, err := h.s.CancelExecution(ctx, sb.ID, ex.ID); err != nil {
			t.Fatalf("cancel: %v", err)
		}

		sandbox, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if sandbox.State != daemon.StateKilled {
			t.Fatalf("sandbox state = %s, want killed after an unconfirmed cancel", sandbox.State)
		}
		if sandbox.KillReason == nil || *sandbox.KillReason != daemon.KillRequested {
			t.Fatalf("kill_reason = %v, want requested", sandbox.KillReason)
		}
		if h.be.State(h.containerOf(sb.ID)).Alive() {
			t.Fatal("container still alive after an unconfirmed cancel")
		}
	})

	t.Run("delete cancels a running execution", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["block"] = memLongBlock
		sb := h.create(daemon.CreateRequest{})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"block"}})
		wantNoError(t, err)

		if _, err := h.s.Delete(ctx, sb.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		done := h.waitCompleted(sb.ID, ex.ID)
		if *done.TerminationReason != daemon.ReasonCanceled {
			t.Fatalf("reason = %s, want canceled: deleting a sandbox cancels its execution", *done.TerminationReason)
		}
	})

	t.Run("cancel after a kill returns the stored record", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["block"] = memLongBlock
		sb := h.create(daemon.CreateRequest{})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"block"}})
		wantNoError(t, err)
		if _, err := h.s.Delete(ctx, sb.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		h.waitCompleted(sb.ID, ex.ID)

		got, err := h.s.CancelExecution(ctx, sb.ID, ex.ID)
		wantNoError(t, err)
		if got.State != daemon.ExecCompleted {
			t.Fatalf("state = %s, want the stored completed record", got.State)
		}
	})

	t.Run("signals reach the process group", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["block"] = memLongBlock
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"block"}})
		wantNoError(t, err)
		if _, err := h.s.CancelExecution(ctx, sb.ID, ex.ID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		proc := h.be.Process(h.containerOf(sb.ID))
		if proc == nil {
			t.Fatal("no process recorded")
		}
		signals := proc.Signals()
		if len(signals) == 0 || signals[0] != daemon.SignalTerm {
			t.Fatalf("signals = %v, want SIGTERM first", signals)
		}
	})
}

// TestExecutionStartsAreSerialized checks that a sandbox never runs two
// executions at once, which keeps container-wide OOM evidence attributable.
func TestExecutionStartsAreSerialized(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.be.Programs["block"] = memBlock
	sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})

	started := 0
	for i := 0; i < 20; i++ {
		_, created, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"block"}})
		if err == nil {
			if !created {
				t.Fatal("a successful start reported no new execution")
			}
			started++
			continue
		}
		wantCode(t, err, daemon.CodeBusy)
	}
	if started != 1 {
		t.Fatalf("%d starts succeeded, want exactly 1", started)
	}
	time.Sleep(100 * time.Millisecond)
}
