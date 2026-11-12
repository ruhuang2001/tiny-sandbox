package daemon

import (
	"context"
	"time"
)

// ContainerIDForTest exposes the backend container ID so daemon_test can drive
// the in-memory backend's failure modes, such as an external stop.
func (s *Sandboxes) ContainerIDForTest(sandboxID string) string {
	sb, err := s.lookup(sandboxID)
	if err != nil {
		return ""
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.container
}

// RetryRemovalsForTest runs one removal-retry pass to completion, so a test
// can assert that a container whose removal failed is retried rather than
// leaked. The production path runs the pass on its own goroutine; the test
// waits for it so the assertions do not race the pass.
func (s *Sandboxes) RetryRemovalsForTest(ctx context.Context) {
	s.retryRemovals(ctx)
	s.waitRemovalPassForTest()
}

// waitRemovalPassForTest blocks until the asynchronous pass has finished.
func (s *Sandboxes) waitRemovalPassForTest() {
	for {
		s.retryMu.Lock()
		running := s.removalPassRunning
		s.retryMu.Unlock()
		if !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// ScheduleRemovalCountForTest reports how many containers are waiting for a
// removal retry.
func (s *Sandboxes) ScheduleRemovalCountForTest() int {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	return len(s.retryIDs)
}

// RetentionForTest runs one retention pass, which the reaper otherwise runs on
// its own beat, so a test can assert the bounds without waiting for a tick.
func (s *Sandboxes) RetentionForTest() {
	s.prune()
}

// RetainedBytesForTest reports the output the store holds.
func (s *Sandboxes) RetainedBytesForTest() int {
	total := 0
	for _, sb := range s.sandboxesInCreationOrder() {
		if sb.mu.TryLock() {
			total += sb.retainedBytesLocked()
			sb.mu.Unlock()
		}
	}
	return total
}
