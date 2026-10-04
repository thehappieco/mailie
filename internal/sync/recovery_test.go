package sync_test

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/store"
	syncengine "github.com/thehappieco/mailie/internal/sync"
)

func TestAFolderThatFailsNeverStopsTheOthers(t *testing.T) {
	// A folder the server will not open is recorded as failing and retried
	// later. The connection stays up, the other folders finish their initial
	// sync, and new mail keeps arriving.
	h := newHarness(t, setup{caps: providertest.GmailCaps()})
	for _, name := range []string{"Broken", "Receipts"} {
		h.box.CreateFolder(name)
		h.box.Deliver(name, mail(name, time.Now().Add(-time.Hour)))
	}
	h.box.Deliver("INBOX", mail("a", time.Now().Add(-time.Hour)))
	h.box.OnCall(func(_ context.Context, c providertest.Call) error {
		if c.Method == providertest.MethodSelect && c.Folder == "Broken" {
			return fmt.Errorf("%w: NO [SERVERBUG] cannot open this mailbox", provider.ErrTemporary)
		}
		return nil
	})
	h.start()
	h.waitFor("the other folders indexed and the broken one recorded", 5*time.Second, func() bool {
		return h.messages() == 2 && h.count(`SELECT count(*) FROM folders
			WHERE name = 'Broken' AND sync_state = 'error' AND sync_error = 'temporary'`) == 1
	})
	h.waitFor("the idle connection", 5*time.Second, func() bool { return h.box.CallCount(providertest.MethodIdle) > 0 })
	sub := h.subscribe()

	h.box.Deliver("INBOX", mail("fresh", time.Now()))
	if got := payload[store.MessageNew](t, next(t, sub, events.TypeMessageNew, 2*time.Second)); got.Subject != "Subject fresh" {
		t.Errorf("message.new = %s", describe(got))
	}
	h.box.Deliver("Receipts", mail("label", time.Now()))
	if err := h.engine.Trigger(context.Background(), h.account); err != nil {
		t.Fatal(err)
	}
	if got := payload[store.MessageNew](t, next(t, sub, events.TypeMessageNew, 2*time.Second)); got.FolderID != h.folderID("Receipts") {
		t.Errorf("message.new = %s, want the label's", describe(got))
	}
	if n := h.box.Opens(provider.RoleSync); n != 1 {
		t.Errorf("%d sync connections: one folder's failure cost the account its connection", n)
	}
	if n := h.count(`SELECT consecutive_failures FROM accounts WHERE id = ?`, h.account); n != 0 {
		t.Errorf("a folder's failure was recorded against the account (%d failures)", n)
	}
}

func TestAPanicInAPassIsRecoveredAndTheAccountReconnects(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.GmailCaps()})
	for i := range 5 {
		h.box.Deliver("INBOX", mail(fmt.Sprintf("m%d", i), time.Now().Add(-time.Hour)))
	}
	var once sync.Once
	h.box.OnCall(func(_ context.Context, c providertest.Call) error {
		if c.Method != providertest.MethodFetchSummaries {
			return nil
		}
		boom := false
		once.Do(func() { boom = true })
		if boom {
			panic("a bug somewhere under a pass")
		}
		return nil
	})
	h.start()
	h.waitLive()
	if n := h.messages(); n != 5 {
		t.Errorf("%d messages indexed after the panic, want 5", n)
	}
	if n := h.box.Opens(provider.RoleSync); n < 2 {
		t.Errorf("%d sync connections: the panic did not become a reconnection", n)
	}
}

func TestStoppingTheEngineEndsIdleAndLogsEveryConnectionOut(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.GmailCaps()})
	h.box.Deliver("INBOX", mail("a", time.Now().Add(-time.Hour)))
	h.live()
	if err := h.engine.Interactive(context.Background(), h.account, func(ctx context.Context, s provider.Session) error {
		return s.Noop(ctx)
	}); err != nil {
		t.Fatal(err)
	}
	h.waitFor("the idle connection to park", 2*time.Second, func() bool { return h.box.OpenSessions(provider.RoleIdle) == 1 })
	if n := h.box.OpenSessions(); n != 3 {
		t.Fatalf("%d connections before stopping, want sync, idle and interactive", n)
	}

	began := time.Now()
	h.stop()
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("stopping took %s", took)
	}
	for _, role := range []provider.Role{provider.RoleSync, provider.RoleIdle, provider.RoleInteractive} {
		if opened, out := h.box.Opens(role), h.box.Logouts(role); opened != out {
			t.Errorf("%d %s connections opened, %d logged out", opened, role, out)
		}
	}
	if n := h.box.ClosedIdling(); n != 0 {
		t.Errorf("%d connections logged out in the middle of IDLE, without DONE", n)
	}
}

