package store_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

// clock is a settable time for a store.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func reserve(t *testing.T, db *store.Store, key, hash string) (store.Send, bool) {
	t.Helper()
	row, reserved, err := db.ReserveSend(context.Background(), store.SendReservation{
		AccountID: "acc_1", Key: key, ComposeHash: hash, MessageID: key + "@example.com", Recipients: 2,
		CreatedBy: "usr_1", UserID: "usr_1",
	})
	if err != nil {
		t.Fatalf("ReserveSend(%s): %v", key, err)
	}
	return row, reserved
}

func finishedEvents(t *testing.T, db *store.Store) []store.SendFinished {
	t.Helper()
	evs, _, err := events.NewJournal(db).Since(context.Background(), 0,
		events.Filter{Types: []events.Type{events.TypeSendFinished}}, 100)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]store.SendFinished, 0, len(evs))
	for _, ev := range evs {
		out = append(out, payload[store.SendFinished](t, ev))
	}
	return out
}

func TestASendingRowBecomesUnknownAtBoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.db")
	f := &indexFixture{t: t, db: storetest.NewAt(t, path, nil), account: "acc_1", now: t0}
	f.exec(`INSERT INTO users(id, email, password_hash, role, password_changed_at, created_at, updated_at)
		VALUES ('usr_1', 'person@example.com', '$argon2id$', 'owner', 1, 1, 1)`)
	f.addAccount("acc_1", "usr_1")
	// A daemon stopped mid-submission, and one that had finished.
	reserve(t, f.db, "in-flight", "h1")
	reserve(t, f.db, "done", "h2")
	if _, _, err := f.db.FinishSend(context.Background(), store.SendOutcome{
		AccountID: "acc_1", Key: "done", State: store.SendSent, Attempts: 1,
	}); err != nil {
		t.Fatal(err)
	}

	n, err := f.db.InterruptSends(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("InterruptSends = %d, %v; want the one in flight", n, err)
	}
	row, err := f.db.SendOf(context.Background(), "acc_1", "in-flight")
	if err != nil || row.State != store.SendUnknown || row.Reason != store.ReasonInterrupted {
		t.Fatalf("the interrupted send = %+v, %v; want unknown, never failed", row, err)
	}
	if row, _ := f.db.SendOf(context.Background(), "acc_1", "done"); row.State != store.SendSent {
		t.Fatalf("a finished send became %s", row.State)
	}
	// Each names its sender, who alone hears of it (internal/service).
	finished := finishedEvents(t, f.db)
	if len(finished) != 2 || finished[1] != (store.SendFinished{AccountID: "acc_1", Key: "in-flight", State: "unknown", UserID: "usr_1"}) {
		t.Fatalf("send.finished = %+v", finished)
	}
	// The key it held is not free: the message may have gone.
	if row, reserved := reserve(t, f.db, "in-flight", "h1"); reserved || row.State != store.SendUnknown {
		t.Fatalf("reserving the interrupted key again = %+v, %t", row, reserved)
	}
	if n, err := f.db.InterruptSends(context.Background()); err != nil || n != 0 {
		t.Fatalf("a second start found %d (%v)", n, err)
	}
}

