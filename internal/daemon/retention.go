package daemon

// Retention bounds the memory the store keeps after sandboxes are gone.
//
// Three separate bounds apply, because they fail differently:
//
//   - MaxSandboxes bounds live sandboxes. Admission refuses a create beyond it,
//     so the host is protected from a caller that never deletes.
//   - MaxRetainedSandboxes bounds killed records. They cost no container, but
//     they do keep history, and an agent that runs one task per sandbox creates
//     one record per task.
//   - MaxRetainedOutputBytes bounds the output the whole process holds. The
//     per-execution limit alone multiplies by history and by sandbox count:
//     200 executions of 1 MiB is 200 MiB for one sandbox.
//
// A dropped sandbox or execution answers 404 afterwards. That is the documented
// cost of the bound, and the same trade an idempotency key already makes when
// its execution is pruned.
//
// The pass runs on the reaper, never on a request path, and it never waits for a
// sandbox lock: a sandbox being serviced is skipped and revisited on the next
// beat, so retention cannot deadlock against a backend call.

// prune applies the retention bounds. Callers hold no lock.
func (s *Sandboxes) prune() {
	s.pruneKilled()
	s.pruneOutput()
}

// pruneKilled drops the oldest killed records above the retention bound. Live
// records are never dropped: they still have a container, and admission counts
// them.
func (s *Sandboxes) pruneKilled() {
	order := s.sandboxesInCreationOrder()

	killed := 0
	for _, sb := range order {
		if sb.isKilled() {
			killed++
		}
	}
	drop := killed - s.maxRetainedSandboxes
	if drop <= 0 {
		return
	}
	// Oldest first, so the survivors are the newest records and a delete that
	// just happened still answers for its own sandbox.
	for _, sb := range order {
		if drop == 0 {
			return
		}
		if sb.isKilled() {
			s.forget(sb)
			drop--
		}
	}
}

// pruneOutput drops the oldest completed executions across sandboxes until the
// process holds at most maxRetainedOutput bytes.
//
// The measurement and the drop walk the same list, so one pass is enough: the
// total is computed once, then each sandbox gives up executions from the front
// of its history, oldest sandbox first, until the total fits. A sandbox that is
// busy when it is measured is skipped; the next beat sees it again.
func (s *Sandboxes) pruneOutput() {
	order := s.sandboxesInCreationOrder()

	total := 0
	for _, sb := range order {
		if sb.mu.TryLock() {
			total += sb.retainedBytesLocked()
			sb.mu.Unlock()
		}
	}

	for _, sb := range order {
		if total <= s.maxRetainedOutput {
			return
		}
		if !sb.mu.TryLock() {
			continue
		}
		for total > s.maxRetainedOutput {
			size, ok := sb.dropOldestLocked()
			if !ok {
				break
			}
			total -= size
		}
		sb.mu.Unlock()
	}
}

// sandboxesInCreationOrder snapshots the store's sandboxes, oldest first.
func (s *Sandboxes) sandboxesInCreationOrder() []*sandbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*sandbox(nil), s.order...)
}

// retainedBytesLocked reports the output the sandbox keeps. Callers hold sb.mu.
func (sb *sandbox) retainedBytesLocked() int {
	total := 0
	for _, ex := range sb.executions {
		total += ex.retained()
	}
	return total
}

// oldestDroppable returns the oldest execution retention may drop, or nil. The
// running execution and the newest completed one are never dropped: the running
// one is producing output, and the newest completed one is the result a caller
// is about to fetch. Callers hold sb.mu.
func (sb *sandbox) oldestDroppable() *execution {
	if len(sb.executions) < 2 {
		return nil
	}
	ex := sb.executions[0]
	if ex == sb.running || !ex.completed() {
		return nil
	}
	return ex
}

// dropOldestLocked drops one completed execution and its idempotency key, and
// reports how many bytes that freed. Callers hold sb.mu.
func (sb *sandbox) dropOldestLocked() (int, bool) {
	ex := sb.oldestDroppable()
	if ex == nil {
		return 0, false
	}
	if ex.key != "" && sb.idempotency[ex.key] == ex {
		delete(sb.idempotency, ex.key)
	}
	freed := ex.retained()
	// Copy rather than reslice, so the dropped execution stays unreachable.
	sb.executions = append([]*execution(nil), sb.executions[1:]...)
	return freed, true
}

// isKilled reports the terminal state under the sandbox lock. The reaper reads
// it without holding that lock, so an unlocked read would race a transition.
func (sb *sandbox) isKilled() bool {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.state == StateKilled
}