func TestTooManyConnectionsGivesBackTheInteractiveConnectionAndBacksOff(t *testing.T) {
	// The provider's cap counts every client the person runs. Refused for
	// that, the engine gives back the one connection it holds on demand and
	// waits before asking again.
	h := newHarness(t, setup{caps: providertest.GmailCaps()})
	h.box.Deliver("INBOX", mail("a", time.Now().Add(-time.Hour)))
	h.live()
	if err := h.engine.Interactive(context.Background(), h.account, func(ctx context.Context, s provider.Session) error {
		return s.Noop(ctx)
	}); err != nil {
		t.Fatal(err)
	}
	if n := h.box.OpenSessions(provider.RoleInteractive); n != 1 {
		t.Fatalf("%d interactive connections kept, want the one", n)
	}

	h.box.FailAlways(providertest.MethodOpen,
		fmt.Errorf("%w: NO [ALERT] Too many simultaneous connections", provider.ErrTooManyConnections))
	h.box.KillSessions(provider.RoleSync)
	h.waitFor("the interactive connection to be given back", 3*time.Second, func() bool {
		return h.box.OpenSessions(provider.RoleInteractive) == 0
	})
	h.waitFor("the failure on the account row", 3*time.Second, func() bool {
		return h.count(`SELECT count(*) FROM accounts WHERE id = ? AND last_error = 'too_many_connections'`, h.account) == 1
	})

	h.box.ClearFailures()
	h.box.Deliver("INBOX", mail("after", time.Now()))
	h.waitFor("the account to sync again", 5*time.Second, func() bool { return h.messages() == 2 })
}

func TestAUIDValidityChangeDuringTheInitialSyncKeepsWhatWasFetched(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.ExchangeCaps(), kind: provider.KindMicrosoft,
		opts: syncengine.Options{BatchSize: 50}})
	now := time.Now()
	for i := range 230 {
		h.box.Deliver("INBOX", mail(fmt.Sprintf("m%03d", i), now.Add(-time.Duration(230-i)*time.Hour)))
	}
	var (
		fetches atomic.Int32
		mu      sync.Mutex
		before  map[string]int64
	)
	h.box.OnCall(func(ctx context.Context, c providertest.Call) error {
		if c.Method != providertest.MethodFetchSummaries || c.Role != provider.RoleSync || fetches.Add(1) != 3 {
			return nil
		}
		// Two batches are in; the server renumbers the folder before the
		// third.
		ids, err := h.idsByMessageID(ctx)
		mu.Lock()
		before = ids
		mu.Unlock()
		if err != nil {
			t.Error(err)
		}
		h.box.ChangeUIDValidity("INBOX")
		return nil
	})
	h.start()
	h.waitLive()

	mu.Lock()
	defer mu.Unlock()
	if len(before) != 100 {
		t.Fatalf("%d rows indexed before the change, want two batches of 50", len(before))
	}
	after, err := h.idsByMessageID(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 230 || h.messages() != 230 {
		t.Fatalf("%d messages (%d live rows) after the resync, want 230 once each", len(after), h.messages())
	}
	for key, id := range before {
		if after[key] != id {
			t.Errorf("%s: row %d became %d", key, id, after[key])
		}
	}
	for _, typ := range []events.Type{events.TypeMessageNew, events.TypeMessageDeleted, events.TypeMessageMoved} {
		if evs := h.journaled(typ); len(evs) != 0 {
			t.Errorf("%d %s for mail the person already had", len(evs), typ)
		}
	}
	done := 0
	for _, ev := range h.journaled(events.TypeFolderChanged) {
		if payload[store.FolderChanged](t, ev).Change == store.FolderInitialDone {
			done++
		}
	}
	if done != 1 {
		t.Errorf("%d initial_done for the inbox, want one: the resync finished its initial sync", done)
	}
	if n := h.count(`SELECT count(*) FROM messages WHERE stale = 1`); n != 0 {
		t.Errorf("%d stale rows left", n)
	}
	if st, err := h.engine.Status(context.Background(), h.account); err != nil || st.InitialProgress != 100 {
		t.Errorf("status after the resync: %s (%v)", describe(st), err)
	}
}

