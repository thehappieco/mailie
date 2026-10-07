package service_test

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// A team mailbox belongs to its workspace: what closing a person, removing a
// mailbox and the team's consent to sync do to it.

// migrated gives a team mailbox the consent migration 0011 copies from its
// linker's own: bound to them, until an owner or an admin confirms it.
func migrated(t *testing.T, f *fixture, accountID, linker string) {
	t.Helper()
	f.exec(t, `UPDATE accounts SET sync_enabled_at = 1700000000, sync_enabled_by = ?, sync_consent_version = 'sync-1',
		sync_enabled_via = 'migration', linked_by = ? WHERE id = ?`, linker, linker, accountID)
}

func TestRemovingAMailboxNeedsItsIdRepeated(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	carol := tm.join(t, f, "carol@example.com", workspace.RoleAdmin)
	const shared, other = "acc_00000000000000aa", "acc_00000000000000bb"
	tm.link(t, f, shared, "support@mail.example")
	tm.link(t, f, other, "billing@mail.example")
	ctx := t.Context()
	remove := func(p service.Principal, id, confirm string) error {
		return f.svc.RemoveAccount(ctx, p, id, service.RemoveAccountRequest{Confirm: confirm})
	}

	for _, confirm := range []string{"", other, strings.ToUpper(shared), " " + shared} {
		wantCode(t, "removing with confirm "+confirm, remove(carol, shared, confirm), service.CodeBadRequest)
	}
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE id = ?`, shared); n != 1 {
		t.Fatal("a removal without its id repeated removed the mailbox")
	}
	// An admin removes a team's mailbox; a member, whatever they hold on
	// it, does not.
	tm.grant(t, shared, workspace.Flags{Read: true, Act: true, Send: true, Manage: true})
	wantCode(t, "Bea, a member, removing it", remove(tm.bea, shared, shared), service.CodeNotAuthorized)
	if err := remove(carol, shared, shared); err != nil {
		t.Fatalf("Carol, an admin, removing it: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE id = ?`, shared); n != 0 {
		t.Error("the mailbox is still there")
	}
	// A personal mailbox is its person's to remove, and an operator
	// mailbox the operator's.
	own := f.mailbox(t, tm.bea, "bea@mail.example")
	wantCode(t, "Ana removing Bea's own mailbox", remove(tm.ana, own, own), service.CodeNotFound)
	if err := remove(tm.bea, own, own); err != nil {
		t.Errorf("Bea removing her own mailbox: %v", err)
	}
	ops := f.mailbox(t, admin(), "ops@mail.example")
	wantCode(t, "the operator without confirm", remove(admin(), ops, ""), service.CodeBadRequest)
	if err := remove(admin(), ops, ops); err != nil {
		t.Errorf("the operator removing its mailbox: %v", err)
	}
}

