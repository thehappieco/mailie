package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

func TestABinaryThatEmbedsTheDaemonWithholdsTeamsThroughItsOptions(t *testing.T) {
	// A database an edition that offers teams made: Ana owns a team.
	cfg := localConfig(t)
	db, err := store.Open(t.Context(), cfg.DatabasePath(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	session := authtest.SignIn(t, auth.NewUsers(db), "ana@example.com")
	team, err := workspace.NewRepository(db, nil).CreateTeam(t.Context(), "Support", ana.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	base := runDaemon(t, cfg, Options{TeamsWithheld: true})

	a := request(t, http.MethodGet, base+"/v1/workspaces", session, "")
	var listed []struct {
		ID       string `json:"id"`
		Kind     string `json:"kind"`
		Withheld bool   `json:"withheld"`
	}
	if err := json.Unmarshal([]byte(a.body), &listed); err != nil || a.status != http.StatusOK {
		t.Fatalf("listing workspaces: %d %s (%v)", a.status, a.body, err)
	}
	for _, w := range listed {
		if w.Withheld != (w.ID == team.ID) {
			t.Errorf("the %s workspace %s is listed with withheld %v", w.Kind, w.ID, w.Withheld)
		}
	}
	for _, path := range []string{"/v1/workspaces/" + team.ID + "/members", "/v1/accounts?workspace=" + team.ID} {
		if a := request(t, http.MethodGet, base+path, session, ""); a.status != http.StatusForbidden {
			t.Errorf("GET %s answered %d %s, want the withheld team's 403", path, a.status, a.body)
		}
	}
	if a := request(t, http.MethodPost, base+"/v1/workspaces", session, `{"name":"Another"}`); a.status != http.StatusForbidden {
		t.Errorf("creating a team answered %d %s, want 403", a.status, a.body)
	}
}

func TestAnEditionThatWithholdsTeamsNeverStartsATeamMailboxsSync(t *testing.T) {
	// A database an edition that offers teams made: a team mailbox Ana reads
	// with the team's consent, and her own mailbox, which she agreed to sync.
	cfg := localConfig(t)
	db, err := store.Open(t.Context(), cfg.DatabasePath(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	team, err := workspace.NewRepository(db, nil).CreateTeam(t.Context(), "Support", ana.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := secrets.NewKeyring(cfg.Credentials.ActiveKeyID, cfg.Credentials.Keys)
	if err != nil {
		t.Fatal(err)
	}
	repo := account.NewRepository(db, keyring)
	const shared, own = "acc_00000000000000a1", "acc_00000000000000b2"
	for _, a := range []account.Account{
		{ID: shared, WorkspaceID: team.ID, Email: "support@mail.example",
			SyncConsent: store.MailboxConsent{At: 1700000000, By: ana.ID, Version: "sync-1"}},
		{ID: own, Email: "ana@mail.example"},
	} {
		a.Provider, a.AuthKind, a.State = provider.KindIMAP, "password", account.StateActive
		a.IMAPHost, a.IMAPPort, a.SMTPHost, a.SMTPPort, a.SMTPTLS, a.LoginUser = "127.0.0.1", 1, "127.0.0.1", 1, "implicit", a.Email
		if _, err := repo.Create(t.Context(), a, ana.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Writer().ExecContext(t.Context(), `UPDATE users SET sync_consent_at = 1700000000 WHERE id = ?`, ana.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{shared, own} {
		if ok, err := db.SyncEligible(t.Context(), id); err != nil || !ok {
			t.Fatalf("%s does not sync where teams are offered: %v, %v", id, ok, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	_, logs := runDaemonLogged(t, cfg, Options{TeamsWithheld: true})

	// The engine starts every mailbox it may sync in one pass, oldest first
	// and the team's before hers: once hers has started, the team's would
	// have.
	started := func(id string) bool {
		for line := range strings.Lines(logs.String()) {
			if strings.Contains(line, `msg="account sync started"`) && strings.Contains(line, " account="+id) {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(10 * time.Second)
	for !started(own) {
		if time.Now().After(deadline) {
			t.Fatalf("the engine did not start Ana's own mailbox within 10s:\n%s", logs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if started(shared) {
		t.Errorf("an edition that withholds teams started a team mailbox's sync:\n%s", logs)
	}
}
