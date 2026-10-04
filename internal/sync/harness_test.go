package sync_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
	syncengine "github.com/thehappieco/mailie/internal/sync"
)

// harness is one person, one mailbox on a fake server, and the engine.
type harness struct {
	t       *testing.T
	db      *store.Store
	bus     *events.Bus
	box     *providertest.FakeMailbox
	engine  *syncengine.Manager
	account string
	user    string

	mu      sync.Mutex
	stopped bool
	stop    func()
}

type setup struct {
	kind        provider.Kind
	caps        provider.Caps
	sharedFlags bool
	// consent false leaves the person without consent to sync.
	noConsent bool
	// unowned makes the account the instance's, switched on by the
	// operator, instead of the person's.
	unowned     bool
	initialDays int
	opts        syncengine.Options
	// log receives the engine's log; nil discards it.
	log *slog.Logger
}

// fast is a schedule where nothing happens by timer within a test unless the
// test asks for it: whatever the engine does, it does because it was
// signalled, triggered, or had work left.
func fast(o syncengine.Options) syncengine.Options {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&o.EligibilityInterval, time.Hour)
	def(&o.RetentionInterval, time.Hour)
	def(&o.DiscoveryInterval, time.Hour)
	def(&o.InboxInterval, time.Hour)
	def(&o.InboxPollInterval, time.Hour)
	def(&o.SentInterval, time.Hour)
	def(&o.TrashInterval, time.Hour)
	def(&o.JunkInterval, time.Hour)
	def(&o.OtherInterval, time.Hour)
	def(&o.FolderRetry, time.Hour)
	def(&o.InboxDiffInterval, time.Hour)
	def(&o.OtherDiffInterval, time.Hour)
	def(&o.ConfirmDiffDelay, time.Hour)
	def(&o.InboxFlagSweep, time.Hour)
	def(&o.OtherFlagSweep, time.Hour)
	def(&o.IdleRenew, time.Hour)
	def(&o.IdleDebounce, 20*time.Millisecond)
	def(&o.IdleStopTimeout, 2*time.Second)
	def(&o.BackoffBase, 10*time.Millisecond)
	def(&o.BackoffMax, 50*time.Millisecond)
	def(&o.LongBackoff, 100*time.Millisecond)
	def(&o.TooManyBackoff, 50*time.Millisecond)
	def(&o.RateLimitMin, 50*time.Millisecond)
	def(&o.OpenTimeout, 5*time.Second)
	def(&o.InteractiveIdle, time.Hour)
	def(&o.ProgressEvery, 10*time.Millisecond)
	def(&o.ShutdownTimeout, 10*time.Second)
	return o
}

func newHarness(t *testing.T, s setup) *harness {
	t.Helper()
	if s.kind == "" {
		s.kind = provider.KindGmail
	}
	if s.initialDays == 0 {
		s.initialDays = 90
	}
	db := storetest.New(t)
	h := &harness{t: t, db: db, account: "acc_1", user: "usr_1"}
	consentAt := 1
	if s.noConsent {
		consentAt = 0
	}
	h.exec(`INSERT INTO users(id, email, password_hash, role, password_changed_at, created_at, updated_at,
		sync_consent_at, sync_consent_version) VALUES (?, 'person@example.com', '$argon2id$', 'owner', 1, 1, 1, ?, 'test')`,
		h.user, consentAt)
	var owner any = h.user
	enabledAt := 0
	if s.unowned {
		owner, enabledAt = nil, 1
	}
	h.exec(`INSERT INTO accounts(id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port,
		smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at, owner_user_id, initial_days,
		sync_enabled_at)
		VALUES (?, 'person@mail.example', ?, 'oauth2', 'imap.mail.example', 993, 'smtp.mail.example', 465, 'implicit',
		'person@mail.example', 0, 'active', 1, 1, 1, ?, ?, ?)`, h.account, string(s.kind), owner, s.initialDays, enabledAt)
	h.box = providertest.NewFakeMailbox(providertest.FakeOptions{Kind: s.kind, Caps: s.caps, SharedFlags: s.sharedFlags})
	h.bus = events.NewBus(events.NewJournal(db))
	h.engine = syncengine.New(syncengine.Deps{
		Store:    db,
		Accounts: account.NewRepository(db, nil),
		Mailboxes: func(_ context.Context, id string) (provider.Mailbox, error) {
			if id != h.account {
				return nil, account.ErrNotFound
			}
			return h.box, nil
		},
		Bus: h.bus,
		Log: s.log,
	}, fast(s.opts))
	return h
}

