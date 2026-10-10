package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
)

// request sends a JSON body with an optional bearer token.
func request(t *testing.T, method, url, token, body string) answer {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return answer{resp.StatusCode, resp.Header, string(raw)}
}

// seeded is a daemon's configuration over a database that already holds what
// the command line makes before the daemon starts: ana, an owner with a
// password, signed in, and an invitation that signs new@example.com up. It
// returns ana's session and the invitation's code.
func seeded(t *testing.T) (cfg config.Config, anaSession, inviteCode string) {
	t.Helper()
	cfg = localConfig(t)
	db, err := store.Open(t.Context(), cfg.DatabasePath(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	authtest.NewUser(t, db, "ana@example.com", auth.RoleOwner)
	users := auth.NewUsers(db)
	anaSession = authtest.SignIn(t, users, "ana@example.com")
	inviteCode, _, err = users.CreateInvite(t.Context(), auth.NewInvite{Email: "new@example.com", Role: auth.RoleMember, CreatedBy: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return cfg, anaSession, inviteCode
}

func TestAnExtensionSignsPeopleInWhereExternalSignInOnlyTurnsPasswordsOff(t *testing.T) {
	// The extension a binary that embeds the daemon would mount: it has
	// done its provider's protocol, which delivered the product key to the
	// person's page and named its public half, pinned here; the page made
	// the person's account key and wrapped it (a public key and a wrap's
	// shape, here). It answers as POST /v1/auth/login answers the console.
	enc := base64.RawURLEncoding.EncodeToString
	publicKey, wrap := enc(authtest.PublicKey(t)), enc(authtest.PlatformWrap(t))
	signIn := func(_ context.Context, r *Router, d Deps) error {
		r.HandleFunc("POST /v1/auth/provider", func(w http.ResponseWriter, req *http.Request) {
			ctx := req.Context()
			in := service.ExternalSignIn{
				Issuer: "https://accounts.example.com", Subject: "subject-of-cy", Email: "cy@example.com",
				EmailVerified: true, Name: "Cy Lima", UserAgent: req.UserAgent(), TTL: 24 * time.Hour,
				WantsKey: true, ProductKeyID: authtest.ProductKeyID,
			}
			if _, _, err := d.Service.PinIdentityKey(ctx, in.Issuer, in.Subject, in.ProductKeyID,
				authtest.ProductKey(in.Subject)); err != nil {
				http.Error(w, service.MessageOf(err), http.StatusForbidden)
				return
			}
			signed, err := d.Service.SignInExternal(ctx, in)
			if err != nil {
				http.Error(w, service.MessageOf(err), http.StatusForbidden)
				return
			}
			if signed.Session == nil {
				session, err := d.Service.EnrolExternal(ctx, service.ExternalEnrolment{
					Ticket: signed.Enrolment.Ticket, PublicKey: publicKey, PlatformWrap: wrap,
					ProductKeyID: in.ProductKeyID, UserAgent: req.UserAgent(),
				})
				if err != nil {
					http.Error(w, service.MessageOf(err), http.StatusForbidden)
					return
				}
				signed.Session = &session
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(signed.Session)
		})
		return nil
	}

	cfg, ana, code := seeded(t)
	base := runDaemon(t, cfg, Options{Extensions: []Extension{signIn}, ExternalSignInOnly: true})

	a := request(t, http.MethodPost, base+"/v1/auth/provider", "", "")
	var session service.Session
	if err := json.Unmarshal([]byte(a.body), &session); a.status != http.StatusOK || err != nil || session.Token == "" {
		t.Fatalf("the extension answered %d %q", a.status, a.body)
	}
	if session.User.HasPassword || session.ExpiresAt-time.Now().Unix() > int64((24*time.Hour).Seconds()) {
		t.Errorf("session = %+v, want no password and a day at most", session)
	}
	// The token is the console's session like any other.
	if a := request(t, http.MethodGet, base+"/v1/auth/me", session.Token, ""); a.status != http.StatusOK ||
		!strings.Contains(a.body, `"has_password":false`) {
		t.Errorf("GET /v1/auth/me answered %d %q", a.status, a.body)
	}

	// And the core's password routes are off, decided by the service. Each
	// request is one that would be done anywhere else: ana's own password,
	// the invitation's own code, ana's own session and current password. So
	// the refusal, and its words, can only be the option's.
	for _, call := range []struct{ path, token, body string }{
		{"/v1/auth/challenge", "", `{"email":"ana@example.com"}`},
		{"/v1/auth/login", "", fmt.Sprintf(`{"email":"ana@example.com","auth_key":%q}`, authtest.AuthKey)},
		{"/v1/auth/signup/open", "", fmt.Sprintf(`{"invite":%q,"email":"new@example.com"}`, code)},
		{"/v1/auth/signup", "", signUpBody(t, code)},
		{"/v1/auth/password/begin", ana, fmt.Sprintf(`{"current_auth_key":%q}`, authtest.AuthKey)},
		{"/v1/auth/stepup", ana, fmt.Sprintf(`{"auth_key":%q}`, authtest.AuthKey)},
	} {
		a := request(t, http.MethodPost, base+call.path, call.token, call.body)
		if a.status != http.StatusForbidden || !strings.Contains(a.body, `"code":"not_authorized"`) ||
			!strings.Contains(a.body, "another way") {
			t.Errorf("POST %s answered %d %q, want 403 not_authorized: people sign in another way", call.path, a.status, a.body)
		}
	}
}

func TestTheZeroOptionsLeavePasswordsOn(t *testing.T) {
	// What `serve` runs: a wrong password is a wrong password, as ever, and
	// the right one signs in.
	cfg, _, _ := seeded(t)
	base := runDaemon(t, cfg, Options{})
	wrong := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	a := request(t, http.MethodPost, base+"/v1/auth/login", "", fmt.Sprintf(`{"email":"nobody@example.com","auth_key":%q}`, wrong))
	if a.status != http.StatusUnauthorized || !strings.Contains(a.body, `"code":"unauthorized"`) {
		t.Errorf("POST /v1/auth/login answered %d %q, want 401 unauthorized", a.status, a.body)
	}
	a = request(t, http.MethodPost, base+"/v1/auth/login", "", fmt.Sprintf(`{"email":"ana@example.com","auth_key":%q}`, authtest.AuthKey))
	if a.status != http.StatusOK || !strings.Contains(a.body, `"has_password":true`) {
		t.Errorf("POST /v1/auth/login with her password answered %d %q, want her session", a.status, a.body)
	}
	// The upgrade's one password in clear left in the release after the
	// one that brought the key scheme: the daemon knows its routes no more,
	// and answers them as any path it does not serve.
	for _, path := range []string{"/v1/auth/upgrade/login", "/v1/auth/upgrade/enrol"} {
		a = request(t, http.MethodPost, base+path, "", fmt.Sprintf(`{"email":"ana@example.com","password":%q}`, authtest.Password))
		if a.status != http.StatusNotFound || !strings.Contains(a.body, `"code":"not_found"`) {
			t.Errorf("POST %s answered %d %q, want the API's 404", path, a.status, a.body)
		}
	}
}

func TestTheSaltAnAddressIsAnsweredOutlivesARestart(t *testing.T) {
	// The salt key is made once, sealed like a credential, and opened at
	// every start: an address is answered one salt, whether or not it has
	// an account, before a restart and after it.
	cfg, _, _ := seeded(t)
	challenge := func() string {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- Run(ctx, cfg, obs.NewLoggerTo(io.Discard, "error", "text"), Options{}) }()
		defer func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Run: %v", err)
			}
		}()
		base := "http://" + cfg.HTTPAddr
		deadline := time.Now().Add(10 * time.Second)
		for {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/v1/healthz", nil)
			if err != nil {
				t.Fatal(err)
			}
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatal("the daemon did not answer within 10s")
			}
			time.Sleep(20 * time.Millisecond)
		}
		a := request(t, http.MethodPost, base+"/v1/auth/challenge", "", `{"email":"nobody@example.com"}`)
		if a.status != http.StatusOK {
			t.Fatalf("the challenge answered %d %q", a.status, a.body)
		}
		return a.body
	}
	first := challenge()
	if again := challenge(); again != first || !strings.Contains(first, `"salt":"`) {
		t.Fatalf("the challenge answered %q, and %q after a restart", first, again)
	}
	db, err := store.Open(t.Context(), cfg.DatabasePath(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if v, err := db.Meta(t.Context(), store.MetaKDFSaltKey); err != nil || v == "" {
		t.Errorf("no salt key kept (%v)", err)
	}
}

func TestADaemonWherePeopleSignInOnlyThroughAnExtensionMakesNoSaltKey(t *testing.T) {
	cfg, _, _ := seeded(t)
	runDaemon(t, cfg, Options{ExternalSignInOnly: true})
	db, err := store.Open(t.Context(), cfg.DatabasePath(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if v, err := db.Meta(t.Context(), store.MetaKDFSaltKey); err != nil || v != "" {
		t.Errorf("a salt key was made (%v): nobody asks for a salt there", err)
	}
}

// signUpBody is a sign-up with an invitation's code and a fresh enrolment.
func signUpBody(t *testing.T, code string) string {
	t.Helper()
	in := authtest.Enrolment(t)
	enc := base64.RawURLEncoding.EncodeToString
	b, err := json.Marshal(map[string]any{
		"invite": code, "email": "new@example.com", "name": "New", "auth_key": in.AuthKey,
		"kdf":        map[string]any{"alg": "argon2id", "m": in.KDF.M, "t": in.KDF.T, "p": in.KDF.P},
		"public_key": enc(in.PublicKey), "password_wrap": enc(in.PasswordWrap), "recovery_wrap": enc(in.RecoveryWrap),
		"recovery_proof": in.RecoveryProof,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
