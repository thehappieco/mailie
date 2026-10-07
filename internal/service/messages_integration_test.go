//go:build integration

// Reading a message from a real Dovecot, through the sync engine's own
// interactive connection: the proof on the wire that reading is a PEEK and
// leaves the message unread, which the in-process server can only suggest.
//
//	docker compose -f it/compose.yml up --wait
//	go test -tags integration -run Dovecot ./internal/service/
package service_test

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/provider"
	imapprovider "github.com/thehappieco/mailie/internal/provider/imap"
	"github.com/thehappieco/mailie/internal/service"
	syncengine "github.com/thehappieco/mailie/internal/sync"
)

func TestReadingFromDovecotGoesThroughTheEnginesConnectionAndLeavesMailUnread(t *testing.T) {
	addr := os.Getenv("MAIL_IT_IMAP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:31143"
	}
	password := os.Getenv("MAIL_IT_IMAP_PASSWORD")
	if password == "" {
		password = "integration"
	}
	user := "reader-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@example.com"
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

	// Another client — the person's phone — puts the report in the INBOX,
	// unread, before sync is switched on.
	phone, err := dovecot.Open(t.Context(), provider.RoleInteractive)
	if err != nil {
		t.Fatalf("Open (is `docker compose -f it/compose.yml up --wait` running?): %v", err)
	}
	defer func() { _ = phone.Close() }()
	if _, err := phone.Append(t.Context(), "INBOX", strings.NewReader(report), int64(len(report)), nil,
		time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("Append: %v", err)
	}

	late := &lateEngine{}
	f := newFixtureWith(t, fixtureOptions{sync: late})
	counted := &countingMailbox{Mailbox: dovecot, opens: map[provider.Role]int{}}
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

	const id = "acc_00000000000000e1"
	host, port := splitHostPort(t, addr)
	if _, err := f.repo.Create(t.Context(), account.Account{
		ID: id, Email: user, Provider: provider.KindIMAP, AuthKind: "password",
		IMAPHost: host, IMAPPort: port, SMTPHost: host, SMTPPort: 31025, SMTPTLS: "starttls", LoginUser: user,
		State: account.StateActive,
	}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetMailboxSync(t.Context(), admin(), id, switchSync(true)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, err := late.m.Status(t.Context(), id)
		if err == nil && st.State == "live" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the initial sync did not finish: %+v %v", st, err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	page, err := f.svc.SearchMessages(t.Context(), reader(), service.SearchRequest{AccountID: id, Query: "septem"})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("search = %+v, %v", page, err)
	}
	msgID := page.Messages[0].ID
	if page.Messages[0].Seen {
		t.Fatal("the report was indexed as read before anyone read it")
	}

	msg, err := f.svc.GetMessage(t.Context(), reader(), service.GetMessageRequest{ID: msgID})
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg.Body.Text == nil || !strings.Contains(*msg.Body.Text, "Olá Ana, segue o relatório.") {
		t.Errorf("text = %v", msg.Body.Text)
	}
	if msg.Body.HTML == nil || !strings.Contains(*msg.Body.HTML, "<b>relatório</b>") {
		t.Errorf("html = %v", msg.Body.HTML)
	}
	pdf, err := f.svc.GetAttachment(t.Context(), reader(), msgID, "2")
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, pdf); body != "%PDF-1.4 zebrafish" || pdf.ContentType != "application/pdf" {
		t.Errorf("attachment = %q as %q", body, pdf.ContentType)
	}
	raw, err := f.svc.GetRaw(t.Context(), reader(), msgID)
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, raw); !strings.Contains(body, "Subject: September report") {
		t.Errorf("the original is %d bytes without its header", len(body))
	}

	// Unread on the server, as the phone sees it.
	if _, err := phone.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}
	flags, err := phone.FetchFlags(t.Context(), imap.UIDSetNum(1), 0)
	if err != nil || len(flags) != 1 {
		t.Fatalf("FetchFlags = %v, %v", flags, err)
	}
	if seen(flags[0].Flags) {
		t.Errorf("Dovecot marked the message read: %v", flags[0].Flags)
	}

	// Three reads, one connection: the engine's interactive one, opened once
	// and kept, beside its sync and idle connections.
	if n := counted.count(provider.RoleInteractive); n != 1 {
		t.Errorf("%d interactive connections were opened for three reads, want the engine's one", n)
	}
	counted.mu.Lock()
	peak := counted.peak
	counted.mu.Unlock()
	if peak > 3 {
		t.Errorf("the account held %d connections at once", peak)
	}

	// Nothing of what was read stayed behind.
	if n := f.count(t, `SELECT count(*) FROM bodies`) + f.count(t, `SELECT count(*) FROM attachment_blobs`); n != 0 {
		t.Errorf("%d body or attachment rows after reading", n)
	}
	if found := f.mentions(t, []string{"zebrafish", "PDF-1.4"}); len(found) > 0 {
		t.Errorf("content reached the database: %v", found)
	}
	if again, err := f.svc.GetMessage(t.Context(), reader(), service.GetMessageRequest{ID: msgID, Format: "text"}); err != nil || again.Seen {
		t.Errorf("after reading, the index says seen=%t (%v)", again.Seen, err)
	}
	if logs := f.logs.String(); strings.Contains(logs, "September report") || strings.Contains(logs, "zebrafish") {
		t.Errorf("the log carries message content:\n%s", logs)
	}
}