func TestAnInterruptedResyncIsFinishedOnTheNextPass(t *testing.T) {
	h := newHarness(t, setup{caps: providertest.ExchangeCaps(), kind: provider.KindMicrosoft})
	for i := range 10 {
		h.box.Deliver("INBOX", mail(fmt.Sprintf("m%d", i), time.Now().Add(-time.Duration(10-i)*time.Hour)))
	}
	sub := h.live()
	h.settled()
	before, err := h.idsByMessageID(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var armed atomic.Bool
	h.box.OnCall(func(_ context.Context, c providertest.Call) error {
		if c.Method == providertest.MethodFetchSummaries && armed.CompareAndSwap(true, false) {
			return fmt.Errorf("%w: NO [UNAVAILABLE] try again later", provider.ErrTemporary)
		}
		return nil
	})
	armed.Store(true)
	h.box.ChangeUIDValidity("INBOX")
	if err := h.engine.Trigger(context.Background(), h.account); err != nil {
		t.Fatal(err)
	}
	h.waitFor("the resync to stop halfway", 3*time.Second, func() bool {
		return h.count(`SELECT count(*) FROM folders WHERE name = 'INBOX' AND sync_state = 'error'`) == 1 &&
			h.count(`SELECT count(*) FROM messages WHERE stale = 1`) == 10
	})
	if st, err := h.engine.Status(context.Background(), h.account); err != nil || st.State != "live" ||
		st.FoldersSynced != st.FoldersTotal {
		t.Errorf("status with a folder's pass failing: %s (%v)", describe(st), err)
	}

	if err := h.engine.Trigger(context.Background(), h.account); err != nil {
		t.Fatal(err)
	}
	for {
		ev := next(t, sub, events.TypeFolderChanged, 3*time.Second)
		if payload[store.FolderChanged](t, ev).Change == store.FolderResyncDone {
			break
		}
	}
	after, err := h.idsByMessageID(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for key, id := range before {
		if after[key] != id {
			t.Errorf("%s: row %d became %d", key, id, after[key])
		}
	}
	for _, typ := range []events.Type{events.TypeMessageNew, events.TypeMessageDeleted, events.TypeMessageMoved} {
		if evs := h.journaled(typ); len(evs) != 0 {
			t.Errorf("%d %s from a resync of known mail", len(evs), typ)
		}
	}
	if n := h.count(`SELECT count(*) FROM messages WHERE stale = 1`) +
		h.count(`SELECT count(*) FROM folders WHERE sync_state <> 'live'`); n != 0 {
		t.Errorf("%d stale rows or folders not live after the resync", n)
	}
}

func TestAResyncResumedAfterADroppedConnectionStillEndsAsAResync(t *testing.T) {
	// Exchange changes a live folder's UIDVALIDITY and the connection drops
	// halfway through the resync. The folder is left saying "resync", not
	// what it was; the resumed resync must still end with resync_done, not
	// tell every consumer a folder live for months has just finished its
	// initial sync.
	h := newHarness(t, setup{caps: providertest.ExchangeCaps(), kind: provider.KindMicrosoft})
	for i := range 10 {
		h.box.Deliver("INBOX", mail(fmt.Sprintf("m%d", i), time.Now().Add(-time.Duration(10-i)*time.Hour)))
	}
	sub := h.live()
	h.settled()

	var armed, dropped atomic.Bool
	release := make(chan struct{})
	h.box.OnCall(func(ctx context.Context, c providertest.Call) error {
		switch {
		case c.Method == providertest.MethodFetchSummaries && armed.CompareAndSwap(true, false):
			dropped.Store(true)
			return fmt.Errorf("%w: connection reset by peer", provider.ErrConnClosed)
		case c.Method == providertest.MethodOpen && dropped.Load():
			// Hold the reconnection until the test has looked.
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	armed.Store(true)
	h.box.ChangeUIDValidity("INBOX")
	if err := h.engine.Trigger(context.Background(), h.account); err != nil {
		t.Fatal(err)
	}
	h.waitFor("the connection to drop mid-resync", 3*time.Second, func() bool {
		return dropped.Load() && h.count(`SELECT count(*) FROM messages WHERE stale = 1`) == 10
	})
	if state := h.count(`SELECT count(*) FROM folders WHERE name = 'INBOX' AND sync_state = 'resync'`); state != 1 {
		t.Fatal("the inbox is not left mid-resync after the drop")
	}
	dropped.Store(false)
	close(release)

	for {
		ev := next(t, sub, events.TypeFolderChanged, 5*time.Second)
		switch change := payload[store.FolderChanged](t, ev).Change; change {
		case store.FolderResyncDone:
			for _, typ := range []events.Type{events.TypeMessageNew, events.TypeMessageDeleted, events.TypeMessageMoved} {
				if evs := h.journaled(typ); len(evs) != 0 {
					t.Errorf("%d %s from a resync of known mail", len(evs), typ)
				}
			}
			return
		case store.FolderInitialDone:
			t.Fatal("the resumed resync of a live folder ended with initial_done")
		}
	}
}

func TestAResyncKeepsAMessageReceivedLateOnTheDayBehindUTC(t *testing.T) {
	// After sync was turned on, the person put back a message from months
	// ago, received at 23:30 -0300 — 02:30 the next day in UTC. It arrived in
	// the folder, so it is indexed, and it is now the oldest row. A resync
	// that searched SINCE that UTC date would not find it on a server that
	// reads the date where the message was received, and would delete it,
	// and announce it deleted, while it is still in the mailbox. And what
	// the wider search finds from before the window stays out of the index.
	h := newHarness(t, setup{caps: providertest.ExchangeCaps(), kind: provider.KindMicrosoft})
	zone := time.FixedZone("-0300", -3*3600)
	h.box.Deliver("INBOX", mail("before the window", time.Now().AddDate(0, 0, -150)))
	for i, key := range []string{"m1", "m2", "m3"} {
		h.box.Deliver("INBOX", mail(key, time.Now().Add(-time.Duration(i+1)*time.Hour)))
	}
	sub := h.live()
	h.settled()
	day := time.Now().In(zone).AddDate(0, 0, -200)
	restored := time.Date(day.Year(), day.Month(), day.Day(), 23, 30, 0, 0, zone)
	h.box.Deliver("INBOX", mail("restored", restored))
	if err := h.engine.Trigger(context.Background(), h.account); err != nil {
		t.Fatal(err)
	}
	h.waitFor("the restored message", 5*time.Second, func() bool { return h.messages() == 4 })
	before, err := h.idsByMessageID(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	h.box.ChangeUIDValidity("INBOX")
	if err := h.engine.Trigger(context.Background(), h.account); err != nil {
		t.Fatal(err)
	}
	for payload[store.FolderChanged](t, next(t, sub, events.TypeFolderChanged, 5*time.Second)).Change != store.FolderResyncDone {
	}
	after, err := h.idsByMessageID(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 4 || after["restored@mail.example"] != before["restored@mail.example"] {
		t.Errorf("after the resync the index holds %v, want the same 4 rows as %v", after, before)
	}
	if n := len(h.journaled(events.TypeMessageDeleted)); n != 0 {
		t.Errorf("%d message.deleted for a message still in the mailbox", n)
	}
	if n := h.count(`SELECT count(*) FROM messages WHERE message_id = 'before the window@mail.example'`); n != 0 {
		t.Error("the resync stored a message from before the initial window")
	}
}

// idsByMessageID maps each indexed message's Message-ID to its row id. It
// does not fail the test, so a hook on the engine's goroutine can call it.
func (h *harness) idsByMessageID(ctx context.Context) (map[string]int64, error) {
	rows, err := h.db.Reader().QueryContext(ctx,
		`SELECT message_id, id FROM messages WHERE account_id = ? AND vanished_at = 0`, h.account)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var (
			key string
			id  int64
		)
		if err := rows.Scan(&key, &id); err != nil {
			return nil, err
		}
		out[key] = id
	}
	return out, rows.Err()
}

// failureRecords keeps what the engine logged about failed connections.
type failureRecords struct {
	mu      sync.Mutex
	records []slog.Record
}

func (f *failureRecords) Enabled(context.Context, slog.Level) bool { return true }

func (f *failureRecords) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "the server closed the connection; reconnecting" || r.Message == "account sync failed; retrying later" {
		f.mu.Lock()
		f.records = append(f.records, r.Clone())
		f.mu.Unlock()
	}
	return nil
}

func (f *failureRecords) WithAttrs([]slog.Attr) slog.Handler { return f }
func (f *failureRecords) WithGroup(string) slog.Handler      { return f }

func (f *failureRecords) levels() []slog.Level {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]slog.Level, 0, len(f.records))
	for _, r := range f.records {
		out = append(out, r.Level)
	}
	return out
}

func TestAServerClosingTheConnectionIsRoutineOnceAndAWarningWhenItKeepsDoingIt(t *testing.T) {
	// A host that closes every connection after three hours is not a
	// failure worth a warning each time: the first close in a row is INFO,
	// and the engine reconnects. Closed again right away, it is a WARN, as is
	// every other failure from the first — a connection that closed for any
	// reason but the server ending it included.
	unparseable := fmt.Errorf("%w: the server sent a response this client cannot parse", provider.ErrConnClosed)
	for _, tc := range []struct {
		name string
		err  error
		n    int
		want []slog.Level
	}{
		{"closed once", fmt.Errorf("%w: * BYE idle for too long", provider.ErrServerEnded), 1, []slog.Level{slog.LevelInfo}},
		{"closed twice", fmt.Errorf("%w: * BYE idle for too long", provider.ErrServerEnded), 2,
			[]slog.Level{slog.LevelInfo, slog.LevelWarn}},
		{"unparseable once", unparseable, 1, []slog.Level{slog.LevelWarn}},
		{"unanswered once", fmt.Errorf("%w: the server did not answer in time", provider.ErrConnClosed), 1,
			[]slog.Level{slog.LevelWarn}},
		{"refused once", fmt.Errorf("%w: NO [UNAVAILABLE] try later", provider.ErrTemporary), 1, []slog.Level{slog.LevelWarn}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged := &failureRecords{}
			h := newHarness(t, setup{caps: providertest.GmailCaps(), log: slog.New(logged)})
			h.box.FailTimes(providertest.MethodOpen, tc.n, tc.err)
			h.start()
			h.waitLive()
			got := logged.levels()
			if len(got) != len(tc.want) {
				t.Fatalf("logged %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("logged %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestASyncConnectionThatDropsBetweenPassesIsLoggedByWhyItDropped(t *testing.T) {
	// Seen closing while nothing was asked of it, the sync connection is
	// logged by why it closed: the server hanging up is routine, a response
	// go-imap could not parse is not — it would end the connection each
	// time it came.
	for _, tc := range []struct {
		name  string
		cause error
		want  slog.Level
	}{
		{"the server hung up", fmt.Errorf("%w: the server closed the connection", provider.ErrServerEnded), slog.LevelInfo},
		{"unparseable", fmt.Errorf("%w: the server sent a response this client cannot parse", provider.ErrConnClosed),
			slog.LevelWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged := &failureRecords{}
			h := newHarness(t, setup{caps: providertest.GmailCaps(), log: slog.New(logged)})
			h.start()
			h.waitLive()
			opens := h.box.CallCount(providertest.MethodOpen)
			if n := h.box.KillSessionsWith(tc.cause, provider.RoleSync); n != 1 {
				t.Fatalf("%d sync connections dropped, want 1", n)
			}
			h.waitFor("the drop to be logged", 5*time.Second, func() bool { return len(logged.levels()) > 0 })
			if got := logged.levels(); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("logged %v, want [%v]", got, tc.want)
			}
			h.waitFor("the engine to reconnect", 5*time.Second, func() bool {
				return h.box.CallCount(providertest.MethodOpen) > opens
			})
		})
	}
}
