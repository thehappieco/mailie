package service_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Withholding teams (docs/workspaces.md, "Editions that withhold teams"): an
// edition that does not offer team workspaces yet uses nothing of a team,
// whatever its database holds, and loses nothing of one.

// withholdTeams restarts the fixture's daemon, over the same database, as an
// edition that withholds teams: what an offering edition made is still
// there, as in a database restored into it.
func (f *fixture) withholdTeams(t *testing.T) {
	t.Helper()
	f.opts.teamsWithheld = true
	f.svc, f.registry = f.build(t, f.opts.registry)
}

// wantWithheld fails the test unless err is the refusal of a withheld team.
func wantWithheld(t *testing.T, what string, err error) {
	t.Helper()
	if service.CodeOf(err) != service.CodeNotAuthorized ||
		!strings.Contains(service.MessageOf(err), "does not offer team workspaces") {
		t.Errorf("%s = %v, want the withheld team's not_authorized", what, err)
	}
}

func TestAnEditionThatWithholdsTeamsRefusesEveryUseOfATeam(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()
	// What an edition that offers teams made: a mailbox with a key that
	// Ana reads, a key of the team, Bea's read on the mailbox without a key,
	// and an invite into the team still waiting.
	orders, key := tm.keyedTeamMailbox(t, f, "orders@mail.example")
	tm.grant(t, tm.shared, workspace.Flags{Read: true})
	created, err := f.svc.CreateWorkspaceKey(ctx, tm.ana, tm.id, keyRequest("assistant", "write",
		service.KeyMailboxRequest{AccountID: tm.shared, Read: true, Act: true}))
	if err != nil {
		t.Fatal(err)
	}
	dan := f.person(t, "dan@example.com", auth.RoleMember)
	invite, _, err := f.users.CreateInvite(ctx, auth.NewInvite{Email: "dan@example.com", WorkspaceID: tm.id,
		WorkspaceRole: workspace.RoleMember, CreatedBy: tm.ana.UserID})
	if err != nil {
		t.Fatal(err)
	}
	supply := key.supply(t, f, tm.bea.UserID)
	first := newBrowserKey(t, "", 1)
	firstKey := service.FirstKeyRequest{PublicKey: b64(first.public), Namespace: first.namespace,
		Grants: []service.GrantToRequest{first.grantTo(t, f, tm.ana.UserID), first.grantTo(t, f, tm.bea.UserID)}}
	before := map[string]int{}
	counts := map[string]string{
		"mailboxes":   `SELECT count(*) FROM accounts WHERE workspace_id = ?`,
		"members":     `SELECT count(*) FROM workspace_members WHERE workspace_id = ? AND status = 'active'`,
		"grants":      `SELECT count(*) FROM mailbox_access g JOIN accounts a ON a.id = g.account_id WHERE a.workspace_id = ?`,
		"sealed":      `SELECT count(*) FROM mailbox_grants g JOIN accounts a ON a.id = g.account_id WHERE a.workspace_id = ?`,
		"keys":        `SELECT count(*) FROM api_keys WHERE workspace_id = ? AND revoked_at = 0`,
		"key access":  `SELECT count(*) FROM key_access x JOIN accounts a ON a.id = x.account_id WHERE a.workspace_id = ?`,
		"invites":     `SELECT count(*) FROM invites WHERE workspace_id = ? AND used_at = 0`,
		"consent off": `SELECT count(*) FROM accounts WHERE workspace_id = ? AND sync_enabled_at = 0`,
	}
	for what, query := range counts {
		before[what] = f.count(t, query, tm.id)
	}

	f.withholdTeams(t)
	k := f.authenticate(t, created.Key)
	outsider := f.person(t, "out@example.com", auth.RoleOwner)

	// Its mailboxes reach nobody: listed nowhere, and refused by name to
	// whoever would otherwise see them.
	for name, p := range map[string]service.Principal{
		"Ana, its owner": tm.ana, "Bea, who reads one": tm.bea, "Carol, an admin": tm.carol, "its key": k,
	} {
		if listed := ids(t, f, p); slices.Contains(listed, tm.shared) || slices.Contains(listed, orders) {
			t.Errorf("%s lists a withheld team's mailboxes: %v", name, listed)
		}
		_, err := f.svc.GetAccount(ctx, p, tm.shared)
		wantWithheld(t, name+" reading a withheld team's mailbox", err)
		_, err = f.svc.ListAccounts(ctx, p, tm.id)
		wantWithheld(t, name+" listing the team's mailboxes", err)
		_, err = f.svc.Storage(ctx, p, tm.id)
		wantWithheld(t, name+" counting what the team's mailboxes hold", err)
		_, err = f.svc.SearchMessages(ctx, p, service.SearchRequest{Workspace: tm.id})
		wantWithheld(t, name+" searching the team's mailboxes", err)
		_, err = f.svc.ListFolders(ctx, p, tm.shared)
		wantWithheld(t, name+" listing a mailbox's folders", err)
	}
	// Someone who would not see it is told it does not exist, as before.
	_, err = f.svc.GetAccount(ctx, outsider, tm.shared)
	wantCode(t, "someone outside the team reading its mailbox", err, service.CodeNotFound)
	_, err = f.svc.ListAccounts(ctx, outsider, tm.id)
	wantCode(t, "someone outside the team listing its mailboxes", err, service.CodeNotFound)
	_, err = f.svc.ListMembers(ctx, outsider, tm.id)
	wantCode(t, "someone outside the team listing its members", err, service.CodeNotFound)

	// Linking.
	req := f.passwordAccount(t, "new@mail.example")
	req.WorkspaceID = tm.id
	_, err = f.svc.AddAccount(ctx, tm.ana, keyed(tm.ana, req))
	wantWithheld(t, "Ana linking a mailbox into the team", err)
	_, err = f.svc.CreateWorkspace(ctx, tm.ana, service.CreateWorkspaceRequest{Name: "Another"})
	wantWithheld(t, "Ana creating a team", err)
	_, err = f.svc.CreateWorkspace(ctx, admin(), service.CreateWorkspaceRequest{Name: "Another", OwnerEmail: "ana@example.com"})
	wantWithheld(t, "the operator creating a team", err)

	// Its keys.
	_, err = f.svc.CreateWorkspaceKey(ctx, tm.ana, tm.id, keyRequest("another", "read"))
	wantWithheld(t, "Ana creating a key", err)
	_, err = f.svc.ListWorkspaceKeys(ctx, tm.ana, tm.id)
	wantWithheld(t, "Ana listing the keys", err)
	_, err = f.svc.SetKeyAccess(ctx, tm.ana, tm.id, created.Prefix, tm.shared, keyAccess(true, false, false))
	wantWithheld(t, "Ana changing what the key holds", err)
	wantWithheld(t, "Ana revoking the key", f.svc.RevokeWorkspaceKey(ctx, tm.ana, tm.id, created.Prefix))

	// Who holds what.
	_, err = f.svc.SetAccess(ctx, tm.ana, tm.shared, tm.bea.UserID, grantRequest(true, true, false, false))
	wantWithheld(t, "Ana giving Bea act", err)
	wantWithheld(t, "Ana taking Bea's read",
		f.svc.RevokeAccess(ctx, tm.ana, tm.shared, tm.bea.UserID, workspace.Flags{Read: true}))
	_, err = f.svc.SetAccess(ctx, admin(), tm.shared, tm.bea.UserID, grantRequest(true, false, false, true))
	wantWithheld(t, "the operator giving Bea manage", err)
	wantWithheld(t, "the operator taking Bea's read",
		f.svc.RevokeAccess(ctx, admin(), tm.shared, tm.bea.UserID, workspace.Flags{Read: true}))
	_, err = f.svc.AccessDirectory(ctx, tm.ana, tm.id)
	wantWithheld(t, "Ana listing who holds what", err)

	// Its consent to sync.
	on := true
	_, err = f.svc.SetMailboxSync(ctx, tm.ana, tm.shared, service.MailboxSyncRequest{Enabled: &on, Version: f.consent().Sync})
	wantWithheld(t, "Ana giving the team's consent to sync", err)

	// Mailbox keys.
	_, err = f.svc.SupplyKey(ctx, tm.ana, orders, tm.bea.UserID, supply)
	wantWithheld(t, "Ana supplying the key to Bea", err)
	_, err = f.svc.WriteFirstKey(ctx, tm.ana, tm.shared, firstKey)
	wantWithheld(t, "Ana writing the first key of the mailbox Bea reads", err)
	_, err = f.svc.MailboxKey(ctx, tm.ana, orders)
	wantWithheld(t, "Ana reading the mailbox key", err)

	// Its people, by anyone.
	_, err = f.svc.ListMembers(ctx, tm.ana, tm.id)
	wantWithheld(t, "Ana listing the members", err)
	_, err = f.svc.CreateTeamInvite(ctx, tm.ana, tm.id, service.TeamInviteRequest{Email: "eve@example.com"})
	wantWithheld(t, "Ana inviting someone", err)
	_, err = f.svc.AcceptInvite(ctx, dan, service.AcceptInviteRequest{Invite: invite})
	wantWithheld(t, "Dan accepting the invite he was sent", err)
	role := string(workspace.RoleAdmin)
	_, err = f.svc.SetMember(ctx, tm.ana, tm.id, tm.bea.UserID, service.MemberRequest{Role: &role})
	wantWithheld(t, "Ana making Bea an admin", err)
	_, err = f.svc.SetMember(ctx, admin(), tm.id, tm.bea.UserID, service.MemberRequest{Role: &role})
	wantWithheld(t, "the operator making Bea an admin", err)
	wantWithheld(t, "Ana removing Bea", f.svc.RemoveMember(ctx, tm.ana, tm.id, tm.bea.UserID))
	wantWithheld(t, "the operator removing Bea", f.svc.RemoveMember(ctx, admin(), tm.id, tm.bea.UserID))
	_, err = f.svc.RenameWorkspace(ctx, tm.ana, tm.id, service.RenameWorkspaceRequest{Name: "Help desk"})
	wantWithheld(t, "Ana renaming the team", err)
	_, err = f.svc.RenameWorkspace(ctx, admin(), tm.id, service.RenameWorkspaceRequest{Name: "Help desk"})
	wantWithheld(t, "the operator renaming the team", err)

	// The operator still reads who is in it and who holds what, as for a
	// closed team.
	if members, err := f.svc.ListMembers(ctx, admin(), tm.id); err != nil || len(members) != 3 {
		t.Errorf("the operator listing the members: %d (%v)", len(members), err)
	}
	if directory, err := f.svc.AccessDirectory(ctx, admin(), tm.id); err != nil || len(directory) != 2 {
		t.Errorf("the operator listing who holds what: %d (%v)", len(directory), err)
	}

	// Nothing of the team was deleted or changed.
	for what, query := range counts {
		if n := f.count(t, query, tm.id); n != before[what] {
			t.Errorf("the team's %s went from %d to %d", what, before[what], n)
		}
	}
	if stopped, reason := f.stopped(t, created.Prefix); stopped {
		t.Errorf("withholding teams stopped the team's key, for %q", reason)
	}
}

