package service_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
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

func TestOnlyAnOwnerOrAdminWhoReadsPassesRead(t *testing.T) {
	// Owners and admins administer who holds what, and read nobody's mail
	// by being one: read passes only from one of them who reads the mailbox
	// now. A member who reads it does not pass it on.
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	carol := tm.join(t, f, "carol@example.com", workspace.RoleAdmin)
	dan := tm.join(t, f, "dan@example.com", workspace.RoleMember)
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
	set := func(p service.Principal, userID string, req service.GrantRequest) error {
		t.Helper()
		_, err := f.svc.SetAccess(t.Context(), p, shared, userID, req)
		return err
	}

	wantCode(t, "Carol, reading nothing, granting herself read", set(carol, carol.UserID, grantRequest(true, false, false, false)),
		service.CodeNotAuthorized)
	wantCode(t, "Carol granting Bea read", set(carol, tm.bea.UserID, grantRequest(true, false, false, false)),
		service.CodeNotAuthorized)
	_, err := f.svc.ListFolders(t.Context(), carol, shared)
	wantCode(t, "Carol listing folders by her role", err, service.CodeNotAuthorized)

	// Ana linked it and reads it: she passes read on, to Carol and to Bea,
	// and Carol, reading it now, passes it on too.
	if err := set(tm.ana, tm.bea.UserID, grantRequest(true, false, false, false)); err != nil {
		t.Fatalf("Ana granting Bea read: %v", err)
	}
	if err := set(tm.ana, carol.UserID, grantRequest(true, false, false, false)); err != nil {
		t.Fatalf("Ana granting Carol read: %v", err)
	}
	if err := set(carol, dan.UserID, grantRequest(true, false, false, false)); err != nil {
		t.Fatalf("Carol passing on read she holds: %v", err)
	}
	if g := held(t, f, carol, tm.id, shared, dan.UserID); !g.Read || g.GrantedBy != carol.UserID {
		t.Errorf("Dan holds %+v", g)
	}
	// Bea reads it and is a member: she passes nothing on.
	wantCode(t, "Bea, a member who reads it, granting read", set(tm.bea, tm.bea.UserID, grantRequest(true, true, false, false)),
		service.CodeNotAuthorized)

	// The operator holds nothing on anyone's mailbox: it grants manage to a
	// member, keeping what is held, and nothing else.
	eve := tm.join(t, f, "eve@example.com", workspace.RoleMember)
	wantCode(t, "the operator granting read", set(admin(), eve.UserID, grantRequest(true, false, false, true)),
		service.CodeNotAuthorized)
	if err := set(admin(), dan.UserID, grantRequest(true, false, false, true)); err != nil {
		t.Errorf("the operator granting Dan manage: %v", err)
	}

	// Nobody outside the team, and no member holding nothing, can tell the
	// mailbox exists; a grant goes only to an active member of its
	// workspace.
	wantCode(t, "Eve, a member holding nothing", set(eve, eve.UserID, grantRequest(false, false, true, false)),
		service.CodeNotFound)
	bob := f.person(t, "bob@example.com", auth.RoleMember)
	wantCode(t, "Bob, outside the team", set(bob, tm.bea.UserID, grantRequest(false, false, true, false)),
		service.CodeNotFound)
	wantCode(t, "a grant to Bob", set(tm.ana, bob.UserID, grantRequest(true, false, false, false)), service.CodeNotFound)
}