func TestClosingAPersonNeverStopsOrRemovesATeamMailbox(t *testing.T) {
	// Ana linked the team's mailbox and gave the team's consent to sync it;
	// Bea reads it too. Disabling Ana, then deleting her, leaves the
	// mailbox, its index and its consent to the team.
	m := newMailFixture(t)
	tm := newSupportTeam(t, m.fixture)
	shared, box := m.ownedBoxIn(t, tm.ana, tm.id, "support@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("a", "Refund for order 4471"))
	m.index(t, shared, box)
	tm.grant(t, shared, workspace.Flags{Read: true})
	m.person(t, "keeper@example.com", auth.RoleOwner)
	ctx := t.Context()
	if _, err := m.svc.SetMember(ctx, tm.ana, tm.id, tm.bea.UserID, service.MemberRequest{Role: ptr("owner")}); err != nil {
		t.Fatal(err)
	}
	before := indexed(t, m.fixture, shared)

	if _, err := m.svc.DisableUser(ctx, admin(), service.CloseUserRequest{Email: "ana@example.com"}); err != nil {
		t.Fatalf("disabling Ana: %v", err)
	}
	if n := indexed(t, m.fixture, shared); n != before || !m.eligible(t, shared) {
		t.Errorf("after disabling its linker: %d messages (%d before), eligible %v", n, before, m.eligible(t, shared))
	}
	deleted, err := m.svc.DeleteUser(ctx, admin(), service.CloseUserRequest{Email: "ana@example.com"})
	if err != nil {
		t.Fatalf("deleting Ana: %v", err)
	}
	if deleted.AccountsRemoved != 0 {
		t.Errorf("deleting Ana removed %d mailboxes", deleted.AccountsRemoved)
	}
	if n := indexed(t, m.fixture, shared); n != before || !m.eligible(t, shared) {
		t.Errorf("after deleting its linker: %d messages (%d before), eligible %v", n, before, m.eligible(t, shared))
	}
	dir, err := m.svc.AccessDirectory(ctx, tm.bea, tm.id)
	if err != nil || len(dir) != 1 || dir[0].LinkedBy != "" || dir[0].Sync == nil || !dir[0].Sync.Enabled ||
		dir[0].Sync.EnabledBy != "" || dir[0].Sync.Version != m.consent().Sync {
		t.Errorf("the directory after Ana went: %+v (%v)", dir, err)
	}
}

func TestATeamMailboxWithNoReaderStopsSyncing(t *testing.T) {
	m := newMailFixture(t)
	tm := newSupportTeam(t, m.fixture)
	shared, box := m.ownedBoxIn(t, tm.ana, tm.id, "support@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("a", "Refund for order 4471"))
	m.index(t, shared, box)
	if !m.eligible(t, shared) {
		t.Fatal("not syncing to begin with")
	}
	// Nobody reads it: its consent stands, and it syncs nothing more.
	m.exec(t, `DELETE FROM mailbox_access WHERE account_id = ?`, shared)
	if m.eligible(t, shared) {
		t.Error("a team mailbox nobody can read is still eligible to sync")
	}
	if st, err := m.svc.SyncStatus(t.Context(), tm.ana, shared); err != nil || st.Enabled {
		t.Errorf("its sync, as its owner sees it: %+v, %v", st, err)
	}
}

func TestAForcedClosureLeavesAMailboxNobodyCanReadMarked(t *testing.T) {
	m := newMailFixture(t)
	tm := newSupportTeam(t, m.fixture)
	carol := tm.join(t, m.fixture, "carol@example.com", workspace.RoleOwner)
	shared, box := m.ownedBoxIn(t, tm.ana, tm.id, "support@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("a", "Refund for order 4471"))
	m.index(t, shared, box)
	m.person(t, "keeper@example.com", auth.RoleOwner)
	ctx := t.Context()

	_, err := m.svc.DisableUser(ctx, admin(), service.CloseUserRequest{Email: "ana@example.com"})
	wantCode(t, "disabling the last reader", err, service.CodeConflict)
	if _, err := m.svc.DisableUser(ctx, admin(), service.CloseUserRequest{Email: "ana@example.com", Force: true}); err != nil {
		t.Fatalf("disabling her with force: %v", err)
	}
	if m.eligible(t, shared) {
		t.Error("a team mailbox nobody can read still syncs")
	}
	dir, err := m.svc.AccessDirectory(ctx, carol, tm.id)
	if err != nil || len(dir) != 1 || !dir[0].NoReader || dir[0].Readers != 0 {
		t.Fatalf("the directory after a forced closure: %+v (%v)", dir, err)
	}
	// Nobody can give it read: read passes only from someone who reads it.
	_, err = m.svc.SetAccess(ctx, carol, shared, carol.UserID, grantRequest(true, false, false, false))
	wantCode(t, "Carol granting herself read on it", err, service.CodeNotAuthorized)
	// Its owners remove it; linking it again gives them read.
	if err := m.svc.RemoveAccount(ctx, carol, shared, service.RemoveAccountRequest{Confirm: shared}); err != nil {
		t.Errorf("Carol removing it: %v", err)
	}
}

func TestDisablingOrDeletingTheLastReaderOnTheInstanceIsRefusedWithoutForce(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
	f.person(t, "keeper@example.com", auth.RoleOwner)
	ctx := t.Context()
	// Bea owns the team too: only the mailbox stands in the way.
	if _, err := f.svc.SetMember(ctx, tm.ana, tm.id, tm.bea.UserID, service.MemberRequest{Role: ptr("owner")}); err != nil {
		t.Fatal(err)
	}
	req := service.CloseUserRequest{Email: "ana@example.com"}
	for name, call := range map[string]func() error{
		"disable": func() error { _, err := f.svc.DisableUser(ctx, admin(), req); return err },
		"delete":  func() error { _, err := f.svc.DeleteUser(ctx, admin(), req); return err },
	} {
		err := call()
		wantCode(t, name+" the last reader", err, service.CodeConflict)
		if msg := service.MessageOf(err); !strings.Contains(msg, shared) || strings.Contains(msg, tm.id) {
			t.Errorf("%s: the refusal names %q, want the mailbox alone", name, msg)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM users WHERE email = 'ana@example.com' AND status = 'active'`); n != 1 {
		t.Fatal("a refused closure changed Ana")
	}
	// Once Bea reads it, Ana goes, without force.
	tm.grant(t, shared, workspace.Flags{Read: true})
	if _, err := f.svc.DisableUser(ctx, admin(), req); err != nil {
		t.Errorf("disabling Ana once Bea reads it: %v", err)
	}
}

func TestConcurrentClosureAndRevocationAlwaysKeepAReader(t *testing.T) {
	// Ana and Bea read the team's mailbox. At the same time the operator
	// disables Ana and an owner revokes Bea's read: one of the two must be
	// refused, whichever commits first.
	for round := range 6 {
		f := newFixture(t)
		tm := newSupportTeam(t, f)
		carol := tm.join(t, f, "carol@example.com", workspace.RoleOwner)
		const shared = "acc_00000000000000aa"
		tm.link(t, f, shared, "support@mail.example")
		tm.grant(t, shared, workspace.Flags{Read: true})
		f.person(t, "keeper@example.com", auth.RoleOwner)
		ctx := t.Context()

		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, errs[0] = f.svc.DisableUser(ctx, admin(), service.CloseUserRequest{Email: "ana@example.com"})
		}()
		go func() {
			defer wg.Done()
			<-start
			errs[1] = f.svc.RevokeAccess(ctx, carol, shared, tm.bea.UserID, workspace.Flags{Read: true})
		}()
		close(start)
		wg.Wait()
		refused := 0
		for _, err := range errs {
			switch {
			case err == nil:
			case service.CodeOf(err) == service.CodeConflict:
				refused++
			default:
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if refused != 1 {
			t.Errorf("round %d: %d of the two were refused (%v), want one", round, refused, errs)
		}
		if n := f.count(t, `SELECT count(*) FROM mailbox_access g JOIN users u ON u.id = g.user_id
			WHERE g.account_id = ? AND g.read = 1 AND u.status = 'active'`, shared); n != 1 {
			t.Errorf("round %d: %d readers left, want 1", round, n)
		}
	}
}

func TestAMigratedTeamConsentStopsWhenItsLinkerWithdrawsUntilTheTeamConfirmsIt(t *testing.T) {
	// The upgrade copied Ana's own consent to the team's mailbox she
	// linked, under a text that promised turning her sync off deletes its
	// index. Until an owner or an admin confirms the team's consent, that
	// promise holds: withdrawing, being disabled, being deleted. Bea reads
	// it too, so closing Ana's account, which deletes the index Bea reads,
	// is refused without force and names the mailbox; her own withdrawal is
	// hers, and never refused.
	for _, how := range []string{"withdrawing", "disabled", "deleted"} {
		t.Run(how, func(t *testing.T) {
			m := newMailFixture(t)
			tm := newSupportTeam(t, m.fixture)
			shared, box := m.ownedBoxIn(t, tm.ana, tm.id, "support@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
			box.Deliver("INBOX", message("a", "Refund for order 4471"))
			m.index(t, shared, box)
			tm.grant(t, shared, workspace.Flags{Read: true})
			m.person(t, "keeper@example.com", auth.RoleOwner)
			ctx := t.Context()
			if _, err := m.svc.SetMember(ctx, tm.ana, tm.id, tm.bea.UserID, service.MemberRequest{Role: ptr("owner")}); err != nil {
				t.Fatal(err)
			}
			migrated(t, m.fixture, shared, tm.ana.UserID)
			if !m.eligible(t, shared) || indexed(t, m.fixture, shared) == 0 {
				t.Fatal("the migrated consent does not sync to begin with")
			}
			switch how {
			case "withdrawing":
				if _, err := m.svc.WithdrawSyncConsent(ctx, tm.ana); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				_, err := m.svc.DisableUser(ctx, admin(), service.CloseUserRequest{Email: "ana@example.com"})
				refusedForTheBoundMailbox(t, m, err, shared)
				disabled, err := m.svc.DisableUser(ctx, admin(), service.CloseUserRequest{Email: "ana@example.com", Force: true})
				if err != nil || !slices.Equal(disabled.TeamSyncsStopped, []string{shared}) {
					t.Fatalf("disabling Ana with force: %+v, %v", disabled, err)
				}
			case "deleted":
				_, err := m.svc.DeleteUser(ctx, admin(), service.CloseUserRequest{Email: "ana@example.com"})
				refusedForTheBoundMailbox(t, m, err, shared)
				deleted, err := m.svc.DeleteUser(ctx, admin(), service.CloseUserRequest{Email: "ana@example.com", Force: true})
				if err != nil || !slices.Equal(deleted.TeamSyncsStopped, []string{shared}) {
					t.Fatalf("deleting Ana with force: %+v, %v", deleted, err)
				}
			}
			if n := indexed(t, m.fixture, shared); n != 0 || m.eligible(t, shared) {
				t.Errorf("after Ana %s: %d messages indexed, eligible %v", how, n, m.eligible(t, shared))
			}
			if n := m.count(t, `SELECT count(*) FROM accounts WHERE id = ? AND sync_enabled_at = 0
				AND sync_enabled_by = '' AND sync_enabled_via = ''`, shared); n != 1 {
				t.Error("the team's mailbox kept its consent")
			}
			// The mailbox stays, the team's: Bea turns it on again for it.
			on := true
			if _, err := m.svc.SetMailboxSync(ctx, tm.bea, shared,
				service.MailboxSyncRequest{Enabled: &on, Version: m.consent().Sync}); err != nil {
				t.Errorf("Bea turning the team's sync on again: %v", err)
			}
		})
	}
}

// refusedForTheBoundMailbox checks that closing a linker was refused for a
// team mailbox still bound to their consent, which someone else reads, and
// that nothing of it changed.
func refusedForTheBoundMailbox(t *testing.T, m *mailFixture, err error, shared string) {
	t.Helper()
	wantCode(t, "closing the linker without force", err, service.CodeConflict)
	if msg := service.MessageOf(err); !strings.Contains(msg, shared) || !strings.Contains(msg, "consent") {
		t.Errorf("the refusal says %q, want the bound mailbox named", msg)
	}
	if n := m.count(t, `SELECT count(*) FROM users WHERE email = 'ana@example.com' AND status = 'active'`); n != 1 {
		t.Error("a refused closure changed Ana")
	}
	if indexed(t, m.fixture, shared) == 0 || !m.eligible(t, shared) {
		t.Error("a refused closure stopped the mailbox")
	}
}

func TestConfirmingAMigratedTeamConsentDetachesItFromTheLinker(t *testing.T) {
	m := newMailFixture(t)
	tm := newSupportTeam(t, m.fixture)
	carol := tm.join(t, m.fixture, "carol@example.com", workspace.RoleAdmin)
	shared, box := m.ownedBoxIn(t, tm.ana, tm.id, "support@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("a", "Refund for order 4471"))
	m.index(t, shared, box)
	tm.grant(t, shared, workspace.Flags{Read: true})
	migrated(t, m.fixture, shared, tm.ana.UserID)
	ctx := t.Context()

	dir, err := m.svc.AccessDirectory(ctx, carol, tm.id)
	if err != nil || len(dir) != 1 || !dir[0].Sync.Migrated || dir[0].Sync.EnabledBy != tm.ana.UserID || dir[0].Sync.Current {
		t.Fatalf("the migrated consent in the directory: %+v (%v)", dir, err)
	}
	// Carol, an admin who reads none of it, confirms it on the team's
	// behalf, at the current text.
	on := true
	if _, err := m.svc.SetMailboxSync(ctx, carol, shared, service.MailboxSyncRequest{Enabled: &on, Version: m.consent().Sync}); err != nil {
		t.Fatal(err)
	}
	dir, err = m.svc.AccessDirectory(ctx, carol, tm.id)
	if err != nil || dir[0].Sync.Migrated || dir[0].Sync.EnabledBy != carol.UserID || !dir[0].Sync.Current {
		t.Fatalf("the confirmed consent: %+v (%v)", dir[0].Sync, err)
	}
	before := indexed(t, m.fixture, shared)
	if before == 0 {
		t.Fatal("confirming deleted the index")
	}
	if _, err := m.svc.WithdrawSyncConsent(ctx, tm.ana); err != nil {
		t.Fatal(err)
	}
	if n := indexed(t, m.fixture, shared); n != before || !m.eligible(t, shared) {
		t.Errorf("Ana's withdrawal after the team confirmed: %d messages (%d before), eligible %v",
			n, before, m.eligible(t, shared))
	}
}

func TestAnOwnerLinksAnOAuthTeamMailboxCompletesItAndOnlyTheyCanComplete(t *testing.T) {
	f, idp := consentFixture(t, "https://console.mailie.example")
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	bea := f.person(t, "bea@example.com", auth.RoleMember)
	team, err := f.svc.CreateWorkspace(t.Context(), ana, service.CreateWorkspaceRequest{Name: "Support"})
	if err != nil {
		t.Fatal(err)
	}
	invite, err := f.svc.CreateTeamInvite(t.Context(), ana, team.ID, service.TeamInviteRequest{Email: "bea@example.com", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	_, code, _ := strings.Cut(invite.URL, "invite=")
	code, _, _ = strings.Cut(code, "&")
	if _, err := f.svc.AcceptInvite(t.Context(), bea, service.AcceptInviteRequest{Invite: code}); err != nil {
		t.Fatal(err)
	}

	added, err := f.svc.AddAccount(t.Context(), ana, service.AddAccountRequest{
		Email: "support@gmail.com", WorkspaceID: team.ID, SyncConsentVersion: f.consent().Sync,
	})
	if err != nil {
		t.Fatal(err)
	}
	if added.Auth == nil || added.Auth.Flow != "web" {
		t.Fatalf("linking an OAuth team mailbox: %+v", added.Auth)
	}
	stored := f.stateOfAccount(t, added.Account.ID)
	if stored.OwnerUserID != "" || stored.LinkedBy != ana.UserID || stored.WorkspaceID != team.ID {
		t.Fatalf("the team mailbox names %q, linked by %q, in %s", stored.OwnerUserID, stored.LinkedBy, stored.WorkspaceID)
	}
	redirect := "https://console.mailie.example/oauth/return?code=the-code&state=" + stateOf(t, added.Auth)
	// Bea is an owner of the team too, and manages the mailbox: the flow is
	// still Ana's alone, and an instance key's never.
	for name, p := range map[string]service.Principal{"Bea, another owner": bea, "an instance key": admin()} {
		if _, err := f.svc.CompleteOAuth(t.Context(), p, redirect); service.CodeOf(err) != service.CodeNotFound {
			t.Errorf("%s completing Ana's flow: %v, want not_found", name, err)
		}
	}
	if n := idp.exchangeCount(); n != 0 {
		t.Fatalf("the code was exchanged %d times for callers who did not start the flow", n)
	}
	a, err := f.svc.CompleteOAuth(t.Context(), ana, redirect)
	if err != nil {
		t.Fatalf("Ana finishing her flow: %v", err)
	}
	if a.State != "active" || !a.Access.Read || !a.Access.Manage || !a.Sync.Enabled {
		t.Errorf("the team mailbox once authorised: state %q, access %+v, sync %+v", a.State, a.Access, a.Sync)
	}
}

func TestDeletingTheOnlyMemberOfATeamStopsItsMailboxesWorkersAndAttempts(t *testing.T) {
	f, engine := newSyncFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	f.person(t, "keeper@example.com", auth.RoleOwner)
	alone, err := f.svc.CreateWorkspace(t.Context(), ana, service.CreateWorkspaceRequest{Name: "Alone"})
	if err != nil {
		t.Fatal(err)
	}
	req := f.passwordAccount(t, "alone@mail.example")
	req.WorkspaceID = alone.ID
	req.SyncConsentVersion = f.consent().Sync
	added, err := f.svc.AddAccount(t.Context(), ana, req)
	if err != nil {
		t.Fatal(err)
	}
	box := added.Account.ID
	f.seedIndex(t, box, "Walrus")
	f.exec(t, `INSERT INTO oauth_pending(state, account_id, owner_user_id, flow, redirect_uri, expires_at)
		VALUES ('st-alone', ?, ?, 'web', 'https://c.example/oauth/return', ?)`, box, ana.UserID, time.Now().Add(time.Hour).Unix())

	deleted, err := f.svc.DeleteUser(t.Context(), admin(), service.CloseUserRequest{Email: "ana@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if deleted.TeamsDeleted != 1 || deleted.AccountsRemoved != 1 {
		t.Errorf("deleted = %+v, want her team and its mailbox", deleted)
	}
	for what, n := range map[string]int{
		"the team":         f.count(t, `SELECT count(*) FROM workspaces WHERE id = ?`, alone.ID),
		"its mailbox":      f.count(t, `SELECT count(*) FROM accounts WHERE id = ?`, box),
		"its index":        f.count(t, `SELECT count(*) FROM messages WHERE account_id = ?`, box),
		"its credentials":  f.count(t, `SELECT count(*) FROM credentials WHERE account_id = ?`, box),
		"its consent flow": f.count(t, `SELECT count(*) FROM oauth_pending WHERE account_id = ?`, box),
	} {
		if n != 0 {
			t.Errorf("%s survived: %d rows", what, n)
		}
	}
	if !slices.Contains(engine.reconciledIDs(), box) {
		t.Error("the engine was not told the mailbox went")
	}
}

func TestADisabledLinkersTeamMailboxIsKeptStoppedWithItsIndex(t *testing.T) {
	// What migration 0011 leaves for a team mailbox whose linker it found
	// disabled: no consent, the index kept, bound to that person. An owner
	// or an admin turns it on again for the team, or it goes with them.
	m := newMailFixture(t)
	tm := newSupportTeam(t, m.fixture)
	shared, box := m.ownedBoxIn(t, tm.ana, tm.id, "support@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("a", "Refund for order 4471"))
	m.index(t, shared, box)
	eve := tm.join(t, m.fixture, "eve@example.com", workspace.RoleAdmin)
	m.exec(t, `UPDATE accounts SET sync_enabled_at = 0, sync_enabled_by = ?, sync_consent_version = '',
		sync_enabled_via = 'migration' WHERE id = ?`, eve.UserID, shared)
	if m.eligible(t, shared) || indexed(t, m.fixture, shared) == 0 {
		t.Fatal("the shape to begin with")
	}
	kept, _, err := m.db.TeamSyncNotices(t.Context())
	if err != nil || !slices.Equal(kept, []string{shared}) {
		t.Errorf("listed at start as kept stopped: %v (%v)", kept, err)
	}
	// Turning it off on the team's behalf deletes the index it kept.
	off := false
	if _, err := m.svc.SetMailboxSync(t.Context(), tm.ana, shared, service.MailboxSyncRequest{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if n := indexed(t, m.fixture, shared); n != 0 {
		t.Errorf("switching off kept %d messages", n)
	}
	if kept, _, err := m.db.TeamSyncNotices(t.Context()); err != nil || len(kept) != 0 {
		t.Errorf("still listed: %v (%v)", kept, err)
	}
}

func TestClosingTheLastReaderOfATeamThatOutlivesThemIsRefusedWithoutForce(t *testing.T) {
	// Ana owns Support and is the only one who reads its mailbox; Bea, its
	// other member, is off: her membership disabled, or her account. The
	// team outlives Ana either way, since it goes only with its last member,
	// and nobody could ever read its mailbox again: read passes only from a
	// reader. Closing Ana needs force.
	for _, how := range []string{"membership disabled", "disabled on the instance"} {
		t.Run(how, func(t *testing.T) {
			f := newFixture(t)
			tm := newSupportTeam(t, f)
			const shared = "acc_00000000000000aa"
			tm.link(t, f, shared, "support@mail.example")
			f.person(t, "keeper@example.com", auth.RoleOwner)
			ctx := t.Context()
			switch how {
			case "membership disabled":
				if _, err := f.svc.SetMember(ctx, tm.ana, tm.id, tm.bea.UserID, service.MemberRequest{Status: ptr("disabled")}); err != nil {
					t.Fatal(err)
				}
			case "disabled on the instance":
				if _, err := f.svc.DisableUser(ctx, admin(), service.CloseUserRequest{Email: "bea@example.com"}); err != nil {
					t.Fatal(err)
				}
			}
			req := service.CloseUserRequest{Email: "ana@example.com"}
			for name, call := range map[string]func() error{
				"disable": func() error { _, err := f.svc.DisableUser(ctx, admin(), req); return err },
				"delete":  func() error { _, err := f.svc.DeleteUser(ctx, admin(), req); return err },
			} {
				err := call()
				wantCode(t, name+" the last reader", err, service.CodeConflict)
				if msg := service.MessageOf(err); !strings.Contains(msg, shared) {
					t.Errorf("%s: the refusal says %q, want the mailbox named", name, msg)
				}
			}
			if n := f.count(t, `SELECT count(*) FROM users WHERE email = 'ana@example.com' AND status = 'active'`); n != 1 {
				t.Fatal("a refused closure changed Ana")
			}
			// With force she goes: the team outlives her, its mailbox with
			// it, marked as read by nobody.
			if _, err := f.svc.DeleteUser(ctx, admin(), service.CloseUserRequest{Email: "ana@example.com", Force: true}); err != nil {
				t.Fatalf("deleting Ana with force: %v", err)
			}
			if n := f.count(t, `SELECT count(*) FROM accounts a JOIN workspaces w ON w.id = a.workspace_id
				WHERE a.id = ? AND w.id = ?`, shared, tm.id); n != 1 {
				t.Error("the team, or its mailbox, went with Ana although Bea is still a member")
			}
		})
	}
}

func TestClosingTheOnlyMemberOfATeamIsNotRefusedForItsMailbox(t *testing.T) {
	// Alone in a team, Ana takes it with her when she is deleted: there is
	// nobody to leave unable to read its mailbox.
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	f.person(t, "keeper@example.com", auth.RoleOwner)
	alone, err := f.svc.CreateWorkspace(t.Context(), ana, service.CreateWorkspaceRequest{Name: "Alone"})
	if err != nil {
		t.Fatal(err)
	}
	req := f.passwordAccount(t, "alone@mail.example")
	req.WorkspaceID = alone.ID
	if _, err := f.svc.AddAccount(t.Context(), ana, req); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.DeleteUser(t.Context(), admin(), service.CloseUserRequest{Email: "ana@example.com"}); err != nil {
		t.Errorf("deleting the only member of a team: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM workspaces WHERE id = ?`, alone.ID); n != 0 {
		t.Error("her team outlived her")
	}
}

func TestConcurrentlyDisablingTheOtherMembersNeverLetsTheLastReaderGoWithoutForce(t *testing.T) {
	// Ana alone reads the team's mailbox; Bea is its other member. At the
	// same time the operator disables Bea's membership and Ana's account.
	// Whichever commits first, Ana's is refused: the team outlives her
	// either way, and its mailbox keeps its reader.
	for round := range 6 {
		f := newFixture(t)
		tm := newSupportTeam(t, f)
		const shared = "acc_00000000000000aa"
		tm.link(t, f, shared, "support@mail.example")
		f.person(t, "keeper@example.com", auth.RoleOwner)
		ctx := t.Context()

		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, errs[0] = f.svc.DisableUser(ctx, admin(), service.CloseUserRequest{Email: "ana@example.com"})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, errs[1] = f.svc.SetMember(ctx, admin(), tm.id, tm.bea.UserID, service.MemberRequest{Status: ptr("disabled")})
		}()
		close(start)
		wg.Wait()
		if service.CodeOf(errs[0]) != service.CodeConflict {
			t.Errorf("round %d: disabling Ana: %v, want a conflict", round, errs[0])
		}
		if errs[1] != nil {
			t.Errorf("round %d: disabling Bea's membership: %v", round, errs[1])
		}
		if n := f.count(t, `SELECT count(*) FROM mailbox_access g JOIN users u ON u.id = g.user_id
			WHERE g.account_id = ? AND g.read = 1 AND u.status = 'active'`, shared); n != 1 {
			t.Errorf("round %d: %d readers left, want 1", round, n)
		}
	}
}

func TestDisablingSomeoneAlreadyDisabledKeepsTheIndexKeptForThem(t *testing.T) {
	// Migration 0011 found Eve, who linked the team's mailbox, disabled: it
	// stays stopped, its index kept, bound to her. Disabling her again
	// changes nothing; it does not delete what the upgrade kept.
	m := newMailFixture(t)
	tm := newSupportTeam(t, m.fixture)
	shared, box := m.ownedBoxIn(t, tm.ana, tm.id, "support@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("a", "Refund for order 4471"))
	m.index(t, shared, box)
	eve := tm.join(t, m.fixture, "eve@example.com", workspace.RoleAdmin)
	m.person(t, "keeper@example.com", auth.RoleOwner)
	m.exec(t, `UPDATE users SET status = 'disabled' WHERE id = ?`, eve.UserID)
	m.exec(t, `UPDATE accounts SET sync_enabled_at = 0, sync_enabled_by = ?, sync_consent_version = '',
		sync_enabled_via = 'migration' WHERE id = ?`, eve.UserID, shared)
	before := indexed(t, m.fixture, shared)
	if before == 0 {
		t.Fatal("nothing indexed to begin with")
	}

	disabled, err := m.svc.DisableUser(t.Context(), admin(), service.CloseUserRequest{Email: "eve@example.com"})
	if err != nil || len(disabled.TeamSyncsStopped) != 0 {
		t.Fatalf("disabling Eve again: %+v, %v", disabled, err)
	}
	if n := indexed(t, m.fixture, shared); n != before {
		t.Errorf("disabling Eve again left %d of the %d messages kept", n, before)
	}
	if n := m.count(t, `SELECT count(*) FROM accounts WHERE id = ? AND sync_enabled_by = ? AND sync_enabled_via = 'migration'`,
		shared, eve.UserID); n != 1 {
		t.Error("disabling Eve again detached the mailbox from her")
	}
}

func TestATeamMailboxNobodyCanReadCannotBeTurnedOnAndStaysBound(t *testing.T) {
	// Eve linked the team's mailbox and was the only one who read it; the
	// upgrade found her disabled, and kept it stopped with its index, bound
	// to her. Nobody can ever read it again, nor be given read on it:
	// turning it on is refused, and leaves it bound, so deleting Eve still
	// deletes the index only she read.
	m := newMailFixture(t)
	tm := newSupportTeam(t, m.fixture)
	shared, box := m.ownedBoxIn(t, tm.ana, tm.id, "support@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("a", "Refund for order 4471"))
	m.index(t, shared, box)
	eve := tm.join(t, m.fixture, "eve@example.com", workspace.RoleAdmin)
	m.person(t, "keeper@example.com", auth.RoleOwner)
	if _, err := tm.ws.SetGrant(t.Context(), shared, eve.UserID, workspace.Flags{Read: true}, tm.ana.UserID, nil); err != nil {
		t.Fatal(err)
	}
	m.exec(t, `DELETE FROM mailbox_access WHERE account_id = ? AND user_id = ?`, shared, tm.ana.UserID)
	m.exec(t, `UPDATE users SET status = 'disabled' WHERE id = ?`, eve.UserID)
	m.exec(t, `UPDATE accounts SET sync_enabled_at = 0, sync_enabled_by = ?, sync_consent_version = '',
		sync_enabled_via = 'migration' WHERE id = ?`, eve.UserID, shared)
	kept, unread, err := m.db.TeamSyncNotices(t.Context())
	if err != nil || len(kept) != 0 || !slices.Equal(unread, []string{shared}) {
		t.Errorf("listed at start: kept %v, read by nobody %v (%v); want it only as read by nobody", kept, unread, err)
	}

	on := true
	_, err = m.svc.SetMailboxSync(t.Context(), tm.ana, shared, service.MailboxSyncRequest{Enabled: &on, Version: m.consent().Sync})
	wantCode(t, "turning on a mailbox nobody can read", err, service.CodeConflict)
	if n := m.count(t, `SELECT count(*) FROM accounts WHERE id = ? AND sync_enabled_at = 0 AND sync_enabled_by = ?
		AND sync_enabled_via = 'migration'`, shared, eve.UserID); n != 1 {
		t.Error("the refused switch changed the mailbox's consent")
	}
	if m.eligible(t, shared) || indexed(t, m.fixture, shared) == 0 {
		t.Fatal("the refused switch changed what is indexed")
	}
	// Nobody else reads it, so deleting Eve is not refused, and its index
	// goes with her.
	deleted, err := m.svc.DeleteUser(t.Context(), admin(), service.CloseUserRequest{Email: "eve@example.com"})
	if err != nil || !slices.Equal(deleted.TeamSyncsStopped, []string{shared}) {
		t.Fatalf("deleting Eve: %+v, %v", deleted, err)
	}
	if n := indexed(t, m.fixture, shared); n != 0 {
		t.Errorf("%d messages only Eve could read outlived her", n)
	}
}