func TestWithholdingTeamsLeavesPersonalWorkspacesAsTheyWere(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	ctx := t.Context()
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
	own := f.mailbox(t, tm.ana, "ana@mail.example")
	personal := authtest.Personal(t, f.db, tm.ana.UserID)
	created, err := f.svc.CreateWorkspaceKey(ctx, tm.ana, personal, keyRequest("mine", "write",
		service.KeyMailboxRequest{AccountID: own, Read: true, Act: true}))
	if err != nil {
		t.Fatal(err)
	}
	before := accessOf(t, f, tm.ana, own).Access

	f.withholdTeams(t)

	// Her own mailbox, its key and her personal workspace are as they were.
	if got := ids(t, f, tm.ana); !slices.Equal(got, []string{own}) {
		t.Errorf("Ana lists %v, want her own mailbox alone", got)
	}
	if got := accessOf(t, f, tm.ana, own).Access; got != before {
		t.Errorf("Ana holds %+v on her own mailbox, want %+v as before", got, before)
	}
	if _, err := f.svc.ListAccounts(ctx, tm.ana, personal); err != nil {
		t.Errorf("Ana listing her personal workspace: %v", err)
	}
	if _, err := f.svc.Storage(ctx, tm.ana, personal); err != nil {
		t.Errorf("Ana counting her personal workspace: %v", err)
	}
	k := f.authenticate(t, created.Key)
	if got := ids(t, f, k); !slices.Equal(got, []string{own}) {
		t.Errorf("her personal key lists %v, want her mailbox", got)
	}
	if a := accessOf(t, f, k, own); !a.Access.Read || !a.Access.Act {
		t.Errorf("her personal key holds %+v, want what it held", a.Access)
	}
	if _, err := f.svc.ListAccounts(ctx, k, personal); err != nil {
		t.Errorf("her personal key listing its workspace: %v", err)
	}
	// She links, and keys, there as before.
	second := f.mailbox(t, tm.ana, "ana@other.example")
	if _, err := f.svc.CreateWorkspaceKey(ctx, tm.ana, personal, keyRequest("another", "read", reads(second))); err != nil {
		t.Errorf("Ana creating a key in her personal workspace: %v", err)
	}
	if keys, err := f.svc.ListWorkspaceKeys(ctx, tm.ana, personal); err != nil || len(keys) != 2 {
		t.Errorf("Ana listing her personal keys: %d (%v)", len(keys), err)
	}
	// The operator's mailboxes too.
	operator, err := f.svc.AddAccount(ctx, admin(), f.passwordAccount(t, "ops@mail.example"))
	if err != nil {
		t.Fatalf("the operator linking a mailbox: %v", err)
	}
	if _, err := f.svc.GetAccount(ctx, admin(), operator.Account.ID); err != nil {
		t.Errorf("the operator reading its mailbox: %v", err)
	}
}

