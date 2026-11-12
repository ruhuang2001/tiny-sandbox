package daemon

import (
	"context"
	"errors"
	"time"
)

// backendCtx bounds one reaper-driven backend call. The reaper is one
// goroutine visiting every sandbox, so a call that never returns would stop
// timeout and eviction checks for all of them; a deadline turns that into a
// skipped sandbox retried on the next beat.
func backendCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, BackendCallTimeout)
}

// dueTimeout reports which sandbox-level timeout, if any, is due for sb at now.
func (sb *sandbox) dueTimeout(now time.Time) (reason KillReason, ok bool) {
	if sb.opts.timeoutSeconds > 0 && sb.deadlined(now) {
		return KillTimeout, true
	}
	if idle := sb.idleFor(now); idle >= 0 && idle >= time.Duration(sb.opts.idleTimeoutSeconds)*time.Second {
		return KillIdleTimeout, true
	}
	return "", false
}

// dueExecutionTimeout reports whether the sandbox's running execution has its
// own deadline due at now, and claims it so no later beat starts a second
// escalation. Callers hold sb.mu.
//
// The deadline is absolute from start, so time spent paused counts. The
// reaper only claims it while the sandbox is running: a paused container
// delivers nothing to signal, and the claim would be wasted. A pause that ends
// after the deadline therefore times the execution out on the first beat after
// the resume, which is the same rule the lifetime timeout uses for a kill.
func (sb *sandbox) dueExecutionTimeout(now time.Time) (*execution, bool) {
	ex := sb.running
	if ex == nil {
		return nil, false
	}
	ex.mu.Lock()
	defer ex.mu.Unlock()
	if ex.state != ExecRunning || ex.deadline == nil || ex.timedOut || now.Before(*ex.deadline) {
		return nil, false
	}
	ex.timedOut = true
	return ex, true
}

// Reap applies every due timeout once. It is the only thing that turns a
// timeout into a transition; reads never do, so a GET can never change state.
//
// The reaper never waits for a sandbox lock: a sandbox that is busy (for
// example inside a blocking cancel) is skipped and visited on the next beat.
// That keeps one slow backend call from delaying the timeout of every other
// sandbox. Callers run Reap at most one second apart.
func (s *Sandboxes) Reap(ctx context.Context) {
	s.mu.Lock()
	order := append([]*sandbox(nil), s.order...)
	s.mu.Unlock()

	now := s.now()
	for _, sb := range order {
		if !sb.mu.TryLock() {
			continue
		}
		s.reapOne(ctx, sb, now)
		sb.mu.Unlock()
	}
}

// reapOne applies the timeouts to one sandbox. Callers hold sb.mu.
func (s *Sandboxes) reapOne(ctx context.Context, sb *sandbox, now time.Time) {
	if sb.state != StateRunning && sb.state != StatePaused {
		return
	}

	reason, due := sb.dueTimeout(now)
	if !due {
		// An execution deadline ends the command, not the sandbox, so it is
		// checked only after the sandbox has survived its own deadlines. The
		// escalation runs on its own goroutine: the reaper is one goroutine
		// visiting every sandbox, and the SIGTERM grace alone is longer than
		// a beat.
		if sb.state == StateRunning {
			if ex, ok := sb.dueExecutionTimeout(now); ok {
				// Record the intent before acting, as cancel does: it
				// explains whatever exit code the signal produces.
				ex.setIntent(ReasonTimeout)
				go s.timeoutExecution(context.WithoutCancel(ctx), sb, ex)
			}
		}
		return
	}

	action := sb.opts.timeoutAction
	if reason == KillIdleTimeout {
		action = sb.opts.idleTimeoutAction
	}

	// The transition table also requires the container to still be there. A
	// backend that reports it gone is an eviction, whichever deadline fired.
	switch action {
	case ActionPause:
		if sb.state != StateRunning {
			return
		}
		callCtx, cancel := backendCtx(ctx)
		err := s.backend.Pause(callCtx, sb.container)
		cancel()
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				s.killLocked(sb, KillEvicted)
				return
			}
			// A failed pause may still have paused the container: the request
			// can time out after Docker acted. Recording the transition only
			// from the error would then leave the record running while the
			// container is frozen, and every later beat would repeat a Pause
			// that Docker answers with "already paused" forever. Ask the
			// runtime what actually happened.
			if liveness, livenessErr := s.liveness(ctx, sb.container); livenessErr == nil &&
				liveness == LivenessPaused {
				sb.state = StatePaused
				sb.expiresAt = nil
			}
			// Otherwise the next beat retries.
			return
		}
		sb.state = StatePaused
		sb.expiresAt = nil
	case ActionKill:
		// terminateLocked bounds the removal itself and detaches it from the
		// caller's context, so a reaper-driven kill during shutdown still
		// completes; it also schedules a retry when the runtime does not
		// answer in time.
		s.terminateLocked(ctx, sb, reason)
	}
}

