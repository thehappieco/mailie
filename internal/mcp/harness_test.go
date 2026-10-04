package mcp_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

// The MCP server is tested against the real service, a temporary database
// and fake mailboxes, as REST is: testing a transport against a fake service
// is how two transports drift apart.

// protocols are the versions every session-level test runs at: the SDK's
// newest, which a client connecting over stdio or in memory negotiates, and
// the newest the stateful HTTP transport serves.
var protocols = []string{"", "2025-11-25"}

func eachProtocol(t *testing.T, run func(t *testing.T, protocol string)) {
	t.Helper()
	for _, protocol := range protocols {
		name := protocol
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) { run(t, protocol) })
	}
}

type harness struct {
	t      *testing.T
	store  *store.Store
	bus    *events.Bus
	svc    *service.Service
	users  *auth.Users
	engine *lendingEngine
	mcp    *mcp.Server
	logs   *syncBuffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db := storetest.New(t)
	bus := events.NewBus(events.NewJournal(db))
	logs := &syncBuffer{}
	logger := obs.NewLoggerTo(logs, "debug", "json")
	key := bytes.Repeat([]byte{0xC3}, secrets.KeyLen)
	keyring, err := secrets.NewKeyring(1, map[uint8][]byte{1: key})
	if err != nil {
		t.Fatal(err)
	}
	registry := account.NewRegistry(t.Context(), account.NewRepository(db, keyring), account.RegistryOptions{
		Google: account.OAuthClient{ClientID: "google-client"}, SpoolDir: t.TempDir(), AllowPrivate: true,
	})
	t.Cleanup(func() { _ = registry.Close() })
	engine := &lendingEngine{boxes: map[string]*providertest.FakeMailbox{}}
	users := auth.NewUsers(db)
	svc := service.New(service.Deps{
		Accounts: registry, Keys: auth.NewKeys(db), Users: users, Store: db, Bus: bus, Sync: engine, Log: logger,
	})
	return &harness{
		t: t, store: db, bus: bus, svc: svc, users: users, engine: engine, logs: logs,
		mcp: mcp.New(svc, logger, "test"),
	}
}

// person is somebody signed in to the console, who agreed to sync.
type person struct {
	user    auth.User
	token   string
	session service.Principal
}

