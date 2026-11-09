package daemon_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/daemon/membackend"
)

// scripted wraps membackend and answers Liveness from a fixed sequence, so the
// ambiguous-137 race can be reproduced deterministically.
type scripted struct {
	*membackend.Backend

	mu        sync.Mutex
	answers   []daemon.Liveness
	livenessN int
	oom       int64
	oomSet    bool
}

func (s *scripted) Liveness(_ context.Context, _ string) (daemon.Liveness, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	answer := s.answers[len(s.answers)-1]
	if s.livenessN < len(s.answers) {
		answer = s.answers[s.livenessN]
	}
	s.livenessN++
	return answer, nil
}

func (s *scripted) OOMKillCount(ctx context.Context, id string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.oomSet {
		return s.oom, nil
	}
	return s.Backend.OOMKillCount(ctx, id)
}

// livenessCalls returns how many times Liveness was asked.
func (s *scripted) livenessCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.livenessN
}

// newScripted builds a harness whose backend answers liveness from answers,
// repeating the last answer forever.
func newScripted(t *testing.T, answers ...daemon.Liveness) (*harness, *scripted) {
	t.Helper()
	clock := newFakeClock()
	be := &scripted{Backend: membackend.New(), answers: answers}
	s := daemon.New(be,
		daemon.WithClock(clock),
		daemon.WithClassifierSleep(noSleep),
		daemon.WithCancelGrace(10*time.Millisecond),
		daemon.WithCancelDeadline(100*time.Millisecond),
	)
	return &harness{t: t, clock: clock, be: be.Backend, s: s}, be
}

// TestClassificationPrecedence covers the reason table in api.md. The order is
// fixed: recorded intent, then a positive OOM delta, then container liveness,
// then the exit code.
func TestClassificationPrecedence(t *testing.T) {
	ctx := context.Background()

	t.Run("exited", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["exit"] = memExit
		sb := h.create(daemon.CreateRequest{})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"exit", "0"}})
		wantNoError(t, err)
		done := h.waitCompleted(sb.ID, ex.ID)
		if *done.TerminationReason != daemon.ReasonExited || *done.ExitCode != 0 {
			t.Fatalf("reason = %s, exit = %d; want exited/0", *done.TerminationReason, *done.ExitCode)
		}
	})

	t.Run("137 with the container alive is exited, never oom", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["exit"] = memExit
		sb := h.create(daemon.CreateRequest{})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"exit", "137"}})
		wantNoError(t, err)
		done := h.waitCompleted(sb.ID, ex.ID)
		if *done.TerminationReason != daemon.ReasonExited {
			t.Fatalf("reason = %s, want exited (137 alone is not oom)", *done.TerminationReason)
		}
		if *done.ExitCode != 137 {
			t.Fatalf("exit = %d, want 137", *done.ExitCode)
		}
	})

	t.Run("oom wins over the exit code", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["exit"] = memExit
		sb := h.create(daemon.CreateRequest{})
		container := h.containerOf(sb.ID)
		h.be.SetOOMKills(container, 7) // the kernel killed something earlier
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"exit", "137"}})
		wantNoError(t, err)
		// The delta must happen during the execution, so the counter moves
		// after the baseline sample. membackend's Exec runs the program
		// asynchronously, which gives us that window.
		h.be.SetOOMKills(container, 8)
		done := h.waitCompleted(sb.ID, ex.ID)
		if *done.TerminationReason != daemon.ReasonOOM {
			t.Fatalf("reason = %s, want oom", *done.TerminationReason)
		}
	})

	t.Run("intent wins over oom", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["block"] = memBlock
		sb := h.create(daemon.CreateRequest{})
		container := h.containerOf(sb.ID)
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"block"}})
		wantNoError(t, err)
		h.be.SetOOMKills(container, 1)
		if _, err := h.s.CancelExecution(ctx, sb.ID, ex.ID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		done, err := h.s.GetExecution(sb.ID, ex.ID)
		wantNoError(t, err)
		if *done.TerminationReason != daemon.ReasonCanceled {
			t.Fatalf("reason = %s, want canceled (intent precedes oom)", *done.TerminationReason)
		}
	})

	t.Run("evicted when the container is stopped", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["block"] = memBlock
		sb := h.create(daemon.CreateRequest{})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"block"}})
		wantNoError(t, err)
		h.be.Kill(h.containerOf(sb.ID))

		done := h.waitCompleted(sb.ID, ex.ID)
		if *done.TerminationReason != daemon.ReasonEvicted {
			t.Fatalf("reason = %s, want evicted", *done.TerminationReason)
		}
		sandbox, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if sandbox.State != daemon.StateKilled || sandbox.KillReason == nil || *sandbox.KillReason != daemon.KillEvicted {
			t.Fatalf("sandbox = %+v, want killed/evicted", sandbox)
		}
	})

	t.Run("evicted when the container disappears", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["block"] = memBlock
		sb := h.create(daemon.CreateRequest{})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"block"}})
		wantNoError(t, err)
		h.be.Disappear(h.containerOf(sb.ID))

		done := h.waitCompleted(sb.ID, ex.ID)
		if *done.TerminationReason != daemon.ReasonEvicted {
			t.Fatalf("reason = %s, want evicted", *done.TerminationReason)
		}
	})
}