// liveness reads one container's liveness with the reaper's per-call bound.
func (s *Sandboxes) liveness(ctx context.Context, containerID string) (Liveness, error) {
	callCtx, cancel := backendCtx(ctx)
	defer cancel()
	return s.backend.Liveness(callCtx, containerID)
}

// retryRemovals retries containers whose removal failed or timed out. A killed
// record whose container is still running is invisible to everything else: the
// eviction sweep skips killed sandboxes and a repeated delete returns the
// stored record without retrying. Without this, a slow runtime would turn a
// bounded timeout into a container that outlives the deadline it was killed
// for, until the next process restart.
//
// The pass runs on its own goroutine with an overall budget, not inline in the
// reaper tick. A hung runtime makes each Remove spend its whole timeout, and a
// long queue would then delay timeout enforcement and eviction detection for
// every healthy sandbox. A pass that overruns its budget simply stops and the
// rest is retried on the next beat.
func (s *Sandboxes) retryRemovals(ctx context.Context) {
	if !s.tryStartRemovalPass() {
		return
	}
	passCtx, cancel := context.WithTimeout(ctx, removalPassBudget)
	go func() {
		defer cancel()
		defer s.finishRemovalPass()
		s.runRemovalPass(passCtx)
	}()
}

// runRemovalPass makes one bounded Remove call per queued container until the
// pass budget runs out.
func (s *Sandboxes) runRemovalPass(ctx context.Context) {
	for _, id := range s.pendingRemovals() {
		if ctx.Err() != nil {
			return
		}
		err := s.removeBounded(ctx, id)
		// ErrNotFound is success here: the container is gone either way.
		if err == nil || errors.Is(err, ErrNotFound) {
			s.forgetRemoval(id)
		}
	}
}

// pendingRemovals snapshots the ids waiting for a retry.
func (s *Sandboxes) pendingRemovals() []string {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	ids := make([]string, 0, len(s.retryIDs))
	for id := range s.retryIDs {
		ids = append(ids, id)
	}
	return ids
}

// removeBounded calls Remove with the smaller of the per-call bound and the
// time the pass has left.
func (s *Sandboxes) removeBounded(ctx context.Context, containerID string) error {
	callCtx, cancel := context.WithTimeout(ctx, BackendCallTimeout)
	defer cancel()
	return s.backend.Remove(callCtx, containerID)
}

// forgetRemoval drops a container that is gone.
func (s *Sandboxes) forgetRemoval(containerID string) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	delete(s.retryIDs, containerID)
}

// tryStartRemovalPass claims the single removal-pass slot, so overlapping
// ticks never run two passes at once.
func (s *Sandboxes) tryStartRemovalPass() bool {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	if s.removalPassRunning {
		return false
	}
	s.removalPassRunning = true
	return true
}

func (s *Sandboxes) finishRemovalPass() {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	s.removalPassRunning = false
}

// scheduleRemoval remembers a container the runtime would not remove. Nothing
// is dropped: a container that outlived a removal has to be retried or it runs
// until the process restarts, and the set is bounded only by the number of
// sandboxes this process has killed.
func (s *Sandboxes) scheduleRemoval(containerID string) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	s.retryIDs[containerID] = struct{}{}
}

// EvictSweep checks every live sandbox's container and evicts those whose
// container stopped or vanished without a request. It is what turns an
// external `docker kill` into killed/evicted within the five-second budget;
// a request addressing the sandbox does the same check earlier.
func (s *Sandboxes) EvictSweep(ctx context.Context) {
	s.mu.Lock()
	order := append([]*sandbox(nil), s.order...)
	s.mu.Unlock()

	for _, sb := range order {
		if !sb.mu.TryLock() {
			continue
		}
		if sb.state == StateRunning || sb.state == StatePaused {
			callCtx, cancel := backendCtx(ctx)
			liveness, err := s.backend.Liveness(callCtx, sb.container)
			cancel()
			switch {
			case err != nil:
				// Unreadable is not proof of death; the next sweep asks again.
			case liveness == LivenessStopped || liveness == LivenessMissing:
				s.killLocked(sb, KillEvicted)
			}
		}
		sb.mu.Unlock()
	}
}

// Run applies timeouts and eviction checks until ctx is done. It is the
// periodic reaper api.md requires: at most one second between checks, which
// keeps eviction detection inside the five-second budget. It is safe to run
// alongside request handling; a sandbox being serviced is skipped, not
// blocked.
func (s *Sandboxes) Run(ctx context.Context) {
	tick := time.NewTicker(s.cleanupInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.Reap(ctx)
			s.EvictSweep(ctx)
			s.retryRemovals(ctx)
			s.prune()
		}
	}
}
