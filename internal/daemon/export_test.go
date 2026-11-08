package daemon

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
