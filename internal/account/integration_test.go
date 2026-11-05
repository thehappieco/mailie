//go:build integration

// Integration tests of the registry against the compose services: a password
// account proved against a real Dovecot before it is stored, a link at the
// provider whose grant is proved by a real XOAUTH2 login as the address it
// learned, and a message submitted to a real SMTP server through the guarded
// dialer.
//
// Both servers are on 127.0.0.1, which production refuses to connect to; the
// registries here opt in to private hosts exactly as MAIL_ACCOUNT_ALLOW_PRIVATE
// does, and the last test shows the same server refused without it.
//
//	docker compose -f it/compose.yml up --wait
//	go test -tags integration ./internal/account/
package account_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
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

// dovecotKey is the HMAC key Dovecot validates XOAUTH2 tokens with
// (it/dovecot/oauth2-keys.dict holds its base64): a test-only key that
// protects nothing.
const dovecotKey = "integration-oauth2-key-not-secret"

// dovecotIDP is a token endpoint whose answer to a code carries an ID token
// naming one address and an access token Dovecot takes for another, or the
// same: a JWT whose email claim is the user it logs in.
type dovecotIDP struct {
	server *httptest.Server

	mu sync.Mutex
	// claims are the next ID token's; tokenFor is the address the next
	// access token logs in.
	claims   map[string]any
	tokenFor string
}

func newDovecotIDP(t *testing.T) *dovecotIDP {
	t.Helper()
	idp := &dovecotIDP{}
	idp.server = httptest.NewServer(http.HandlerFunc(idp.token))
	t.Cleanup(idp.server.Close)
	return idp
}

func (d *dovecotIDP) endpoint() oauth2.Endpoint {
	return oauth2.Endpoint{AuthURL: d.server.URL + "/authorize", TokenURL: d.server.URL + "/token"}
}

// names makes the next answer's ID token carry claims, and its access token
// log in as tokenFor.
func (d *dovecotIDP) names(claims map[string]any, tokenFor string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.claims, d.tokenFor = claims, tokenFor
}

func (d *dovecotIDP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	claims, tokenFor := d.claims, d.tokenFor
	d.mu.Unlock()
	now := time.Now()
	answer := map[string]any{
		// Dovecot's local validation wants a subject beside the email.
		"access_token": signedJWT(map[string]any{
			"sub": "subject-of-" + tokenFor, "email": tokenFor, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		}),
		"refresh_token": "refresh-" + strconv.FormatInt(now.UnixNano(), 36), "token_type": "Bearer", "expires_in": 3600,
	}
	if r.Form.Get("grant_type") == "authorization_code" {
		payload, _ := json.Marshal(claims)
		answer["id_token"] = "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".c2lnbmF0dXJl"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}

// signedJWT is an HS256 JWT of claims under dovecotKey, with no azp and no
// kid: the key Dovecot looks up is its default one.
func signedJWT(claims map[string]any) string {
	enc := base64.RawURLEncoding
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(dovecotKey))
	mac.Write([]byte(signing))
	return signing + "." + enc.EncodeToString(mac.Sum(nil))
}

func TestALinkAtTheProviderLogsInToDovecotAsTheAddressItLearned(t *testing.T) {
	// Gmail's and Microsoft's servers stand in Dovecot's place: a grant is
	// proved by a real XOAUTH2 login as the address the provider's ID token
	// named, and Dovecot opens a mailbox only to a token for that user.
	imapHost, imapPort := itAddr(t, "MAIL_IT_IMAP_ADDR", "127.0.0.1:31143")
	dovecot := net.JoinHostPort(imapHost, strconv.Itoa(imapPort))
	idp := newDovecotIDP(t)
	repo, db := newRepo(t)
	registry := account.NewRegistry(t.Context(), repo, account.RegistryOptions{
		GoogleWeb:    account.OAuthClient{ClientID: googleClient, ClientSecret: "google-secret", Endpoint: idp.endpoint()},
		MicrosoftWeb: account.OAuthClient{ClientID: msClient, ClientSecret: "ms-secret", Endpoint: idp.endpoint()},
		PublicURL:    publicURL,
		SpoolDir:     t.TempDir(),
		AllowPrivate: true,
		// Dovecot here speaks plain TCP; the daemon cannot set this.
		AllowInsecureAuth: true,
		RedirectIMAP:      map[string]string{"imap.gmail.com:993": dovecot, "outlook.office365.com:993": dovecot},
	})
	t.Cleanup(func() { _ = registry.Close() })
	ana := authtest.NewUser(t, db, "ana@example.test", auth.RoleMember)
	link := func(kind provider.Kind, claims map[string]any, tokenFor string) (account.Account, error) {
		t.Helper()
		_, flow, err := registry.StartLink(t.Context(), account.LinkStart{Provider: kind, OwnerUserID: ana.ID})
		if err != nil {
			t.Fatal(err)
		}
		idp.names(claims, tokenFor)
		key := authtest.LinkKey(t)
		return registry.CompleteLink(t.Context(), account.Redirect{Code: "the-code", State: flow.State}, ana.ID,
			account.LinkCompletion{Key: &key, LinkedBy: ana.ID})
	}
	fresh := func(name string) string {
		return name + "-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@example.test"
	}

	for kind, claimsOf := range map[provider.Kind]func(string) map[string]any{
		provider.KindGmail:     googleClaims,
		provider.KindMicrosoft: func(email string) map[string]any { return microsoftClaims(email, workTenant) },
	} {
		address := fresh("link")
		created, err := link(kind, claimsOf(address), address)
		if err != nil {
			t.Fatalf("%s: a link whose token logs in as the address it names (is `docker compose -f it/compose.yml up "+
				"--wait` running?): %v", kind, err)
		}
		if created.Email != address || created.LoginUser != address || created.State != account.StateActive {
			t.Errorf("%s: the mailbox the link created: %+v", kind, created)
		}
		// Its grant opens it, on Dovecot, as that address.
		mailbox, err := registry.Mailbox(t.Context(), created.ID)
		if err != nil {
			t.Fatal(err)
		}
		session, err := mailbox.Open(t.Context(), provider.RoleInteractive)
		if err != nil {
			t.Fatalf("%s: opening the linked mailbox: %v", kind, err)
		}
		folders, err := session.ListFolders(t.Context(), false)
		_ = session.Close()
		if err != nil || len(folders) == 0 {
			t.Errorf("%s: its folders: %v, %v", kind, folders, err)
		}
	}

	// A provider that names one address and grants another account: Dovecot
	// refuses the login as the address named, and nothing is stored.
	before := countOf(t, db, `SELECT count(*) FROM accounts`)
	named := fresh("named")
	if _, err := link(provider.KindGmail, googleClaims(named), fresh("granted")); !errors.Is(err, account.ErrMailboxRefused) {
		t.Fatalf("a grant for another account than the one named: %v, want ErrMailboxRefused", err)
	}
	if n := countOf(t, db, `SELECT count(*) FROM accounts`); n != before {
		t.Errorf("a refused link stored %d mailboxes", n-before)
	}
}