func (h *harness) person(email string, role auth.Role) person {
	h.t.Helper()
	u := authtest.NewUser(h.t, h.store, email, role)
	token := authtest.SignIn(h.t, h.users, email)
	p, err := h.svc.Authenticate(h.t.Context(), token, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, _, err := h.store.GrantSyncConsent(h.t.Context(), u.ID, service.DefaultSyncConsentVersion); err != nil {
		h.t.Fatal(err)
	}
	return person{user: u, token: token, session: p}
}

// allowActions records the person's consent to actions.
func (h *harness) allowActions(pp person) {
	h.t.Helper()
	if _, err := h.store.GrantActionsConsent(h.t.Context(), pp.user.ID, service.DefaultActionsConsentVersion); err != nil {
		h.t.Fatal(err)
	}
}

// key creates a key the way a person does in the console.
func (h *harness) key(pp person, scope string, accounts ...string) string {
	h.t.Helper()
	created, err := h.svc.CreateMyAPIKey(h.t.Context(), pp.session, service.PersonalKeyRequest{
		Name: "assistant", Scope: scope, AccountIDs: accounts, TermsVersion: service.DefaultKeyTermsVersion,
	})
	if err != nil {
		h.t.Fatalf("create key: %v", err)
	}
	return created.Key
}

// mailbox registers an active mailbox on a fake mail server: owned by
// ownerID, or nobody's (and switched on by the operator) when it is empty.
func (h *harness) mailbox(id, ownerID, email string) *providertest.FakeMailbox {
	h.t.Helper()
	if _, err := account.NewRepository(h.store, nil).Create(h.t.Context(), account.Account{
		ID: id, Email: email, Provider: provider.KindIMAP, AuthKind: "password", OwnerUserID: ownerID,
		IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example", SMTPPort: 465,
		SMTPTLS: "implicit", LoginUser: email, State: account.StateActive,
	}); err != nil {
		h.t.Fatal(err)
	}
	if ownerID == "" {
		if _, err := h.store.SetInstanceSync(h.t.Context(), id, true, "cli"); err != nil {
			h.t.Fatal(err)
		}
	}
	box := providertest.NewFakeMailbox(providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.CreateFolder("Trash", goimap.MailboxAttrTrash)
	h.engine.mu.Lock()
	h.engine.boxes[id] = box
	h.engine.mu.Unlock()
	return box
}

// deliver puts a message in the mailbox's inbox, indexes the mailbox as
// sync would, and returns the message's id.
func (h *harness) deliver(accountID string, box *providertest.FakeMailbox, msg providertest.FakeMessage) int64 {
	h.t.Helper()
	uid := box.Deliver("INBOX", msg)
	storetest.IndexMailbox(h.t, h.store, accountID, box)
	return h.row(accountID, uid)
}

func (h *harness) row(accountID string, uid goimap.UID) int64 {
	h.t.Helper()
	var id int64
	if err := h.store.Reader().QueryRowContext(h.t.Context(),
		`SELECT m.id FROM messages m JOIN folders f ON f.id = m.folder_id
		  WHERE m.account_id = ? AND f.name = 'INBOX' AND m.uid = ?`, accountID, uint32(uid)).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// announce journals message.new for an indexed message and publishes it, as
// the sync engine does once the message is committed.
func (h *harness) announce(accountID string, id int64, subject string) {
	h.t.Helper()
	var folderID int64
	if err := h.store.Reader().QueryRowContext(h.t.Context(),
		`SELECT folder_id FROM messages WHERE id = ?`, id).Scan(&folderID); err != nil {
		h.t.Fatal(err)
	}
	ev, err := events.New(events.TypeMessageNew, accountID, time.Now(), store.MessageNew{
		AccountID: accountID, MessageID: id, FolderID: folderID, FolderRole: "inbox", Subject: subject,
		InternalDate: time.Now().Unix(), FirstCopy: true, FirstInboxCopy: true,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	var written []events.Event
	if err := h.store.Write(h.t.Context(), func(tx *sql.Tx) error {
		var err error
		written, err = h.bus.Journal().Append(h.t.Context(), tx, []events.Event{ev})
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	h.bus.Publish(written...)
}

// connect opens a session in memory with a key, as a client that launched
// the daemon over stdio would have one.
func (h *harness) connect(key, protocol string, opts *sdk.ClientOptions) *sdk.ClientSession {
	h.t.Helper()
	p, err := h.svc.AuthenticateTool(h.t.Context(), key, nil)
	if err != nil {
		h.t.Fatalf("AuthenticateTool: %v", err)
	}
	ct, st := sdk.NewInMemoryTransports()
	ss, err := h.mcp.NewSession(p).Connect(h.t.Context(), st, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = ss.Close() })
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, opts).
		Connect(h.t.Context(), ct, &sdk.ClientSessionOptions{ProtocolVersion: protocol})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// call calls a tool and returns its result, failing on a protocol error.
func call(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

// ok is a call that must succeed, decoded from its structured content.
func ok[T any](t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) T {
	t.Helper()
	res := call(t, cs, name, args)
	if res.IsError {
		t.Fatalf("%s failed: %s", name, text(res))
	}
	var out T
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %v in %s", name, err, raw)
	}
	return out
}

// refused is a call that must fail with the given code, and returns its
// message.
func refused(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any, code service.Code) string {
	t.Helper()
	res := call(t, cs, name, args)
	msg := text(res)
	if !res.IsError {
		t.Fatalf("%s succeeded, want %s: %s", name, code, msg)
	}
	if !strings.HasPrefix(msg, string(code)+": ") {
		t.Fatalf("%s failed with %q, want %s", name, msg, code)
	}
	return msg
}

// text is a result's text content.
func text(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// readResource reads a resource into v, or returns the error.
func readResource(t *testing.T, cs *sdk.ClientSession, uri string, v any) error {
	t.Helper()
	res, err := cs.ReadResource(t.Context(), &sdk.ReadResourceParams{URI: uri})
	if err != nil {
		return err
	}
	if len(res.Contents) != 1 {
		t.Fatalf("%s: %d contents", uri, len(res.Contents))
	}
	return json.Unmarshal([]byte(res.Contents[0].Text), v)
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

// syncBuffer is a log destination the daemon's goroutines can share.
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

// letter is a message with a text body, an HTML body and a small PDF.
func letter(subject, from, body, filename, attachment string) providertest.FakeMessage {
	id := fmt.Sprintf("%d@example.org", time.Now().UnixNano())
	raw := strings.ReplaceAll(fmt.Sprintf(`From: %s
To: Ana <ana@example.org>
Subject: %s
Message-ID: <%s>
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="mixed"

--mixed
Content-Type: multipart/alternative; boundary="alt"

--alt
Content-Type: text/plain; charset=utf-8

%s
--alt
Content-Type: text/html; charset=utf-8

<p>%s</p>
--alt--
--mixed
Content-Type: application/pdf
Content-Disposition: attachment; filename="%s"
Content-Transfer-Encoding: base64

%s
--mixed--
`, from, subject, id, body, body, filename, base64Lines(attachment)), "\n", "\r\n")
	return providertest.FakeMessage{
		MessageID: id, Subject: subject, From: from,
		To: []string{"Ana <ana@example.org>"}, InternalDate: time.Now().Add(-time.Minute), Raw: []byte(raw),
	}
}

func base64Lines(s string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(s))
	var b strings.Builder
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\n")
		enc = enc[76:]
	}
	b.WriteString(enc)
	return b.String()
}
