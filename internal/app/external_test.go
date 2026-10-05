package app

import (
	"context"
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
	// done its provider's protocol, and answers as POST /v1/auth/login
	// answers the console.
	signIn := func(_ context.Context, r *Router, d Deps) error {
		r.HandleFunc("POST /v1/auth/provider", func(w http.ResponseWriter, req *http.Request) {
			session, err := d.Service.SignInExternal(req.Context(), service.ExternalSignIn{
				Issuer: "https://accounts.example.com", Subject: "subject-of-cy", Email: "cy@example.com",
				EmailVerified: true, Name: "Cy Lima", UserAgent: req.UserAgent(), TTL: 24 * time.Hour,
			})
			if err != nil {
				http.Error(w, service.MessageOf(err), http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(session)
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
		{"/v1/auth/login", "", fmt.Sprintf(`{"email":"ana@example.com","password":%q}`, authtest.Password)},
		{"/v1/auth/signup", "", fmt.Sprintf(`{"invite":%q,"email":"new@example.com","name":"New","password":"a long enough password"}`, code)},
		{"/v1/auth/password", ana, fmt.Sprintf(`{"current":%q,"next":"a brand new password"}`, authtest.Password)},
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
	a := request(t, http.MethodPost, base+"/v1/auth/login", "", `{"email":"nobody@example.com","password":"any password at all"}`)
	if a.status != http.StatusUnauthorized || !strings.Contains(a.body, `"code":"unauthorized"`) {
		t.Errorf("POST /v1/auth/login answered %d %q, want 401 unauthorized", a.status, a.body)
	}
	a = request(t, http.MethodPost, base+"/v1/auth/login", "", fmt.Sprintf(`{"email":"ana@example.com","password":%q}`, authtest.Password))
	if a.status != http.StatusOK || !strings.Contains(a.body, `"has_password":true`) {
		t.Errorf("POST /v1/auth/login with her password answered %d %q, want her session", a.status, a.body)
	}
}