func TestAWithheldTeamIsListedWithItsNotice(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	created, err := f.svc.CreateWorkspaceKey(t.Context(), tm.ana, tm.id, keyRequest("assistant", "read"))
	if err != nil {
		t.Fatal(err)
	}
	principals := func() map[string]service.Principal {
		return map[string]service.Principal{
			"Ana, its owner": tm.ana, "Bea, a member": tm.bea, "the operator": admin(),
			"a key of the team": f.authenticate(t, created.Key),
		}
	}
	// Where teams are offered, nothing is withheld.
	for name, p := range principals() {
		if listed := listedTeam(t, f, p, tm.id); listed.Withheld {
			t.Errorf("%s lists the team withheld where teams are offered", name)
		}
	}

	f.withholdTeams(t)

	for name, p := range principals() {
		listed := listedTeam(t, f, p, tm.id)
		if !listed.Withheld || listed.Name != "Support" || listed.Kind != "team" {
			t.Errorf("%s lists the team as %+v, want it withheld", name, listed)
		}
	}
	// Nothing else is withheld.
	for name, p := range map[string]service.Principal{"Ana": tm.ana, "the operator": admin()} {
		listed, err := f.svc.ListWorkspaces(t.Context(), p)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range listed {
			if w.Kind != "team" && w.Withheld {
				t.Errorf("%s lists a %s workspace withheld", name, w.Kind)
			}
		}
	}
}

