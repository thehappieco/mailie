package service_test

import (
	"context"
	"testing"

	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Platform decision 0060: only an owner changes an owner's access to a
// mailbox. An admin gives an owner no flag, takes none away and takes none of
// their access; owners change each other's, and an admin still changes an
// admin's and a member's.

// ownersTeam is Support with Ana, its owner, who linked a keyless mailbox and
// reads it; Carol, an admin who reads it too (Ana gave her read); Dan,
// another owner, and Eve, another admin, holding nothing yet; and Bea, a
// member.
type ownersTeam struct {
	supportTeam
	carol, dan, eve service.Principal
	shared          string
}

func newOwnersTeam(t *testing.T, f *fixture) ownersTeam {
	t.Helper()
	tm := ownersTeam{supportTeam: newSupportTeam(t, f), shared: "acc_00000000000000aa"}
	tm.carol = tm.join(t, f, "carol@example.com", workspace.RoleAdmin)
	tm.dan = tm.join(t, f, "dan@example.com", workspace.RoleOwner)
	tm.eve = tm.join(t, f, "eve@example.com", workspace.RoleAdmin)
	tm.link(t, f, tm.shared, "support@mail.example")
	if _, err := f.svc.SetAccess(t.Context(), tm.ana, tm.shared, tm.carol.UserID, grantRequest(true, false, false, false)); err != nil {
		t.Fatalf("Ana giving Carol read: %v", err)
	}
	return tm
}

// set is p setting exactly flags for userID on the shared mailbox.
func (tm ownersTeam) set(t *testing.T, f *fixture, p service.Principal, userID string, req service.GrantRequest) error {
	t.Helper()
	_, err := f.svc.SetAccess(t.Context(), p, tm.shared, userID, req)
	return err
}

// flagsOf is what userID holds on the shared mailbox, as Ana's directory
// lists it.
func (tm ownersTeam) flagsOf(t *testing.T, f *fixture, userID string) workspace.Flags {
	t.Helper()
	g := held(t, f, tm.ana, tm.id, tm.shared, userID)
	return workspace.Flags{Read: g.Read, Act: g.Act, Send: g.Send, Manage: g.Manage}
}

func TestAnAdminCannotGiveAnOwnerAnyFlag(t *testing.T) {
	f := newFixture(t)
	tm := newOwnersTeam(t, f)
	for name, req := range map[string]service.GrantRequest{
		"read":               grantRequest(true, false, false, false),
		"send":               grantRequest(false, false, true, false),
		"read, act and send": grantRequest(true, true, true, false),
	} {
		wantCode(t, "Carol giving Dan, an owner, "+name, tm.set(t, f, tm.carol, tm.dan.UserID, req),
			service.CodeNotAuthorized)
	}
	if got := tm.flagsOf(t, f, tm.dan.UserID); got.Any() {
		t.Errorf("Dan holds %+v after an admin's refused grants", got)
	}
	// Ana, the owner who linked it, holds read, act and send as stored
	// flags: the same flags again are a change of an owner's access too.
	ana := tm.flagsOf(t, f, tm.ana.UserID)
	wantCode(t, "Carol setting Ana's flags to what she holds",
		tm.set(t, f, tm.carol, tm.ana.UserID, grantRequest(ana.Read, ana.Act, ana.Send, false)), service.CodeNotAuthorized)
}

func TestAnAdminCannotTakeAFlagFromAnOwnerNorAllTheirAccess(t *testing.T) {
	f := newFixture(t)
	tm := newOwnersTeam(t, f)
	if err := tm.set(t, f, tm.ana, tm.dan.UserID, grantRequest(true, true, true, false)); err != nil {
		t.Fatalf("Ana giving Dan read, act and send: %v", err)
	}
	wantCode(t, "Carol taking Dan's send by setting his flags",
		tm.set(t, f, tm.carol, tm.dan.UserID, grantRequest(true, true, false, false)), service.CodeNotAuthorized)
	wantCode(t, "Carol taking Dan's send", f.svc.RevokeAccess(t.Context(), tm.carol, tm.shared, tm.dan.UserID,
		workspace.Flags{Send: true}), service.CodeNotAuthorized)
	wantCode(t, "Carol taking all of Dan's access", f.svc.RevokeAccess(t.Context(), tm.carol, tm.shared, tm.dan.UserID,
		workspace.Flags{}), service.CodeNotAuthorized)
	wantCode(t, "Carol taking Ana's act", f.svc.RevokeAccess(t.Context(), tm.carol, tm.shared, tm.ana.UserID,
		workspace.Flags{Act: true}), service.CodeNotAuthorized)
	if got := tm.flagsOf(t, f, tm.dan.UserID); got != (workspace.Flags{Read: true, Act: true, Send: true}) {
		t.Errorf("Dan holds %+v after an admin's refused changes", got)
	}
}

func TestAnOwnerStillChangesAnotherOwnersAccess(t *testing.T) {
	f := newFixture(t)
	tm := newOwnersTeam(t, f)
	if err := tm.set(t, f, tm.ana, tm.dan.UserID, grantRequest(true, true, true, false)); err != nil {
		t.Fatalf("Ana giving Dan, another owner, read, act and send: %v", err)
	}
	if err := tm.set(t, f, tm.ana, tm.dan.UserID, grantRequest(true, false, true, false)); err != nil {
		t.Errorf("Ana taking Dan's act: %v", err)
	}
	// Dan, who reads it now, changes Ana's.
	if err := f.svc.RevokeAccess(t.Context(), tm.dan, tm.shared, tm.ana.UserID, workspace.Flags{Send: true}); err != nil {
		t.Errorf("Dan taking Ana's send: %v", err)
	}
	if got := tm.flagsOf(t, f, tm.ana.UserID); got.Send {
		t.Errorf("Ana holds %+v after Dan took her send", got)
	}
	if err := f.svc.RevokeAccess(t.Context(), tm.ana, tm.shared, tm.dan.UserID, workspace.Flags{}); err != nil {
		t.Errorf("Ana taking all of Dan's access: %v", err)
	}
	if got := tm.flagsOf(t, f, tm.dan.UserID); got.Any() {
		t.Errorf("Dan holds %+v after Ana took all of it", got)
	}
}

func TestAnAdminStillChangesAnAdminsAndAMembersAccess(t *testing.T) {
	f := newFixture(t)
	tm := newOwnersTeam(t, f)
	for name, userID := range map[string]string{"Eve, an admin": tm.eve.UserID, "Bea, a member": tm.bea.UserID} {
		if err := tm.set(t, f, tm.carol, userID, grantRequest(true, true, true, false)); err != nil {
			t.Errorf("Carol giving %s read, act and send: %v", name, err)
		}
		if err := tm.set(t, f, tm.carol, userID, grantRequest(true, false, true, false)); err != nil {
			t.Errorf("Carol taking %s's act: %v", name, err)
		}
		if err := f.svc.RevokeAccess(t.Context(), tm.carol, tm.shared, userID, workspace.Flags{}); err != nil {
			t.Errorf("Carol taking all of %s's access: %v", name, err)
		}
		if got := tm.flagsOf(t, f, userID); got.Any() {
			t.Errorf("%s holds %+v after Carol took all of it", name, got)
		}
	}
	if err := tm.set(t, f, tm.carol, tm.bea.UserID, grantRequest(false, false, false, true)); err != nil {
		t.Errorf("Carol giving Bea, a member, manage: %v", err)
	}
	if err := tm.set(t, f, tm.carol, tm.carol.UserID, grantRequest(true, false, true, false)); err != nil {
		t.Errorf("Carol giving herself send: %v", err)
	}
	if err := f.svc.RevokeAccess(t.Context(), tm.carol, tm.shared, tm.carol.UserID, workspace.Flags{Send: true}); err != nil {
		t.Errorf("Carol dropping her own send: %v", err)
	}
}

func TestAnAdminsChangeIsRefusedInItsWriteWhenItsTargetBecameAnOwnerMeanwhile(t *testing.T) {
	// Carol raises Dan's flags while he is an admin; the freshness hook,
	// asked between her read-only check and the write, sees him made an
	// owner. The write reads his role again and refuses.
	hook := &freshHook{}
	f := newFixtureWith(t, fixtureOptions{freshness: hook})
	tm := newOwnersTeam(t, f)
	f.exec(t, `UPDATE workspace_members SET role = 'admin' WHERE workspace_id = ? AND user_id = ?`, tm.id, tm.dan.UserID)
	hook.take()
	hook.answerWith(func(ctx context.Context, act service.FreshAct) error {
		_, err := f.db.Writer().ExecContext(ctx, `UPDATE workspace_members SET role = 'owner' WHERE workspace_id = ? AND user_id = ?`,
			tm.id, tm.dan.UserID)
		return err
	})
	wantCode(t, "Carol giving Dan send as he becomes an owner",
		tm.set(t, f, tm.carol, tm.dan.UserID, grantRequest(false, false, true, false)), service.CodeNotAuthorized)
	if asked := hook.take(); len(asked) != 1 || asked[0].Kind != service.FreshRaiseFlags {
		t.Fatalf("the hook was asked %+v, want the raise once", asked)
	}
	if got := tm.flagsOf(t, f, tm.dan.UserID); got.Any() {
		t.Errorf("Dan holds %+v", got)
	}
}

func TestAnAdminsRefusedChangeOfAnOwnersAccessAsksTheFreshnessHookNothing(t *testing.T) {
	hook := &freshHook{}
	f := newFixtureWith(t, fixtureOptions{freshness: hook})
	tm := newOwnersTeam(t, f)
	hook.take()
	wantCode(t, "Carol giving Dan, an owner, send",
		tm.set(t, f, tm.carol, tm.dan.UserID, grantRequest(false, false, true, false)), service.CodeNotAuthorized)
	if asked := hook.take(); len(asked) != 0 {
		t.Errorf("an admin's refused change of an owner's access asked the hook %+v", asked)
	}
}

func TestTheOperatorStillRevokesAnOwnersFlags(t *testing.T) {
	f := newFixture(t)
	tm := newOwnersTeam(t, f)
	if err := tm.set(t, f, tm.ana, tm.dan.UserID, grantRequest(true, true, true, false)); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RevokeAccess(t.Context(), admin(), tm.shared, tm.dan.UserID, workspace.Flags{Send: true}); err != nil {
		t.Errorf("the operator taking Dan's send: %v", err)
	}
	if got := tm.flagsOf(t, f, tm.dan.UserID); got != (workspace.Flags{Read: true, Act: true}) {
		t.Errorf("Dan holds %+v", got)
	}
}

func TestAnAdminStillHandsAWaitingOwnerTheMailboxKey(t *testing.T) {
	// Supplying the key a waiting owner is owed changes no flag: an admin
	// who reads the mailbox hands it over as any reader does.
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	shared, key := tm.keyedTeamMailbox(t, f, "support@mail.example")
	carol := tm.join(t, f, "carol@example.com", workspace.RoleAdmin)
	dan := tm.join(t, f, "dan@example.com", workspace.RoleOwner)
	if _, err := f.svc.SetAccess(t.Context(), tm.ana, shared, carol.UserID, key.give(t, f, carol.UserID)); err != nil {
		t.Fatalf("Ana giving Carol read: %v", err)
	}
	f.exec(t, `INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act, send, manage, granted_by, created_at, updated_at)
		VALUES (?, ?, ?, 1, 0, 0, 0, ?, 0, 0)`, shared, tm.id, dan.UserID, tm.ana.UserID)
	if a := accessOf(t, f, dan, shared); a.Access.Read || !a.Access.WaitingKey {
		t.Fatalf("Dan with the flag and no key: %+v", a.Access)
	}
	if _, err := f.svc.SupplyKey(t.Context(), carol, shared, dan.UserID, key.supply(t, f, dan.UserID)); err != nil {
		t.Fatalf("Carol, an admin who reads it, supplying Dan, a waiting owner, the key: %v", err)
	}
	if a := accessOf(t, f, dan, shared); !a.Access.Read {
		t.Errorf("Dan once supplied: %+v", a.Access)
	}
}
