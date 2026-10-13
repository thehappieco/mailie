package service_test

import (
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Suspending a person, which an extension of the daemon does following a
// decision taken elsewhere: what ends, what is kept unusable, and what a
// resume gives back (docs/key-scheme.md section 12.13).

// suspend suspends a person as an extension does, and fails the test unless
// they are suspended now.
func (f *fixture) suspend(t *testing.T, userID string) service.SuspendedPerson {
	t.Helper()
	got, err := f.svc.SuspendPerson(t.Context(), userID)
	if err != nil || !got.Suspended {
		t.Fatalf("suspending %s: %+v, %v", userID, got, err)
	}
	return got
}

// resume lifts a person's suspension as an extension does, and fails the
// test unless they are active again.
func (f *fixture) resume(t *testing.T, userID string) {
	t.Helper()
	if active, err := f.svc.ResumePerson(t.Context(), userID); err != nil || !active {
		t.Fatalf("resuming %s: active %v, %v", userID, active, err)
	}
}

// signInAgain opens a new session for a person made by f.person, as their
// browser signs in.
func (f *fixture) signInAgain(t *testing.T, email string) service.Principal {
	t.Helper()
	p, err := f.users.AuthenticateSession(t.Context(), authtest.SignIn(t, f.users, email))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// takeGrant deletes a person's grant on a mailbox and keeps their flag, as a
// reset leaves them: they wait for the key.
func (f *fixture) takeGrant(t *testing.T, accountID, userID string) {
	t.Helper()
	f.exec(t, `DELETE FROM mailbox_grants WHERE account_id = ? AND user_id = ?`, accountID, userID)
}

// recipientIDs lists the people of a mailbox key's answer.
func recipientIDs(rs []service.KeyRecipient) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.UserID)
	}
	return out
}

