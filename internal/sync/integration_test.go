//go:build integration

// The engine against a real Dovecot: the CONDSTORE tier, SPECIAL-USE roles, a
// real IDLE and folder names in UTF-8, none of which the in-process fakes can
// prove on the wire. The uidpoll tier runs against the same server with the
// account pinned to it, which is how Exchange is synced.
//
//	docker compose -f it/compose.yml up --wait
//	go test -tags integration ./internal/sync/
package sync_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	imapprovider "github.com/thehappieco/mailie/internal/provider/imap"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
	syncengine "github.com/thehappieco/mailie/internal/sync"
)

func dovecot(t *testing.T) *imapprovider.Mailbox {
	t.Helper()
	addr := os.Getenv("MAIL_IT_IMAP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:31143"
	}
	password := os.Getenv("MAIL_IT_IMAP_PASSWORD")
	if password == "" {
		password = "integration"
	}
	user := strings.ToLower(strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())) +
		"-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@example.com"
	mb, err := imapprovider.New(provider.Config{
		Kind: provider.KindIMAP, IMAPAddr: addr,
		Credentials: provider.Credentials{User: user, Password: password},
		SpoolDir:    t.TempDir(),
		// The compose file runs Dovecot without TLS; only tests can say so.
		AllowInsecureAuth: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return mb
}

func appendMail(t *testing.T, sess provider.Session, folder, key string, at time.Time) {
	t.Helper()
	raw := fmt.Sprintf("From: Sender <sender@example.com>\r\nTo: person@example.com\r\nSubject: Subject %s\r\n"+
		"Message-ID: <%s@example.com>\r\nDate: %s\r\n\r\nbody of %s\r\n", key, key, at.Format(time.RFC1123Z), key)
	if _, err := sess.Append(t.Context(), folder, strings.NewReader(raw), int64(len(raw)), nil, at); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

// otherClient is a second client of the same mailbox: the person's phone,
// changing things the engine has to notice.
func otherClient(t *testing.T, mb provider.Mailbox) provider.Session {
	t.Helper()
	sess, err := mb.Open(t.Context(), provider.RoleInteractive)
	if err != nil {
		t.Fatalf("Open (is `docker compose -f it/compose.yml up --wait` running?): %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// delimiter is the server's hierarchy separator, as LIST reports it.
func delimiter(t *testing.T, sess provider.Session) rune {
	t.Helper()
	folders, err := sess.ListFolders(t.Context(), false)
	if err != nil {
		t.Fatalf("ListFolders: %v", err)
	}
	for _, f := range folders {
		if f.Delim != 0 {
			return f.Delim
		}
	}
	t.Fatal("the server reported no hierarchy separator")
	return 0
}

// flagFetch is one FETCH FLAGS the engine sent.
type flagFetch struct {
	folder       string
	changedSince uint64
}

// recordingMailbox passes everything to the real adapter and notes the flag
// fetches the engine's sessions send, so a test can say what went on the wire.
type recordingMailbox struct {
	provider.Mailbox

	mu      sync.Mutex
	fetches []flagFetch
}

func (m *recordingMailbox) Open(ctx context.Context, role provider.Role) (provider.Session, error) {
	s, err := m.Mailbox.Open(ctx, role)
	if err != nil {
		return nil, err
	}
	return &recordingSession{Session: s, box: m}, nil
}

func (m *recordingMailbox) flagFetches() []flagFetch {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]flagFetch(nil), m.fetches...)
}

type recordingSession struct {
	provider.Session
	box *recordingMailbox
}

func (s *recordingSession) FetchFlags(ctx context.Context, set imap.UIDSet, changedSince uint64) ([]provider.FlagUpdate, error) {
	folder := ""
	if sel := s.Selected(); sel != nil {
		folder = sel.Name
	}
	s.box.mu.Lock()
	s.box.fetches = append(s.box.fetches, flagFetch{folder: folder, changedSince: changedSince})
	s.box.mu.Unlock()
	return s.Session.FetchFlags(ctx, set, changedSince)
}

// realEngine is the engine over a fresh database, syncing one mailbox.
type realEngine struct {
	t      *testing.T
	db     *store.Store
	bus    *events.Bus
	engine *syncengine.Manager
	sub    *events.Subscription
}

func runEngine(t *testing.T, mb provider.Mailbox, tier string) *realEngine {
	t.Helper()
	db := storetest.New(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Writer().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users(id, email, password_hash, role, password_changed_at, created_at, updated_at,
		sync_consent_at, sync_consent_version) VALUES ('usr_1', 'p@example.com', '$argon2id$', 'owner', 1, 1, 1, 1, 'test')`)
	exec(`INSERT INTO accounts(id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port, smtp_tls,
		login_user, save_sent_copy, state, state_changed_at, created_at, updated_at, owner_user_id, initial_days, sync_tier)
		VALUES ('acc_1', 'p@example.com', 'imap', 'password', '127.0.0.1', 31143, '127.0.0.1', 31025, 'starttls',
		'p@example.com', 1, 'active', 1, 1, 1, 'usr_1', 90, ?)`, tier)
	e := &realEngine{t: t, db: db, bus: events.NewBus(events.NewJournal(db))}
	e.engine = syncengine.New(syncengine.Deps{
		Store: db, Accounts: account.NewRepository(db, nil), Bus: e.bus,
		Mailboxes: func(context.Context, string) (provider.Mailbox, error) { return mb, nil },
	}, fast(syncengine.Options{ConfirmDiffDelay: 300 * time.Millisecond}))
	sub, err := e.bus.Subscribe(ctx, 0, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	e.sub = sub
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.engine.Run(runCtx)
	}()
	t.Cleanup(func() {
		stop()
		<-done
		sub.Close()
	})
	return e
}

// live waits for the initial sync to finish.
func (e *realEngine) live() {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		st, err := e.engine.Status(context.Background(), "acc_1")
		if err == nil && st.State == "live" {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("the initial sync did not finish: %+v %v", st, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *realEngine) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.Reader().QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *realEngine) uidOf(subject string) imap.UID {
	e.t.Helper()
	var uid imap.UID
	if err := e.db.Reader().QueryRowContext(context.Background(),
		`SELECT uid FROM messages WHERE subject = ?`, subject).Scan(&uid); err != nil {
		e.t.Fatalf("%s: %v", subject, err)
	}
	return uid
}

// wait reads the bus until an event of type typ arrives.
func (e *realEngine) wait(typ events.Type) events.Event {
	e.t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev := <-e.sub.Events():
			if ev.Type == typ {
				return ev
			}
		case <-timeout:
			e.t.Fatalf("no %s from the real server", typ)
			return events.Event{}
		}
	}
}

// waitRemoved reads the bus until row id is announced gone: message.deleted,
// or message.moved{to: null} when another folder holds the message.
func (e *realEngine) waitRemoved(id int64) {
	e.t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev := <-e.sub.Events():
			switch ev.Type {
			case events.TypeMessageDeleted:
				if payload[store.MessageDeleted](e.t, ev).MessageID == id {
					return
				}
			case events.TypeMessageMoved:
				if p := payload[store.MessageMoved](e.t, ev); p.MessageID == id && !p.NewCopy && p.To == nil {
					return
				}
			}
		case <-timeout:
			e.t.Fatalf("row %d was never announced gone", id)
		}
	}
}

func (e *realEngine) trigger() {
	e.t.Helper()
	if err := e.engine.Trigger(context.Background(), "acc_1"); err != nil {
		e.t.Fatal(err)
	}
}

// expungeTakesTwoDiffs moves a message out of the inbox from another client
// and checks the engine first hides the row, then deletes it only when a
// later diff confirms the absence.
func (e *realEngine) expungeTakesTwoDiffs(other provider.Session, subject string) {
	e.t.Helper()
	uid := e.uidOf(subject)
	var id int64
	if err := e.db.Reader().QueryRowContext(context.Background(),
		`SELECT id FROM messages WHERE subject = ?`, subject).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	if _, err := other.Select(e.t.Context(), "INBOX", false, 0); err != nil {
		e.t.Fatal(err)
	}
	if _, err := other.Move(e.t.Context(), imap.UIDSetNum(uid), "Trash"); err != nil {
		e.t.Fatalf("Move: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for e.count(`SELECT count(*) FROM messages WHERE id = ? AND vanished_at <> 0`, id) == 0 {
		if time.Now().After(deadline) {
			e.t.Fatal("the moved message was never tombstoned")
		}
		time.Sleep(5 * time.Millisecond)
	}
	tombstoned := time.Now()
	// Gone from the inbox is a deletion, or a copy removed when the pass
	// over Trash has already indexed where it went.
	e.waitRemoved(id)
	if took := time.Since(tombstoned); took < 250*time.Millisecond {
		e.t.Errorf("deleted %s after the first absence; the confirming diff is 300 ms later", took)
	}
	if n := e.count(`SELECT count(*) FROM messages WHERE id = ?`, id); n != 0 {
		e.t.Error("the moved message is still indexed in the inbox")
	}
}

func TestTheEngineSyncsARealServerOnTheCondStoreTier(t *testing.T) {
	mb := dovecot(t)
	other := otherClient(t, mb)
	now := time.Now()
	// Below the window, and below its lowest UID: never indexed. (Old mail
	// with a UID above the window's lowest is a gap the diff finds, and
	// leaves out too: it is outside the window.)
	appendMail(t, other, "INBOX", "ancient", now.AddDate(0, 0, -200))
	for i := range 3 {
		appendMail(t, other, "INBOX", fmt.Sprintf("in%d", i), now.Add(-time.Duration(3-i)*time.Hour))
	}
	appendMail(t, other, "Sent", "sent0", now.Add(-time.Hour))
	// A person's own folder, named in their language: selected, fetched and
	// announced by its UTF-8 name, under an accented parent.
	accented := "Ações" + string(delimiter(t, other)) + "Relatórios 2026"
	if err := other.Create(t.Context(), "Ações"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := other.Create(t.Context(), accented); err != nil {
		t.Fatalf("Create: %v", err)
	}
	appendMail(t, other, accented, "relatorio0", now.Add(-time.Hour))

	e := runEngine(t, mb, "auto")
	e.live()
	if st, _ := e.engine.Status(context.Background(), "acc_1"); st.Tier != syncengine.TierCondStore {
		t.Fatalf("tier %q against Dovecot, want condstore", st.Tier)
	}
	if n := e.count(`SELECT count(*) FROM messages`); n != 5 {
		t.Fatalf("%d messages indexed, want the 3 in the window, the one in Sent and the one in %q", n, accented)
	}
	if n := e.count(`SELECT count(*) FROM folders WHERE role = 'sent' AND name = 'Sent' AND role_source = 'special-use'`); n != 1 {
		t.Error("Sent's role did not come from SPECIAL-USE")
	}
	if n := e.count(`SELECT count(*) FROM folders WHERE name = 'INBOX' AND highest_modseq > 0`); n != 1 {
		t.Error("the inbox has no recorded modification sequence")
	}
	if n := e.count(`SELECT count(*) FROM folders f JOIN messages m ON m.folder_id = f.id
		WHERE f.name = ? AND f.display_name = 'Relatórios 2026' AND f.sync_state = 'live'`, accented); n != 1 {
		t.Errorf("the accented folder is not indexed under its UTF-8 name")
	}

	// New mail, through a real IDLE, within seconds.
	sent := time.Now()
	appendMail(t, other, "INBOX", "fresh", time.Now())
	ev := e.wait(events.TypeMessageNew)
	if took := time.Since(sent); took > 5*time.Second {
		t.Errorf("new mail took %s through IDLE", took)
	}
	if got := payload[store.MessageNew](t, ev); got.Subject != "Subject fresh" || got.FolderRole != "inbox" {
		t.Errorf("message.new = %s", ev.Payload)
	}

	// New mail in the accented folder, which nothing watches: a requested
	// pass finds it.
	appendMail(t, other, accented, "relatorio1", time.Now())
	e.trigger()
	ev = e.wait(events.TypeMessageNew)
	if got := payload[store.MessageNew](t, ev); got.Subject != "Subject relatorio1" || got.FolderRole != "" {
		t.Errorf("message.new in the accented folder = %s", ev.Payload)
	}

	// A deletion, confirmed by a second diff.
	e.expungeTakesTwoDiffs(other, "Subject in0")
}

func TestCondStoreFlagChangesAreFetchedWithChangedSince(t *testing.T) {
	mb := &recordingMailbox{Mailbox: dovecot(t)}
	other := otherClient(t, mb.Mailbox)
	for i := range 3 {
		appendMail(t, other, "INBOX", fmt.Sprintf("in%d", i), time.Now().Add(-time.Hour))
	}
	appendMail(t, other, "Sent", "sent0", time.Now().Add(-time.Hour))
	e := runEngine(t, mb, "auto")
	e.live()

	// In the inbox IDLE says something changed; in Sent nothing does, and a
	// requested pass asks.
	for _, c := range []struct{ folder, subject string }{{"INBOX", "Subject in1"}, {"Sent", "Subject sent0"}} {
		before := len(mb.flagFetches())
		uid := e.uidOf(c.subject)
		if _, err := other.Select(t.Context(), c.folder, false, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := other.StoreFlags(t.Context(), imap.UIDSetNum(uid), provider.FlagAdd, []imap.Flag{imap.FlagFlagged}, 0); err != nil {
			t.Fatalf("StoreFlags: %v", err)
		}
		if c.folder != "INBOX" {
			e.trigger()
		}
		got := payload[store.MessageFlags](t, e.wait(events.TypeMessageFlags))
		if !got.Flagged {
			t.Errorf("%s: message.flags = %+v", c.folder, got)
		}
		asked := false
		for _, f := range mb.flagFetches()[before:] {
			if f.folder == c.folder && f.changedSince > 0 {
				asked = true
			}
		}
		if !asked {
			t.Errorf("%s: the change was not fetched with CHANGEDSINCE: %+v", c.folder, mb.flagFetches()[before:])
		}
	}
	if n := e.count(`SELECT count(*) FROM messages WHERE flagged = 1`); n != 2 {
		t.Errorf("%d flagged rows, want 2", n)
	}
}

func TestTheEngineSyncsARealServerOnTheUIDPollTier(t *testing.T) {
	// Exchange has no CONDSTORE; the account is pinned to the tier it gets,
	// and the same server has to be synced without a modification sequence.
	mb := &recordingMailbox{Mailbox: dovecot(t)}
	other := otherClient(t, mb.Mailbox)
	for i := range 3 {
		appendMail(t, other, "INBOX", fmt.Sprintf("in%d", i), time.Now().Add(-time.Hour))
	}
	e := runEngine(t, mb, syncengine.TierUIDPoll)
	e.live()
	if n := e.count(`SELECT count(*) FROM accounts WHERE sync_tier_resolved = 'uidpoll'`); n != 1 {
		t.Fatal("the pinned tier was not the one resolved")
	}

	appendMail(t, other, "INBOX", "fresh", time.Now())
	if got := payload[store.MessageNew](t, e.wait(events.TypeMessageNew)); got.Subject != "Subject fresh" {
		t.Errorf("message.new = %+v", got)
	}

	uid := e.uidOf("Subject in2")
	if _, err := other.Select(t.Context(), "INBOX", false, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := other.StoreFlags(t.Context(), imap.UIDSetNum(uid), provider.FlagAdd, []imap.Flag{imap.FlagSeen}, 0); err != nil {
		t.Fatalf("StoreFlags: %v", err)
	}
	if got := payload[store.MessageFlags](t, e.wait(events.TypeMessageFlags)); !got.Seen {
		t.Errorf("message.flags = %+v", got)
	}
	for _, f := range mb.flagFetches() {
		if f.changedSince != 0 {
			t.Errorf("the uidpoll tier sent CHANGEDSINCE %d in %s", f.changedSince, f.folder)
		}
	}

	e.expungeTakesTwoDiffs(other, "Subject in0")
	if n := e.count(`SELECT count(*) FROM messages WHERE vanished_at = 0`); n != 3 {
		t.Errorf("%d live rows, want in1, in2 and fresh", n)
	}
}
