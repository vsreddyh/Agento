package gateway

// Cap and Len are test-only accessors for SessionStore, kept in the test build
// rather than in agent.go so the production surface is exactly what the gateway
// itself uses.
//
// They exist at all only because session counts are internal bookkeeping and not a
// health signal: exposing them on an unauthenticated endpoint would tell a prober
// how many conversations the gateway is holding. Tests still need them to assert
// LRU eviction directly rather than inferring it from behaviour.

func (s *SessionStore) Cap() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cap
}

func (s *SessionStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byID)
}
