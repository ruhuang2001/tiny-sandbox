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
			}
			// Any other failure is retried on the next beat.
			return
		}
		sb.state = StatePaused
		sb.expiresAt = nil
	case ActionKill:
		callCtx, cancel := backendCtx(ctx)
		terminated := s.terminateWith(callCtx, sb, reason)
		cancel()
		if !terminated {
			// Removal failed or timed out. The record is still killed; the
			// container is left for startup cleanup, as the contract says.
			return
		}
	}
}

// terminateWith is terminateLocked with a report of whether removal succeeded,
// so the reaper can tell a completed transition from one whose container is
// still around. ctx already carries the reaper's per-call deadline; wrapping
// it in WithoutCancel would drop that deadline, so it is used as is.
func (s *Sandboxes) terminateWith(ctx context.Context, sb *sandbox, reason KillReason) bool {
	s.killLocked(sb, reason)
	if sb.container == "" {
		return true
	}
	return s.backend.Remove(ctx, sb.container) == nil
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
		}
	}
}
