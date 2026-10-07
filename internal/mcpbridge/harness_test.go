package mcpbridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/mcp"
	"github.com/thehappieco/mailie/internal/mcpbridge"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

// The bridge is tested against a real Mailie server: the service over a
// temporary database and fake mailboxes, behind the very HTTP handler the
// daemon mounts at /mcp. Whatever the bridge changed on the way would show
// as a difference from the same server asked directly.

// mailie is a Mailie server on a loopback address.
type mailie struct {
	t      *testing.T
	store  *store.Store
	bus    *events.Bus
	engine *lendingEngine
	tools  *mcp.Server
	opts   mcp.HTTPOptions
	srv    *httptest.Server
	logs   *syncBuffer

	mu       sync.Mutex
	handler  http.Handler
	requests []string // "METHOD key-prefix" of every request to /mcp
	// versions are the MCP-Protocol-Version of every request in a session.
	versions []string
}

func newMailie(t *testing.T, o mcp.HTTPOptions) *mailie {
	t.Helper()
	db := storetest.New(t)
	bus := events.NewBus(events.NewJournal(db))
	logs := &syncBuffer{}
	logger := obs.NewLoggerTo(logs, "debug", "json")
	keyring, err := secrets.NewKeyring(1, map[uint8][]byte{1: bytes.Repeat([]byte{0xC3}, secrets.KeyLen)})
	if err != nil {
		t.Fatal(err)
	}
	registry := account.NewRegistry(t.Context(), account.NewRepository(db, keyring), account.RegistryOptions{
		SpoolDir: t.TempDir(), AllowPrivate: true,
	})
	t.Cleanup(func() { _ = registry.Close() })
	engine := &lendingEngine{boxes: map[string]*providertest.FakeMailbox{}}
	svc := service.New(service.Deps{
		Accounts: registry, Keys: auth.NewKeys(db), Users: auth.NewUsers(db), Store: db, Bus: bus, Sync: engine, Log: logger,
	})
	m := &mailie{t: t, store: db, bus: bus, engine: engine, tools: mcp.New(svc, logger, "test"), opts: o, logs: logs}
	m.handler = m.tools.HTTPHandler(o)
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		prefix, _, _ := strings.Cut(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
		m.requests = append(m.requests, r.Method+" "+prefix)
		if r.Header.Get("Mcp-Session-Id") != "" {
			m.versions = append(m.versions, r.Header.Get("Mcp-Protocol-Version"))
		}
		h := m.handler
		m.mu.Unlock()
		h.ServeHTTP(w, r)
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		m.srv.CloseClientConnections()
		m.srv.Close()
	})
	return m
}

// endpoint is the server's MCP endpoint.
func (m *mailie) endpoint() string { return m.srv.URL + "/mcp" }

// restart forgets every session, as a daemon that restarts does: a new
// handler, and the connections to the old one dropped.
func (m *mailie) restart() {
	m.mu.Lock()
	m.handler = m.tools.HTTPHandler(m.opts)
	m.mu.Unlock()
	m.srv.CloseClientConnections()
}

// seen is every request /mcp received, as "METHOD key-prefix".
func (m *mailie) seen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.requests...)
}

// sessionVersions are the protocol versions requests in a session named.
func (m *mailie) sessionVersions() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.versions...)
}

// count is how many requests of method /mcp received.
func (m *mailie) count(method string) int {
	n := 0
	for _, r := range m.seen() {
		if strings.HasPrefix(r, method+" ") {
			n++
		}
	}
	return n
}

// waitForRequests waits until /mcp has received n requests of method, and
// a moment more for the last to reach its tool.
func (m *mailie) waitForRequests(method string, n int) {
	m.t.Helper()
	waitFor(m.t, fmt.Sprintf("%d %s requests", n, method), func() bool { return m.count(method) >= n })
	time.Sleep(300 * time.Millisecond)
}

// key is an instance key, which reaches the mailboxes nobody owns.
func (m *mailie) key(scope auth.Scope) string {
	m.t.Helper()
	return authtest.NewKey(m.t, m.store, scope, "")
}