func TestAnAdminTurnsOnSendForAnyoneAndActForAReader(t *testing.T) {
	// Act and send are an owner's or an admin's to give, whatever they hold
	// themselves: act to someone who reads after the change, send to any
	// active member, themselves included. Manage is stored for members
	// only; owners and admins hold it by their role.
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

	if err := set(carol, tm.bea.UserID, grantRequest(false, false, true, false)); err != nil {
		t.Errorf("Carol, holding nothing, giving Bea send: %v", err)
	}
	if err := set(carol, carol.UserID, grantRequest(false, false, true, false)); err != nil {
		t.Errorf("Carol giving herself send: %v", err)
	}
	wantCode(t, "Carol giving Bea act without read", set(carol, tm.bea.UserID, grantRequest(false, true, true, false)),
		service.CodeBadRequest)
	if err := set(tm.ana, tm.bea.UserID, grantRequest(true, false, true, false)); err != nil {
		t.Fatal(err)
	}
	if err := set(carol, tm.bea.UserID, grantRequest(true, true, true, false)); err != nil {
		t.Errorf("Carol giving Bea, who reads, act: %v", err)
	}
	if err := set(carol, tm.bea.UserID, grantRequest(true, true, true, true)); err != nil {
		t.Errorf("Carol giving Bea, a member, manage: %v", err)
	}
	wantCode(t, "Carol giving herself manage", set(carol, carol.UserID, grantRequest(false, false, true, true)),
		service.CodeBadRequest)
	wantCode(t, "the operator giving Carol manage", set(admin(), carol.UserID, grantRequest(false, false, true, true)),
		service.CodeBadRequest)
	if g := held(t, f, tm.ana, tm.id, shared, tm.bea.UserID); !g.Read || !g.Act || !g.Send || !g.Manage || g.GrantedBy != carol.UserID {
		t.Errorf("Bea holds %+v", g)
	}
	// Taking a flag away needs nothing held; an owner or an admin drops
	// their own flags, and a member never.
	if err := set(carol, tm.bea.UserID, grantRequest(true, false, true, true)); err != nil {
		t.Errorf("Carol taking act away: %v", err)
	}
	if err := f.svc.RevokeAccess(t.Context(), carol, shared, carol.UserID, workspace.Flags{Send: true}); err != nil {
		t.Errorf("Carol dropping her own send: %v", err)
	}
	wantCode(t, "Bea dropping her own send", f.svc.RevokeAccess(t.Context(), tm.bea, shared, tm.bea.UserID,
		workspace.Flags{Send: true}), service.CodeNotAuthorized)
}

