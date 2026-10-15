package app

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

func TestThePlatformSourceCreatesNoPersonalWorkspaceAndRefusesEveryChange(t *testing.T) {
	// A database with Ana, signed in and in a team, and Bea, who has no
	// personal workspace; then a daemon whose workspaces a directory's
	// documents send.
	cfg := localConfig(t)
	db, err := store.Open(t.Context(), cfg.DatabasePath(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleOwner)
	bea := authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	bare := authtest.Personal(t, db, bea.ID)
	for _, q := range []string{`DELETE FROM workspace_members WHERE workspace_id = ?`, `DELETE FROM workspaces WHERE id = ?`} {
		if _, err := db.Writer().ExecContext(t.Context(), q, bare); err != nil {
			t.Fatal(err)
		}
	}
	users := auth.NewUsers(db)
	session := authtest.SignIn(t, users, "ana@example.com")
	team, err := workspace.NewRepository(db, nil).CreateTeam(t.Context(), "Support", ana.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := users.CreateInvite(t.Context(), auth.NewInvite{Email: "new@example.com", Role: auth.RoleMember, CreatedBy: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	d := startDaemon(t, cfg, Options{WorkspaceSource: workspace.Platform()})
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/workspaces", `{"name":"Another"}`},
		{http.MethodPatch, "/v1/workspaces/" + team.ID, `{"name":"Renamed"}`},
		{http.MethodPost, "/v1/workspaces/" + team.ID + "/invites", `{"email":"cy@example.com","role":"member"}`},
		{http.MethodDelete, "/v1/workspaces/" + team.ID + "/members/" + ana.ID, ``},
	} {
		if a := request(t, c.method, d.base+c.path, session, c.body); a.status != http.StatusConflict ||
			!strings.Contains(a.body, `"code":"conflict"`) {
			t.Errorf("%s %s answered %d %s, want the conflict of workspaces managed elsewhere", c.method, c.path, a.status, a.body)
		}
	}
	// A person signing up gets no workspace from the source: a directory's
	// document sends theirs.
	a := request(t, http.MethodPost, d.base+"/v1/auth/signup/open", "", fmt.Sprintf(`{"invite":%q,"email":"new@example.com"}`, code))
	var opened struct {
		SealID string `json:"seal_id"`
	}
	if err := json.Unmarshal([]byte(a.body), &opened); err != nil || a.status != http.StatusOK {
		t.Fatalf("opening the invitation: %d %s (%v)", a.status, a.body, err)
	}
	in := authtest.Enrolment(t)
	enc := base64.RawURLEncoding.EncodeToString
	body, err := json.Marshal(map[string]any{
		"invite": code, "email": "new@example.com", "name": "New", "auth_key": in.AuthKey, "seal_id": opened.SealID,
		"kdf":        map[string]any{"alg": "argon2id", "m": in.KDF.M, "t": in.KDF.T, "p": in.KDF.P},
		"public_key": enc(in.PublicKey), "password_wrap": enc(in.PasswordWrap), "recovery_wrap": enc(in.RecoveryWrap),
		"recovery_proof": in.RecoveryProof,
	})
	if err != nil {
		t.Fatal(err)
	}
	if a := request(t, http.MethodPost, d.base+"/v1/auth/signup", "", string(body)); a.status != http.StatusCreated {
		t.Fatalf("signing up: %d %s", a.status, a.body)
	}
	d.stop()
	if err := d.wait(); err != nil {
		t.Fatal(err)
	}

	db, err = store.Open(t.Context(), cfg.DatabasePath(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var personal int
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT count(*) FROM workspaces w JOIN users u ON u.id = w.person_id
		WHERE u.email IN ('bea@example.com', 'new@example.com')`).Scan(&personal); err != nil {
		t.Fatal(err)
	}
	if personal != 0 {
		t.Errorf("%d personal workspaces were made under a source whose directory sends them", personal)
	}
	var name string
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT name FROM workspaces WHERE id = ?`, team.ID).Scan(&name); err != nil ||
		name != "Support" {
		t.Errorf("the team is named %q (%v)", name, err)
	}
}
