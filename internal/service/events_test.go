package service_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/service"
)

func TestResumedStreamsReconnectingTogetherNeverStallTheDaemonsReads(t *testing.T) {
	// After a restart every open console and SSE client reconnects with its
	// Last-Event-ID within the same second. Each replay checks, per event,
	// whether the caller may see the account — a database lookup for
	// accounts it has not met yet. With the lookup made inside the journal's
	// open cursor, four replays at once held all four reader connections and
	// waited on each other for good, and every read in the daemon with them.
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	bob := f.person(t, "bob@example.com", auth.RoleMember)
	carol := f.person(t, "carol@example.com", auth.RoleMember)
	anas := f.mailbox(t, ana, "ana@mail.example")
	bobs := f.mailbox(t, bob, "bob@mail.example")
	carols := f.mailbox(t, carol, "carol@mail.example")
	start := f.publish(t, newMail(t, anas, "inbox", "start"))[0].Seq
	var evs []events.Event
	for range 40 {
		evs = append(evs, newMail(t, anas, "inbox", "ana"), newMail(t, bobs, "inbox", "bob"),
			newMail(t, carols, "inbox", "carol"))
	}
	f.publish(t, evs...)

	for round := range 200 {
		ctx, cancel := context.WithCancel(t.Context())
		gate := make(chan struct{})
		var wg sync.WaitGroup
		for i := range 6 {
			who := []service.Principal{ana, bob, carol}[i%3]
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				st, err := f.svc.Subscribe(ctx, who, start, service.EventFilter{})
				if err != nil {
					t.Error(err)
					return
				}
				defer st.Close()
				// A person's own 40 events; bob and carol never saw "start".
				for n := 0; n < 40; n++ {
					if _, ok := <-st.Events(); !ok {
						t.Errorf("a stream closed early: %v", st.Err())
						return
					}
				}
			}()
		}
		close(gate)
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
			cancel()
		case <-time.After(5 * time.Second):
			probe, stop := context.WithTimeout(t.Context(), time.Second)
			_, perr := f.svc.ListAccounts(probe, bob)
			stop()
			cancel()
			t.Fatalf("round %d: resumed streams hung; another reader's ListAccounts: %v", round, perr)
		}
		// Reads go on for everyone else meanwhile.
		if _, err := f.svc.ListAccounts(t.Context(), bob); err != nil {
			t.Fatalf("round %d: ListAccounts: %v", round, err)
		}
	}
}
