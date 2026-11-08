package daemon_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/daemon/membackend"
)

// TestBackendFailures covers what the daemon reports when the container
// runtime fails, and the contract's rule that a vanished container is an
// eviction rather than a 502 that will never clear.
func TestBackendFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("pause failure is a backend error", func(t *testing.T) {
		clock := newFakeClock()
		be := &flaky{Backend: membackend.New(), pauseErr: errBoom}
		s := daemon.New(be, daemon.WithClock(clock))
		sb, err := s.Create(ctx, daemon.CreateRequest{})
		wantNoError(t, err)

		_, err = s.Pause(ctx, sb.ID)
		wantCode(t, err, daemon.CodeBackendError)
		// The sandbox must stay running: a failed transition changes nothing.
		got, err := s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateRunning {
			t.Fatalf("state = %s after a failed pause, want running", got.State)
		}
	})

	t.Run("pause on a vanished container evicts", func(t *testing.T) {
		clock := newFakeClock()
		be := &flaky{Backend: membackend.New(), pauseErr: daemon.ErrNotFound}
		s := daemon.New(be, daemon.WithClock(clock))
		sb, err := s.Create(ctx, daemon.CreateRequest{})
		wantNoError(t, err)

		_, err = s.Pause(ctx, sb.ID)
		wantCode(t, err, daemon.CodeInvalidState)
		got, err := s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateKilled || got.KillReason == nil || *got.KillReason != daemon.KillEvicted {
			t.Fatalf("sandbox = %+v, want killed/evicted", got)
		}
	})

	t.Run("unreadable liveness is an eviction", func(t *testing.T) {
		clock := newFakeClock()
		be := &flaky{Backend: membackend.New(), livenessErr: errBoom}
		be.Programs["echo"] = memEcho
		s := daemon.New(be,
			daemon.WithClock(clock),
			daemon.WithClassifierSleep(noSleep),
		)
		sb, err := s.Create(ctx, daemon.CreateRequest{})
		wantNoError(t, err)
		ex, _, err := s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"echo", "hi"}})
		wantNoError(t, err)

		deadline := time.Now().Add(5 * time.Second)
		var done daemon.Execution
		for time.Now().Before(deadline) {
			done, err = s.GetExecution(sb.ID, ex.ID)
			wantNoError(t, err)
			if done.State == daemon.ExecCompleted {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if done.State != daemon.ExecCompleted {
			t.Fatal("execution did not complete")
		}
		if *done.TerminationReason != daemon.ReasonEvicted {
			t.Fatalf("reason = %s, want evicted when the container cannot be observed", *done.TerminationReason)
		}
	})
}

// TestErrorType covers the contract error type as clients see it.
func TestErrorType(t *testing.T) {
	cause := errors.New("cause")
	err := &daemon.Error{Code: daemon.CodeBusy, Message: "busy", Err: cause}
	if err.Error() != "busy: cause" {
		t.Fatalf("Error() = %q", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Fatal("Unwrap does not expose the cause")
	}
	if got := daemon.CodeOf(err); got != daemon.CodeBusy {
		t.Fatalf("CodeOf = %s, want busy", got)
	}
	if got := daemon.CodeOf(cause); got != daemon.CodeInternal {
		t.Fatalf("CodeOf(plain error) = %s, want internal", got)
	}

	plain := &daemon.Error{Code: daemon.CodeNotFound, Message: "missing"}
	if plain.Error() != "missing" {
		t.Fatalf("Error() = %q", plain.Error())
	}
}

// TestLastEventID checks the helper an SDK uses to resume without replay.
func TestLastEventID(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.be.Programs["echo"] = memEcho
	sb := h.create(daemon.CreateRequest{})
	ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"echo", "x"}})
	wantNoError(t, err)
	h.waitCompleted(sb.ID, ex.ID)

	st, err := h.s.Events(sb.ID, ex.ID, 0)
	wantNoError(t, err)
	events := collect(t, st)
	st.Close()

	got, err := h.s.LastEventID(sb.ID, ex.ID)
	wantNoError(t, err)
	if got != events[len(events)-1].ID {
		t.Fatalf("LastEventID = %d, want %d", got, events[len(events)-1].ID)
	}
	_, err = h.s.LastEventID(sb.ID, "exe_missing")
	wantCode(t, err, daemon.CodeNotFound)
}

// TestEventsErrors covers stream lookup failures.
func TestEventsErrors(t *testing.T) {
	h := newHarness(t)
	sb := h.create(daemon.CreateRequest{})
	_, err := h.s.Events(sb.ID, "exe_missing", 0)
	wantCode(t, err, daemon.CodeNotFound)
	_, err = h.s.Events("sbx_missing", "exe_any", 0)
	wantCode(t, err, daemon.CodeNotFound)
}

// TestConcurrentOperations runs every entry point at once under -race, which
// is the point of the stage gate: the store's locking must hold under load.
func TestConcurrentOperations(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.be.Programs["echo"] = memEcho
	h.be.Programs["exit"] = memExit

	const workers = 8
	done := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		go func(_ int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 40; j++ {
				sb, err := h.s.Create(ctx, daemon.CreateRequest{TimeoutSeconds: ptr(0)})
				if err != nil {
					t.Errorf("create: %v", err)
					return
				}
				ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{
					Command: []string{"echo", "x"},
				})
				if err != nil {
					t.Errorf("start: %v", err)
					return
				}
				h.s.Touch(sb.ID)
				_ = h.s.List()
				_, _ = h.s.ListExecutions(sb.ID)
				h.s.Reap(ctx)
				h.s.EvictSweep(ctx)
				if _, err := h.s.CancelExecution(ctx, sb.ID, ex.ID); err != nil {
					t.Errorf("cancel: %v", err)
					return
				}
				if _, err := h.s.Delete(ctx, sb.ID); err != nil {
					t.Errorf("delete: %v", err)
					return
				}
			}
		}(i)
	}
	for i := 0; i < workers; i++ {
		<-done
	}
}
