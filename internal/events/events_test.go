package events_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

func newBus(t *testing.T) (*events.Bus, *events.Journal, *store.Store) {
	t.Helper()
	s := storetest.New(t)
	j := events.NewJournal(s)
	return events.NewBus(j), j, s
}

// commit journals events the way the sync engine does — inside the transaction
// that writes the state — and publishes them only after it lands.
func commit(t *testing.T, s *store.Store, j *events.Journal, b *events.Bus, evs ...events.Event) []events.Event {
	t.Helper()
	ctx := context.Background()
	var written []events.Event
	err := s.Write(ctx, func(tx *sql.Tx) error {
		var err error
		written, err = j.Append(ctx, tx, evs)
		return err
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if b != nil {
		b.Publish(written...)
	}
	return written
}

func mustEvent(t *testing.T, typ events.Type, account string, payload any) events.Event {
	t.Helper()
	ev, err := events.New(typ, account, time.Unix(1_700_000_000, 0).UTC(), payload)
	if err != nil {
		t.Fatalf("events.New: %v", err)
	}
	return ev
}

func drain(t *testing.T, sub *events.Subscription, want int) []events.Event {
	t.Helper()
	var got []events.Event
	deadline := time.After(3 * time.Second)
	for len(got) < want {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatalf("stream closed after %d of %d events", len(got), want)
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timed out after %d of %d events", len(got), want)
		}
	}
	return got
}

func TestSequenceNumbersAreAssignedByTheJournal(t *testing.T) {
	_, j, s := newBus(t)
	written := commit(t, s, j, nil,
		mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"id": "1"}),
		mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"id": "2"}),
	)
	if written[0].Seq == 0 || written[1].Seq <= written[0].Seq {
		t.Fatalf("sequence numbers are not monotonic: %d, %d", written[0].Seq, written[1].Seq)
	}
}