func TestASendTheSentFolderConfirmsStillNamesItsSender(t *testing.T) {
	// An unknown send turns sent when its copy appears in Sent, in the
	// sync's transaction; that notice is the sender's as much as the first,
	// and names them, so that it reaches them alone.
	f := newIndex(t)
	reserve(t, f.db, "lost", "h1")
	if _, _, err := f.db.FinishSend(context.Background(), store.SendOutcome{
		AccountID: "acc_1", Key: "lost", State: store.SendUnknown, Attempts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	ids := f.gmailFolders()
	copyInSent := mail(1, "lost")
	copyInSent.Envelope.MessageID = "lost@example.com"
	f.live(ids["[Gmail]/Sent Mail"], copyInSent)
	if row, err := f.db.SendOf(context.Background(), "acc_1", "lost"); err != nil || row.State != store.SendSent {
		t.Fatalf("the send after its copy appeared: %+v, %v", row, err)
	}
	finished := finishedEvents(t, f.db)
	if len(finished) != 2 || finished[1] != (store.SendFinished{AccountID: "acc_1", Key: "lost", State: "sent", UserID: "usr_1"}) {
		t.Fatalf("send.finished = %+v", finished)
	}
}

func TestSendRowsAreDeletedAfterThirtyDays(t *testing.T) {
	c := &clock{now: t0}
	f := &indexFixture{t: t, db: storetest.NewAt(t, filepath.Join(t.TempDir(), "mail.db"), c.Now), account: "acc_1", now: t0}
	f.exec(`INSERT INTO users(id, email, password_hash, role, password_changed_at, created_at, updated_at)
		VALUES ('usr_1', 'person@example.com', '$argon2id$', 'owner', 1, 1, 1)`)
	f.addAccount("acc_1", "usr_1")
	for _, key := range []string{"old", "unknown-old", "recent", "stuck"} {
		reserve(t, f.db, key, key)
	}
	for key, state := range map[string]string{"old": store.SendSent, "unknown-old": store.SendUnknown} {
		if _, _, err := f.db.FinishSend(context.Background(), store.SendOutcome{AccountID: "acc_1", Key: key, State: state}); err != nil {
			t.Fatal(err)
		}
	}
	// Changed later: its thirty days start then.
	c.set(t0.Add(10 * 24 * time.Hour))
	if _, _, err := f.db.FinishSend(context.Background(), store.SendOutcome{AccountID: "acc_1", Key: "recent", State: store.SendFailed}); err != nil {
		t.Fatal(err)
	}

	const thirtyDays = 30 * 24 * time.Hour
	if store.SendRetention != thirtyDays {
		t.Fatalf("SendRetention = %s; the privacy policy says thirty days", store.SendRetention)
	}
	c.set(t0.Add(thirtyDays + time.Minute))
	records, notices, err := f.db.SweepSends(context.Background(), time.Hour)
	if err != nil || records != 2 || notices != 2 {
		t.Fatalf("SweepSends = %d records, %d notices, %v; want the two changed over thirty days ago, and what "+
			"was journaled about them", records, notices, err)
	}
	for key, want := range map[string]bool{"old": false, "unknown-old": false, "recent": true, "stuck": true} {
		_, err := f.db.SendOf(context.Background(), "acc_1", key)
		if got := err == nil; got != want {
			t.Errorf("%s kept = %t (%v), want %t", key, got, err, want)
		}
	}
	// The send.finished of a swept record is part of it: the journal keeps
	// its newest events whatever their age, so without the sweep it would
	// outlive the record.
	if finished := finishedEvents(t, f.db); len(finished) != 1 || finished[0].Key != "recent" {
		t.Fatalf("send.finished after the sweep = %+v; want only the recent send's", finished)
	}
	// One an hour short of its thirty days goes now, not an hour late.
	c.set(t0.Add(10*24*time.Hour + thirtyDays - 30*time.Minute))
	if records, notices, err := f.db.SweepSends(context.Background(), time.Hour); err != nil || records != 1 || notices != 1 {
		t.Fatalf("SweepSends = %d, %d, %v; want the recent one, due before the next sweep", records, notices, err)
	}
	if finished := finishedEvents(t, f.db); len(finished) != 0 {
		t.Fatalf("send.finished left = %+v", finished)
	}
	// A send still sending is never swept: the start makes it unknown first.
	if _, err := f.db.SendOf(context.Background(), "acc_1", "stuck"); err != nil {
		t.Fatalf("a send in flight was swept: %v", err)
	}
}

func TestAKeyIsTakenOnceAndAFailedOneCanBeTakenAgain(t *testing.T) {
	f := newIndex(t)
	first, reserved := reserve(t, f.db, "k", "h1")
	if !reserved || first.State != store.SendSending || first.Recipients != 2 || first.UserID != "usr_1" {
		t.Fatalf("first = %+v, %t", first, reserved)
	}
	// Taken: the second request sees the first's row, whatever it sends.
	if row, reserved := reserve(t, f.db, "k", "h2"); reserved || row.ComposeHash != "h1" {
		t.Fatalf("second = %+v, %t", row, reserved)
	}
	if _, _, err := f.db.FinishSend(context.Background(), store.SendOutcome{
		AccountID: "acc_1", Key: "k", State: store.SendFailed, Reason: "recipients_refused", Attempts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// Failed sent nothing: the key starts over, with the new message.
	again, reserved := reserve(t, f.db, "k", "h2")
	if !reserved || again.State != store.SendSending || again.ComposeHash != "h2" || again.Reason != "" || again.Attempts != 0 {
		t.Fatalf("after a failure = %+v, %t", again, reserved)
	}
	// Only a row still sending is finished.
	if _, _, err := f.db.FinishSend(context.Background(), store.SendOutcome{AccountID: "acc_1", Key: "k", State: store.SendSent}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.db.FinishSend(context.Background(), store.SendOutcome{AccountID: "acc_1", Key: "k", State: store.SendFailed}); !errors.Is(err, store.ErrNoSend) {
		t.Fatalf("finishing a finished send: %v", err)
	}

	// The daily limit counts the person's sends of the last day, in the
	// transaction that takes the key.
	reserve(t, f.db, "k2", "h")
	reserve(t, f.db, "k3", "h")
	_, _, err := f.db.ReserveSend(context.Background(), store.SendReservation{
		AccountID: "acc_1", Key: "k4", ComposeHash: "h", MessageID: "k4@example.com", UserID: "usr_1", DailyLimit: 3,
	})
	if !errors.Is(err, store.ErrSendQuota) {
		t.Fatalf("the fourth send of a day with a limit of three: %v", err)
	}
	// A key has no person: its own sends are what its limit counts, the
	// person's none of them.
	for i := range 3 {
		key := fmt.Sprintf("key-%d", i)
		if _, reserved, err := f.db.ReserveSend(context.Background(), store.SendReservation{
			AccountID: "acc_1", Key: key, ComposeHash: "h", MessageID: key + "@example.com", CreatedBy: "key:cccccccc",
			DailyLimit: 3,
		}); err != nil || !reserved {
			t.Fatalf("a key's send %d: %t, %v", i, reserved, err)
		}
	}
	if _, _, err := f.db.ReserveSend(context.Background(), store.SendReservation{
		AccountID: "acc_1", Key: "key-3", ComposeHash: "h", MessageID: "key-3@example.com", CreatedBy: "key:cccccccc",
		DailyLimit: 3,
	}); !errors.Is(err, store.ErrSendQuota) {
		t.Fatalf("a key's fourth send of a day with a limit of three: %v", err)
	}
	// Without a limit — an instance key's, the operator's — nothing is
	// counted.
	if _, reserved, err := f.db.ReserveSend(context.Background(), store.SendReservation{
		AccountID: "acc_1", Key: "k5", ComposeHash: "h", MessageID: "k5@example.com", CreatedBy: "key:cccccccc",
	}); err != nil || !reserved {
		t.Fatalf("a send without a limit: %t, %v", reserved, err)
	}
}

func TestAFailedKeyTakenAgainCountsOnTheDayItIsTakenAgain(t *testing.T) {
	c := &clock{now: t0}
	f := &indexFixture{t: t, db: storetest.NewAt(t, filepath.Join(t.TempDir(), "mail.db"), c.Now), account: "acc_1", now: t0}
	f.exec(`INSERT INTO users(id, email, password_hash, role, password_changed_at, created_at, updated_at)
		VALUES ('usr_1', 'person@example.com', '$argon2id$', 'owner', 1, 1, 1)`)
	f.addAccount("acc_1", "usr_1")
	take := func(key string) error {
		t.Helper()
		_, _, err := f.db.ReserveSend(context.Background(), store.SendReservation{
			AccountID: "acc_1", Key: key, ComposeHash: key, MessageID: key + "@example.com", UserID: "usr_1", DailyLimit: 3,
		})
		return err
	}
	// A key that failed yesterday: it sent nothing, and counted then.
	if err := take("yesterday"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.db.FinishSend(context.Background(), store.SendOutcome{
		AccountID: "acc_1", Key: "yesterday", State: store.SendFailed, Reason: "refused",
	}); err != nil {
		t.Fatal(err)
	}

	c.set(t0.Add(25 * time.Hour))
	for _, key := range []string{"today-1", "today-2"} {
		if err := take(key); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
	}
	// Taken again today it may send any message, so it is one of today's.
	if err := take("yesterday"); err != nil {
		t.Fatalf("taking the failed key again: %v", err)
	}
	if err := take("today-3"); !errors.Is(err, store.ErrSendQuota) {
		t.Fatalf("a fourth send today, with the limit at three and a key from yesterday taken again: %v", err)
	}
}
