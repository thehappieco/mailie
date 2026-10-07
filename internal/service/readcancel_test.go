package service_test

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/provider"
	imapprovider "github.com/thehappieco/mailie/internal/provider/imap"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	syncengine "github.com/thehappieco/mailie/internal/sync"
)

// lateEngine lets the fixture's service be built before the engine that
// needs the fixture's database: it forwards to the engine once it is set.
type lateEngine struct{ m *syncengine.Manager }

func (e *lateEngine) Trigger(ctx context.Context, id string) error { return e.m.Trigger(ctx, id) }
func (e *lateEngine) Status(ctx context.Context, id string) (service.SyncStatus, error) {
	return e.m.Status(ctx, id)
}
func (e *lateEngine) Reconcile(id string) { e.m.Reconcile(id) }
func (e *lateEngine) Interactive(ctx context.Context, id string, fn func(context.Context, provider.Session) error) error {
	return e.m.Interactive(ctx, id, fn)
}

// countingMailbox counts the connections the engine opens, by role, and the
// most that were open at once.
type countingMailbox struct {
	provider.Mailbox

	mu    sync.Mutex
	opens map[provider.Role]int
	open  int
	peak  int
}

func (m *countingMailbox) Open(ctx context.Context, role provider.Role) (provider.Session, error) {
	s, err := m.Mailbox.Open(ctx, role)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.opens[role]++
	m.open++
	m.peak = max(m.peak, m.open)
	m.mu.Unlock()
	return &countedSession{Session: s, box: m}, nil
}

func (m *countingMailbox) count(role provider.Role) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opens[role]
}

type countedSession struct {
	provider.Session
	box  *countingMailbox
	once sync.Once
}

func (s *countedSession) Close() error {
	s.once.Do(func() {
		s.box.mu.Lock()
		s.box.open--
		s.box.mu.Unlock()
	})
	return s.Session.Close()
}

// cancelOnFetch watches what a mailbox's connections send and, once armed,
// cancels a context the moment a body section fetch goes out: a caller that
// gives up while the FETCH is on the wire, which is what the console does to
// the read of one message whenever a person opens the next.
type cancelOnFetch struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	fired  bool
}

func (c *cancelOnFetch) arm(cancel context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancel, c.fired = cancel, false
}

func (c *cancelOnFetch) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// BODY.PEEK[1] is a body part; the engine's own fetches ask for
	// BODY.PEEK[HEADER.FIELDS ...].
	if c.cancel != nil && bytes.Contains(p, []byte("BODY.PEEK[1")) {
		c.cancel()
		c.cancel, c.fired = nil, true
	}
	return len(p), nil
}

func (c *cancelOnFetch) didFire() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fired
}

func TestAReadCancelledMidFetchLeavesTheEnginesConnectionForTheNextRead(t *testing.T) {
	// A person moving down the list cancels each read as they open the next
	// message. The FETCH in flight still finishes on the wire, and the
	// account's interactive connection must survive it: dropping it would
	// mean a new login for every message opened, which Gmail counts against
	// the account.
	srv := providertest.NewIMAPServer(t, providertest.IMAPOptions{Password: "hunter2"})
	srv.Append(t, "INBOX", "From: Bea <bea@example.org>\r\nTo: Ana <ana@example.org>\r\n"+
		"Subject: Lunch\r\nMessage-ID: <lunch-1@example.org>\r\nMIME-Version: 1.0\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\n\r\nNoon at the usual place?\r\n", nil, time.Now().Add(-time.Hour))
	trip := &cancelOnFetch{}
	mailbox, err := imapprovider.New(provider.Config{
		Kind: provider.KindIMAP, IMAPAddr: srv.Addr,
		Credentials: provider.Credentials{User: srv.User, Password: "hunter2"},
		SpoolDir:    t.TempDir(), AllowInsecureAuth: true, DebugWriter: trip,
	})
	if err != nil {
		t.Fatal(err)
	}

	late := &lateEngine{}
	f := newFixtureWith(t, fixtureOptions{sync: late})
	counted := &countingMailbox{Mailbox: mailbox, opens: map[provider.Role]int{}}
	late.m = syncengine.New(syncengine.Deps{
		Store: f.db, Accounts: f.repo, Bus: f.bus,
		Mailboxes: func(context.Context, string) (provider.Mailbox, error) { return counted, nil },
	}, syncengine.Options{})
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		late.m.Run(runCtx)
	}()
	defer func() {
		stop()
		<-done
	}()

	const id = "acc_00000000000000c1"
	host, port := splitHostPort(t, srv.Addr)
	if _, err := f.repo.Create(t.Context(), account.Account{
		ID: id, Email: srv.User, Provider: provider.KindIMAP, AuthKind: "password",
		IMAPHost: host, IMAPPort: port, SMTPHost: host, SMTPPort: port, SMTPTLS: "implicit", LoginUser: srv.User,
		State: account.StateActive,
	}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetMailboxSync(t.Context(), admin(), id, switchSync(true)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		st, err := late.m.Status(t.Context(), id)
		if err == nil && st.State == "live" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the initial sync did not finish: %+v %v", st, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	page, err := f.svc.SearchMessages(t.Context(), reader(), service.SearchRequest{AccountID: id})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("search = %+v, %v", page, err)
	}
	msgID := page.Messages[0].ID

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	trip.arm(cancel)
	if _, err := f.svc.GetMessage(ctx, reader(), service.GetMessageRequest{ID: msgID}); err == nil {
		t.Fatal("a read cancelled mid-fetch reported success")
	}
	if !trip.didFire() {
		t.Fatal("the read was not cancelled while its FETCH was on the wire")
	}

	msg, err := f.svc.GetMessage(t.Context(), reader(), service.GetMessageRequest{ID: msgID})
	if err != nil {
		t.Fatalf("the next read: %v", err)
	}
	if msg.Body.Text == nil || !strings.Contains(*msg.Body.Text, "Noon at the usual place?") {
		t.Errorf("text = %v", msg.Body.Text)
	}
	if n := counted.count(provider.RoleInteractive); n != 1 {
		t.Errorf("%d interactive connections were opened for a cancelled read and the next one, want 1", n)
	}
}