func TestAnOwnerOrAdminManagesEveryTeamMailboxAndReadsNone(t *testing.T) {
	// By their role an owner or an admin sees each team mailbox's card —
	// its address, state and sync counters — re-authorizes it and decides
	// who holds what on it; none of what it holds, on any path.
	m := newMailFixture(t)
	tm := newSupportTeam(t, m.fixture)
	carol := tm.join(t, m.fixture, "carol@example.com", workspace.RoleAdmin)
	dan := tm.join(t, m.fixture, "dan@example.com", workspace.RoleOwner)
	shared, box := m.ownedBoxIn(t, tm.ana, tm.id, "support@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.CreateFolder("Refunds")
	box.Deliver("INBOX", message("a", "Refund for order 4471"))
	m.index(t, shared, box)
	ctx := t.Context()
	msg := m.messageID(t, shared, "INBOX", 1)

	for name, p := range map[string]service.Principal{"Carol, an admin": carol, "Dan, an owner": dan} {
		if got := ids(t, m.fixture, p); !slices.Contains(got, shared) {
			t.Errorf("%s does not list the team's mailbox: %v", name, got)
		}
		card, err := m.svc.GetAccount(ctx, p, shared)
		if err != nil {
			t.Fatalf("%s reading the card: %v", name, err)
		}
		if card.Access != (service.AccountAccess{Manage: true}) || card.Email != "support@mail.example" {
			t.Errorf("%s's card: access %+v, email %q", name, card.Access, card.Email)
		}
		if raw, err := json.Marshal(card); err != nil || strings.Contains(string(raw), "Refund") ||
			strings.Contains(string(raw), "INBOX") || strings.Contains(string(raw), "example.org") {
			t.Errorf("%s's card carries something of the mailbox's contents: %s", name, raw)
		}
		if st, err := m.svc.SyncStatus(ctx, p, shared); err != nil || !st.Enabled {
			t.Errorf("%s's view of its sync: %+v, %v", name, st, err)
		}
		_, err = m.svc.ListFolders(ctx, p, shared)
		wantCode(t, name+" listing its folders", err, service.CodeNotAuthorized)
		_, err = m.svc.SearchMessages(ctx, p, service.SearchRequest{AccountID: shared})
		wantCode(t, name+" searching it", err, service.CodeNotAuthorized)
		if page, err := m.svc.SearchMessages(ctx, p, service.SearchRequest{Query: "refund"}); err != nil ||
			len(page.Messages) != 0 || page.NextCursor != "" {
			t.Errorf("%s finds %d messages (%v)", name, len(page.Messages), err)
		}
		_, err = m.svc.GetMessage(ctx, p, service.GetMessageRequest{ID: msg})
		wantCode(t, name+" reading a message", err, service.CodeNotFound)
		if st, err := m.svc.Storage(ctx, p, tm.id); err != nil || len(st.Mailboxes) != 0 || st.Total.Messages != 0 {
			t.Errorf("%s is told what the index holds: %+v (%v)", name, st, err)
		}
		wantCode(t, name+" following its new mail", m.svc.MayFollow(ctx, p, shared), service.CodeNotAuthorized)
		_, err = m.svc.WaitForNewMail(ctx, p, 0, time.Second, service.EventFilter{AccountIDs: []string{shared}})
		wantCode(t, name+" waiting on it", err, service.CodeNotAuthorized)
		res, err := m.svc.WaitForNewMail(ctx, p, 0, time.Second, service.EventFilter{})
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range res.Events {
			if ev.AccountID == shared {
				t.Errorf("%s's long poll carries %+v", name, ev)
			}
		}
		stream, err := m.svc.Subscribe(ctx, p, 0, service.EventFilter{})
		if err != nil {
			t.Fatal(err)
		}
		m.publish(t, newMail(t, shared, "inbox", "Refund for order 4472"))
		select {
		case ev := <-stream.Events():
			t.Errorf("%s's stream carries %+v", name, ev)
		case <-time.After(300 * time.Millisecond):
		}
		stream.Close()
		_, err = m.svc.Subscribe(ctx, p, 0, service.EventFilter{AccountIDs: []string{shared}})
		wantCode(t, name+" streaming it", err, service.CodeNotAuthorized)
		dir, err := m.svc.AccessDirectory(ctx, p, tm.id)
		if err != nil || len(dir) != 1 || dir[0].AccountID != shared || dir[0].LinkedBy != tm.ana.UserID ||
			dir[0].Readers != 1 || dir[0].Sync == nil || !dir[0].Sync.Enabled {
			t.Errorf("%s's access directory: %+v (%v)", name, dir, err)
		}
	}
	// A member holding nothing sees none of it.
	if got := ids(t, m.fixture, tm.bea); slices.Contains(got, shared) {
		t.Errorf("Bea lists %v", got)
	}
}

func TestOnlyOwnersAndAdminsSeeMembersAndTheDirectory(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	carol := tm.join(t, f, "carol@example.com", workspace.RoleAdmin)
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
	tm.grant(t, shared, workspace.Flags{Read: true, Manage: true})
	ctx := t.Context()

	for name, p := range map[string]service.Principal{"Ana, the owner": tm.ana, "Carol, an admin": carol, "the operator": admin()} {
		if members, err := f.svc.ListMembers(ctx, p, tm.id); err != nil || len(members) != 3 {
			t.Errorf("%s lists %d members (%v)", name, len(members), err)
		}
		if dir, err := f.svc.AccessDirectory(ctx, p, tm.id); err != nil || len(dir) != 1 {
			t.Errorf("%s's directory: %+v (%v)", name, dir, err)
		}
	}
	// Bea reads and manages the mailbox, as a member: its card, and no
	// list of the team's people or of who holds what.
	_, err := f.svc.ListMembers(ctx, tm.bea, tm.id)
	wantCode(t, "Bea listing members", err, service.CodeNotAuthorized)
	_, err = f.svc.AccessDirectory(ctx, tm.bea, tm.id)
	wantCode(t, "Bea reading the directory", err, service.CodeNotAuthorized)
	if card, err := f.svc.GetAccount(ctx, tm.bea, shared); err != nil || !card.Access.Manage || !card.Access.Read {
		t.Errorf("Bea's card: %+v (%v)", card.Access, err)
	}
	// Her manage is the card and re-authorizing: not removing it, nor
	// changing who holds what.
	wantCode(t, "Bea removing it", f.svc.RemoveAccount(ctx, tm.bea, shared, service.RemoveAccountRequest{Confirm: shared}),
		service.CodeNotAuthorized)
	_, err = f.svc.SetAccess(ctx, tm.bea, shared, carol.UserID, grantRequest(false, false, true, false))
	wantCode(t, "Bea granting", err, service.CodeNotAuthorized)
}

func TestAMemberOrAdminCannotLeave(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	carol := tm.join(t, f, "carol@example.com", workspace.RoleAdmin)
	dan := tm.join(t, f, "dan@example.com", workspace.RoleOwner)
	ctx := t.Context()
	wantCode(t, "Bea, a member, leaving", f.svc.RemoveMember(ctx, tm.bea, tm.id, tm.bea.UserID), service.CodeNotAuthorized)
	wantCode(t, "Carol, an admin, leaving", f.svc.RemoveMember(ctx, carol, tm.id, carol.UserID), service.CodeNotAuthorized)
	// An owner leaves while another owner remains, and is removed by
	// another owner.
	if err := f.svc.RemoveMember(ctx, dan, tm.id, dan.UserID); err != nil {
		t.Errorf("Dan, an owner, leaving: %v", err)
	}
	wantCode(t, "Ana, the last owner, leaving", f.svc.RemoveMember(ctx, tm.ana, tm.id, tm.ana.UserID), service.CodeConflict)
	// An admin removes members, and an owner anyone.
	if err := f.svc.RemoveMember(ctx, carol, tm.id, tm.bea.UserID); err != nil {
		t.Errorf("Carol removing Bea: %v", err)
	}
	if err := f.svc.RemoveMember(ctx, tm.ana, tm.id, carol.UserID); err != nil {
		t.Errorf("Ana removing Carol: %v", err)
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
				return f.svc.RemoveAccount(ctx, p, shared, service.RemoveAccountRequest{Confirm: shared})
			},
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
	wantCode(t, "Bea removing it, a member", f.svc.RemoveAccount(ctx, tm.bea, shared, service.RemoveAccountRequest{Confirm: shared}),
		service.CodeNotAuthorized)
}

func TestATeamMailboxSyncsUnderItsWorkspacesConsent(t *testing.T) {
	// No person's consent covers a team mailbox: not its linker's, not its
	// readers'. An owner or an admin gives the team's, on its behalf, and
	// it syncs while someone reads it.
	m := newMailFixture(t)
	tm := newSupportTeam(t, m.fixture)
	carol := tm.join(t, m.fixture, "carol@example.com", workspace.RoleAdmin)
	ctx := t.Context()
	for _, p := range []service.Principal{tm.ana, tm.bea, carol} {
		if _, err := m.svc.GrantSyncConsent(ctx, p, m.consent().Sync); err != nil {
			t.Fatal(err)
		}
	}
	req := m.passwordAccount(t, "support@mail.example")
	req.WorkspaceID = tm.id
	linked, err := m.svc.AddAccount(ctx, tm.ana, keyed(tm.ana, req))
	if err != nil {
		t.Fatal(err)
	}
	shared := linked.Account.ID
	if m.eligible(t, shared) || linked.Account.Sync.Enabled {
		t.Fatal("a team mailbox syncs under its linker's own consent")
	}
	on := true
	_, err = m.svc.SetMailboxSync(ctx, tm.bea, shared, service.MailboxSyncRequest{Enabled: &on, Version: m.consent().Sync})
	wantCode(t, "Bea, a member holding nothing, turning it on", err, service.CodeNotFound)
	tm.grant(t, shared, workspace.Flags{Read: true})
	_, err = m.svc.SetMailboxSync(ctx, tm.bea, shared, service.MailboxSyncRequest{Enabled: &on, Version: m.consent().Sync})
	wantCode(t, "Bea, a member who reads it, turning it on", err, service.CodeNotAuthorized)
	_, err = m.svc.SetMailboxSync(ctx, keyOf(t, m.fixture, carol, auth.ScopeAdmin, tm.id), shared,
		service.MailboxSyncRequest{Enabled: &on, Version: m.consent().Sync})
	wantCode(t, "a key of Carol's turning it on", err, service.CodeNotAuthorized)
	st, err := m.svc.SetMailboxSync(ctx, carol, shared, service.MailboxSyncRequest{Enabled: &on, Version: m.consent().Sync})
	if err != nil || !st.Enabled || !m.eligible(t, shared) {
		t.Fatalf("Carol, an admin, turning it on for the team: %+v, %v", st, err)
	}
	// A personal mailbox's is its person's, never switched for them.
	own := m.mailbox(t, tm.ana, "ana@mail.example")
	_, err = m.svc.SetMailboxSync(ctx, tm.ana, own, service.MailboxSyncRequest{Enabled: &on, Version: m.consent().Sync})
	wantCode(t, "Ana switching her personal mailbox's sync", err, service.CodeBadRequest)
}

func TestSwitchingATeamMailboxsSyncOffDeletesItsIndexForEveryone(t *testing.T) {
	m := newMailFixture(t)
	tm := newSupportTeam(t, m.fixture)
	carol := tm.join(t, m.fixture, "carol@example.com", workspace.RoleAdmin)
	shared, box := m.ownedBoxIn(t, tm.ana, tm.id, "support@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("a", "Refund for order 4471"))
	m.index(t, shared, box)
	tm.grant(t, shared, workspace.Flags{Read: true})
	ctx := t.Context()
	if indexed(t, m.fixture, shared) == 0 {
		t.Fatal("nothing indexed to begin with")
	}
	// Carol reads nothing of it, and turns it off for everyone who does.
	off := false
	if _, err := m.svc.SetMailboxSync(ctx, carol, shared, service.MailboxSyncRequest{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if n := indexed(t, m.fixture, shared); n != 0 || m.eligible(t, shared) {
		t.Errorf("after the team's sync went off: %d messages indexed, eligible %v", n, m.eligible(t, shared))
	}
	for _, p := range []service.Principal{tm.ana, tm.bea} {
		if page, err := m.svc.SearchMessages(ctx, p, service.SearchRequest{AccountID: shared}); err != nil || len(page.Messages) != 0 {
			t.Errorf("a reader's search after the team's sync went off: %d (%v)", len(page.Messages), err)
		}
	}
	dir, err := m.svc.AccessDirectory(ctx, tm.ana, tm.id)
	if err != nil || len(dir) != 1 || dir[0].Sync == nil || dir[0].Sync.Enabled || dir[0].Sync.EnabledBy != "" {
		t.Errorf("the directory after the team's sync went off: %+v (%v)", dir, err)
	}
}

func TestWithdrawingAPersonsSyncNeverTouchesATeamMailbox(t *testing.T) {
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

	// Ana linked the team's mailbox and gave the team's consent; withdrawing
	// her own deletes her personal index, and the team's stays the team's.
	if _, err := m.svc.WithdrawSyncConsent(t.Context(), tm.ana); err != nil {
		t.Fatal(err)
	}
	if n := indexed(t, m.fixture, own); n != 0 {
		t.Errorf("her personal mailbox still holds %d indexed messages", n)
	}
	for _, id := range []string{shared, bobs} {
		if n := indexed(t, m.fixture, id); n != 1 {
			t.Errorf("%s lost its index: %d", id, n)
		}
	}
	if !m.eligible(t, shared) {
		t.Error("the team's mailbox stopped with her withdrawal")
	}
	page, err := m.svc.SearchMessages(t.Context(), tm.bea, service.SearchRequest{AccountID: shared})
	if err != nil || len(page.Messages) != 1 {
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
	members, err := f.svc.ListMembers(ctx, tm.ana, tm.id)
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

	// A member administers nothing.
	_, err = f.svc.CreateTeamInvite(ctx, tm.bea, tm.id, service.TeamInviteRequest{Email: "gil@example.com"})
	wantCode(t, "Bea inviting", err, service.CodeNotAuthorized)
	_, err = f.svc.ListTeamInvites(ctx, tm.bea, tm.id)
	wantCode(t, "Bea listing invites", err, service.CodeNotAuthorized)
	_, err = f.svc.RenameWorkspace(ctx, tm.bea, tm.id, service.RenameWorkspaceRequest{Name: "Bea's"})
	wantCode(t, "Bea renaming the team", err, service.CodeNotAuthorized)
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
	if err := mark(keyOf(t, m.fixture, tm.bea, auth.ScopeWrite, tm.id)); err != nil {
		t.Errorf("a write key holding act: %v", err)
	}
	wantCode(t, "a read key", mark(keyOf(t, m.fixture, tm.bea, auth.ScopeRead, tm.id)), service.CodeNotAuthorized)
	// Losing act stops her, whoever else may act.
	tm.grant(t, shared, workspace.Flags{Read: true})
	wantCode(t, "Bea once act is gone", mark(tm.bea), service.CodeNotAuthorized)
	if err := mark(tm.ana); err != nil {
		t.Errorf("Ana: %v", err)
	}
}

// refusedUntouched fails unless err is the refusal of an action on a mailbox
// that is not syncing, and no command that changes a mailbox reached box.
func refusedUntouched(t *testing.T, box *providertest.FakeMailbox, what string, err error) {
	t.Helper()
	wantCode(t, what, err, service.CodeConflict)
	if msg := service.MessageOf(err); !strings.Contains(msg, "nothing was changed on the mail server") {
		t.Errorf("%s: message = %q", what, msg)
	}
	for _, c := range box.Calls() {
		switch c.Method {
		case providertest.MethodStoreFlags, providertest.MethodMove, providertest.MethodCopy:
			t.Fatalf("%s: %s reached the server", what, c.Method)
		}
	}
}

func TestAMailboxKeptStoppedWithItsIndexIsNotChangedByARefusedAction(t *testing.T) {
	// Bea reads the team's mailbox and may act on it. It stops syncing and
	// keeps its index for its readers, as migration 0011 left a mailbox whose
	// linker it found disabled: no row of that index can change any more. An
	// action is refused before the server is touched — on the connection when
	// the mailbox stops while the action waits for it, and when it is accepted
	// afterwards.
	m := newMailFixture(t)
	tm := newSupportTeam(t, m.fixture)
	shared, box := m.ownedBoxIn(t, tm.ana, tm.id, "support@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.CreateFolder("Trash", imap.MailboxAttrTrash)
	box.CreateFolder("Work")
	box.Deliver("INBOX", message("a", "Refund for order 4471"))
	m.index(t, shared, box)
	tm.grant(t, shared, workspace.Flags{Read: true, Act: true})
	b := &actionBox{m: m, owner: tm.bea, id: shared, box: box}
	b.allow(t)
	row, inbox, work := b.row(t, "INBOX", "a"), b.folder(t, "INBOX"), b.folder(t, "Work")
	acts := []struct {
		name string
		act  func(context.Context) error
	}{
		{"marking read", func(ctx context.Context) error {
			_, err := m.svc.SetFlags(ctx, tm.bea, service.SetFlagsRequest{IDs: []int64{row}, Seen: yes()})
			return err
		}},
		{"moving", func(ctx context.Context) error {
			_, err := m.svc.MoveMessages(ctx, tm.bea, service.MoveRequest{IDs: []int64{row}, To: strconv.FormatInt(work, 10)})
			return err
		}},
		{"trashing", func(ctx context.Context) error {
			_, err := m.svc.TrashMessages(ctx, tm.bea, service.TrashRequest{IDs: []int64{row}})
			return err
		}},
	}

	h := hold(box, func(c providertest.Call) bool {
		return c.Method == providertest.MethodSelect && c.Role == provider.RoleInteractive
	})
	done := make(chan error, 1)
	go func() { done <- acts[0].act(t.Context()) }()
	<-h.entered
	keepStopped(t, m, tm, shared)
	close(h.released)
	err := <-done
	box.OnCall(nil)
	refusedUntouched(t, box, acts[0].name+" while the mailbox stopped", err)

	opened := box.Opens(provider.RoleInteractive)
	for _, tc := range acts {
		box.ResetCalls()
		refusedUntouched(t, box, tc.name+" once it is kept stopped", tc.act(t.Context()))
	}
	if n := box.Opens(provider.RoleInteractive) - opened; n != 0 {
		t.Errorf("the refused actions opened %d connections", n)
	}
	if seen(box.Flags("INBOX", b.serverUID("INBOX", "a"))) ||
		!b.on("INBOX", "a") || b.on("Work", "a") || b.on("Trash", "a") {
		t.Fatal("the mail server changed")
	}
	if n := m.count(t, `SELECT count(*) FROM messages WHERE id = ? AND folder_id = ? AND seen = 0`, row, inbox); n != 1 {
		t.Fatal("the index changed")
	}
}

func TestAnActionCutShortWhenItsMailboxStopsSyncingNeverSaysTheServerIsUnchanged(t *testing.T) {
	// Bea marks read two messages of the team's mailbox, in two folders. Ana,
	// its owner, turns the team's sync off once the first folder is done:
	// before the second folder's command, which is then not sent, or between
	// that command and its recording. The server has changed either way, and
	// the answer says it may have.
	for _, tc := range []struct {
		name string
		// at is the second folder's call the sync is turned off during.
		at providertest.Method
		// sent is whether the second folder's command goes out.
		sent bool
	}{
		{"before the second folder's command", providertest.MethodSelect, false},
		{"between the second folder's command and its recording", providertest.MethodStoreFlags, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMailFixture(t)
			tm := newSupportTeam(t, m.fixture)
			shared, box := m.ownedBoxIn(t, tm.ana, tm.id, "support@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
			box.CreateFolder("Work")
			inboxUID := box.Deliver("INBOX", message("a", "Refund for order 4471"))
			workUID := box.Deliver("Work", message("b", "Rota for October"))
			m.index(t, shared, box)
			tm.grant(t, shared, workspace.Flags{Read: true, Act: true})
			b := &actionBox{m: m, owner: tm.bea, id: shared, box: box}
			b.allow(t)
			ids := []int64{b.row(t, "INBOX", "a"), b.row(t, "Work", "b")}

			h := hold(box, func(c providertest.Call) bool {
				return c.Method == tc.at && c.Role == provider.RoleInteractive && c.Folder == "Work"
			})
			done := make(chan error, 1)
			go func() {
				_, err := m.svc.SetFlags(t.Context(), tm.bea, service.SetFlagsRequest{IDs: ids, Seen: yes()})
				done <- err
			}()
			<-h.entered
			off := false
			if _, err := m.svc.SetMailboxSync(t.Context(), tm.ana, shared, service.MailboxSyncRequest{Enabled: &off}); err != nil {
				t.Fatal(err)
			}
			close(h.released)
			err := <-done
			box.OnCall(nil)
			wantCode(t, "marking read across the switch", err, service.CodeConflict)
			if msg := service.MessageOf(err); !strings.Contains(msg, "may have made the change") {
				t.Errorf("message = %q", msg)
			}
			if !seen(box.Flags("INBOX", inboxUID)) {
				t.Fatal("the first folder, done before the switch, was not changed")
			}
			if got := seen(box.Flags("Work", workUID)); got != tc.sent {
				t.Fatalf("the second folder marked read: %v, want %v", got, tc.sent)
			}
		})
	}
}

func TestAWorkspaceKeyNeverChangesAMailboxWhoseIndexCannotFollowIt(t *testing.T) {
	// A key Ana gave read and act on the team's mailbox acts under its key
	// terms, with nobody's consent asked. Where the mailbox stopped syncing
	// and kept its index — kept stopped since the upgrade, or read by no
	// person any more, which a key never counts as — it is refused like
	// anyone, before the server is touched.
	for _, tc := range []struct {
		name string
		stop func(t *testing.T, m *mailFixture, tm supportTeam, accountID string)
	}{
		{"kept stopped since the upgrade", keepStopped},
		{"read by nobody", func(t *testing.T, m *mailFixture, tm supportTeam, accountID string) {
			// Bea reads it, so Ana may drop her own flags; Bea is then
			// disabled anyway, and the key, which is not hers, stays.
			ctx := t.Context()
			tm.grant(t, accountID, workspace.Flags{Read: true})
			if err := m.svc.RevokeAccess(ctx, tm.ana, accountID, tm.ana.UserID, workspace.Flags{}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.svc.DisableUser(ctx, admin(), service.CloseUserRequest{Email: "bea@example.com", Force: true}); err != nil {
				t.Fatal(err)
			}
			_, unread, err := m.db.TeamSyncNotices(ctx)
			if err != nil || !slices.Contains(unread, accountID) || m.eligible(t, accountID) || indexed(t, m.fixture, accountID) == 0 {
				t.Fatalf("not read by nobody with its index: %v (%v)", unread, err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMailFixture(t)
			tm := newSupportTeam(t, m.fixture)
			shared, box := m.ownedBoxIn(t, tm.ana, tm.id, "support@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
			uid := box.Deliver("INBOX", message("a", "Refund for order 4471"))
			m.index(t, shared, box)
			row := m.messageID(t, shared, "INBOX", uid)
			key := m.authenticate(t, authtest.NewWorkspaceKey(t, m.db, auth.ScopeWrite, tm.id, tm.ana.UserID,
				workspace.KeyGrant{AccountID: shared, Flags: workspace.Flags{Read: true, Act: true}}))
			tc.stop(t, m, tm, shared)

			_, err := m.svc.SetFlags(t.Context(), key, service.SetFlagsRequest{IDs: []int64{row}, Seen: yes()})
			refusedUntouched(t, box, "the key marking read", err)
			if n := box.Opens(provider.RoleInteractive); n != 0 {
				t.Errorf("the refused action opened %d connections", n)
			}
			if seen(box.Flags("INBOX", uid)) {
				t.Fatal("the mail server changed")
			}
			if n := m.count(t, `SELECT count(*) FROM messages WHERE id = ? AND seen = 0`, row); n != 1 {
				t.Fatal("the index changed")
			}
		})
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

	// A person's own workspaces, and nothing of the operator's; a key lists
	// the one it belongs to, and administers none.
	mine, err := f.svc.ListWorkspaces(ctx, tm.bea)
	if err != nil || len(mine) != 3 || mine[0].Kind != "personal" {
		t.Errorf("Bea lists %+v (%v): personal first, then her two teams", mine, err)
	}
	if listed, err := f.svc.ListWorkspaces(ctx, keyOf(t, f, tm.bea, auth.ScopeRead, tm.id)); err != nil ||
		len(listed) != 1 || listed[0].ID != tm.id {
		t.Errorf("a key of the team lists %+v (%v)", listed, err)
	}
	_, err = f.svc.ListMembers(ctx, keyOf(t, f, tm.ana, auth.ScopeAdmin, tm.id), tm.id)
	wantCode(t, "a key of the team administering", err, service.CodeNotAuthorized)
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
		// Opened first, as a browser does; one that does not open is
		// refused again by the sign-up, for its own reason.
		opened, _ := f.svc.OpenSignUp(ctx, service.SignUpOpenRequest{Invite: code, Email: "alice@example.com"})
		return f.svc.SignUp(ctx, service.SignUpRequest{Invite: code, Email: "alice@example.com", Name: "Alice",
			SealID: opened.SealID, Enrolment: wireEnrolment(t)}, "test")
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
	// Ana is the team's only owner, and the only person who reads its
	// mailbox: Bea is a member who holds nothing on it.
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
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

	// Insisting leaves the team to its other members, without an owner
	// until the operator names one, and its mailbox to the team, with
	// nobody who can read it.
	req.Force = true
	deleted, err := f.svc.DeleteUser(ctx, admin(), req)
	if err != nil {
		t.Fatalf("deleting her with force: %v", err)
	}
	if deleted.AccountsRemoved != 0 || deleted.TeamsDeleted != 0 {
		t.Errorf("deleted = %+v", deleted)
	}
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE id = ?`, shared); n != 1 {
		t.Error("closing a person removed a team's mailbox")
	}
	members, err := f.svc.ListMembers(ctx, admin(), tm.id)
	if err != nil || len(members) != 1 || members[0].UserID != tm.bea.UserID {
		t.Errorf("the team's members: %+v (%v)", members, err)
	}
	if _, err := f.svc.SetMember(ctx, admin(), tm.id, tm.bea.UserID, service.MemberRequest{Role: ptr("owner")}); err != nil {
		t.Errorf("the operator naming a new owner: %v", err)
	}
}