// mailbox registers an active mailbox nobody owns, switched on by the
// operator, on a fake mail server.
func (m *mailie) mailbox(id, email string) *providertest.FakeMailbox {
	m.t.Helper()
	if _, err := account.NewRepository(m.store, nil).Create(m.t.Context(), account.Account{
		ID: id, Email: email, Provider: provider.KindIMAP, AuthKind: "password",
		IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example", SMTPPort: 465,
		SMTPTLS: "implicit", LoginUser: email, State: account.StateActive,
	}, ""); err != nil {
		m.t.Fatal(err)
	}
	if _, err := m.store.SetMailboxSync(m.t.Context(), id, true, "cli", "", nil); err != nil {
		m.t.Fatal(err)
	}
	box := providertest.NewFakeMailbox(providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.CreateFolder("Trash", goimap.MailboxAttrTrash)
	m.engine.mu.Lock()
	m.engine.boxes[id] = box
	m.engine.mu.Unlock()
	return box
}

// deliver puts a message in the inbox, indexes the mailbox as sync would,
// and returns the message's id.
func (m *mailie) deliver(accountID string, box *providertest.FakeMailbox, subject, from, body string) int64 {
	m.t.Helper()
	uid := box.Deliver("INBOX", letter(subject, from, body))
	storetest.IndexMailbox(m.t, m.store, accountID, box)
	var id int64
	if err := m.store.Reader().QueryRowContext(m.t.Context(),
		`SELECT m.id FROM messages m JOIN folders f ON f.id = m.folder_id
		  WHERE m.account_id = ? AND f.name = 'INBOX' AND m.uid = ?`, accountID, uint32(uid)).Scan(&id); err != nil {
		m.t.Fatal(err)
	}
	return id
}

// announce journals message.new for an indexed message and publishes it, as
// the sync engine does once the message is committed.
func (m *mailie) announce(accountID string, id int64, subject string) {
	m.t.Helper()
	var folderID int64
	if err := m.store.Reader().QueryRowContext(m.t.Context(),
		`SELECT folder_id FROM messages WHERE id = ?`, id).Scan(&folderID); err != nil {
		m.t.Fatal(err)
	}
	ev, err := events.New(events.TypeMessageNew, accountID, time.Now(), store.MessageNew{
		AccountID: accountID, MessageID: id, FolderID: folderID, FolderRole: "inbox", Subject: subject,
		InternalDate: time.Now().Unix(), FirstCopy: true, FirstInboxCopy: true,
	})
	if err != nil {
		m.t.Fatal(err)
	}
	var written []events.Event
	if err := m.store.Write(m.t.Context(), func(tx *sql.Tx) error {
		var err error
		written, err = m.bus.Journal().Append(m.t.Context(), tx, []events.Event{ev})
		return err
	}); err != nil {
		m.t.Fatal(err)
	}
	m.bus.Publish(written...)
}

// waitForLog waits until a line of the server's log holds every one of want.
func (m *mailie) waitForLog(want ...string) {
	m.t.Helper()
	waitFor(m.t, "a server log line with "+strings.Join(want, " "), func() bool {
		for line := range strings.Lines(m.logs.String()) {
			all := true
			for _, w := range want {
				all = all && strings.Contains(line, w)
			}
			if all {
				return true
			}
		}
		return false
	})
}

// bridged is a client session through a bridge, and the bridge's end.
type bridged struct {
	cs   *sdk.ClientSession
	done chan error
	logs *syncBuffer
}

// result is what the bridge ended with, waiting for it.
func (b *bridged) result(t *testing.T) error {
	t.Helper()
	select {
	case err := <-b.done:
		b.done <- err // for whoever asks next
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("the bridge did not end within 15 s")
		return nil
	}
}

// bridge runs a bridge to endpoint with key, and connects a client to it in
// memory, as a client that launched `mailserver mcp connect` would be. The
// client's session is closed when the test ends, which ends the bridge.
func bridge(t *testing.T, endpoint, key string, opts *sdk.ClientOptions) (*bridged, error) {
	t.Helper()
	return bridgeWith(t, endpoint, key, sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, opts))
}

// bridgeWith is bridge for a client the test made.
func bridgeWith(t *testing.T, endpoint, key string, c *sdk.Client) (*bridged, error) {
	t.Helper()
	client, local := sdk.NewInMemoryTransports()
	b := &bridged{done: make(chan error, 1), logs: &syncBuffer{}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		b.done <- mcpbridge.Run(ctx, local, mcpbridge.Options{
			Endpoint: endpoint, Key: key, Log: obs.NewLoggerTo(b.logs, "debug", "text"), UserAgent: "bridge-test",
		})
	}()
	t.Cleanup(func() {
		if b.cs != nil {
			_ = b.cs.Close()
		}
		select {
		case <-b.done:
		case <-time.After(10 * time.Second):
			t.Error("the bridge did not end within 10 s of its client closing")
		}
		cancel()
	})
	cs, err := c.Connect(t.Context(), client, nil)
	if err != nil {
		return b, err
	}
	b.cs = cs
	return b, nil
}

