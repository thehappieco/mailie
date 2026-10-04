//go:build integration

// Sending through a real submission server, Mailpit, from a generic IMAP
// account on a real Dovecot: what went over the wire, headers and all, and
// the copy Mailie files in Dovecot's Sent folder — once, whatever the retries.
//
//	docker compose -f it/compose.yml up --wait
//	go test -tags integration -run Mailpit ./internal/service/
package service_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/provider"
	imapprovider "github.com/thehappieco/mailie/internal/provider/imap"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

func TestSendingThroughMailpitFromDovecotFilesOneExactCopyInSent(t *testing.T) {
	imapAddr := envOr("MAIL_IT_IMAP_ADDR", "127.0.0.1:31143")
	smtpAddr := envOr("MAIL_IT_SMTP_ADDR", "127.0.0.1:31025")
	api := envOr("MAIL_IT_MAILPIT_API", "http://127.0.0.1:31080")
	password := envOr("MAIL_IT_IMAP_PASSWORD", "integration")
	stamp := strconv.FormatInt(time.Now().UnixNano(), 36)
	user := "sender-" + stamp + "@example.com"
	dovecot, err := imapprovider.New(provider.Config{
		Kind: provider.KindIMAP, IMAPAddr: imapAddr,
		Credentials: provider.Credentials{User: user, Password: password},
		SpoolDir:    t.TempDir(), AllowInsecureAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The message ana answers, in her INBOX, with a thread behind it.
	phone, err := dovecot.Open(t.Context(), provider.RoleInteractive)
	if err != nil {
		t.Fatalf("Open (is `docker compose -f it/compose.yml up --wait` running?): %v", err)
	}
	defer func() { _ = phone.Close() }()
	question := strings.ReplaceAll("From: Bea Lima <bea@example.org>\nTo: "+user+"\nSubject: Lunch on Friday?\n"+
		"Message-ID: <question-"+stamp+"@example.org>\nReferences: <root-"+stamp+"@example.org>\n"+
		"MIME-Version: 1.0\nContent-Type: text/plain; charset=utf-8\n\nShall we?\n", "\n", "\r\n")
	if _, err := phone.Append(t.Context(), "INBOX", strings.NewReader(question), int64(len(question)), nil,
		time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("Append: %v", err)
	}

	f := newFixtureWith(t, fixtureOptions{})
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	const id = "acc_00000000000000e5"
	imapHost, imapPort := splitHostPort(t, imapAddr)
	smtpHost, smtpPort := splitHostPort(t, smtpAddr)
	if _, err := f.repo.Create(t.Context(), account.Account{
		ID: id, Email: user, DisplayName: "Ana Lima", Provider: provider.KindIMAP, AuthKind: "password",
		OwnerUserID: ana.UserID, IMAPHost: imapHost, IMAPPort: imapPort, SMTPHost: smtpHost, SMTPPort: smtpPort,
		SMTPTLS: "starttls", LoginUser: user, SaveSentCopy: true, State: account.StateActive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.SavePassword(t.Context(), id, password); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.GrantSyncConsent(t.Context(), ana, service.DefaultSyncConsentVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.GrantSendConsent(t.Context(), ana, service.DefaultSendConsentVersion); err != nil {
		t.Fatal(err)
	}
	storetest.IndexMailbox(t, f.db, id, dovecot)
	var parent int64
	if err := f.db.Reader().QueryRowContext(t.Context(),
		`SELECT id FROM messages WHERE account_id = ? AND message_id = ?`, id, "question-"+stamp+"@example.org").Scan(&parent); err != nil {
		t.Fatal(err)
	}

	send := func() service.SendResult {
		t.Helper()
		res, err := f.svc.SendMessage(t.Context(), ana, service.SendRequest{
			IdempotencyKey: "it-" + stamp,
			Compose: service.Compose{
				AccountID: id, To: []service.Address{{Name: "Bea Lima", Email: "bea@example.org"}},
				Text: "Friday at noon works.", InReplyTo: parent, Confirm: true,
			},
			Attachments: func(add func(service.Upload) error) error {
				return add(service.Upload{Filename: "agenda.pdf", ContentType: "application/pdf",
					Body: strings.NewReader("%PDF-1.4 agenda " + stamp)})
			},
		})
		if err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
		return res
	}
	res := send()
	if res.State != service.SendStateSent || res.Replayed {
		t.Fatalf("SendMessage = %+v", res)
	}

	// What Mailpit received: one message, each id in one pair of brackets.
	raw := mailpitRaw(t, api, res.MessageID)
	one := regexp.MustCompile(`^<[^<>\s]+>$`)
	for name, want := range map[string][]string{
		"Message-ID":  {"<" + res.MessageID + ">"},
		"In-Reply-To": {"<question-" + stamp + "@example.org>"},
		"References":  {"<root-" + stamp + "@example.org>", "<question-" + stamp + "@example.org>"},
	} {
		got := header(t, raw, name)
		if len(got) != 1 {
			t.Fatalf("%s appears %d times", name, len(got))
		}
		tokens := strings.Fields(got[0])
		if strings.Join(tokens, " ") != strings.Join(want, " ") {
			t.Errorf("%s = %q, want %q", name, got[0], want)
		}
		for _, tok := range tokens {
			if !one.MatchString(tok) {
				t.Errorf("%s holds %q", name, tok)
			}
		}
	}
	if subject := header(t, raw, "Subject"); len(subject) != 1 || subject[0] != "Re: Lunch on Friday?" {
		t.Errorf("Subject = %q", subject)
	}
	if !bytes.Contains(raw, []byte("agenda.pdf")) {
		t.Error("the attachment did not go")
	}

	// The same request again is the record, and files nothing more.
	if again := send(); !again.Replayed || again.MessageID != res.MessageID {
		t.Fatalf("the retry = %+v", again)
	}
	if _, err := phone.Select(t.Context(), "Sent", true, 0); err != nil {
		t.Fatal(err)
	}
	copies, err := phone.SearchMessageID(t.Context(), res.MessageID)
	if err != nil || len(copies) != 1 {
		t.Fatalf("Dovecot's Sent holds %d copies (%v), want exactly one", len(copies), err)
	}
	part, err := phone.FetchRaw(t.Context(), copies[0], 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = part.Body.Close() }()
	filed, err := io.ReadAll(part.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(bytes.TrimRight(raw, "\r\n"), bytes.TrimRight(filed, "\r\n")) {
		t.Errorf("the copy in Sent is not what was transmitted:\n%s\n--- Mailpit:\n%s", filed, raw)
	}
	flags, err := phone.FetchFlags(t.Context(), imap.UIDSetNum(copies[0]), 0)
	if err != nil || len(flags) != 1 || !seen(flags[0].Flags) {
		t.Errorf("the copy's flags = %+v (%v), want it read", flags, err)
	}
	status, err := f.svc.SendStatus(t.Context(), ana, id, "it-"+stamp)
	if err != nil || status.SentCopy != "appended" || status.State != service.SendStateSent {
		t.Fatalf("SendStatus = %+v, %v", status, err)
	}
}

// mailpitRaw finds the message Mailpit holds with a Message-ID and returns
// its source.
func mailpitRaw(t *testing.T, api, messageID string) []byte {
	t.Helper()
	get := func(path string) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, api+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Mailpit API: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("Mailpit API %s: %d %v", path, resp.StatusCode, err)
		}
		return body
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var listing struct {
			Messages []struct {
				ID        string `json:"ID"`
				MessageID string `json:"MessageID"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(get("/api/v1/messages?limit=200"), &listing); err != nil {
			t.Fatal(err)
		}
		var found []string
		for _, m := range listing.Messages {
			if strings.Trim(m.MessageID, "<>") == messageID {
				found = append(found, m.ID)
			}
		}
		if len(found) > 1 {
			t.Fatalf("Mailpit received the message %d times", len(found))
		}
		if len(found) == 1 {
			return get("/api/v1/message/" + found[0] + "/raw")
		}
		if time.Now().After(deadline) {
			t.Fatalf("Mailpit never received %s", messageID)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
