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

// dueTimeout reports which timeout, if any, is due for sb at now.
func (sb *sandbox) dueTimeout(now time.Time) (reason KillReason, ok bool) {
	if sb.opts.timeoutSeconds > 0 && sb.deadlined(now) {
		return KillTimeout, true
	}
	if idle := sb.idleFor(now); idle >= 0 && idle >= time.Duration(sb.opts.idleTimeoutSeconds)*time.Second {
		return KillIdleTimeout, true
	}
	return "", false
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
func (s *Sandboxes) retryRemovals(ctx context.Context) {
	s.retryMu.Lock()
	ids := make([]string, 0, len(s.retryIDs))
	for id := range s.retryIDs {
		ids = append(ids, id)
	}
	s.retryMu.Unlock()

	for _, id := range ids {
		callCtx, cancel := backendCtx(ctx)
		err := s.backend.Remove(callCtx, id)
		cancel()
		// ErrNotFound is success here: the container is gone either way.
		if err == nil || errors.Is(err, ErrNotFound) {
			s.retryMu.Lock()
			delete(s.retryIDs, id)
			s.retryMu.Unlock()
		}
	}
}

// scheduleRemoval remembers a container the runtime would not remove. The set
// is bounded: a runtime that cannot remove several hundred containers has a
// problem the retry loop cannot solve, and startup cleanup still catches what
// is dropped here.
func (s *Sandboxes) scheduleRemoval(containerID string) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	if len(s.retryIDs) >= maxRemovalRetries {
		return
	}
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
		}
	}
}
