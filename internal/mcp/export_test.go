package mcp

import "time"

// SetWaitStep shortens the steps of wait_for_new_mail, so a test sees its
// progress notifications in seconds rather than minutes.
func SetWaitStep(s *Server, d time.Duration) { s.waitStep = d }

// SetBetweenSteps runs fn between two steps of every wait.
func SetBetweenSteps(s *Server, fn func()) { s.betweenSteps = fn }

// SetClock replaces the event store's clock and its sweep timer.
func (s *EventStore) SetClock(now func() time.Time, afterFunc func(time.Duration, func()) *time.Timer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now, s.afterFunc = now, afterFunc
}

// Tracked reports how many streams and items the store keeps track of for a
// session, whatever they hold.
func (s *EventStore) Tracked(session string) (streams, items int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.sessions[session] {
		streams++
		items += len(st.items)
	}
	return streams, items
}

// SetMaxCallsInFlight changes how many calls one key may run at once.
func SetMaxCallsInFlight(s *Server, n int) { s.running = newCallLimit(n) }

// CallsInFlight reports how many calls a key is running.
func CallsInFlight(s *Server, keyPrefix string) int { return s.running.count(keyPrefix) }

// SessionsOpen reports how many sessions a key holds open over HTTP.
func SessionsOpen(h *HTTP, keyPrefix string) int { return h.sessions.open(keyPrefix) }