// start runs the engine until the test ends.
func (h *harness) start() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.engine.Run(ctx)
	}()
	h.stop = func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.stopped {
			return
		}
		h.stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			h.t.Error("the engine did not stop")
		}
	}
	h.t.Cleanup(func() {
		h.stop()
		if v := h.box.Violations(); len(v) > 0 {
			h.t.Errorf("the engine misused its connections:\n  %s", strings.Join(v, "\n  "))
		}
		if n := h.box.OpenSessions(); n != 0 {
			h.t.Errorf("%d connections still open after the engine stopped", n)
		}
	})
}

func (h *harness) exec(query string, args ...any) {
	h.t.Helper()
	err := h.db.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), query, args...)
		return err
	})
	if err != nil {
		h.t.Fatalf("exec %q: %v", query, err)
	}
}

func (h *harness) count(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.db.Reader().QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		h.t.Fatalf("query %q: %v", query, err)
	}
	return n
}

// messages counts the account's indexed rows that are live.
func (h *harness) messages() int {
	return h.count(`SELECT count(*) FROM messages WHERE account_id = ? AND vanished_at = 0`, h.account)
}

func (h *harness) waitFor(what string, within time.Duration, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitLive waits until every synced folder has finished its initial sync.
func (h *harness) waitLive() {
	h.t.Helper()
	h.waitFor("the initial sync to finish", 10*time.Second, func() bool {
		st, err := h.engine.Status(context.Background(), h.account)
		return err == nil && st.State == "live"
	})
}

// waitProgressDone waits for the sync.progress that closes the initial sync.
// It is journaled just after the last folder goes live, in a transaction of
// its own, so a live status can be seen a moment before it.
func (h *harness) waitProgressDone() []events.Event {
	h.t.Helper()
	var evs []events.Event
	h.waitFor("sync.progress to reach 100", 5*time.Second, func() bool {
		evs = h.journaled(events.TypeSyncProgress)
		return len(evs) > 0 && payload[store.SyncProgress](h.t, evs[len(evs)-1]).Progress == 100
	})
	return evs
}

// subscribe attaches to the bus from now on.
func (h *harness) subscribe() *events.Subscription {
	h.t.Helper()
	sub, err := h.bus.Subscribe(context.Background(), 0, events.Filter{})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(sub.Close)
	return sub
}

// next reads events until one of type t arrives.
func next(t *testing.T, sub *events.Subscription, typ events.Type, within time.Duration) events.Event {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case ev := <-sub.Events():
			if ev.Type == typ {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %s within %s", typ, within)
			return events.Event{}
		}
	}
}

// journaled lists the account's journaled events of one type.
func (h *harness) journaled(typ events.Type) []events.Event {
	h.t.Helper()
	rows, err := h.db.Reader().QueryContext(context.Background(),
		`SELECT seq, type, account_id, payload_json FROM events WHERE account_id = ? AND type = ? ORDER BY seq`,
		h.account, string(typ))
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []events.Event
	for rows.Next() {
		var (
			ev      events.Event
			typ     string
			payload string
		)
		if err := rows.Scan(&ev.Seq, &typ, &ev.AccountID, &payload); err != nil {
			h.t.Fatal(err)
		}
		ev.Type, ev.Payload = events.Type(typ), json.RawMessage(payload)
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatal(err)
	}
	return out
}

func payload[T any](t *testing.T, ev events.Event) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(ev.Payload, &out); err != nil {
		t.Fatalf("decode %s: %v", ev.Type, err)
	}
	return out
}

func mail(key string, at time.Time) providertest.FakeMessage {
	return providertest.FakeMessage{
		MessageID: key + "@mail.example", Subject: "Subject " + key, From: "Sender <sender-" + key + "@mail.example>",
		To: []string{"person@mail.example"}, InternalDate: at,
	}
}

// uidsOf reads a UID set as IMAP writes it ("1:3,7"). A range ending in "*"
// yields its start only; the tests use it for sets of known UIDs.
func uidsOf(t *testing.T, set string) []imap.UID {
	t.Helper()
	var out []imap.UID
	for _, part := range strings.Split(set, ",") {
		lo, hi, isRange := strings.Cut(part, ":")
		a, err := strconv.ParseUint(lo, 10, 32)
		if err != nil {
			t.Fatalf("parse %q: %v", set, err)
		}
		b := a
		if isRange && hi != "*" {
			if b, err = strconv.ParseUint(hi, 10, 32); err != nil {
				t.Fatalf("parse %q: %v", set, err)
			}
		}
		if a > b {
			a, b = b, a
		}
		for u := a; u <= b; u++ {
			out = append(out, imap.UID(u))
		}
	}
	return out
}

func (h *harness) folderID(name string) int64 {
	h.t.Helper()
	var id int64
	err := h.db.Reader().QueryRowContext(context.Background(),
		`SELECT id FROM folders WHERE account_id = ? AND name = ?`, h.account, name).Scan(&id)
	if err != nil {
		h.t.Fatalf("folder %q: %v", name, err)
	}
	return id
}

func describe(v any) string { return fmt.Sprintf("%+v", v) }
