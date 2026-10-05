package mcpbridge

import (
	"testing"
	"time"
)

// SetListenTimings shortens how a bridge started after it asks for its
// standalone stream again (the first wait, the longest, and how long a
// stream the server still holds is waited for), until the test ends.
func SetListenTimings(t testing.TB, firstDelay, maxDelay, held time.Duration) {
	t.Helper()
	previous := defaultListenTimings
	defaultListenTimings = listenTimings{first: firstDelay, max: maxDelay, held: held}
	t.Cleanup(func() { defaultListenTimings = previous })
}