func TestAnEditionThatWithholdsTeamsSyncsNoTeamMailboxAndKeepsItsIndex(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	// What an edition that offers teams made: a team mailbox Ana reads, with
	// the team's consent and an index, and her own mailbox, which she agreed
	// to sync.
	shared, _ := tm.keyedTeamMailbox(t, f, "support@mail.example")
	tm.syncOn(t, f, shared)
	f.seedMailbox(t, shared, "indexed where teams are offered")
	own := f.mailbox(t, tm.ana, "ana@mail.example")
	f.exec(t, `UPDATE users SET sync_consent_at = 1700000000 WHERE id = ?`, tm.ana.UserID)
	if !f.eligible(t, shared) || !f.eligible(t, own) {
		t.Fatal("the mailboxes do not sync where teams are offered")
	}

	// The same database, under an edition that withholds teams.
	f.withholdTeams(t)
	withheld := storetest.NewAtWith(t, f.db.Path(), store.Options{TeamsWithheld: true})
	if eligibleIn(t, withheld, shared) {
		t.Error("a team mailbox syncs where teams are withheld")
	}
	listed, err := withheld.SyncEligibleAccounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(listed, []string{own}) {
		t.Errorf("the engine would sync %v where teams are withheld, want Ana's own mailbox alone", listed)
	}
	if ok, err := withheld.SyncEligible(t.Context(), shared); err != nil || ok {
		t.Errorf("the engine would start the team mailbox: %v, %v", ok, err)
	}
	if !eligibleIn(t, withheld, own) {
		t.Error("Ana's own mailbox stopped syncing where teams are withheld")
	}
	// Nothing of it is lost: its index, its consent and its readers stay.
	if n := indexed(t, f, shared); n != 1 {
		t.Errorf("withheld, the team mailbox's index holds %d messages, want it kept", n)
	}
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE id = ? AND sync_enabled_at <> 0`, shared); n != 1 {
		t.Error("withholding teams took the team's consent to sync")
	}
	if !f.eligible(t, shared) {
		t.Error("an edition that offers teams would not sync the team mailbox again")
	}
}
