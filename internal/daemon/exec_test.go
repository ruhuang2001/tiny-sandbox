package daemon_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
)

var errBoom = errors.New("boom")

// TestExecutions covers the execution resource rules from api.md.
func TestExecutions(t *testing.T) {
	ctx := context.Background()

	t.Run("newest first and summary omits output", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["echo"] = memEcho
		sb := h.create(daemon.CreateRequest{})
		ex, created, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"echo", "hello"}})
		wantNoError(t, err)
		if !created {
			t.Fatal("first start reported no new execution")
		}
		done := h.waitCompleted(sb.ID, ex.ID)
		if done.Stdout != "hello\n" {
			t.Fatalf("stdout = %q, want hello\\n", done.Stdout)
		}

		list, err := h.s.ListExecutions(sb.ID)
		wantNoError(t, err)
		if len(list) != 1 || list[0].ID != ex.ID {
			t.Fatalf("executions = %+v", list)
		}
		summary := list[0]
		if summary.State != daemon.ExecCompleted || summary.ExitCode == nil || *summary.ExitCode != 0 {
			t.Fatalf("summary = %+v", summary)
		}
		if summary.TerminationReason == nil || *summary.TerminationReason != daemon.ReasonExited {
			t.Fatalf("reason = %v, want exited", summary.TerminationReason)
		}
	})

	t.Run("empty command is invalid", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{})
		_, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{})
		wantCode(t, err, daemon.CodeInvalidRequest)
	})

	t.Run("idempotency key too long", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{})
		_, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{
			Command:        []string{"true"},
			IdempotencyKey: strings.Repeat("k", daemon.MaxIdempotencyKey+1),
		})
		wantCode(t, err, daemon.CodeInvalidRequest)
	})

	t.Run("reused key returns the existing execution", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{})
		req := daemon.ExecuteRequest{Command: []string{"sleep"}, IdempotencyKey: "k1"}
		first, created, err := h.s.StartExecution(ctx, sb.ID, req)
		wantNoError(t, err)
		if !created {
			t.Fatal("first start reported no new execution")
		}
		again, created, err := h.s.StartExecution(ctx, sb.ID, req)
		wantNoError(t, err)
		if created {
			t.Fatal("repeated start created a second execution")
		}
		if again.ID != first.ID {
			t.Fatalf("repeated start returned %s, want %s", again.ID, first.ID)
		}
		h.waitCompleted(sb.ID, first.ID)
		// A replay after completion returns the completed record, not a
		// second process.
		after, created, err := h.s.StartExecution(ctx, sb.ID, req)
		wantNoError(t, err)
		if created || after.ID != first.ID || after.State != daemon.ExecCompleted {
			t.Fatalf("replay after completion = %+v, created=%v", after, created)
		}
	})

	t.Run("reused key with a different command conflicts", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{})
		_, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{
			Command: []string{"sleep"}, IdempotencyKey: "k1",
		})
		wantNoError(t, err)
		_, _, err = h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{
			Command: []string{"echo", "different"}, IdempotencyKey: "k1",
		})
		wantCode(t, err, daemon.CodeIdempotencyConflict)
	})

	t.Run("busy while another execution runs", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["sleep"] = memBlock
		sb := h.create(daemon.CreateRequest{})
		first, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"sleep"}})
		wantNoError(t, err)

		_, _, err = h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"echo", "x"}})
		wantCode(t, err, daemon.CodeBusy)

		h.waitCompleted(sb.ID, first.ID)

		// Once the first completes the sandbox accepts another.
		_, created, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"echo", "x"}})
		wantNoError(t, err)
		if !created {
			t.Fatal("start after completion reported no new execution")
		}
	})

	t.Run("backend exec failure", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{})
		h.be.ExecErr = errBoom
		_, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"true"}})
		wantCode(t, err, daemon.CodeBackendError)
		// The failed start must not leave the sandbox busy.
		h.be.Programs["echo"] = memEcho
		_, created, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"echo", "ok"}})
		wantNoError(t, err)
		if !created {
			t.Fatal("sandbox stayed busy after a failed start")
		}
	})

	t.Run("unknown execution", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{})
		_, err := h.s.GetExecution(sb.ID, "exe_missing")
		wantCode(t, err, daemon.CodeNotFound)
		_, err = h.s.CancelExecution(ctx, sb.ID, "exe_missing")
		wantCode(t, err, daemon.CodeNotFound)
	})
}
