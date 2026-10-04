package mcp

import (
	"context"
	"sync"
	"testing"
)

func TestAnInboxSubscribedAsTheLastOtherIsUnsubscribedIsStillWatched(t *testing.T) {
	// A client replacing its subscription to one inbox with one to another
	// sends both at once, and the SDK handles them at once. Whichever order
	// they land in, the new inbox must end up watched. The bad interleaving
	// is narrow; under -race, as make check runs, it comes up within a few
	// hundred rounds when there is one.
	const a, b = "acc_000000000000000a", "acc_000000000000000b"
	for range 5000 {
		w := &watcher{accounts: map[string]bool{}}
		w.start = func(ctx context.Context) { <-ctx.Done() }
		w.add(t.Context(), nil, a)
		ready := make(chan struct{})
		var both sync.WaitGroup
		both.Go(func() { <-ready; w.remove(a) })
		both.Go(func() { <-ready; w.add(t.Context(), nil, b) })
		close(ready)
		both.Wait()
		w.mu.Lock()
		watching := w.cancel != nil
		w.mu.Unlock()
		if !watching {
			t.Fatal("an inbox subscribed while the last other one was unsubscribed is not watched")
		}
		w.remove(b)
	}
}
