package daemon

import "context"

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

// RetryRemovalsForTest runs one removal-retry pass, so a test can assert that
// a container whose removal failed is retried rather than leaked.
func (s *Sandboxes) RetryRemovalsForTest(ctx context.Context) { s.retryRemovals(ctx) }

// ScheduleRemovalCountForTest reports how many containers are waiting for a
// removal retry.
func (s *Sandboxes) ScheduleRemovalCountForTest() int {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	return len(s.retryIDs)
}