func TestASubscriberAttachedBeforeTheReplayMissesNothing(t *testing.T) {
	// The ordering inside Subscribe is the point: attach, then take the
	// cursor, then read. An event committed after the cursor wakes a read
	// that finds it; reading before attaching could miss the wake-up.
	ctx := context.Background()
	b, j, s := newBus(t)

	first := commit(t, s, j, b, mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": 1}))

	sub, err := b.Subscribe(ctx, 0, events.Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	commit(t, s, j, b, mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": 2}))

	// since=0 means "from now", so only the second event should arrive.
	got := drain(t, sub, 1)
	if got[0].Seq == first[0].Seq {
		t.Fatal("a since=0 subscriber was replayed history it did not ask for")
	}
}

func TestResumingFromACursorReplaysExactlyWhatWasMissed(t *testing.T) {
	ctx := context.Background()
	b, j, s := newBus(t)

	written := commit(t, s, j, b,
		mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": 1}),
		mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": 2}),
		mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": 3}),
	)

	// A client that disconnected after the first event reconnects with its
	// Last-Event-ID.
	sub, err := b.Subscribe(ctx, written[0].Seq, events.Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	got := drain(t, sub, 2)
	if got[0].Seq != written[1].Seq || got[1].Seq != written[2].Seq {
		t.Fatalf("replayed %d,%d; want %d,%d", got[0].Seq, got[1].Seq, written[1].Seq, written[2].Seq)
	}
	if sub.Gap() {
		t.Error("a two-event gap should not count as a gap in retention")
	}
}

func TestAnEventIsNeverDeliveredTwice(t *testing.T) {
	ctx := context.Background()
	b, j, s := newBus(t)
	written := commit(t, s, j, nil, mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": 1}))

	sub, err := b.Subscribe(ctx, 0, events.Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	// Publishing something the subscriber's cursor already covers — which is
	// what happens when a replay and a live publish overlap.
	b.Publish(written...)
	commit(t, s, j, b, mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": 2}))

	got := drain(t, sub, 1)
	if len(got) != 1 {
		t.Fatalf("got %d events", len(got))
	}
	select {
	case extra := <-sub.Events():
		t.Fatalf("a duplicate reached the subscriber: seq %d", extra.Seq)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestASlowConsumerMissesNothing(t *testing.T) {
	// The journal is the buffer: a consumer that stops reading holds its
	// subscription where it is, and gets everything, in order, once it reads
	// again. Nothing is dropped for being slow.
	ctx := context.Background()
	b, j, s := newBus(t)

	sub, err := b.Subscribe(ctx, 0, events.Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	// Nothing reads from the subscription while far more than its queue is
	// committed.
	batch := make([]events.Event, 0, 1200)
	for i := 0; i < 1200; i++ {
		batch = append(batch, mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": i}))
	}
	written := commit(t, s, j, b, batch...)
	time.Sleep(50 * time.Millisecond)

	got := drain(t, sub, len(written))
	for i, ev := range got {
		if ev.Seq != written[i].Seq {
			t.Fatalf("event %d is seq %d, want %d", i, ev.Seq, written[i].Seq)
		}
	}
	if sub.Err() != nil {
		t.Fatalf("the stream failed: %v", sub.Err())
	}
}

func TestACursorOlderThanRetentionIsReportedAsAGap(t *testing.T) {
	ctx := context.Background()
	b, j, s := newBus(t)

	var all []events.Event
	for i := 1; i <= 3; i++ {
		all = append(all, commit(t, s, j, nil, mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": i}))...)
	}

	// While the client was away, retention removed the first two events. Its
	// cursor points at the first, so the second is a hole in the stream.
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM events WHERE seq <= ?`, all[1].Seq)
		return err
	}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	sub, err := b.Subscribe(ctx, all[0].Seq, events.Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	if !sub.Gap() {
		t.Fatal("a cursor with pruned events after it must be reported as a gap, not resumed silently")
	}
	// What survives is still delivered: a gap means "incomplete", not "empty".
	got := drain(t, sub, 1)
	if got[0].Seq != all[2].Seq {
		t.Fatalf("delivered seq %d, want %d", got[0].Seq, all[2].Seq)
	}
}

func TestAContiguousCursorIsNotReportedAsAGap(t *testing.T) {
	ctx := context.Background()
	b, j, s := newBus(t)

	var all []events.Event
	for i := 1; i <= 3; i++ {
		all = append(all, commit(t, s, j, nil, mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": i}))...)
	}
	// Retention removed everything the client had already seen, and nothing
	// more: there is no gap, so this is an ordinary resume.
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM events WHERE seq <= ?`, all[0].Seq)
		return err
	}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	sub, err := b.Subscribe(ctx, all[0].Seq, events.Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	if sub.Gap() {
		t.Fatal("a cursor immediately below the retained range is a clean resume, not a gap")
	}
}

func TestAFilteredReplayDoesNotStallOnRejectedEvents(t *testing.T) {
	// The cursor has to advance past events the filter rejected. If it only
	// tracked delivered events, the next read would start inside the rejected
	// run and return it again for as long as the subscription lived.
	ctx := context.Background()
	b, j, s := newBus(t)

	var batch []events.Event
	for i := 0; i < 600; i++ {
		batch = append(batch, mustEvent(t, events.TypeMessageNew, "acc_other", map[string]any{"n": i}))
	}
	batch = append(batch, mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": 999}))
	written := commit(t, s, j, nil, batch...)

	sub, err := b.Subscribe(ctx, 0, events.Filter{AccountIDs: []string{"acc_1"}})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	if sub.Cursor() != written[len(written)-1].Seq {
		t.Fatalf("cursor = %d, want the journal head %d", sub.Cursor(), written[len(written)-1].Seq)
	}

	// Resuming from an old cursor must walk past the 600 rejected rows in
	// batches and still find the one match beyond them.
	resumed, err := b.Subscribe(ctx, written[0].Seq, events.Filter{AccountIDs: []string{"acc_1"}})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer resumed.Close()

	got := drain(t, resumed, 1)
	if got[0].Seq != written[len(written)-1].Seq {
		t.Fatalf("replayed seq %d, want %d", got[0].Seq, written[len(written)-1].Seq)
	}
}

func TestAFilterLimitsBothReplayAndLiveDelivery(t *testing.T) {
	ctx := context.Background()
	b, j, s := newBus(t)

	commit(t, s, j, nil,
		mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": 1}),
		mustEvent(t, events.TypeMessageNew, "acc_2", map[string]any{"n": 2}),
	)

	sub, err := b.Subscribe(ctx, 0, events.Filter{AccountIDs: []string{"acc_1"}, Types: []events.Type{events.TypeMessageNew}})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	commit(t, s, j, b,
		mustEvent(t, events.TypeMessageNew, "acc_2", map[string]any{"n": 3}),
		mustEvent(t, events.TypeFolderChanged, "acc_1", map[string]any{"n": 4}),
		mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": 5}),
	)

	got := drain(t, sub, 1)
	var payload map[string]int
	if err := json.Unmarshal(got[0].Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["n"] != 5 {
		t.Fatalf("the filter let through event %d; only acc_1 message.new should arrive", payload["n"])
	}
}

func TestAnUnknownEventTypeIsRefusedAtTheJournal(t *testing.T) {
	// A type invented at a call site would be invisible to every filter, so
	// it fails loudly at the one place events are created.
	ctx := context.Background()
	_, j, s := newBus(t)

	err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := j.Append(ctx, tx, []events.Event{{Type: "message.invented", AccountID: "acc_1", Payload: []byte("{}")}})
		return err
	})
	if err == nil {
		t.Fatal("the journal accepted an unknown event type")
	}
}

func TestSequenceNumbersSurviveARestart(t *testing.T) {
	// The cursor a client holds has to keep meaning the same thing across a
	// daemon restart, which is why the journal assigns it and not the bus.
	ctx := context.Background()
	dir := t.TempDir() + "/mail.db"

	first := storetest.NewAt(t, dir, nil)
	j1 := events.NewJournal(first)
	written := commit(t, first, j1, nil, mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": 1}))
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second := storetest.NewAt(t, dir, nil)
	j2 := events.NewJournal(second)
	later := commit(t, second, j2, nil, mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": 2}))
	if later[0].Seq <= written[0].Seq {
		t.Fatalf("sequence restarted: %d then %d", written[0].Seq, later[0].Seq)
	}

	missed, scanned, err := j2.Since(ctx, written[0].Seq, events.Filter{}, 0)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if scanned != later[0].Seq {
		t.Fatalf("Since scanned up to %d, want %d", scanned, later[0].Seq)
	}
	if len(missed) != 1 || missed[0].Seq != later[0].Seq {
		t.Fatalf("replay after restart returned %d events", len(missed))
	}
}

func TestPruneKeepsTheNewestEventsAndAnythingAWebhookStillNeeds(t *testing.T) {
	ctx := context.Background()
	_, j, s := newBus(t)

	var all []events.Event
	for i := 0; i < 20; i++ {
		all = append(all, commit(t, s, j, nil, mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": i}))...)
	}
	// Age every event past the retention window.
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE events SET created_at = 0`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// One of the oldest still has a delivery waiting to be retried.
	pendingSeq := all[0].Seq
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO webhooks(id, url, secret_ciphertext, keyid, created_at, updated_at)
			 VALUES ('whk_1', 'https://example.com/hook', x'00', 1, 0, 0)`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO webhook_deliveries(webhook_id, event_seq, next_attempt_at, status, created_at, updated_at)
			 VALUES ('whk_1', ?, 0, 'pending', 0, 0)`, pendingSeq)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := j.Prune(ctx, time.Now(), events.Retention{MaxAge: time.Hour, MinRows: 5}); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	var remaining int
	if err := s.Reader().QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	// Five newest kept by MinRows, plus the one a webhook still needs.
	if remaining != 6 {
		t.Fatalf("after pruning %d events remain, want 6", remaining)
	}
	var pendingSurvived int
	if err := s.Reader().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE seq = ?`, pendingSeq).Scan(&pendingSurvived); err != nil {
		t.Fatal(err)
	}
	if pendingSurvived != 1 {
		t.Fatal("retention deleted the payload a pending webhook delivery was about to send")
	}
}

func TestLatestAndEarliestBoundTheRetainedRange(t *testing.T) {
	ctx := context.Background()
	_, j, s := newBus(t)

	if seq, err := j.Latest(ctx); err != nil || seq != 0 {
		t.Fatalf("Latest on an empty journal = %d (%v), want 0", seq, err)
	}
	written := commit(t, s, j, nil,
		mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": 1}),
		mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": 2}),
	)
	latest, err := j.Latest(ctx)
	if err != nil || latest != written[1].Seq {
		t.Fatalf("Latest = %d (%v), want %d", latest, err, written[1].Seq)
	}
	earliest, err := j.Earliest(ctx)
	if err != nil || earliest != written[0].Seq {
		t.Fatalf("Earliest = %d (%v), want %d", earliest, err, written[0].Seq)
	}
}

func TestTheCursorAfterCloseResumesWithoutLosingAnything(t *testing.T) {
	// A long poll takes the cursor after closing its subscription and hands
	// it back as next_cursor. Every event up to it must have been queued, and
	// none after it: resuming from it must give exactly the rest.
	ctx := context.Background()
	b, j, s := newBus(t)
	sub, err := b.Subscribe(ctx, 0, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}

	batch := make([]events.Event, 0, 1500)
	for i := range 1500 {
		batch = append(batch, mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": i}))
	}
	written := commit(t, s, j, b, batch...)
	got := drain(t, sub, 100)
	sub.Close()
	for ev := range sub.Events() {
		got = append(got, ev)
	}
	for i, ev := range got {
		if ev.Seq != written[i].Seq {
			t.Fatalf("event %d is seq %d, want %d: the stream has a hole", i, ev.Seq, written[i].Seq)
		}
	}
	if cursor := sub.Cursor(); cursor != got[len(got)-1].Seq {
		t.Fatalf("cursor %d after close, but the last event queued was %d", cursor, got[len(got)-1].Seq)
	}

	resumed, err := b.Subscribe(ctx, sub.Cursor(), events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	rest := drain(t, resumed, len(written)-len(got))
	if rest[0].Seq != written[len(got)].Seq || rest[len(rest)-1].Seq != written[len(written)-1].Seq {
		t.Errorf("resumed %d..%d, want %d..%d", rest[0].Seq, rest[len(rest)-1].Seq,
			written[len(got)].Seq, written[len(written)-1].Seq)
	}
}

func TestAReplayLongerThanTheQueueDeliversEverything(t *testing.T) {
	ctx := context.Background()
	b, j, s := newBus(t)
	first := commit(t, s, j, nil, mustEvent(t, events.TypeMessageNew, "acc_1", nil))[0].Seq
	batch := make([]events.Event, 0, 1200)
	for range 1200 {
		batch = append(batch, mustEvent(t, events.TypeMessageNew, "acc_1", nil))
	}
	commit(t, s, j, nil, batch...)

	sub, err := b.Subscribe(ctx, first, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	got := drain(t, sub, 1200)
	for i, ev := range got {
		if ev.Seq != first+1+int64(i) {
			t.Fatalf("event %d is seq %d, want %d", i, ev.Seq, first+1+int64(i))
		}
	}
}

func TestEventsPublishedOutOfOrderAreAllDelivered(t *testing.T) {
	// Two workers commit in sequence order — the database has one writer —
	// but publish in whatever order their goroutines get to it. The later
	// one arriving first must not move the cursor past the earlier one.
	ctx := context.Background()
	b, j, s := newBus(t)
	sub, err := b.Subscribe(ctx, 0, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	n := commit(t, s, j, nil, mustEvent(t, events.TypeMessageNew, "acc_a", map[string]any{"n": "N"}))[0]
	n1 := commit(t, s, j, nil, mustEvent(t, events.TypeMessageNew, "acc_b", map[string]any{"n": "N+1"}))[0]
	b.Publish(n1)
	first := drain(t, sub, 1)
	b.Publish(n)
	got := append(first, drain(t, sub, 1)...)
	if got[0].Seq != n.Seq || got[1].Seq != n1.Seq {
		t.Fatalf("delivered %d, %d; want %d, %d", got[0].Seq, got[1].Seq, n.Seq, n1.Seq)
	}

	// And a subscriber that resumes from where this one stands has nothing
	// missing behind it.
	sub.Close()
	resumed, err := b.Subscribe(ctx, sub.Cursor(), events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if sub.Cursor() != n1.Seq {
		t.Fatalf("cursor %d, want %d", sub.Cursor(), n1.Seq)
	}
	select {
	case ev := <-resumed.Events():
		t.Fatalf("resuming from the cursor replayed seq %d: it had been skipped", ev.Seq)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestConcurrentPublishersNeverLoseAnEvent(t *testing.T) {
	// Several goroutines commit and publish at once, as the sync workers of
	// several accounts and the account registry do. Every committed event
	// reaches every subscriber, in sequence order, whatever order the
	// publishes land in, and whatever a subscriber's filter costs.
	ctx := context.Background()
	b, j, s := newBus(t)
	slow := func(ev events.Event) bool {
		time.Sleep(50 * time.Microsecond)
		return true
	}
	subs := make([]*events.Subscription, 0, 4)
	for i := range 4 {
		f := events.Filter{}
		if i%2 == 1 {
			f.Allow = slow
		}
		sub, err := b.Subscribe(ctx, 0, f)
		if err != nil {
			t.Fatal(err)
		}
		defer sub.Close()
		subs = append(subs, sub)
	}

	const workers, per = 8, 50
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range per {
				var out []events.Event
				err := s.Write(ctx, func(tx *sql.Tx) error {
					var err error
					out, err = j.Append(ctx, tx, []events.Event{
						mustEvent(t, events.TypeMessageNew, fmt.Sprintf("acc_%d", w), map[string]any{"i": i}),
					})
					return err
				})
				if err != nil {
					t.Error(err)
					return
				}
				b.Publish(out...)
			}
		}()
	}
	wg.Wait()
	for i, sub := range subs {
		got := drain(t, sub, workers*per)
		for k := 1; k < len(got); k++ {
			if got[k].Seq != got[k-1].Seq+1 {
				t.Fatalf("subscriber %d: seq %d followed by %d", i, got[k-1].Seq, got[k].Seq)
			}
		}
	}
}

func TestAnEventCommittedDuringAReplayIsDeliveredAfterIt(t *testing.T) {
	// The engine commits and publishes while a reconnecting client's replay
	// is being read. Resuming after 1 with {2, 3} in the journal and 4
	// committed in the middle of the scan delivers 2, 3, 4 — the live event
	// must not overtake the replay and move the cursor past it.
	ctx := context.Background()
	b, j, s := newBus(t)
	written := commit(t, s, j, nil,
		mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": 1}),
		mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": 2}),
		mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": 3}),
	)
	var (
		fired atomic.Bool
		live  atomic.Int64
	)
	allow := func(ev events.Event) bool {
		if fired.CompareAndSwap(false, true) {
			live.Store(commit(t, s, j, b, mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": 4}))[0].Seq)
		}
		return true
	}
	sub, err := b.Subscribe(ctx, written[0].Seq, events.Filter{Allow: allow})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	got := drain(t, sub, 3)
	want := []int64{written[1].Seq, written[2].Seq, live.Load()}
	for i := range want {
		if got[i].Seq != want[i] {
			t.Fatalf("delivered %d, %d, %d; want %v", got[0].Seq, got[1].Seq, got[2].Seq, want)
		}
	}
	select {
	case ev := <-sub.Events():
		t.Fatalf("an extra event: seq %d", ev.Seq)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestResumedSubscriptionsEachGetAContiguousRunWhilePublishersRun(t *testing.T) {
	// Clients reconnect with Last-Event-ID while the engine keeps committing.
	// Every one of them gets every event after its cursor, with no hole where
	// a live publish landed during its replay.
	ctx := context.Background()
	b, j, s := newBus(t)
	var start []events.Event
	for i := range 300 {
		start = append(start, mustEvent(t, events.TypeMessageNew, "acc_1", map[string]any{"n": i}))
	}
	base := commit(t, s, j, nil, start...)

	stop := make(chan struct{})
	published := make(chan int64, 1)
	go func() {
		var last int64
		defer func() { published <- last }()
		for {
			select {
			case <-stop:
				return
			default:
			}
			last = commit(t, s, j, b, mustEvent(t, events.TypeMessageNew, "acc_2", nil))[0].Seq
		}
	}()

	subs := make([]*events.Subscription, 8)
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i := range subs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			sub, err := b.Subscribe(ctx, base[i*10].Seq, events.Filter{})
			if err != nil {
				t.Error(err)
				return
			}
			subs[i] = sub
		}()
	}
	close(gate)
	wg.Wait()
	time.Sleep(100 * time.Millisecond)
	close(stop)
	last := <-published

	for i, sub := range subs {
		if sub == nil {
			t.FailNow()
		}
		want := int(last - base[i*10].Seq)
		got := drain(t, sub, want)
		for k, ev := range got {
			if ev.Seq != base[i*10].Seq+1+int64(k) {
				t.Fatalf("subscription %d: event %d is seq %d, want %d", i, k, ev.Seq, base[i*10].Seq+1+int64(k))
			}
		}
		sub.Close()
	}
}

func TestAnAllowThatReadsTheDatabaseDoesNotStarveTheReaders(t *testing.T) {
	// The ownership check looks accounts up. Run from inside an open journal
	// cursor, a handful of replays at once would hold every reader connection
	// and each wait for another one, for good. Allow runs only after the
	// batch is read and its cursor closed.
	b, j, s := newBus(t)
	var evs []events.Event
	for i := range 200 {
		evs = append(evs, mustEvent(t, events.TypeMessageNew, fmt.Sprintf("acc_%d", i%3), nil))
	}
	written := commit(t, s, j, nil, evs...)
	for round := range 20 {
		ctx, cancel := context.WithCancel(context.Background())
		lookup := func(ev events.Event) bool {
			var n int
			err := s.Reader().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE seq = ?`, ev.Seq).Scan(&n)
			return err == nil && n == 1
		}
		gate := make(chan struct{})
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				sub, err := b.Subscribe(ctx, written[0].Seq, events.Filter{Allow: lookup})
				if err != nil {
					t.Error(err)
					return
				}
				defer sub.Close()
				for range len(written) - 1 {
					if _, ok := <-sub.Events(); !ok {
						if ctx.Err() == nil {
							t.Errorf("the stream closed early: %v", sub.Err())
						}
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
		case <-time.After(10 * time.Second):
			cancel()
			<-done
			t.Fatalf("round %d: concurrent replays with a database-reading filter hung", round)
		}
	}
}

func TestClosingASubscriptionWhileEventsArePublishedNeverPanics(t *testing.T) {
	// A client disconnecting while the engine publishes must not take the
	// daemon down with a send on a closed channel.
	ctx := context.Background()
	b, _, _ := newBus(t)
	ev := mustEvent(t, events.TypeMessageNew, "acc_1", nil)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for seq := int64(1); ; seq++ {
			select {
			case <-stop:
				return
			default:
			}
			ev.Seq = seq
			b.Publish(ev)
		}
	}()
	for range 2000 {
		sub, err := b.Subscribe(ctx, 0, events.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		sub.Close()
	}
	close(stop)
	<-done
}

func TestAnAllowFunctionFiltersTheReplayAndTheLiveStream(t *testing.T) {
	ctx := context.Background()
	b, j, s := newBus(t)
	start := commit(t, s, j, nil, mustEvent(t, events.TypeMessageNew, "acc_mine", nil))[0].Seq
	commit(t, s, j, nil,
		mustEvent(t, events.TypeMessageNew, "acc_theirs", nil),
		mustEvent(t, events.TypeMessageNew, "acc_mine", nil))

	mine := func(ev events.Event) bool { return ev.AccountID == "acc_mine" }
	sub, err := b.Subscribe(ctx, start, events.Filter{Allow: mine})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	commit(t, s, j, b,
		mustEvent(t, events.TypeMessageNew, "acc_theirs", nil),
		mustEvent(t, events.TypeMessageNew, "acc_mine", nil))
	for _, ev := range drain(t, sub, 2) {
		if ev.AccountID != "acc_mine" {
			t.Errorf("received %s's event", ev.AccountID)
		}
	}
	select {
	case ev := <-sub.Events():
		t.Errorf("an extra event of %s", ev.AccountID)
	case <-time.After(50 * time.Millisecond):
	}
}
