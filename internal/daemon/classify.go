package daemon

import (
	"context"
	"time"
)

// sampler is the side channel classification needs after a process exits. A
// Backend satisfies it; tests substitute a scripted sequence of answers.
type sampler interface {
	Liveness(ctx context.Context, containerID string) (Liveness, error)
	OOMKillCount(ctx context.Context, containerID string) (int64, error)
}

// ExecOutcome is what classification concluded about one finished process.
type ExecOutcome struct {
	// Reason is the termination reason recorded on the Execution.
	Reason Reason
	// ExitCode is the runtime's exit code, or -1 when it could not report
	// one. The contract exposes null in that case.
	ExitCode int
	// SandboxLost is true when the container stopped or disappeared without
	// the daemon asking, so the sandbox must become killed with reason
	// evicted.
	SandboxLost bool

	// sampled is the liveness sequence an ambiguous 137 observed, and elapsed
	// is how long the settle window ran. The runner logs both when the verdict
	// is evicted or when the window ran out, so a misclassification on a
	// machine we cannot reproduce on leaves a record of what the runtime
	// actually reported.
	sampled []Liveness
	elapsed time.Duration
}

// classify decides the termination reason. The precedence is fixed by
// docs/api.md: recorded intent, then a positive delta in the cgroup oom_kill
// counter, then container liveness, then the exit code.
//
// The liveness step is where two builds went wrong. `docker inspect` used to
// intermittently miss a container record, so one "not running" answer after a
// 137 proves nothing. And right after an external kill the daemon can still
// report "running" for a moment, so one "alive" answer proves nothing either.
// A 137 with no intent and no OOM delta is therefore sampled across the whole
// window, and the last observation decides: the state has had time to settle
// in both directions.
func classify(ctx context.Context, s sampler, containerID string, oomBaseline int64, exitCode int, intent *intentRecord, sleep SleepFunc) ExecOutcome {
	if intent != nil {
		switch intent.reason {
		case ReasonCanceled:
			return ExecOutcome{Reason: ReasonCanceled, ExitCode: exitCode}
		case ReasonTimeout:
			return ExecOutcome{Reason: ReasonTimeout, ExitCode: exitCode}
		}
	}

	// A positive delta attributes the kill to the OOM killer whatever the exit
	// code says. A failed read is treated as no delta: claiming oom on a
	// failed read would be a guess.
	if current, err := s.OOMKillCount(ctx, containerID); err == nil && oomBaseline >= 0 && current > oomBaseline {
		return ExecOutcome{Reason: ReasonOOM, ExitCode: exitCode}
	}

	ambiguous := exitCode == 137
	samples := 1
	if ambiguous {
		samples = ambiguousSamples
	}

	// Sample until the state settles or the window expires, and decide on the
	// last observation.
	//
	// Both directions of staleness exist. A container record can briefly be
	// absent before it settles (the false-negative this re-sampling was built
	// for), and right after an external kill it can keep reporting "running"
	// while the daemon catches up (the false-positive that made CI classify a
	// `docker kill` as a clean exit). Breaking on the first "alive" answer only
	// guards the first, and a window of a few samples was not long enough on a
	// loaded CI runner. So the window is a time budget rather than a sample
	// count, and it ends early only on a state that cannot be stale: two
	// consecutive "stopped" answers. "Missing" never ends it early, because a
	// transient miss is exactly the other race, and "running" never does,
	// because a stale run is the race this window exists for.
	liveness := LivenessRunning
	var lastErr error
	observed := false
	var sampled []Liveness
	started := time.Now()
	for i := 0; i < samples; i++ {
		if i > 0 && !sleep(ctx, ambiguousInterval) {
			break
		}
		answer, err := s.Liveness(ctx, containerID)
		if err != nil {
			lastErr = err
			continue
		}
		lastErr = nil
		previous := liveness
		liveness = answer
		observed = true
		if ambiguous {
			sampled = append(sampled, answer)
		}
		if i+1 >= ambiguousMinSamples && answer == LivenessStopped && previous == LivenessStopped {
			break
		}
	}
	elapsed := time.Since(started)
	withTrace := func(out ExecOutcome) ExecOutcome {
		out.sampled = sampled
		out.elapsed = elapsed
		return out
	}

	if !observed && lastErr != nil {
		// The container could not be observed at all: the runtime failed, not
		// the process. Treat it as the container being lost so the sandbox
		// does not claim a clean exit while its container is unaccounted for.
		return withTrace(ExecOutcome{Reason: ReasonEvicted, ExitCode: exitCode, SandboxLost: true})
	}
	if !liveness.Alive() {
		// The container stopped or vanished without the daemon asking. This is
		// `docker kill`, `docker rm`, or an external stop.
		return withTrace(ExecOutcome{Reason: ReasonEvicted, ExitCode: exitCode, SandboxLost: true})
	}

	// The process exited on its own while its container still lives. This is
	// the ordinary case, and it includes exit 137 from an in-sandbox
	// `kill -9`: no intent, no OOM delta, container alive.
	return withTrace(ExecOutcome{Reason: ReasonExited, ExitCode: exitCode})
}
