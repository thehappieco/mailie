package service_test

import (
	"database/sql"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// join makes a new person a member of the team with role, and returns their
// session.
func (tm supportTeam) join(t *testing.T, f *fixture, email string, role workspace.Role) service.Principal {
	t.Helper()
	p := f.person(t, email, auth.RoleMember)
	if err := f.db.Write(t.Context(), func(tx *sql.Tx) error {
		return tm.ws.AddMemberTx(t.Context(), tx, tm.id, p.UserID, role, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	return p
}

// grantRequest is a PUT of exactly these flags.
func grantRequest(read, act, send, manage bool) service.GrantRequest {
	return service.GrantRequest{Read: &read, Act: &act, Send: &send, Manage: &manage}
}

// held is what a person holds on a mailbox now, as the access directory
// lists it.
func held(t *testing.T, f *fixture, p service.Principal, workspaceID, accountID, userID string) service.Grant {
	t.Helper()
	dir, err := f.svc.AccessDirectory(t.Context(), p, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, mb := range dir {
		if mb.AccountID != accountID {
			continue
		}
		for _, g := range mb.Grants {
			if g.UserID == userID {
				return g
			}
		}
	}
	return service.Grant{}
}

func indexed(t *testing.T, f *fixture, accountID string) int {
	t.Helper()
	return f.count(t, `SELECT count(*) FROM messages WHERE account_id = ?`, accountID)
}

func TestAnAdminCannotGrantReadTheyDoNotHold(t *testing.T) {
	// Owners and admins administer who holds what, and read nobody's mail
	// by being one: access to mail passes only from someone who has it.
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	carol := tm.join(t, f, "carol@example.com", workspace.RoleAdmin)
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
	set := func(p service.Principal, userID string, req service.GrantRequest) error {
		t.Helper()
		_, err := f.svc.SetAccess(t.Context(), p, shared, userID, req)
		return err
	}

	wantCode(t, "Carol granting herself read", set(carol, carol.UserID, grantRequest(true, false, false, false)),
		service.CodeNotAuthorized)
	wantCode(t, "Carol granting Bea read", set(carol, tm.bea.UserID, grantRequest(true, false, false, false)),
		service.CodeNotAuthorized)
	wantCode(t, "Carol granting Bea send", set(carol, tm.bea.UserID, grantRequest(false, false, true, false)),
		service.CodeNotAuthorized)
	// Manage is administration, which she may hand out — to herself as
	// well — and it opens no mail.
	if err := set(carol, tm.bea.UserID, grantRequest(false, false, false, true)); err != nil {
		t.Fatalf("Carol granting Bea manage: %v", err)
	}
	if err := set(carol, carol.UserID, grantRequest(false, false, false, true)); err != nil {
		t.Fatalf("Carol granting herself manage: %v", err)
	}
	wantCode(t, "Carol, a manager now, granting herself read", set(carol, carol.UserID, grantRequest(true, false, false, true)),
		service.CodeNotAuthorized)
	_, err := f.svc.ListFolders(t.Context(), carol, shared)
	wantCode(t, "Carol listing folders with manage alone", err, service.CodeNotAuthorized)

	// Ana linked it and holds every flag: she passes read on, and Carol,
	// holding act then, passes act on too.
	if err := set(tm.ana, carol.UserID, grantRequest(true, true, false, true)); err != nil {
		t.Fatalf("Ana granting Carol read and act: %v", err)
	}
	if err := set(carol, tm.bea.UserID, grantRequest(true, true, false, true)); err != nil {
		t.Fatalf("Carol passing on read and act she holds: %v", err)
	}
	wantCode(t, "Carol passing on send she does not hold", set(carol, tm.bea.UserID, grantRequest(true, true, true, true)),
		service.CodeNotAuthorized)
	// Taking a flag away needs nothing held.
	if err := set(carol, tm.bea.UserID, grantRequest(true, false, false, true)); err != nil {
		t.Fatalf("Carol taking act away: %v", err)
	}
	if g := held(t, f, carol, tm.id, shared, tm.bea.UserID); !g.Read || g.Act || g.Send || !g.Manage || g.GrantedBy != carol.UserID {
		t.Errorf("Bea holds %+v", g)
	}

	// The operator holds nothing on anyone's mailbox: it grants manage,
	// keeping what is held, and nothing else.
	wantCode(t, "the operator granting send", set(admin(), tm.bea.UserID, grantRequest(true, false, true, true)),
		service.CodeNotAuthorized)
	if err := set(admin(), carol.UserID, grantRequest(true, true, false, true)); err != nil {
		t.Errorf("the operator keeping what Carol holds: %v", err)
	}

	// A member who manages nothing changes nothing, and Bob, who is not in
	// the team, cannot tell the mailbox exists.
	dan := tm.join(t, f, "dan@example.com", workspace.RoleMember)
	wantCode(t, "Dan, a member holding nothing", set(dan, dan.UserID, grantRequest(false, false, false, true)),
		service.CodeNotFound)
	bob := f.person(t, "bob@example.com", auth.RoleMember)
	wantCode(t, "Bob, outside the team", set(bob, tm.bea.UserID, grantRequest(false, false, false, true)),
		service.CodeNotFound)
	// A grant goes only to an active member of the mailbox's workspace.
	wantCode(t, "a grant to Bob", set(tm.ana, bob.UserID, grantRequest(true, false, false, false)), service.CodeNotFound)
}

func TestOwnersAndAdminsSeeTheAccessDirectoryButReadNoMailUntilGranted(t *testing.T) {
	m := newMailFixture(t)
	tm := newSupportTeam(t, m.fixture)
	carol := tm.join(t, m.fixture, "carol@example.com", workspace.RoleAdmin)
	dan := tm.join(t, m.fixture, "dan@example.com", workspace.RoleOwner)
	shared, box := m.ownedBoxIn(t, tm.ana, tm.id, "support@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("a", "Refund for order 4471"))
	m.index(t, shared, box)

	for name, p := range map[string]service.Principal{"Carol, an admin": carol, "Dan, an owner": dan} {
		if got := ids(t, m.fixture, p); slices.Contains(got, shared) {
			t.Errorf("%s lists the team's mailbox: %v", name, got)
		}
		_, err := m.svc.GetAccount(t.Context(), p, shared)
		wantCode(t, name+" reading the mailbox's card", err, service.CodeNotFound)
		_, err = m.svc.SearchMessages(t.Context(), p, service.SearchRequest{AccountID: shared})
		wantCode(t, name+" searching it", err, service.CodeNotFound)
		if page, err := m.svc.SearchMessages(t.Context(), p, service.SearchRequest{Query: "refund"}); err != nil || len(page.Messages) != 0 {
			t.Errorf("%s finds %d messages (%v)", name, len(page.Messages), err)
		}
		if st, err := m.svc.Storage(t.Context(), p, tm.id); err != nil || len(st.Mailboxes) != 0 {
			t.Errorf("%s is told what the index holds: %+v (%v)", name, st, err)
		}
		dir, err := m.svc.AccessDirectory(t.Context(), p, tm.id)
		if err != nil || len(dir) != 1 || dir[0].AccountID != shared || dir[0].LinkedBy != tm.ana.UserID {
			t.Errorf("%s's access directory: %+v (%v)", name, dir, err)
		}
	}
	// A member sees in the directory only what they manage.
	if dir, err := m.svc.AccessDirectory(t.Context(), tm.bea, tm.id); err != nil || len(dir) != 0 {
		t.Errorf("Bea, managing nothing, sees %+v (%v)", dir, err)
	}
}

func TestAnotherWorkspacesMailboxIsNotFoundNeverForbidden(t *testing.T) {
	f := newFixtureWith(t, fixtureOptions{publicURL: "http://localhost:5174"})
	tm := newSupportTeam(t, f)
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
	bob := f.person(t, "bob@example.com", auth.RoleMember)
	ctx := t.Context()

	notFound := func(who string, p service.Principal) {
		t.Helper()
		calls := map[string]func() error{
			"GetAccount":  func() error { _, err := f.svc.GetAccount(ctx, p, shared); return err },
			"ListFolders": func() error { _, err := f.svc.ListFolders(ctx, p, shared); return err },
			"SyncStatus":  func() error { _, err := f.svc.SyncStatus(ctx, p, shared); return err },
			"TriggerSync": func() error { return f.svc.TriggerSync(ctx, p, shared) },
			"StartOAuth":  func() error { _, err := f.svc.StartOAuth(ctx, p, shared, ""); return err },
			"RemoveAccount": func() error {
				return f.svc.RemoveAccount(ctx, p, shared)
			},
			"TakeOver": func() error { _, err := f.svc.TakeOver(ctx, p, shared); return err },
			"SetAccess": func() error {
				_, err := f.svc.SetAccess(ctx, p, shared, p.UserID, grantRequest(false, false, false, true))
				return err
			},
			"RevokeAccess": func() error { return f.svc.RevokeAccess(ctx, p, shared, tm.ana.UserID, workspace.Flags{}) },
			"MayFollow":    func() error { return f.svc.MayFollow(ctx, p, shared) },
			"Search": func() error {
				_, err := f.svc.SearchMessages(ctx, p, service.SearchRequest{AccountID: shared})
				return err
			},
			"Subscribe": func() error {
				st, err := f.svc.Subscribe(ctx, p, 0, service.EventFilter{AccountIDs: []string{shared}})
				if st != nil {
					st.Close()
				}
				return err
			},
			"WaitForNewMail": func() error {
				_, err := f.svc.WaitForNewMail(ctx, p, 0, time.Second, service.EventFilter{AccountIDs: []string{shared}})
				return err
			},
		}
		for name, call := range calls {
			if code := service.CodeOf(call()); code != service.CodeNotFound {
				t.Errorf("%s, %s: %q, want not_found", who, name, code)
			}
		}
	}
	notFound("Bob, outside the team", bob)
	notFound("Bea, a member holding nothing", tm.bea)

	// The team itself does not exist for Bob either.
	for name, call := range map[string]func() error{
		"ListMembers":     func() error { _, err := f.svc.ListMembers(ctx, bob, tm.id); return err },
		"AccessDirectory": func() error { _, err := f.svc.AccessDirectory(ctx, bob, tm.id); return err },
		"RenameWorkspace": func() error {
			_, err := f.svc.RenameWorkspace(ctx, bob, tm.id, service.RenameWorkspaceRequest{Name: "Mine"})
			return err
		},
		"ListTeamInvites": func() error { _, err := f.svc.ListTeamInvites(ctx, bob, tm.id); return err },
		"CreateTeamInvite": func() error {
			_, err := f.svc.CreateTeamInvite(ctx, bob, tm.id, service.TeamInviteRequest{Email: "eve@example.com"})
			return err
		},
		"ListAccounts": func() error { _, err := f.svc.ListAccounts(ctx, bob, tm.id); return err },
		"Storage":      func() error { _, err := f.svc.Storage(ctx, bob, tm.id); return err },
	} {
		if code := service.CodeOf(call()); code != service.CodeNotFound {
			t.Errorf("Bob, %s on the team: %q, want not_found", name, code)
		}
	}

	// Once Bea sees the mailbox — she may send from it — what she lacks is
	// a flag, and saying so tells her nothing she does not know.
	tm.grant(t, shared, workspace.Flags{Send: true})
	_, err := f.svc.ListFolders(ctx, tm.bea, shared)
	wantCode(t, "Bea listing folders with send alone", err, service.CodeNotAuthorized)
	_, err = f.svc.StartOAuth(ctx, tm.bea, shared, "")
	wantCode(t, "Bea re-authorising without manage", err, service.CodeNotAuthorized)
	wantCode(t, "Bea removing it without manage", f.svc.RemoveAccount(ctx, tm.bea, shared), service.CodeNotAuthorized)
}

func TestATakeOverKeepsTheIndexAndMovesTheConsent(t *testing.T) {
	m := newMailFixture(t)
	tm := newSupportTeam(t, m.fixture)
	carol := tm.join(t, m.fixture, "carol@example.com", workspace.RoleAdmin)
	shared, box := m.ownedBoxIn(t, tm.ana, tm.id, "support@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("a", "Refund for order 4471"))
	box.Deliver("INBOX", message("b", "Where is my parcel?"))
	m.index(t, shared, box)
	before := indexed(t, m.fixture, shared)
	ctx := t.Context()

	// Ana linked it, so she stays while it is linked.
	wantCode(t, "removing Ana, its linker",
		m.svc.RemoveMember(ctx, tm.ana, tm.id, tm.ana.UserID), service.CodeConflict)

	// Bea, a member, may not link here, so she may not take a link over.
	if _, err := m.svc.SetAccess(ctx, tm.ana, shared, tm.bea.UserID, grantRequest(true, true, true, true)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.svc.GrantSyncConsent(ctx, tm.bea, m.consent().Sync); err != nil {
		t.Fatal(err)
	}
	_, err := m.svc.TakeOver(ctx, tm.bea, shared)
	wantCode(t, "Bea, a member, taking over", err, service.CodeConflict)

	// Carol, an admin, needs every flag, and her own consent to sync.
	_, err = m.svc.TakeOver(ctx, carol, shared)
	wantCode(t, "Carol holding nothing", err, service.CodeNotFound)
	if _, err := m.svc.SetAccess(ctx, tm.ana, shared, carol.UserID, grantRequest(true, true, false, true)); err != nil {
		t.Fatal(err)
	}
	_, err = m.svc.TakeOver(ctx, carol, shared)
	wantCode(t, "Carol without send", err, service.CodeConflict)
	if _, err := m.svc.SetAccess(ctx, tm.ana, shared, carol.UserID, grantRequest(true, true, true, true)); err != nil {
		t.Fatal(err)
	}
	_, err = m.svc.TakeOver(ctx, carol, shared)
	wantCode(t, "Carol before agreeing to sync", err, service.CodeConflict)
	if _, err := m.svc.GrantSyncConsent(ctx, carol, m.consent().Sync); err != nil {
		t.Fatal(err)
	}
	taken, err := m.svc.TakeOver(ctx, carol, shared)
	if err != nil {
		t.Fatalf("TakeOver: %v", err)
	}
	if taken.LinkedBy != carol.UserID || !taken.Access.Read || !taken.Sync.Enabled {
		t.Errorf("after the take-over the account is %+v", taken)
	}
	if n := indexed(t, m.fixture, shared); n != before || n == 0 {
		t.Errorf("the index holds %d messages, %d before", n, before)
	}

	// Ana is an ordinary member of it now: she may leave once another owner
	// is there, and her withdrawal no longer touches the team's index.
	if _, err := m.svc.SetMember(ctx, tm.ana, tm.id, carol.UserID, service.MemberRequest{Role: ptr("owner")}); err != nil {
		t.Fatal(err)
	}
	if err := m.svc.RemoveMember(ctx, tm.ana, tm.id, tm.ana.UserID); err != nil {
		t.Fatalf("Ana leaving after the take-over: %v", err)
	}
	if _, err := m.svc.WithdrawSyncConsent(ctx, tm.ana); err != nil {
		t.Fatal(err)
	}
	if n := indexed(t, m.fixture, shared); n != before {
		t.Errorf("Ana's withdrawal deleted the team's index: %d messages, %d before", n, before)
	}
	if ok, err := m.db.SyncEligible(ctx, shared); err != nil || !ok {
		t.Errorf("the mailbox no longer syncs under Carol's consent: %v %v", ok, err)
	}
}

func TestWithdrawingSyncDeletesTheIndexOfEveryMailboxThePersonLinked(t *testing.T) {
	m := newMailFixture(t)
	tm := newSupportTeam(t, m.fixture)
	opts := providertest.FakeOptions{Caps: providertest.GmailCaps()}
	own, ownBox := m.ownedBoxIn(t, tm.ana, "", "ana@home.example", opts)
	shared, sharedBox := m.ownedBoxIn(t, tm.ana, tm.id, "support@mail.example", opts)
	bob := m.person(t, "bob@example.com", auth.RoleMember)
	bobs, bobBox := m.ownedBoxIn(t, bob, "", "bob@mail.example", opts)
	for id, box := range map[string]*providertest.FakeMailbox{own: ownBox, shared: sharedBox, bobs: bobBox} {
		box.Deliver("INBOX", message("a", "Hello"))
		m.index(t, id, box)
	}
	tm.grant(t, shared, workspace.Flags{Read: true})

	if _, err := m.svc.WithdrawSyncConsent(t.Context(), tm.ana); err != nil {
		t.Fatal(err)
	}
	// The team mailbox Bea reads synced under Ana's consent, which is gone.
	for _, id := range []string{own, shared} {
		if n := indexed(t, m.fixture, id); n != 0 {
			t.Errorf("%s still holds %d indexed messages", id, n)
		}
	}
	if n := indexed(t, m.fixture, bobs); n != 1 {
		t.Errorf("Bob's mailbox lost its index: %d", n)
	}
	page, err := m.svc.SearchMessages(t.Context(), tm.bea, service.SearchRequest{AccountID: shared})
	if err != nil || len(page.Messages) != 0 {
		t.Errorf("Bea's search of the team's mailbox: %d messages (%v)", len(page.Messages), err)
	}
}

func TestTheLastOwnerCannotLeaveOrBeDemotedThroughTheService(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	ctx := t.Context()
	wantCode(t, "Ana leaving", f.svc.RemoveMember(ctx, tm.ana, tm.id, tm.ana.UserID), service.CodeConflict)
	_, err := f.svc.SetMember(ctx, tm.ana, tm.id, tm.ana.UserID, service.MemberRequest{Role: ptr("admin")})
	wantCode(t, "Ana demoting herself", err, service.CodeConflict)
	_, err = f.svc.SetMember(ctx, tm.ana, tm.id, tm.ana.UserID, service.MemberRequest{Status: ptr("disabled")})
	wantCode(t, "Ana disabling herself", err, service.CodeConflict)
	_, err = f.svc.SetMember(ctx, admin(), tm.id, tm.ana.UserID, service.MemberRequest{Role: ptr("member")})
	wantCode(t, "the operator demoting her", err, service.CodeConflict)
	members, err := f.svc.ListMembers(ctx, tm.bea, tm.id)
	if err != nil || len(members) != 2 || !members[0].LastOwner || members[0].UserID != tm.ana.UserID {
		t.Fatalf("members = %+v (%v); the last owner is marked in advance", members, err)
	}

	if _, err := f.svc.SetMember(ctx, tm.ana, tm.id, tm.bea.UserID, service.MemberRequest{Role: ptr("owner")}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetMember(ctx, tm.ana, tm.id, tm.ana.UserID, service.MemberRequest{Role: ptr("member")}); err != nil {
		t.Fatalf("Ana stepping down once Bea owns the team: %v", err)
	}
	wantCode(t, "Bea leaving, the last owner now", f.svc.RemoveMember(ctx, tm.bea, tm.id, tm.bea.UserID), service.CodeConflict)
}

func TestAnAdminCannotChangeAnotherAdmin(t *testing.T) {
	f := newFixtureWith(t, fixtureOptions{publicURL: "http://localhost:5174"})
	tm := newSupportTeam(t, f)
	carol := tm.join(t, f, "carol@example.com", workspace.RoleAdmin)
	dan := tm.join(t, f, "dan@example.com", workspace.RoleAdmin)
	ctx := t.Context()
	set := func(p service.Principal, userID string, req service.MemberRequest) error {
		_, err := f.svc.SetMember(ctx, p, tm.id, userID, req)
		return err
	}

	wantCode(t, "Carol demoting Dan", set(carol, dan.UserID, service.MemberRequest{Role: ptr("member")}), service.CodeNotAuthorized)
	wantCode(t, "Carol disabling Dan", set(carol, dan.UserID, service.MemberRequest{Status: ptr("disabled")}), service.CodeNotAuthorized)
	wantCode(t, "Carol removing Dan", f.svc.RemoveMember(ctx, carol, tm.id, dan.UserID), service.CodeNotAuthorized)
	wantCode(t, "Carol removing Ana, an owner", f.svc.RemoveMember(ctx, carol, tm.id, tm.ana.UserID), service.CodeNotAuthorized)
	wantCode(t, "Carol making Bea an admin", set(carol, tm.bea.UserID, service.MemberRequest{Role: ptr("admin")}), service.CodeNotAuthorized)
	// Members are hers to administer.
	if err := set(carol, tm.bea.UserID, service.MemberRequest{Status: ptr("disabled")}); err != nil {
		t.Errorf("Carol disabling Bea: %v", err)
	}
	if err := set(carol, tm.bea.UserID, service.MemberRequest{Status: ptr("active")}); err != nil {
		t.Errorf("Carol enabling Bea again: %v", err)
	}

	// Invites likewise: members only, and she does not see the others.
	_, err := f.svc.CreateTeamInvite(ctx, carol, tm.id, service.TeamInviteRequest{Email: "eve@example.com", Role: "admin"})
	wantCode(t, "Carol inviting an admin", err, service.CodeNotAuthorized)
	mine, err := f.svc.CreateTeamInvite(ctx, carol, tm.id, service.TeamInviteRequest{Email: "eve@example.com"})
	if err != nil || mine.Role != "member" || mine.URL == "" {
		t.Fatalf("Carol inviting a member: %+v %v", mine, err)
	}
	owners, err := f.svc.CreateTeamInvite(ctx, tm.ana, tm.id, service.TeamInviteRequest{Email: "fay@example.com", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := f.svc.ListTeamInvites(ctx, carol, tm.id)
	if err != nil || len(listed) != 1 || listed[0].ID != mine.ID || listed[0].URL != "" {
		t.Errorf("Carol lists %+v (%v), want her member invite, without its link", listed, err)
	}
	wantCode(t, "Carol revoking Ana's admin invite", f.svc.RevokeTeamInvite(ctx, carol, tm.id, owners.ID), service.CodeNotFound)
	if all, err := f.svc.ListTeamInvites(ctx, tm.ana, tm.id); err != nil || len(all) != 2 {
		t.Errorf("Ana lists %+v (%v)", all, err)
	}
	if err := f.svc.RevokeTeamInvite(ctx, tm.ana, tm.id, owners.ID); err != nil {
		t.Errorf("Ana revoking her invite: %v", err)
	}

	// A member administers nothing, and may leave.
	_, err = f.svc.CreateTeamInvite(ctx, tm.bea, tm.id, service.TeamInviteRequest{Email: "gil@example.com"})
	wantCode(t, "Bea inviting", err, service.CodeNotAuthorized)
	_, err = f.svc.ListTeamInvites(ctx, tm.bea, tm.id)
	wantCode(t, "Bea listing invites", err, service.CodeNotAuthorized)
	_, err = f.svc.RenameWorkspace(ctx, tm.bea, tm.id, service.RenameWorkspaceRequest{Name: "Bea's"})
	wantCode(t, "Bea renaming the team", err, service.CodeNotAuthorized)
	if err := f.svc.RemoveMember(ctx, tm.bea, tm.id, tm.bea.UserID); err != nil {
		t.Errorf("Bea leaving: %v", err)
	}
}

// beaReadsTheTeamsMailbox is a mailbox Ana linked in the team, indexed with
// one message, which Bea may read; and a way to make Bea a key for a tool.
func beaReadsTheTeamsMailbox(t *testing.T) (*mailFixture, supportTeam, string, func(accounts ...string) string) {
	t.Helper()
	m := newMailFixture(t)
	tm := newSupportTeam(t, m.fixture)
	shared, box := m.ownedBoxIn(t, tm.ana, tm.id, "support@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("a", "Refund for order 4471"))
	m.index(t, shared, box)
	if _, err := m.svc.SetAccess(t.Context(), tm.ana, shared, tm.bea.UserID, grantRequest(true, false, false, false)); err != nil {
		t.Fatal(err)
	}
	key := func(accounts ...string) string {
		t.Helper()
		created, err := m.svc.CreateMyAPIKey(t.Context(), tm.bea, service.PersonalKeyRequest{
			Name: "assistant", Scope: "read", AccountIDs: accounts, TermsVersion: service.DefaultKeyTermsVersion,
		})
		if err != nil {
			t.Fatal(err)
		}
		return created.Key
	}
	return m, tm, shared, key
}

func TestAPersonKeyLosesAMailboxWhenItsPersonLosesTheGrant(t *testing.T) {
	m, tm, shared, key := beaReadsTheTeamsMailbox(t)
	ctx := t.Context()
	secret := key()
	p, err := m.svc.Authenticate(ctx, secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	if page, err := m.svc.SearchMessages(ctx, p, service.SearchRequest{AccountID: shared}); err != nil || len(page.Messages) != 1 {
		t.Fatalf("her key reads %d messages (%v)", len(page.Messages), err)
	}

	if err := m.svc.RevokeAccess(ctx, tm.ana, shared, tm.bea.UserID, workspace.Flags{}); err != nil {
		t.Fatal(err)
	}
	// The same principal, held: the rule runs on every call.
	_, err = m.svc.SearchMessages(ctx, p, service.SearchRequest{AccountID: shared})
	wantCode(t, "her key searching the mailbox she lost", err, service.CodeNotFound)
	if page, err := m.svc.SearchMessages(ctx, p, service.SearchRequest{Query: "refund"}); err != nil || len(page.Messages) != 0 {
		t.Errorf("her key still finds %d messages (%v)", len(page.Messages), err)
	}
	if got := ids(t, m.fixture, p); slices.Contains(got, shared) {
		t.Errorf("her key lists %v", got)
	}
	// And the key itself keeps working for the rest.
	if _, err := m.svc.Authenticate(ctx, secret, nil); err != nil {
		t.Errorf("her key stopped working: %v", err)
	}
}

func TestAKeyMadeForALostMailboxIsRevoked(t *testing.T) {
	m, tm, shared, key := beaReadsTheTeamsMailbox(t)
	ctx := t.Context()
	secret := key(shared)
	if _, err := m.svc.Authenticate(ctx, secret, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.svc.RevokeAccess(ctx, tm.ana, shared, tm.bea.UserID, workspace.Flags{Read: true}); err != nil {
		t.Fatal(err)
	}
	_, err := m.svc.Authenticate(ctx, secret, nil)
	wantCode(t, "the key made for that mailbox alone", err, service.CodeUnauthorized)
	// Nor does it wake up when she is granted read again.
	if _, err := m.svc.SetAccess(ctx, tm.ana, shared, tm.bea.UserID, grantRequest(true, false, false, false)); err != nil {
		t.Fatal(err)
	}
	_, err = m.svc.Authenticate(ctx, secret, nil)
	wantCode(t, "the key once read is back", err, service.CodeUnauthorized)
}

// teamSends is a mailbox Ana linked in the team, sending, with every flag
// hers and nothing granted to Bea yet.
func teamSends(t *testing.T) (*sendBox, supportTeam) {
	t.Helper()
	m := newMailFixture(t)
	tm := newSupportTeam(t, m.fixture)
	return m.sendingBoxIn(t, tm.ana, tm.id, "support@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()}), tm
}

func TestActingNeedsTheActorsConsentAndTheActFlag(t *testing.T) {
	m := newMailFixture(t)
	tm := newSupportTeam(t, m.fixture)
	shared, box := m.ownedBoxIn(t, tm.ana, tm.id, "support@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("a", "Refund for order 4471"))
	m.index(t, shared, box)
	ctx := t.Context()
	id := m.messageID(t, shared, "INBOX", 1)
	// Ana, who linked it, has allowed actions; that is her consent, not
	// anybody else's.
	if _, err := m.svc.GrantActionsConsent(ctx, tm.ana, m.consent().Actions); err != nil {
		t.Fatal(err)
	}
	mark := func(p service.Principal) error {
		_, err := m.svc.SetFlags(ctx, p, service.SetFlagsRequest{IDs: []int64{id}, Seen: yes()})
		return err
	}

	tm.grant(t, shared, workspace.Flags{Read: true})
	wantCode(t, "Bea with read alone", mark(tm.bea), service.CodeNotAuthorized)
	tm.grant(t, shared, workspace.Flags{Read: true, Act: true})
	wantCode(t, "Bea with act, before she allowed actions", mark(tm.bea), service.CodeConflict)
	if n := box.Opens(provider.RoleInteractive); n != 0 {
		t.Fatalf("refused actions opened %d connections", n)
	}
	if _, err := m.svc.GrantActionsConsent(ctx, tm.bea, m.consent().Actions); err != nil {
		t.Fatal(err)
	}
	if err := mark(tm.bea); err != nil {
		t.Fatalf("Bea with act and her consent: %v", err)
	}
	if err := mark(keyOf(tm.bea, auth.ScopeWrite)); err != nil {
		t.Errorf("her write key: %v", err)
	}
	wantCode(t, "her read key", mark(keyOf(tm.bea, auth.ScopeRead)), service.CodeNotAuthorized)
	// Losing act stops her, whoever else may act.
	tm.grant(t, shared, workspace.Flags{Read: true})
	wantCode(t, "Bea once act is gone", mark(tm.bea), service.CodeNotAuthorized)
	if err := mark(tm.ana); err != nil {
		t.Errorf("Ana: %v", err)
	}
}

func TestSendingNeedsTheSendersConsentAndTheSendFlag(t *testing.T) {
	b, tm := teamSends(t)
	ctx := t.Context()
	tm.grant(t, b.id, workspace.Flags{Read: true})
	_, err := b.send(t, tm.bea, "k1", b.compose("client@example.org"))
	wantCode(t, "Bea with read alone", err, service.CodeNotAuthorized)
	if a, err := b.m.svc.GetAccount(ctx, tm.bea, b.id); err != nil || a.Send.Available || a.Send.Reason != "not_granted" {
		t.Errorf("Bea's card says %+v (%v)", a.Send, err)
	}
	tm.grant(t, b.id, workspace.Flags{Read: true, Send: true})
	// Ana allowed sending; Bea has not.
	_, err = b.send(t, tm.bea, "k2", b.compose("client@example.org"))
	wantCode(t, "Bea with send, before she allowed sending", err, service.CodeConflict)
	if n := len(b.smtp.Messages()); n != 0 {
		t.Fatalf("%d messages left before anyone may send", n)
	}
	if _, err := b.m.svc.GrantSendConsent(ctx, tm.bea, b.m.consent().Send); err != nil {
		t.Fatal(err)
	}
	if _, err := b.send(t, tm.bea, "k3", b.compose("client@example.org")); err != nil {
		t.Fatalf("Bea with send and her consent: %v", err)
	}
	if n := len(b.smtp.Messages()); n != 1 {
		t.Errorf("%d messages reached the server, want 1", n)
	}
}

func TestAMessageFromASharedMailboxGoesOutUnderTheSendersName(t *testing.T) {
	b, tm := teamSends(t)
	ctx := t.Context()
	tm.grant(t, b.id, workspace.Flags{Read: true, Send: true})
	if _, err := b.m.svc.GrantSendConsent(ctx, tm.bea, b.m.consent().Send); err != nil {
		t.Fatal(err)
	}
	if _, err := b.m.svc.UpdateProfile(ctx, tm.ana, service.ProfileRequest{Name: "Ana Lima"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.m.svc.UpdateProfile(ctx, tm.bea, service.ProfileRequest{Name: "Bea Souza"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.send(t, tm.bea, "from-bea", b.compose("client@example.org")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.send(t, tm.ana, "from-ana", b.compose("client@example.org")); err != nil {
		t.Fatal(err)
	}
	sent := b.smtp.Messages()
	if len(sent) != 2 {
		t.Fatalf("%d messages reached the server", len(sent))
	}
	if from := header(t, sent[0].Raw, "From"); len(from) != 1 || from[0] != `"Bea Souza" <support@mail.example>` {
		t.Errorf("Bea's message: From = %q, want her name", from)
	}
	if from := header(t, sent[1].Raw, "From"); len(from) != 1 || from[0] != `"Ana Lima" <support@mail.example>` {
		t.Errorf("Ana's message: From = %q, want her name", from)
	}
	if a, err := b.m.svc.GetAccount(ctx, tm.bea, b.id); err != nil || a.Send.FromName != "Bea Souza" {
		t.Errorf("Bea's card shows from_name %q (%v)", a.Send.FromName, err)
	}
}

func TestASendKeyIsNeverReplayedToAnotherPerson(t *testing.T) {
	b, tm := teamSends(t)
	ctx := t.Context()
	tm.grant(t, b.id, workspace.Flags{Read: true, Send: true})
	if _, err := b.m.svc.GrantSendConsent(ctx, tm.bea, b.m.consent().Send); err != nil {
		t.Fatal(err)
	}
	const key = "the-same-key"
	if _, err := b.send(t, tm.ana, key, b.compose("client@example.org")); err != nil {
		t.Fatal(err)
	}
	// The same key and the same text from Bea: never Ana's send handed back.
	_, err := b.send(t, tm.bea, key, b.compose("client@example.org"))
	wantCode(t, "Bea reusing Ana's key", err, service.CodeConflict)
	_, err = b.m.svc.SendStatus(ctx, tm.bea, b.id, key)
	wantCode(t, "Bea reading Ana's send", err, service.CodeNotFound)
	if st, err := b.m.svc.SendStatus(ctx, tm.ana, b.id, key); err != nil || st.State != store.SendSent {
		t.Errorf("Ana reading her own send: %+v %v", st, err)
	}
	if n := len(b.smtp.Messages()); n != 1 {
		t.Errorf("%d messages reached the server, want 1", n)
	}
}

func TestAStreamFollowsAccessAsItChanges(t *testing.T) {
	// Bea's stream carries the team mailbox's events while she may read it,
	// says when she gains or loses it, and ends once everything it followed
	// is gone; the long poll leaves out a mailbox she lost.
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
	own := f.mailbox(t, tm.bea, "bea@mail.example")
	ctx := t.Context()

	all, err := f.svc.Subscribe(ctx, tm.bea, 0, service.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer all.Close()
	if changes, err := all.CheckAccess(ctx); err != nil || len(changes) != 0 {
		t.Fatalf("a new stream reports %+v %v", changes, err)
	}
	if _, err := f.svc.SetAccess(ctx, tm.ana, shared, tm.bea.UserID, grantRequest(true, false, false, false)); err != nil {
		t.Fatal(err)
	}
	if changes, err := all.CheckAccess(ctx); err != nil || len(changes) != 1 ||
		changes[0] != (service.AccessChange{AccountID: shared, Read: true}) {
		t.Fatalf("after the grant: %+v %v", changes, err)
	}
	only, err := f.svc.Subscribe(ctx, tm.bea, 0, service.EventFilter{AccountIDs: []string{shared}})
	if err != nil {
		t.Fatal(err)
	}
	defer only.Close()
	f.publish(t, newMail(t, shared, "inbox", "Refund"))
	for name, st := range map[string]*service.Stream{"every mailbox": all, "the team's": only} {
		select {
		case ev := <-st.Events():
			if ev.AccountID != shared {
				t.Errorf("%s: %+v", name, ev)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: no event", name)
		}
	}

	if err := f.svc.RevokeAccess(ctx, tm.ana, shared, tm.bea.UserID, workspace.Flags{Read: true}); err != nil {
		t.Fatal(err)
	}
	if changes, err := all.CheckAccess(ctx); err != nil || len(changes) != 1 ||
		changes[0] != (service.AccessChange{AccountID: shared, Read: false}) {
		t.Errorf("the stream of every mailbox, once read is gone: %+v %v", changes, err)
	}
	if _, err := only.CheckAccess(ctx); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("the stream of the team's mailbox alone goes on: %v", err)
	}
	f.publish(t, newMail(t, shared, "inbox", "Second refund"), newMail(t, own, "inbox", "Hello"))
	select {
	case ev := <-all.Events():
		if ev.AccountID != own {
			t.Errorf("after losing read the stream carries %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no event of her own mailbox")
	}
	// Resuming from an old cursor brings nothing of it back.
	res, err := f.svc.WaitForNewMail(ctx, tm.bea, 1, time.Second, service.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range res.Events {
		if ev.AccountID == shared {
			t.Errorf("the long poll hands back %+v", ev)
		}
	}
	_, err = f.svc.WaitForNewMail(ctx, tm.bea, 0, time.Second, service.EventFilter{AccountIDs: []string{shared}})
	wantCode(t, "a long poll naming the lost mailbox", err, service.CodeNotFound)
}

func TestAStreamOfEverythingOutlivesTheLossOfEveryMailboxItRead(t *testing.T) {
	// Bea reads one mailbox, the team's, and nothing else. Losing it leaves
	// her stream of everything with nothing to carry, but a stream opened
	// now would open just the same and stay open: hers stays too, and the
	// mailbox granted back appears on it. Only leaving the workspace a
	// stream was narrowed to ends that stream for good.
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
	tm.grant(t, shared, workspace.Flags{Read: true})
	ctx := t.Context()
	open := func(filter service.EventFilter) *service.Stream {
		t.Helper()
		st, err := f.svc.Subscribe(ctx, tm.bea, 0, filter)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(st.Close)
		return st
	}
	all, team := open(service.EventFilter{}), open(service.EventFilter{Workspace: tm.id})
	expect := func(what string, st *service.Stream, read bool) {
		t.Helper()
		changes, err := st.CheckAccess(ctx)
		if err != nil || len(changes) != 1 || changes[0] != (service.AccessChange{AccountID: shared, Read: read}) {
			t.Errorf("%s: %+v %v, want the mailbox's read %v", what, changes, err, read)
		}
	}

	if err := f.svc.RevokeAccess(ctx, tm.ana, shared, tm.bea.UserID, workspace.Flags{}); err != nil {
		t.Fatal(err)
	}
	expect("the stream of everything, once its only mailbox is gone", all, false)
	expect("the stream of the team, once its only mailbox is gone", team, false)
	if _, err := f.svc.SetAccess(ctx, tm.ana, shared, tm.bea.UserID, grantRequest(true, false, false, false)); err != nil {
		t.Fatal(err)
	}
	expect("the stream of everything, once read is back", all, true)
	expect("the stream of the team, once read is back", team, true)
	f.publish(t, newMail(t, shared, "inbox", "Refund"))
	select {
	case ev := <-all.Events():
		if ev.AccountID != shared {
			t.Errorf("the stream carries %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the mailbox granted back does not reach the stream")
	}

	if err := f.svc.RemoveMember(ctx, tm.ana, tm.id, tm.bea.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := team.CheckAccess(ctx); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("the stream of a team Bea left goes on: %v", err)
	}
	expect("the stream of everything, once she left the team", all, false)
}

func TestAPlatformSourcedServerChangesNoWorkspaceLocally(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	svc := service.New(service.Deps{
		Accounts: f.registry, Keys: f.keys, Users: f.users.WithWorkspaceSource(workspace.Platform()), Store: f.db,
		Workspaces: workspace.NewRepository(f.db, workspace.Platform()), PublicURL: "http://localhost:5174",
	})
	ctx := t.Context()
	_, err := svc.CreateWorkspace(ctx, tm.ana, service.CreateWorkspaceRequest{Name: "Sales"})
	wantCode(t, "creating a team", err, service.CodeConflict)
	_, err = svc.RenameWorkspace(ctx, tm.ana, tm.id, service.RenameWorkspaceRequest{Name: "Help"})
	wantCode(t, "renaming one", err, service.CodeConflict)
	_, err = svc.SetMember(ctx, tm.ana, tm.id, tm.bea.UserID, service.MemberRequest{Role: ptr("admin")})
	wantCode(t, "changing a member", err, service.CodeConflict)
	_, err = svc.CreateTeamInvite(ctx, tm.ana, tm.id, service.TeamInviteRequest{Email: "eve@example.com"})
	wantCode(t, "inviting", err, service.CodeConflict)
	// What stays Mailie's whatever the source: grants on its mailboxes.
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
	if _, err := svc.SetAccess(ctx, tm.ana, shared, tm.bea.UserID, grantRequest(true, false, false, false)); err != nil {
		t.Errorf("granting: %v", err)
	}
}

func TestTheOperatorAdministersEveryTeamAndReadsNoMail(t *testing.T) {
	f := newFixtureWith(t, fixtureOptions{publicURL: "http://localhost:5174"})
	tm := newSupportTeam(t, f)
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
	ctx := t.Context()
	op := admin()

	all, err := f.svc.ListWorkspaces(ctx, op)
	if err != nil || len(all) != 4 || all[0].ID != workspace.OperatorID || all[0].Members == nil {
		t.Fatalf("the operator lists %+v (%v): its own, two personal and the team", all, err)
	}
	created, err := f.svc.CreateWorkspace(ctx, op, service.CreateWorkspaceRequest{Name: "Sales", OwnerEmail: "bea@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if members, err := f.svc.ListMembers(ctx, op, created.ID); err != nil || len(members) != 1 ||
		members[0].UserID != tm.bea.UserID || members[0].Role != "owner" {
		t.Errorf("the new team's members: %+v (%v)", members, err)
	}
	if _, err := f.svc.CreateTeamInvite(ctx, op, tm.id, service.TeamInviteRequest{Email: "eve@example.com", Role: "owner"}); err != nil {
		t.Errorf("the operator inviting an owner: %v", err)
	}
	if _, err := f.svc.SetMember(ctx, op, tm.id, tm.bea.UserID, service.MemberRequest{Role: ptr("admin")}); err != nil {
		t.Errorf("the operator changing a role: %v", err)
	}
	// None of it reads anybody's mailbox.
	_, err = f.svc.GetAccount(ctx, op, shared)
	wantCode(t, "the operator reading the team's mailbox", err, service.CodeNotFound)

	// A person's own workspaces, and nothing of the operator's; a key of
	// theirs lists the same, and administers none of them.
	mine, err := f.svc.ListWorkspaces(ctx, tm.bea)
	if err != nil || len(mine) != 3 || mine[0].Kind != "personal" {
		t.Errorf("Bea lists %+v (%v): personal first, then her two teams", mine, err)
	}
	if listed, err := f.svc.ListWorkspaces(ctx, keyOf(tm.bea, auth.ScopeRead)); err != nil || len(listed) != 3 {
		t.Errorf("her key lists %+v (%v)", listed, err)
	}
	_, err = f.svc.ListMembers(ctx, keyOf(tm.bea, auth.ScopeAdmin), tm.id)
	wantCode(t, "her key administering", err, service.CodeNotAuthorized)
	_, err = f.svc.ListMembers(ctx, reader(), tm.id)
	wantCode(t, "a read instance key administering", err, service.CodeNotAuthorized)
	if listed, err := f.svc.ListWorkspaces(ctx, reader()); err != nil || len(listed) != 1 || listed[0].ID != workspace.OperatorID {
		t.Errorf("an instance key lists %+v (%v)", listed, err)
	}
}

// ptr is a pointer to a request field's value.
func ptr(s string) *string { return &s }

func TestAMemberCannotMintAnAccountThroughATeamInvite(t *testing.T) {
	// Mallory, a member of the server, creates a team of her own and
	// invites Alice into it, an address with no account, for which the
	// operator has an owner invite waiting. Signing up with Mallory's link
	// would give Alice's address an account with Mallory's password.
	f := newFixtureWith(t, fixtureOptions{publicURL: "http://localhost:5174"})
	ctx := t.Context()
	mallory := f.person(t, "mallory@example.com", auth.RoleMember)
	codeOf := func(link string) string {
		t.Helper()
		_, fragment, _ := strings.Cut(link, "#")
		values, err := url.ParseQuery(fragment)
		if err != nil || values.Get("invite") == "" {
			t.Fatalf("invite link %q: %v", link, err)
		}
		return values.Get("invite")
	}
	signUp := func(code string) (service.Session, error) {
		return f.svc.SignUp(ctx, service.SignUpRequest{Invite: code, Email: "alice@example.com", Name: "Alice",
			Password: "long enough password"}, "test")
	}

	owner, err := f.svc.CreateInvite(ctx, admin(), service.InviteRequest{Email: "alice@example.com", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	team, err := f.svc.CreateWorkspace(ctx, mallory, service.CreateWorkspaceRequest{Name: "Squat"})
	if err != nil {
		t.Fatal(err)
	}
	squat, err := f.svc.CreateTeamInvite(ctx, mallory, team.ID, service.TeamInviteRequest{Email: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = signUp(codeOf(squat.URL))
	wantCode(t, "signing up with a member's team invite", err, service.CodeNotAuthorized)
	if n := f.count(t, `SELECT count(*) FROM users WHERE email = 'alice@example.com'`); n != 0 {
		t.Fatal("an account was created through a member's team invite")
	}

	// The operator's invite is still there, and makes the owner it named.
	session, err := signUp(codeOf(owner.URL))
	if err != nil {
		t.Fatalf("the operator's invite after the refusal: %v", err)
	}
	if session.User.Role != "owner" {
		t.Errorf("Alice signed up as %q, want owner", session.User.Role)
	}
}

func TestClosingTheAccountOfSomeoneATeamDependsOnNeedsForce(t *testing.T) {
	// Ana is the team's only owner, and its mailbox, which Bea reads,
	// syncs under her consent.
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
	tm.grant(t, shared, workspace.Flags{Read: true})
	f.person(t, "keeper@example.com", auth.RoleOwner) // so the server keeps an owner of its own
	ctx := t.Context()
	req := service.CloseUserRequest{Email: "ana@example.com"}

	_, err := f.svc.DisableUser(ctx, admin(), req)
	wantCode(t, "disabling her", err, service.CodeConflict)
	if msg := service.MessageOf(err); !strings.Contains(msg, tm.id) || !strings.Contains(msg, shared) {
		t.Errorf("the refusal does not name the team and the mailbox: %q", msg)
	}
	_, err = f.svc.DeleteUser(ctx, admin(), req)
	wantCode(t, "deleting her", err, service.CodeConflict)
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE id = ?`, shared); n != 1 {
		t.Fatal("a refused deletion removed the team's mailbox")
	}

	// Insisting removes what synced under her consent, and leaves the team
	// to its other members, without an owner until the operator names one.
	req.Force = true
	deleted, err := f.svc.DeleteUser(ctx, admin(), req)
	if err != nil {
		t.Fatalf("deleting her with force: %v", err)
	}
	if deleted.AccountsRemoved != 1 || deleted.TeamsDeleted != 0 {
		t.Errorf("deleted = %+v", deleted)
	}
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE id = ?`, shared); n != 0 {
		t.Error("the mailbox that synced under her consent survived her")
	}
	members, err := f.svc.ListMembers(ctx, admin(), tm.id)
	if err != nil || len(members) != 1 || members[0].UserID != tm.bea.UserID {
		t.Errorf("the team's members: %+v (%v)", members, err)
	}
	if _, err := f.svc.SetMember(ctx, admin(), tm.id, tm.bea.UserID, service.MemberRequest{Role: ptr("owner")}); err != nil {
		t.Errorf("the operator naming a new owner: %v", err)
	}
}
