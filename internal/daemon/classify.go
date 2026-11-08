package daemon

import "context"

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
}

// classify decides the termination reason. The precedence is fixed by
// docs/api.md: recorded intent, then a positive delta in the cgroup oom_kill
// counter, then container liveness, then the exit code.
//
// The liveness step is where the earlier build went wrong: `docker inspect`
// intermittently misses a container record, so one "not running" answer after
// a 137 is not proof of anything. A 137 with no intent and no OOM delta is
// therefore re-sampled a few times; only a consistent answer decides.
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

	liveness := LivenessRunning
	var lastErr error
	for i := 0; i < samples; i++ {
		if i > 0 && !sleep(ctx, ambiguousInterval) {
			break
		}
		liveness, lastErr = s.Liveness(ctx, containerID)
		if lastErr == nil && liveness.Alive() {
			break
		}
	}

	if lastErr != nil {
		// The container could not be observed at all: the runtime failed, not
		// the process. Treat it as the container being lost so the sandbox
		// does not claim a clean exit while its container is unaccounted for.
		return ExecOutcome{Reason: ReasonEvicted, ExitCode: exitCode, SandboxLost: true}
	}
	if !liveness.Alive() {
		// The container stopped or vanished without the daemon asking. This is
		// `docker kill`, `docker rm`, or an external stop.
		return ExecOutcome{Reason: ReasonEvicted, ExitCode: exitCode, SandboxLost: true}
	}

	// The process exited on its own while its container still lives. This is
	// the ordinary case, and it includes exit 137 from an in-sandbox
	// `kill -9`: no intent, no OOM delta, container alive.
	return ExecOutcome{Reason: ReasonExited, ExitCode: exitCode}
}
