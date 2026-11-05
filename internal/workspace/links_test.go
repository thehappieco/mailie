package workspace_test

import (
	"slices"
	"testing"

	"github.com/thehappieco/mailie/internal/workspace"
)

// A link at the provider into a team (migration 0026) ends with whatever
// ends its starter's right to link into it.

// seedLink writes a link at the provider userID started, into a team, or
// into their personal workspace when workspaceID is empty, waiting for its
// return.
func (f *fixture) seedLink(id, userID, workspaceID string) {
	f.t.Helper()
	var into any
	if workspaceID != "" {
		into = workspaceID
	}
	f.exec(`INSERT INTO oauth_links(id, state, owner_user_id, workspace_id, provider, oauth_client, pkce_verifier,
		redirect_uri, created_at, expires_at) VALUES (?, 'state-' || ?, ?, ?, 'gmail', 'web', 'verifier',
		'https://mail.example.test/oauth/return', 900, 9999999999)`, id, id, userID, into)
}

// links lists the links left, by id.
func (f *fixture) links() []string {
	f.t.Helper()
	return queryAll(f.t, f, `SELECT id FROM oauth_links ORDER BY id`)
}

func TestALinkIntoATeamEndsWithItsStartersPlaceThereAndWithTheTeamsClosing(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	ana, bea, cid, dan := f.person("ana@example.org"), f.person("bea@example.org"), f.person("cid@example.org"),
		f.person("dan@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{
		bea.ID: workspace.RoleAdmin, cid.ID: workspace.RoleAdmin, dan.ID: workspace.RoleAdmin,
	})
	sales := f.team("Sales", bea, nil)
	f.seedLink("lnk_ana_support", ana.ID, team.ID)
	f.seedLink("lnk_bea_own", bea.ID, "")
	f.seedLink("lnk_bea_sales", bea.ID, sales.ID)
	f.seedLink("lnk_bea_support", bea.ID, team.ID)
	f.seedLink("lnk_cid_support", cid.ID, team.ID)
	f.seedLink("lnk_dan_support", dan.ID, team.ID)

	// Bea made a member links nothing into the team any more; her links
	// elsewhere stay.
	if _, err := f.ws.SetMember(ctx, team.ID, bea.ID, workspace.MemberChange{Role: role(workspace.RoleMember)}, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.links(); !slices.Equal(got, []string{"lnk_ana_support", "lnk_bea_own", "lnk_bea_sales",
		"lnk_cid_support", "lnk_dan_support"}) {
		t.Errorf("after Bea's demotion: %v", got)
	}
	// Nor does Cid, her membership disabled, nor Dan once he leaves.
	if _, err := f.ws.SetMember(ctx, team.ID, cid.ID, workspace.MemberChange{Status: status(workspace.StatusDisabled)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.ws.RemoveMember(ctx, team.ID, dan.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.links(); !slices.Equal(got, []string{"lnk_ana_support", "lnk_bea_own", "lnk_bea_sales"}) {
		t.Errorf("after Cid's disable and Dan's leaving: %v", got)
	}
	// Nothing is linked into a closed team: every link into it ends.
	if _, _, err := f.ws.SetClosed(ctx, team.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := f.links(); !slices.Equal(got, []string{"lnk_bea_own", "lnk_bea_sales"}) {
		t.Errorf("once the team closed: %v", got)
	}
}

func TestALinkIntoATeamFromAnotherSourceEndsWhenTheDirectoryHoldsOrDemotesItsStarter(t *testing.T) {
	f := newFixture(t)
	ana, bea, _ := heldTeam(t, f)
	f.mirrorMember(mirroredTeam, bea.ID, workspace.RoleAdmin)
	f.seedLink("lnk_ana", ana.ID, mirroredTeam)
	f.seedLink("lnk_bea", bea.ID, mirroredTeam)

	// Bea alone reads its mailbox: her end is held, and her link ends.
	if res := f.end(mirroredTeam, bea.ID, true); !res.Held {
		t.Fatalf("Bea's end: %+v, want it held", res)
	}
	if got := f.links(); !slices.Equal(got, []string{"lnk_ana"}) {
		t.Errorf("after Bea's hold: %v", got)
	}
	// Ana made a member by the directory links nothing into it any more.
	f.mirrorMember(mirroredTeam, ana.ID, workspace.RoleMember)
	if got := f.links(); len(got) != 0 {
		t.Errorf("after Ana's demotion: %v", got)
	}
}