// direct connects a client to the server's endpoint itself, over HTTP with
// the key in a header, as Claude Code or Cursor do.
func direct(t *testing.T, endpoint, key string, opts *sdk.ClientOptions) *sdk.ClientSession {
	t.Helper()
	transport := &sdk.StreamableClientTransport{
		Endpoint: endpoint, MaxRetries: -1,
		HTTPClient: &http.Client{Transport: bearer{key, http.DefaultTransport}},
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, opts).Connect(t.Context(), transport, nil)
	if err != nil {
		t.Fatalf("direct connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// bearer adds the key to every request.
type bearer struct {
	key  string
	next http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return b.next.RoundTrip(r)
}

// halfOpen is a TCP proxy in front of a server whose connections can be cut
// on the client's side only, as a network change, a NAT that forgets a
// connection or a laptop that sleeps cuts them: the server, still connected
// to the proxy, notices nothing, and goes on holding a stream for a client
// that is gone until the proxy lets go of its side too (as nginx does when
// its read timeout passes).
type halfOpen struct {
	ln net.Listener

	mu sync.Mutex
	// live are the connections not cut; cutOff the ones cut on the client's
	// side only.
	live, cutOff []proxied
}

// proxied is one connection through the proxy: the client's side and the
// server's.
type proxied struct{ client, server net.Conn }

func newHalfOpen(t *testing.T, m *mailie) *halfOpen {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	upstream := m.srv.Listener.Addr().String()
	p := &halfOpen{ln: ln}
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", upstream)
			if err != nil {
				_ = client.Close()
				continue
			}
			p.mu.Lock()
			p.live = append(p.live, proxied{client, server})
			p.mu.Unlock()
			// Neither copy closes the other side when its own ends: only cut
			// and release do.
			go func() { _, _ = io.Copy(server, client) }()
			go func() { _, _ = io.Copy(client, server) }()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, c := range append(p.live, p.cutOff...) {
			_ = c.client.Close()
			_ = c.server.Close()
		}
	})
	return p
}

// endpoint is the server's MCP endpoint through the proxy.
func (p *halfOpen) endpoint() string { return "http://" + p.ln.Addr().String() + "/mcp" }

// cut closes the client's side of every connection so far, and only that.
func (p *halfOpen) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.live {
		_ = c.client.Close()
	}
	p.cutOff, p.live = append(p.cutOff, p.live...), nil
}

// release closes the server's side of the connections cut: the server
// notices at last that their client is gone.
func (p *halfOpen) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.cutOff {
		_ = c.server.Close()
	}
	p.cutOff = nil
}

// waitFor polls cond for up to 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 10 s for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// lendingEngine lends each account's fake session to the service, as the
// sync engine lends its interactive connection.
type lendingEngine struct {
	mu    sync.Mutex
	boxes map[string]*providertest.FakeMailbox
}

var _ service.InteractiveRunner = (*lendingEngine)(nil)

func (e *lendingEngine) Trigger(context.Context, string) error { return service.ErrSyncNotRunning }

func (e *lendingEngine) Status(context.Context, string) (service.SyncStatus, error) {
	return service.SyncStatus{}, service.ErrSyncNotRunning
}

func (e *lendingEngine) Reconcile(string) {}

func (e *lendingEngine) Interactive(ctx context.Context, id string, fn func(context.Context, provider.Session) error) error {
	e.mu.Lock()
	box, ok := e.boxes[id]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: no mail server for %s in this test", provider.ErrConnClosed, id)
	}
	sess, err := box.Open(ctx, provider.RoleInteractive)
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()
	return fn(ctx, sess)
}

// syncBuffer is a log several goroutines write.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// letter is a plain-text message.
func letter(subject, from, body string) providertest.FakeMessage {
	id := fmt.Sprintf("%d@example.org", time.Now().UnixNano())
	raw := strings.ReplaceAll(fmt.Sprintf("From: %s\nTo: Ana <ana@example.org>\nSubject: %s\nMessage-ID: <%s>\n"+
		"MIME-Version: 1.0\nContent-Type: text/plain; charset=utf-8\n\n%s\n", from, subject, id, body), "\n", "\r\n")
	return providertest.FakeMessage{
		MessageID: id, Subject: subject, From: from,
		To: []string{"Ana <ana@example.org>"}, InternalDate: time.Now().Add(-time.Minute), Raw: []byte(raw),
	}
}
