//go:build integration

// Integration tests of the registry against the compose services: a password
// account proved against a real Dovecot before it is stored, and a message
// submitted to a real SMTP server through the guarded dialer.
//
// Both servers are on 127.0.0.1, which production refuses to connect to; the
// registries here opt in to private hosts exactly as MAIL_ACCOUNT_ALLOW_PRIVATE
// does, and the last test shows the same server refused without it.
//
//	docker compose -f it/compose.yml up --wait
//	go test -tags integration ./internal/account/
package account_test

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/provider"
)

func itAddr(t *testing.T, env, fallback string) (string, int) {
	t.Helper()
	addr := os.Getenv(env)
	if addr == "" {
		addr = fallback
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

func itRegistry(t *testing.T, allowPrivate bool) *account.Registry {
	t.Helper()
	repo, _ := newRepo(t)
	registry := account.NewRegistry(t.Context(), repo, account.RegistryOptions{
		SpoolDir:     t.TempDir(),
		AllowPrivate: allowPrivate,
		// The compose services speak plain TCP; the daemon cannot set this.
		AllowInsecureAuth: true,
	})
	t.Cleanup(func() { _ = registry.Close() })
	return registry
}

// itRequest is a generic IMAP account on Dovecot, whose static passdb takes
// any fresh username with one password, and Mailpit for submission.
func itRequest(t *testing.T, password string) account.AddRequest {
	t.Helper()
	imapHost, imapPort := itAddr(t, "MAIL_IT_IMAP_ADDR", "127.0.0.1:31143")
	smtpHost, smtpPort := itAddr(t, "MAIL_IT_SMTP_ADDR", "127.0.0.1:31025")
	user := "registry-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@example.com"
	return account.AddRequest{
		Email: user, Provider: provider.KindIMAP, Password: password,
		IMAPHost: imapHost, IMAPPort: imapPort, SMTPHost: smtpHost, SMTPPort: smtpPort, SMTPTLS: "starttls",
	}
}

func TestARealServerProvesAPasswordAccountBeforeItIsStored(t *testing.T) {
	registry := itRegistry(t, true)

	_, _, err := registry.Add(t.Context(), itRequest(t, "not the password"))
	if !errors.Is(err, account.ErrLoginRefused) {
		t.Fatalf("a wrong password against Dovecot: %v, want ErrLoginRefused", err)
	}
	if accounts, _ := registry.Repo().List(t.Context()); len(accounts) != 0 {
		t.Fatalf("a refused login left %d accounts behind", len(accounts))
	}

	created, _, err := registry.Add(t.Context(), itRequest(t, envOr("MAIL_IT_IMAP_PASSWORD", "integration")))
	if err != nil {
		t.Fatalf("Add (is `docker compose -f it/compose.yml up --wait` running?): %v", err)
	}
	if created.State != account.StateActive {
		t.Fatalf("state = %s, want active once the login worked", created.State)
	}
}

func TestSubmissionGoesThroughTheDaemonsDialFunction(t *testing.T) {
	// The daemon hands go-mail its own dial function, which is what lets the
	// private-address check cover SMTP. This is the one place that path meets
	// a real server — with AllowPrivate on, so without the check itself:
	// dialguard_test.go proves the refusal, and submission_test.go the
	// implicit-TLS handshake the same function performs on port 465.
	registry := itRegistry(t, true)
	created, _, err := registry.Add(t.Context(), itRequest(t, envOr("MAIL_IT_IMAP_PASSWORD", "integration")))
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := registry.Mailbox(t.Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	subject := "daemon dialer " + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := mailbox.Sender().Send(t.Context(), provider.Outgoing{
		MessageID: strconv.FormatInt(time.Now().UnixNano(), 36) + "@example.com",
		From:      provider.Address{Email: created.Email},
		To:        []provider.Address{{Email: "someone@example.com"}},
		Subject:   subject,
		TextBody:  "sent through the registry",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	api := os.Getenv("MAIL_IT_MAILPIT_API")
	if api == "" {
		api = "http://127.0.0.1:31080"
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if mailpitHas(t, api, subject) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Mailpit never received %q", subject)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestTheSameServerIsRefusedWithoutThePrivateHostOptIn(t *testing.T) {
	registry := itRegistry(t, false)
	_, _, err := registry.Add(t.Context(), itRequest(t, envOr("MAIL_IT_IMAP_PASSWORD", "integration")))
	if !errors.Is(err, account.ErrPrivateHost) {
		t.Fatalf("Dovecot on 127.0.0.1 without the opt-in: %v, want ErrPrivateHost", err)
	}
}

func mailpitHas(t *testing.T, api, subject string) bool {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, api+"/api/v1/messages?limit=200", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Mailpit API: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var listing struct {
		Messages []struct {
			Subject string `json:"Subject"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatalf("Mailpit API: %v", err)
	}
	for _, m := range listing.Messages {
		if strings.EqualFold(m.Subject, subject) {
			return true
		}
	}
	return false
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
