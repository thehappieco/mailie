package app

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/store"
)

// noPeople is the refusal of every password and invitation route on a server
// with no people.
const noPeople = "this server has no people"

// saltKeyKept reports whether the database at path holds a salt key.
func saltKeyKept(t *testing.T, path string) bool {
	t.Helper()
	db, err := store.Open(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	v, err := db.Meta(t.Context(), store.MetaKDFSaltKey)
	if err != nil {
		t.Fatal(err)
	}
	return v != ""
}

func TestADaemonWithNoPeopleRefusesPasswordsAndSessionsAndMakesNoSaltKey(t *testing.T) {
	// A database the open console's people left: Ana, signed in, and an
	// invitation, under the configuration serve runs by default. Nobody
	// signs in, signs up or is invited, her session reaches nothing, and the
	// key nobody asks a salt of is never made.
	cfg, ana, code := seeded(t)
	cfg.PasswordSignIn = false
	base, logs := runDaemonLogged(t, cfg, Options{})

	for _, call := range []struct{ path, body string }{
		{"/v1/auth/challenge", `{"email":"ana@example.com"}`},
		{"/v1/auth/login", fmt.Sprintf(`{"email":"ana@example.com","auth_key":%q}`, authtest.AuthKey)},
		{"/v1/auth/signup/open", fmt.Sprintf(`{"invite":%q,"email":"new@example.com"}`, code)},
		{"/v1/auth/signup", signUpBody(t, code)},
	} {
		a := request(t, http.MethodPost, base+call.path, "", call.body)
		if a.status != http.StatusForbidden || !strings.Contains(a.body, `"code":"not_authorized"`) ||
			!strings.Contains(a.body, noPeople) {
			t.Errorf("POST %s answered %d %q, want 403 not_authorized: %s", call.path, a.status, a.body, noPeople)
		}
	}
	nobody := request(t, http.MethodGet, base+"/v1/auth/me", strings.Repeat("A", len(ana)), "")
	for _, path := range []string{"/v1/auth/me", "/v1/workspaces", "/v1/accounts"} {
		if a := request(t, http.MethodGet, base+path, ana, ""); a.status != http.StatusUnauthorized || a.body != nobody.body {
			t.Errorf("GET %s with her session answered %d %q, want %d %q as for a token nobody issued", path,
				a.status, a.body, nobody.status, nobody.body)
		}
	}

	// Nothing about people at start, a console served or not: no first
	// invitation, no owner missing, and a key is the only way in.
	out := logs.String()
	for _, never := range []string{"nobody can sign in", "nobody administers", "/v1/users/invites", "console sessions"} {
		if strings.Contains(out, never) {
			t.Errorf("the start says %q:\n%s", never, out)
		}
	}
	if line := logLine(out, "no api keys exist yet"); !strings.Contains(line, "level=WARN") ||
		!strings.Contains(line, "nothing can call this daemon") || !strings.Contains(line, "apikey create --bootstrap") {
		t.Errorf("the start says %s", line)
	}
	if line := logLine(out, "msg=starting"); !strings.Contains(line, "password_sign_in=false") {
		t.Errorf("the start says %s", line)
	}
	if saltKeyKept(t, cfg.DatabasePath()) {
		t.Error("a salt key was made: nobody asks for a salt on a server with no people")
	}
}

func TestOnlyADaemonWhosePeopleSignInWithAPasswordMakesASaltKeyOrHintsAtAnInvitation(t *testing.T) {
	for _, c := range []struct {
		name      string
		passwords bool
		opts      Options
		people    bool
	}{
		{"no people", false, Options{}, false},
		{"people with a password", true, Options{}, true},
		{"an extension, no passwords", false, Options{ExternalSignInOnly: true}, false},
		{"an extension, passwords asked for", true, Options{ExternalSignInOnly: true}, false},
	} {
		// An empty database, with a console: nobody has been invited yet.
		cfg := localConfig(t)
		cfg.PasswordSignIn = c.passwords
		said, logs := startLines(t, cfg, c.opts, "nobody can sign in to the console yet")
		if hinted := len(said) == 1 && strings.Contains(said[0], "/v1/users/invites"); hinted != c.people {
			t.Errorf("%s: hinted at the first invitation %t, want %t:\n%s", c.name, hinted, c.people, logs)
		}
		if kept := saltKeyKept(t, cfg.DatabasePath()); kept != c.people {
			t.Errorf("%s: a salt key kept %t, want %t", c.name, kept, c.people)
		}
		consoleSessions := strings.Contains(logs, "only console sessions can call this daemon")
		if want := c.passwords || c.opts.ExternalSignInOnly; consoleSessions != want {
			t.Errorf("%s: says only console sessions call it %t, want %t:\n%s", c.name, consoleSessions, want, logs)
		}
	}
}