func TestSuspendingAPersonEndsTheirSessionsKeysAndInvitesAndKeepsTheirGrants(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	ctx := t.Context()
	shared, key := tm.keyedTeamMailbox(t, f, "support@mail.example")
	if _, err := f.svc.SetAccess(ctx, tm.ana, shared, tm.bea.UserID, key.give(t, f, tm.bea.UserID)); err != nil {
		t.Fatal(err)
	}
	phone := authtest.SignIn(t, f.users, "bea@example.com")
	prefix, secret := f.issue(t, tm.bea.UserID)
	// A team of her own, with an invite she made still waiting; and a reset
	// invitation the operator made for her.
	own, err := f.svc.CreateWorkspace(ctx, tm.bea, service.CreateWorkspaceRequest{Name: "Bea's"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.users.CreateInvite(ctx, auth.NewInvite{Email: "dan@example.com", WorkspaceID: own.ID,
		WorkspaceRole: workspace.RoleMember, CreatedBy: tm.bea.UserID}); err != nil {
		t.Fatal(err)
	}
	reset, _, err := f.users.CreateReset(ctx, tm.bea.UserID, false, "cli")
	if err != nil {
		t.Fatal(err)
	}

	got := f.suspend(t, tm.bea.UserID)
	if want := (service.SuspendedPerson{ID: tm.bea.UserID, Suspended: true, SessionsEnded: 2, KeysRevoked: 1,
		InvitesExpired: 1}); got != want {
		t.Errorf("suspending Bea: %+v, want %+v", got, want)
	}
	for name, token := range map[string]string{"session": phone, "key": secret} {
		if _, err := f.svc.Authenticate(ctx, token, nil); service.CodeOf(err) != service.CodeUnauthorized {
			t.Errorf("a suspended person's %s still works: %v", name, err)
		}
	}
	if stopped, reason := f.stopped(t, prefix); !stopped || reason != workspace.KeyStopCreatorSuspended {
		t.Errorf("her key: stopped %v for %q, want stopped for her suspension", stopped, reason)
	}
	if n := f.count(t, `SELECT count(*) FROM invites WHERE created_by = ? AND expires_at > ?`,
		tm.bea.UserID, time.Now().Unix()); n != 0 {
		t.Errorf("%d invites she made still wait", n)
	}
	if _, err := f.users.OpenReset(ctx, reset, "bea@example.com"); err == nil {
		t.Error("her reset invitation still opens")
	}
	if n := f.count(t, `SELECT count(*) FROM reset_invites WHERE user_id = ?`, tm.bea.UserID); n != 0 {
		t.Errorf("%d reset invitations kept", n)
	}

	// What she holds is kept: memberships, flags and her sealed grant.
	if n := f.count(t, `SELECT count(*) FROM mailbox_grants WHERE user_id = ?`, tm.bea.UserID); n != 1 {
		t.Errorf("suspended, she holds %d grants, want hers kept", n)
	}
	if n := f.count(t, `SELECT count(*) FROM mailbox_access WHERE user_id = ? AND read = 1`, tm.bea.UserID); n != 1 {
		t.Errorf("suspended, she holds read on %d mailboxes, want the flag kept", n)
	}
	if n := f.count(t, `SELECT count(*) FROM workspace_members WHERE user_id = ? AND status = 'active'`,
		tm.bea.UserID); n != 3 {
		t.Errorf("suspended, she is an active member of %d workspaces, want her three kept", n)
	}
	members, err := f.svc.ListMembers(ctx, tm.ana, tm.id)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(members, func(m service.Member) bool { return m.UserID == tm.bea.UserID })
	if i < 0 || !members[i].PersonSuspended || !members[i].PersonDisabled || members[i].Status != "active" {
		t.Errorf("the team lists her as %+v, want her membership kept and her person suspended", members)
	}

	// Again: nothing left to end, and no error.
	if again := f.suspend(t, tm.bea.UserID); again.SessionsEnded != 0 || again.KeysRevoked != 0 || again.InvitesExpired != 0 {
		t.Errorf("suspending her again ended %+v", again)
	}
	wantCode(t, "suspending nobody", func() error { _, err := f.svc.SuspendPerson(ctx, ""); return err }(),
		service.CodeBadRequest)
	wantCode(t, "suspending someone unknown", func() error {
		_, err := f.svc.SuspendPerson(ctx, "usr_0000000000000000")
		return err
	}(), service.CodeNotFound)
}

func TestASuspendedPersonReadsNothingAndWaitsForNothing(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	ctx := t.Context()
	shared, key := tm.keyedTeamMailbox(t, f, "support@mail.example")
	waiting, other := tm.keyedTeamMailbox(t, f, "orders@mail.example")
	for id, k := range map[string]browserKey{shared: key, waiting: other} {
		if _, err := f.svc.SetAccess(ctx, tm.ana, id, tm.bea.UserID, k.give(t, f, tm.bea.UserID)); err != nil {
			t.Fatal(err)
		}
	}
	f.takeGrant(t, waiting, tm.bea.UserID)
	mine := f.mailbox(t, tm.bea, "bea@mail.example")
	if a := accessOf(t, f, tm.bea, shared); !a.Access.Read {
		t.Fatalf("before: Bea does not read the team mailbox: %+v", a.Access)
	}
	if a := accessOf(t, f, tm.bea, waiting); !a.Access.WaitingKey {
		t.Fatalf("before: Bea does not wait for the key: %+v", a.Access)
	}

	f.suspend(t, tm.bea.UserID)

	// A request already on its way with her session reaches nothing.
	if listed, err := f.svc.ListAccounts(ctx, tm.bea, ""); err == nil && len(listed) != 0 {
		t.Errorf("suspended, Bea lists %d mailboxes", len(listed))
	}
	for _, id := range []string{shared, waiting, mine} {
		if _, err := f.svc.GetAccount(ctx, tm.bea, id); service.CodeOf(err) != service.CodeNotFound {
			t.Errorf("suspended, Bea reads %s: %v", id, err)
		}
	}
	all := []string{shared, waiting, mine}
	if held, err := tm.ws.Access(ctx, tm.bea.UserID, all); err != nil || len(held) != 0 {
		t.Errorf("suspended, Bea holds %+v (%v)", held, err)
	}
	if waits, err := tm.ws.WaitingForKey(ctx, tm.bea.UserID, all); err != nil || len(waits) != 0 {
		t.Errorf("suspended, Bea waits for the keys of %v (%v)", waits, err)
	}
	// Nobody's console counts her as a reader or offers her the key.
	for _, id := range []string{shared, waiting} {
		state, err := f.svc.MailboxKey(ctx, tm.ana, id)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(recipientIDs(state.Waiting), tm.bea.UserID) {
			t.Errorf("Ana's console offers the key of %s to suspended Bea", id)
		}
	}
	directory, err := f.svc.AccessDirectory(ctx, tm.ana, tm.id)
	if err != nil {
		t.Fatal(err)
	}
	for _, mb := range directory {
		if mb.Readers != 1 {
			t.Errorf("%s counts %d readers, want Ana alone", mb.AccountID, mb.Readers)
		}
	}
}

func TestASuspendedPersonsGrantIsNeverServed(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	ctx := t.Context()
	in := vouched("cy-subject", "cy@example.com")
	session := f.signedIn(t, in)
	cy, err := f.users.AuthenticateSession(ctx, session.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Write(ctx, func(tx *sql.Tx) error {
		return tm.ws.AddMemberTx(ctx, tx, tm.id, cy.UserID, workspace.RoleMember, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	shared, key := tm.keyedTeamMailbox(t, f, "support@mail.example")
	for _, p := range []service.Principal{tm.bea, cy} {
		if _, err := f.svc.SetAccess(ctx, tm.ana, shared, p.UserID, key.give(t, f, p.UserID)); err != nil {
			t.Fatal(err)
		}
	}
	before, err := f.svc.MailboxKey(ctx, cy, shared)
	if err != nil || before.Grant == "" {
		t.Fatalf("before: Cy's console reads %+v (%v)", before, err)
	}

	f.suspend(t, cy.UserID)
	f.suspend(t, tm.bea.UserID)

	// No sign-in answers a wrap or opens a session: through the provider,
	// asking for the key, or with the password.
	signed, err := f.svc.SignInExternal(ctx, f.pinned(t, in))
	if !errors.Is(err, auth.ErrUserDisabled) || signed.Session != nil || signed.PlatformWrap != "" || signed.Enrolment != nil {
		t.Errorf("a suspended person signing in through the provider: %+v, %v", signed, err)
	}
	login, err := f.svc.Login(ctx, service.LoginRequest{Email: "bea@example.com", AuthKey: authtest.AuthKey}, "test")
	if service.CodeOf(err) != service.CodeUnauthorized || login.PasswordWrap != "" {
		t.Errorf("a suspended person signing in with the password: %+v, %v", login, err)
	}
	// A session from before reads no grant, and nobody else is offered one.
	for _, p := range []service.Principal{cy, tm.bea} {
		if state, err := f.svc.MailboxKey(ctx, p, shared); err == nil {
			t.Errorf("a suspended person's session reads the mailbox key: %+v", state)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM mailbox_grants WHERE user_id IN (?, ?)`, cy.UserID, tm.bea.UserID); n != 2 {
		t.Errorf("suspended, they hold %d grants, want theirs kept", n)
	}

	// Resumed, Cy signs in again and is served the grant she held.
	f.resume(t, cy.UserID)
	signed, err = f.svc.SignInExternal(ctx, f.pinned(t, in))
	if err != nil || signed.Session == nil || signed.PlatformWrap == "" {
		t.Fatalf("Cy signing in once resumed: %+v, %v", signed, err)
	}
	again, err := f.users.AuthenticateSession(ctx, signed.Session.Token)
	if err != nil {
		t.Fatal(err)
	}
	after, err := f.svc.MailboxKey(ctx, again, shared)
	if err != nil || after.Grant != before.Grant {
		t.Errorf("once resumed, Cy's console reads %+v (%v), want the grant she held", after, err)
	}
}

func TestNobodySuppliesTheKeyToASuspendedPerson(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	ctx := t.Context()
	shared, key := tm.keyedTeamMailbox(t, f, "support@mail.example")
	if _, err := f.svc.SetAccess(ctx, tm.ana, shared, tm.bea.UserID, key.give(t, f, tm.bea.UserID)); err != nil {
		t.Fatal(err)
	}
	f.takeGrant(t, shared, tm.bea.UserID)
	other, otherKey := tm.keyedTeamMailbox(t, f, "orders@mail.example")
	const plain = "acc_00000000000000e2"
	tm.link(t, f, plain, "plain@mail.example")
	if _, err := f.svc.SetAccess(ctx, tm.ana, plain, tm.bea.UserID, readFlag("")); err != nil {
		t.Fatal(err)
	}
	// Grants sealed for her before she was suspended, as a console would
	// have read her.
	supply := key.supply(t, f, tm.bea.UserID)
	give := otherKey.give(t, f, tm.bea.UserID)
	first := newBrowserKey(t, "", 1)
	withHers := service.FirstKeyRequest{PublicKey: b64(first.public), Namespace: first.namespace,
		Grants: []service.GrantToRequest{first.grantTo(t, f, tm.ana.UserID), first.grantTo(t, f, tm.bea.UserID)}}

	f.suspend(t, tm.bea.UserID)

	_, err := f.svc.SupplyKey(ctx, tm.ana, shared, tm.bea.UserID, supply)
	wantCode(t, "supplying the key to her", err, service.CodeNotFound)
	_, err = f.svc.SetAccess(ctx, tm.ana, other, tm.bea.UserID, give)
	wantCode(t, "giving her read with a grant", err, service.CodeNotFound)
	_, err = f.svc.WriteFirstKey(ctx, tm.ana, plain, withHers)
	wantCode(t, "a first key with a grant for her", err, service.CodeConflict)
	if n := f.count(t, `SELECT count(*) FROM mailbox_grants WHERE user_id = ?`, tm.bea.UserID); n != 0 {
		t.Errorf("suspended, she was given %d grants", n)
	}
	// A first key written meanwhile is sealed to those who read it now.
	withHers.Grants = withHers.Grants[:1]
	if _, err := f.svc.WriteFirstKey(ctx, tm.ana, plain, withHers); err != nil {
		t.Fatalf("a first key while she is suspended: %v", err)
	}
	f.resume(t, tm.bea.UserID)
	bea := f.signInAgain(t, "bea@example.com")
	for _, id := range []string{shared, plain} {
		if a := accessOf(t, f, bea, id); a.Access.Read || !a.Access.WaitingKey {
			t.Errorf("once resumed, Bea on %s: %+v, want her waiting for the key", id, a.Access)
		}
	}
}

func TestASuspendedPersonProtectsNothing(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	ctx := t.Context()
	olga := f.person(t, "olga@example.com", auth.RoleOwner)
	pat := f.person(t, "pat@example.com", auth.RoleOwner)
	owner := "owner"
	if _, err := f.svc.SetMember(ctx, tm.ana, tm.id, tm.bea.UserID, service.MemberRequest{Role: &owner}); err != nil {
		t.Fatal(err)
	}
	shared, key := tm.keyedTeamMailbox(t, f, "support@mail.example")
	alone, aloneKey := tm.keyedTeamMailbox(t, f, "orders@mail.example")
	for id, k := range map[string]browserKey{shared: key, alone: aloneKey} {
		if _, err := f.svc.SetAccess(ctx, tm.ana, id, tm.bea.UserID, k.give(t, f, tm.bea.UserID)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.svc.RevokeAccess(ctx, tm.ana, alone, tm.ana.UserID, workspace.Flags{Read: true}); err != nil {
		t.Fatal(err)
	}

	// Nothing holds a suspension up: Bea is a team's owner beside Ana, the
	// last reader of a mailbox, and Pat an owner of the instance.
	f.suspend(t, tm.bea.UserID)
	f.suspend(t, pat.UserID)

	// While they are suspended they count for nothing: Ana is the team's
	// last owner and the last reader of the mailbox Bea read too, and Olga
	// the instance's last active owner.
	err := f.svc.RevokeAccess(ctx, tm.ana, shared, tm.ana.UserID, workspace.Flags{Read: true})
	wantCode(t, "Ana's read, Bea suspended", err, service.CodeConflict)
	member := "member"
	_, err = f.svc.SetMember(ctx, tm.ana, tm.id, tm.ana.UserID, service.MemberRequest{Role: &member})
	wantCode(t, "Ana stepping down, Bea suspended", err, service.CodeConflict)
	members, err := f.svc.ListMembers(ctx, tm.ana, tm.id)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		switch m.UserID {
		case tm.ana.UserID:
			if !m.LastOwner || !slices.Equal(m.LastReaderOf, []string{shared}) {
				t.Errorf("Ana is listed %+v, want the last owner and the last reader of %s", m, shared)
			}
		case tm.bea.UserID:
			if m.LastOwner || len(m.LastReaderOf) != 0 {
				t.Errorf("suspended Bea is listed %+v, protecting something", m)
			}
		}
	}
	_, err = f.svc.DisableUser(ctx, admin(), service.CloseUserRequest{Email: "olga@example.com"})
	wantCode(t, "disabling Olga, Pat suspended", err, service.CodeConflict)
	if _, err := f.users.Get(ctx, olga.UserID); err != nil {
		t.Fatal(err)
	}
}

func TestResumingRestoresReadingOnlyWithTheFlagAndAGrantAtTheCurrentEpoch(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	ctx := t.Context()
	// Bea reads four mailboxes before her suspension: two with a key, with
	// her grant, and two without, by the flag.
	shared, key := tm.keyedTeamMailbox(t, f, "support@mail.example")
	taken, takenKey := tm.keyedTeamMailbox(t, f, "orders@mail.example")
	const plain, keyedMeanwhile = "acc_00000000000000e3", "acc_00000000000000e4"
	tm.link(t, f, plain, "plain@mail.example")
	tm.link(t, f, keyedMeanwhile, "later@mail.example")
	for id, req := range map[string]service.GrantRequest{
		shared: key.give(t, f, tm.bea.UserID), taken: takenKey.give(t, f, tm.bea.UserID),
		plain: readFlag(""), keyedMeanwhile: readFlag(""),
	} {
		if _, err := f.svc.SetAccess(ctx, tm.ana, id, tm.bea.UserID, req); err != nil {
			t.Fatal(err)
		}
	}

	f.suspend(t, tm.bea.UserID)
	// Meanwhile: her read on one is taken, and another gets its first key,
	// which seals nothing to her.
	if err := f.svc.RevokeAccess(ctx, tm.ana, taken, tm.bea.UserID, workspace.Flags{}); err != nil {
		t.Fatalf("taking a suspended person's read: %v", err)
	}
	first := newBrowserKey(t, "", 1)
	if _, err := f.svc.WriteFirstKey(ctx, tm.ana, keyedMeanwhile, service.FirstKeyRequest{
		PublicKey: b64(first.public), Namespace: first.namespace,
		Grants: []service.GrantToRequest{first.grantTo(t, f, tm.ana.UserID)},
	}); err != nil {
		t.Fatal(err)
	}

	f.resume(t, tm.bea.UserID)
	bea := f.signInAgain(t, "bea@example.com")
	for id, want := range map[string]service.AccountAccess{
		shared: {Read: true}, plain: {Read: true}, keyedMeanwhile: {WaitingKey: true},
	} {
		a := accessOf(t, f, bea, id)
		if a.Access.Read != want.Read || a.Access.WaitingKey != want.WaitingKey {
			t.Errorf("once resumed, Bea on %s: %+v, want read %v, waiting %v", id, a.Access, want.Read, want.WaitingKey)
		}
	}
	if _, err := f.svc.GetAccount(ctx, bea, taken); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("once resumed, Bea sees the mailbox whose read was taken: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM mailbox_grants WHERE account_id = ? AND user_id = ?`, taken,
		tm.bea.UserID); n != 0 {
		t.Errorf("taking her read kept %d of her grants", n)
	}
	// Resuming someone active changes nothing.
	f.resume(t, tm.bea.UserID)
}

func TestResumingBringsBackNoKeyAndNoSession(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.person(t, "owner@example.com", auth.RoleOwner)
	bea := f.person(t, "bea@example.com", auth.RoleMember)
	phone := authtest.SignIn(t, f.users, "bea@example.com")
	prefix, secret := f.issue(t, bea.UserID)

	f.suspend(t, bea.UserID)
	f.resume(t, bea.UserID)

	for name, token := range map[string]string{"session": phone, "key": secret} {
		if _, err := f.svc.Authenticate(ctx, token, nil); service.CodeOf(err) != service.CodeUnauthorized {
			t.Errorf("resuming brought her %s back: %v", name, err)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM sessions WHERE user_id = ? AND revoked_at = 0`, bea.UserID); n != 0 {
		t.Errorf("resumed, she holds %d live sessions", n)
	}
	if stopped, reason := f.stopped(t, prefix); !stopped || reason != workspace.KeyStopCreatorSuspended {
		t.Errorf("resumed, her key is stopped %v for %q", stopped, reason)
	}
	// She signs in again, and that session works.
	again := authtest.SignIn(t, f.users, "bea@example.com")
	if _, err := f.svc.Authenticate(ctx, again, nil); err != nil {
		t.Errorf("signing in once resumed: %v", err)
	}
	// Resuming someone disabled here lifts nothing.
	if _, err := f.svc.DisableUser(ctx, admin(), service.CloseUserRequest{Email: "bea@example.com"}); err != nil {
		t.Fatal(err)
	}
	if active, err := f.svc.ResumePerson(ctx, bea.UserID); err != nil || active {
		t.Errorf("resuming someone disabled here: active %v, %v", active, err)
	}
	if got, err := f.svc.SuspendPerson(ctx, bea.UserID); err != nil || got.Suspended {
		t.Errorf("suspending someone disabled here: %+v, %v", got, err)
	}
	wantCode(t, "resuming someone unknown", func() error {
		_, err := f.svc.ResumePerson(ctx, "usr_0000000000000000")
		return err
	}(), service.CodeNotFound)
}

func TestASuspendedPersonsPersonalMailboxesPauseAndCatchUpOnResume(t *testing.T) {
	f, engine := newSyncFixture(t)
	ctx := t.Context()
	bea := f.person(t, "bea@example.com", auth.RoleMember)
	mine := f.mailbox(t, bea, "bea@mail.example")
	if _, err := f.svc.GrantSyncConsent(ctx, bea, f.consent().Sync); err != nil {
		t.Fatal(err)
	}
	f.seedMailbox(t, mine, "kept while away")
	if !f.eligible(t, mine) {
		t.Fatal("her mailbox does not sync before her suspension")
	}

	heard := len(engine.reconciledIDs())
	f.suspend(t, bea.UserID)
	if f.eligible(t, mine) {
		t.Error("a suspended person's mailbox still syncs")
	}
	if !slices.Contains(engine.reconciledIDs()[heard:], mine) {
		t.Error("the engine did not hear of her mailbox when she was suspended")
	}
	// Paused, not withdrawn: her index and her consent stay.
	if n := indexed(t, f, mine); n != 1 {
		t.Errorf("suspended, her mailbox keeps %d messages, want its index kept", n)
	}
	if n := f.count(t, `SELECT count(*) FROM users WHERE id = ? AND sync_consent_at <> 0`, bea.UserID); n != 1 {
		t.Error("suspending her withdrew her consent to sync")
	}

	heard = len(engine.reconciledIDs())
	f.resume(t, bea.UserID)
	if !f.eligible(t, mine) {
		t.Error("a resumed person's mailbox does not sync")
	}
	if !slices.Contains(engine.reconciledIDs()[heard:], mine) {
		t.Error("the engine did not hear of her mailbox when she was resumed")
	}
}

func TestATeamMailboxASuspendedPersonAloneReadsWaitsForThemRatherThanForNobody(t *testing.T) {
	f, engine := newSyncFixture(t)
	tm := newSupportTeam(t, f)
	ctx := t.Context()
	alone, key := tm.keyedTeamMailbox(t, f, "support@mail.example")
	tm.syncOn(t, f, alone)
	if _, err := f.svc.SetAccess(ctx, tm.ana, alone, tm.bea.UserID, key.give(t, f, tm.bea.UserID)); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RevokeAccess(ctx, tm.ana, alone, tm.ana.UserID, workspace.Flags{Read: true}); err != nil {
		t.Fatal(err)
	}
	f.seedMailbox(t, alone, "kept while away")

	heard := len(engine.reconciledIDs())
	f.suspend(t, tm.bea.UserID)
	if f.eligible(t, alone) {
		t.Error("a team mailbox only a suspended person reads still syncs")
	}
	if !slices.Contains(engine.reconciledIDs()[heard:], alone) {
		t.Error("the engine did not hear of the mailbox she alone read")
	}
	// Nobody reads it now, but it is not one nobody can ever read again.
	directory, err := f.svc.AccessDirectory(ctx, tm.ana, tm.id)
	if err != nil {
		t.Fatal(err)
	}
	if len(directory) != 1 || directory[0].Readers != 0 || directory[0].NoReader {
		t.Errorf("the directory shows %+v, want no reader now and none of the marks of a mailbox nobody can read", directory)
	}
	if _, unread, err := f.db.TeamSyncNotices(ctx); err != nil || slices.Contains(unread, alone) {
		t.Errorf("the start notices call it a mailbox nobody can read: %v (%v)", unread, err)
	}
	if n := indexed(t, f, alone); n != 1 {
		t.Errorf("suspended, its index holds %d messages, want it kept", n)
	}

	f.resume(t, tm.bea.UserID)
	if !f.eligible(t, alone) {
		t.Error("the mailbox does not sync once she is resumed")
	}
	// Disabled here, she would never read it again: it is marked.
	if _, err := f.svc.DisableUser(ctx, admin(), service.CloseUserRequest{Email: "bea@example.com", Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, unread, err := f.db.TeamSyncNotices(ctx); err != nil || !slices.Contains(unread, alone) {
		t.Errorf("disabled here, the start notices leave it out: %v (%v)", unread, err)
	}
}

func TestATeamMailboxSyncingUnderASuspendedPersonsOwnConsentPausesAndKeepsItsIndex(t *testing.T) {
	f, engine := newSyncFixture(t)
	tm := newSupportTeam(t, f)
	shared, _ := tm.keyedTeamMailbox(t, f, "support@mail.example")
	// The consent migration 0011 copied from Bea's own, still bound to her;
	// Ana reads the mailbox.
	migrated(t, f, shared, tm.bea.UserID)
	f.seedMailbox(t, shared, "kept while away")
	if !f.eligible(t, shared) {
		t.Fatal("the mailbox does not sync before her suspension")
	}

	heard := len(engine.reconciledIDs())
	f.suspend(t, tm.bea.UserID)
	if f.eligible(t, shared) {
		t.Error("a mailbox syncs under the consent of a suspended person")
	}
	if !slices.Contains(engine.reconciledIDs()[heard:], shared) {
		t.Error("the engine did not hear of the mailbox bound to her consent")
	}
	if n := indexed(t, f, shared); n != 1 {
		t.Errorf("suspended, its index holds %d messages, want it kept", n)
	}
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE id = ? AND sync_enabled_via = 'migration' AND
		sync_enabled_by = ? AND sync_enabled_at <> 0`, shared, tm.bea.UserID); n != 1 {
		t.Error("suspending her took the consent bound to her")
	}

	heard = len(engine.reconciledIDs())
	f.resume(t, tm.bea.UserID)
	if !f.eligible(t, shared) {
		t.Error("the mailbox does not sync once she is resumed")
	}
	if !slices.Contains(engine.reconciledIDs()[heard:], shared) {
		t.Error("the engine did not hear of the mailbox bound to her consent when she was resumed")
	}
}