// TestAmbiguous137Race is the race the design notes call out, in both
// directions. `docker inspect` intermittently misses a container record right
// after an exit, and right after an external kill the daemon can still report
// "running" for a few milliseconds. Neither a single miss nor a single
// "running" answer decides; the state is sampled across the whole window and
// the last observation is the settled one.
func TestAmbiguous137Race(t *testing.T) {
	ctx := context.Background()

	t.Run("a transient miss does not become evicted", func(t *testing.T) {
		h, be := newScripted(t, daemon.LivenessStopped, daemon.LivenessMissing, daemon.LivenessRunning)
		be.Programs["exit"] = memExit
		sb := h.create(daemon.CreateRequest{})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"exit", "137"}})
		wantNoError(t, err)
		done := h.waitCompleted(sb.ID, ex.ID)

		if *done.TerminationReason != daemon.ReasonExited {
			t.Fatalf("reason = %s, want exited", *done.TerminationReason)
		}
		if be.livenessCalls() < 2 {
			t.Fatalf("liveness asked %d times; the 137 path must re-sample", be.livenessCalls())
		}
		sandbox, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if sandbox.State == daemon.StateKilled {
			t.Fatal("a transient miss killed the sandbox")
		}
	})

	// The mirror image, and the reason CI failed: the daemon keeps answering
	// "running" for a moment after an external kill. Breaking on the first
	// alive answer classified a `docker kill` as a clean exit.
	t.Run("a stale running answer settles into evicted", func(t *testing.T) {
		h, be := newScripted(t,
			daemon.LivenessRunning, // still stale right after the kill
			daemon.LivenessRunning,
			daemon.LivenessStopped, // the daemon records the stop
		)
		be.Programs["exit"] = memExit
		sb := h.create(daemon.CreateRequest{})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"exit", "137"}})
		wantNoError(t, err)
		done := h.waitCompleted(sb.ID, ex.ID)
		if *done.TerminationReason != daemon.ReasonEvicted {
			t.Fatalf("reason = %s, want evicted: a stale running answer must not decide", *done.TerminationReason)
		}
	})

	// The exact shape CI produced: the daemon kept answering "running" for
	// longer than the old fixed five-sample window, so the last sample still
	// said alive and a `docker kill` was recorded as a clean exit. The window
	// is now a time budget, so an answer sequence this long still settles.
	t.Run("a long stale running answer still settles into evicted", func(t *testing.T) {
		answers := make([]daemon.Liveness, 0, 9)
		for i := 0; i < 8; i++ {
			answers = append(answers, daemon.LivenessRunning)
		}
		answers = append(answers, daemon.LivenessStopped)
		h, be := newScripted(t, answers...)
		be.Programs["exit"] = memExit
		sb := h.create(daemon.CreateRequest{})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"exit", "137"}})
		wantNoError(t, err)
		done := h.waitCompleted(sb.ID, ex.ID)
		if *done.TerminationReason != daemon.ReasonEvicted {
			t.Fatalf("reason = %s, want evicted after a stale running window", *done.TerminationReason)
		}
	})

	// A stop that has already settled must not pay the whole window: two
	// consecutive "stopped" answers are the confirmation, after the minimum.
	t.Run("a confirmed stop ends the window early", func(t *testing.T) {
		h, be := newScripted(t, daemon.LivenessStopped, daemon.LivenessStopped)
		be.Programs["exit"] = memExit
		sb := h.create(daemon.CreateRequest{})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"exit", "137"}})
		wantNoError(t, err)
		done := h.waitCompleted(sb.ID, ex.ID)
		if *done.TerminationReason != daemon.ReasonEvicted {
			t.Fatalf("reason = %s, want evicted", *done.TerminationReason)
		}
		if n := be.livenessCalls(); n != 3 {
			t.Fatalf("liveness asked %d times; a settled stop must end the window after 3 samples", n)
		}
	})

	t.Run("a consistent miss is evicted", func(t *testing.T) {
		h, be := newScripted(t, daemon.LivenessMissing)
		be.Programs["exit"] = memExit
		sb := h.create(daemon.CreateRequest{})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"exit", "137"}})
		wantNoError(t, err)
		done := h.waitCompleted(sb.ID, ex.ID)
		if *done.TerminationReason != daemon.ReasonEvicted {
			t.Fatalf("reason = %s, want evicted", *done.TerminationReason)
		}
		if be.livenessCalls() < 3 {
			t.Fatalf("liveness asked %d times; a 137 must be sampled repeatedly", be.livenessCalls())
		}
	})

	t.Run("a consistently alive container is exited", func(t *testing.T) {
		h, be := newScripted(t, daemon.LivenessRunning)
		be.Programs["exit"] = memExit
		sb := h.create(daemon.CreateRequest{})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"exit", "137"}})
		wantNoError(t, err)
		done := h.waitCompleted(sb.ID, ex.ID)
		if *done.TerminationReason != daemon.ReasonExited {
			t.Fatalf("reason = %s, want exited: an in-sandbox kill leaves the container alive", *done.TerminationReason)
		}
		if be.livenessCalls() < 3 {
			t.Fatalf("liveness asked %d times; the 137 path must sample the window", be.livenessCalls())
		}
	})

	t.Run("a non-137 exit is sampled once", func(t *testing.T) {
		h, be := newScripted(t, daemon.LivenessRunning)
		be.Programs["exit"] = memExit
		sb := h.create(daemon.CreateRequest{})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"exit", "1"}})
		wantNoError(t, err)
		h.waitCompleted(sb.ID, ex.ID)
		if n := be.livenessCalls(); n != 1 {
			t.Fatalf("liveness asked %d times for exit 1, want 1", n)
		}
	})
}
