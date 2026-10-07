//go:build integration

// An assistant on a real Dovecot, through the MCP HTTP transport, the
// service and the sync engine's own connections: it searches, reads a
// message and its attachment, waits for mail that arrives by IDLE, and the
// server still has every message unread.
//
//	docker compose -f it/compose.yml up --wait
//	go test -tags integration -run Dovecot ./internal/mcp/
package mcp_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/mcp"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/provider"
	imapprovider "github.com/thehappieco/mailie/internal/provider/imap"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store/storetest"
	syncengine "github.com/thehappieco/mailie/internal/sync"
)

func TestAnAssistantReadsAndWaitsOnDovecotAndNothingIsMarkedRead(t *testing.T) {
	addr := os.Getenv("MAIL_IT_IMAP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:31143"
	}
	password := os.Getenv("MAIL_IT_IMAP_PASSWORD")
	if password == "" {
		password = "integration"
	}
	user := "assistant-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@example.com"
	dovecot, err := imapprovider.New(provider.Config{
		Kind: provider.KindIMAP, IMAPAddr: addr,
		Credentials: provider.Credentials{User: user, Password: password},
		SpoolDir:    t.TempDir(),
		// The compose file runs Dovecot without TLS; only tests can say so.
		AllowInsecureAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	phone, err := dovecot.Open(t.Context(), provider.RoleInteractive)
	if err != nil {
		t.Fatalf("Open (is `docker compose -f it/compose.yml up --wait` running?): %v", err)
	}
	defer func() { _ = phone.Close() }()
	appendMessage(t, phone, letter("Quarterly numbers", "Bea <bea@example.org>", "Revenue is up.", "q3.pdf", "%PDF q3"))

	// The daemon: the store, the engine on Dovecot, the service, MCP.
	db := storetest.New(t)
	bus := events.NewBus(events.NewJournal(db))
	logs := &syncBuffer{}
	logger := obs.NewLoggerTo(logs, "debug", "json")
	keyring, err := secrets.NewKeyring(1, map[uint8][]byte{1: bytes.Repeat([]byte{0x7e}, secrets.KeyLen)})
	if err != nil {
		t.Fatal(err)
	}
	repo := account.NewRepository(db, keyring)
	registry := account.NewRegistry(t.Context(), repo, account.RegistryOptions{SpoolDir: t.TempDir(), AllowPrivate: true})
	t.Cleanup(func() { _ = registry.Close() })
	engine := syncengine.New(syncengine.Deps{
		Store: db, Accounts: repo, Bus: bus, Log: logger,
		Mailboxes: func(context.Context, string) (provider.Mailbox, error) { return dovecot, nil },
	}, syncengine.Options{})
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		engine.Run(runCtx)
	}()
	defer func() {
		stop()
		<-done
	}()
	svc := service.New(service.Deps{
		Accounts: registry, Keys: auth.NewKeys(db), Users: auth.NewUsers(db), Store: db, Bus: bus, Sync: engine,
		Log: logger,
	})

	// The operator's mailbox: an instance key reaches it through a tool.
	const id = "acc_00000000000000f1"
	host, portStr, _ := strings.Cut(addr, ":")
	port, _ := strconv.Atoi(portStr)
	if _, err := repo.Create(t.Context(), account.Account{
		ID: id, Email: user, Provider: provider.KindIMAP, AuthKind: "password",
		IMAPHost: host, IMAPPort: port, SMTPHost: host, SMTPPort: 31025, SMTPTLS: "starttls", LoginUser: user,
		State: account.StateActive,
	}, ""); err != nil {
		t.Fatal(err)
	}
	operator := service.Principal{KeyPrefix: "aaaaaaaa", Scope: auth.ScopeAdmin}
	if _, err := svc.SetMailboxSync(t.Context(), operator, id, switchSync(true)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, err := engine.Status(t.Context(), id)
		if err == nil && st.State == "live" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the initial sync did not finish: %+v %v", st, err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.New(svc, logger, "it").HTTPHandler(mcp.HTTPOptions{}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cs, err := connectHTTP(t, srv, authtest.NewKey(t, db, auth.ScopeRead, ""), "2025-11-25")
	if err != nil {
		t.Fatal(err)
	}

	page := ok[service.MessagePage](t, cs, "search_messages", map[string]any{"q": "quarterly"})
	if len(page.Messages) != 1 || page.Messages[0].Seen {
		t.Fatalf("search found %+v", page.Messages)
	}
	first := page.Messages[0].ID
	m := ok[service.Message](t, cs, "get_message", map[string]any{"id": first, "format": "both"})
	if m.Body.Text == nil || !strings.Contains(*m.Body.Text, "Revenue is up.") {
		t.Fatalf("get_message read %+v", m.Body)
	}
	res := call(t, cs, "get_attachment", map[string]any{"id": first, "part": "2"})
	if res.IsError || !strings.Contains(text(res), "q3.pdf") {
		t.Fatalf("get_attachment: %s", text(res))
	}

	// New mail, delivered by another client while the assistant waits: the
	// engine hears it by IDLE and the wait returns it.
	waited := make(chan waitAnswer, 1)
	go func() {
		waited <- ok[waitAnswer](t, cs, "wait_for_new_mail", map[string]any{"timeout_seconds": 60})
	}()
	time.Sleep(500 * time.Millisecond)
	appendMessage(t, phone, letter("Board meeting", "Carl <carl@example.org>", "Thursday at ten.", "agenda.pdf", "%PDF agenda"))
	got := <-waited
	if got.TimedOut || len(got.Messages) != 1 || got.Messages[0].Subject != "Board meeting" {
		t.Fatalf("the wait answered %+v", got)
	}
	m = ok[service.Message](t, cs, "get_message", map[string]any{"id": got.Messages[0].ID})
	if m.Body.Text == nil || !strings.Contains(*m.Body.Text, "Thursday at ten.") {
		t.Errorf("the new message read %+v", m.Body)
	}

	// Unread on the server, as the phone sees them.
	if _, err := phone.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}
	flags, err := phone.FetchFlags(t.Context(), imap.UIDSet{imap.UIDRange{Start: 1, Stop: 0}}, 0)
	if err != nil || len(flags) != 2 {
		t.Fatalf("FetchFlags = %v, %v", flags, err)
	}
	for _, f := range flags {
		if hasFlag(f.Flags, imap.FlagSeen) {
			t.Errorf("UID %d was marked read on Dovecot: %v", f.UID, f.Flags)
		}
	}
	if l := logs.String(); strings.Contains(l, "Revenue") || strings.Contains(l, "Thursday") || strings.Contains(l, "quarterly") {
		t.Errorf("the log carries content or search text:\n%s", l)
	}
}

// switchSync is a request to switch an operator mailbox's sync on or off.
func switchSync(on bool) service.MailboxSyncRequest {
	return service.MailboxSyncRequest{Enabled: &on}
}

func appendMessage(t *testing.T, s provider.Session, msg providertest.FakeMessage) {
	t.Helper()
	if _, err := s.Append(t.Context(), "INBOX", bytes.NewReader(msg.Raw), int64(len(msg.Raw)), nil,
		time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("Append: %v", err)
	}
}
