package app

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
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
