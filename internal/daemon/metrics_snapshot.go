package daemon

// SessionCounts returns the same atomic registry census as status, without
// allocating session rows or exposing paths and client identities.
func (s *Server) SessionCounts() (socket, retained, transport int) {
	snapshot := s.sessions.statusSnapshot(false)
	return snapshot.socket, snapshot.retained, snapshot.transport
}
