package service_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Removing a mailbox keeps it 30 days, recoverable by its workspace's
// owners, and tells its readers (docs/workspaces.md, "Removed mailboxes").

// connect gives a mailbox a stored credential, as a login that worked
// leaves: a mailbox without one never connected, and is deleted for good at
// once when removed.
func (f *fixture) connect(t *testing.T, accountID string) {
	t.Helper()
	if err := f.repo.SavePassword(t.Context(), accountID, "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
}

// removed removes a mailbox as p, its id repeated, and fails the test
// unless it is kept.
func (f *fixture) removed(t *testing.T, p service.Principal, accountID string) service.Removal {
	t.Helper()
	removal, err := f.svc.RemoveAccount(t.Context(), p, accountID, service.RemoveAccountRequest{Confirm: accountID})
	if err != nil || removal.Purged || removal.PurgeAfter == 0 {
		t.Fatalf("removing %s: %+v, %v, want it kept", accountID, removal, err)
	}
	return removal
}

// notices lists what p is told.
func (f *fixture) notices(t *testing.T, p service.Principal) []service.PersonNotice {
	t.Helper()
	got, err := f.svc.Notices(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// stream opens p's event stream, for its notices.
func (f *fixture) stream(t *testing.T, p service.Principal) *service.Stream {
	t.Helper()
	st, err := f.svc.Subscribe(t.Context(), p, 0, service.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

func newNotice(t *testing.T, st *service.Stream) bool {
	t.Helper()
	got, err := st.NewNotices(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestOnlyAnOwnerRemovesATeamMailbox(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()
	f.connect(t, tm.shared)
	tm.grant(t, tm.shared, workspace.Flags{Read: true, Act: true, Send: true, Manage: true})
	created, err := f.svc.CreateWorkspaceKey(ctx, tm.ana, tm.id, keyRequest("assistant", "read", reads(tm.shared)))
	if err != nil {
		t.Fatal(err)
	}
	k := f.authenticate(t, created.Key)
	outsider := f.person(t, "out@example.com", auth.RoleOwner)

	wantCode(t, "Bea, a member who manages it", f.removeErr(t, tm.bea, tm.shared), service.CodeNotAuthorized)
	wantCode(t, "Carol, an admin", f.removeErr(t, tm.carol, tm.shared), service.CodeNotAuthorized)
	wantCode(t, "the team's key", f.removeErr(t, k, tm.shared), service.CodeNotAuthorized)
	wantCode(t, "an owner of the instance outside the team", f.removeErr(t, outsider, tm.shared), service.CodeNotFound)
	wantCode(t, "the operator", f.removeErr(t, admin(), tm.shared), service.CodeNotFound)
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE id = ? AND removed_at = 0`, tm.shared); n != 1 {
		t.Fatal("a refused removal removed the mailbox")
	}
	removal := f.removed(t, tm.ana, tm.shared)
	if removal.AccountID != tm.shared || removal.PurgeAfter < time.Now().Add(30*24*time.Hour-time.Minute).Unix() {
		t.Errorf("Ana's removal: %+v, want it kept 30 days", removal)
	}
	// An admin made an owner removes them too.
	other := "acc_00000000000000bb"
	tm.link(t, f, other, "billing@mail.example")
	f.connect(t, other)
	if _, err := f.svc.SetMember(ctx, tm.ana, tm.id, tm.carol.UserID, service.MemberRequest{Role: ptr("owner")}); err != nil {
		t.Fatal(err)
	}
	f.removed(t, tm.carol, other)
}

func TestAnAdminMayAbandonOnlyTheirOwnLinkThatNeverConnected(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()
	link := func(id, email, linker string) {
		t.Helper()
		if _, err := f.repo.Create(ctx, account.Account{
			ID: id, WorkspaceID: tm.id, Email: email, Provider: provider.KindGmail, AuthKind: "oauth2",
			IMAPHost: "imap.gmail.com", IMAPPort: 993, SMTPHost: "smtp.gmail.com", SMTPPort: 587,
			SMTPTLS: "starttls", LoginUser: email,
		}, linker); err != nil {
			t.Fatal(err)
		}
	}
	const carols, anas, connected = "acc_00000000000000c1", "acc_00000000000000c2", "acc_00000000000000c3"
	link(carols, "carol.link@gmail.com", tm.carol.UserID)
	link(anas, "ana.link@gmail.com", tm.ana.UserID)
	link(connected, "carol.done@gmail.com", tm.carol.UserID)
	f.connect(t, connected)

	// Someone else's link that never connected is an owner's to abandon.
	wantCode(t, "Carol abandoning Ana's link", f.removeErr(t, tm.carol, anas), service.CodeNotAuthorized)
	// Her own one that connected is an owner's to remove.
	wantCode(t, "Carol removing her link that connected", f.removeErr(t, tm.carol, connected), service.CodeNotAuthorized)
	// Her own one that never connected she abandons, and it is gone at once.
	removal, err := f.svc.RemoveAccount(ctx, tm.carol, carols, service.RemoveAccountRequest{Confirm: carols})
	if err != nil || !removal.Purged || removal.PurgeAfter != 0 {
		t.Fatalf("Carol abandoning her own link: %+v, %v, want it deleted at once", removal, err)
	}
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE id = ?`, carols); n != 0 {
		t.Error("the abandoned link is still there")
	}
	listed, err := f.svc.ListRemovedMailboxes(ctx, tm.ana, tm.id)
	if err != nil || len(listed) != 0 {
		t.Errorf("Ana's removed mailboxes list %+v (%v): nothing of an abandoned link is kept", listed, err)
	}
	// Ana, an owner, abandons any link that never connected.
	if removal, err := f.svc.RemoveAccount(ctx, tm.ana, anas, service.RemoveAccountRequest{Confirm: anas}); err != nil || !removal.Purged {
		t.Errorf("Ana abandoning her link: %+v, %v", removal, err)
	}
}

func TestReadersAreToldOnceAndTheActorIsNot(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()
	f.connect(t, tm.shared)
	tm.grant(t, tm.shared, workspace.Flags{Read: true})
	streams := map[string]*service.Stream{"Ana": f.stream(t, tm.ana), "Bea": f.stream(t, tm.bea), "Carol": f.stream(t, tm.carol)}
	for name, st := range streams {
		if newNotice(t, st) {
			t.Errorf("%s is told of a notice before anything happened", name)
		}
	}

	removal := f.removed(t, tm.ana, tm.shared)
	// Bea, who read it, is told; Ana, who removed it, and Carol, an admin
	// who read nothing of it, are not.
	got := f.notices(t, tm.bea)
	if len(got) != 1 {
		t.Fatalf("Bea's notices: %+v", got)
	}
	n := got[0]
	if n.Kind != "removed" || n.AccountID != tm.shared || n.WorkspaceID != tm.id || n.WorkspaceName != "Support" ||
		n.Mailbox != "support@mail.example" || n.State != "removed" || n.RecoverableUntil != removal.PurgeAfter ||
		!strings.HasPrefix(n.ID, "mbe_") || n.At == 0 {
		t.Errorf("Bea's notice: %+v", n)
	}
	for name, p := range map[string]service.Principal{"Ana": tm.ana, "Carol": tm.carol} {
		if got := f.notices(t, p); len(got) != 0 {
			t.Errorf("%s is told %+v", name, got)
		}
	}
	// Her stream says so once; the others' say nothing.
	if !newNotice(t, streams["Bea"]) {
		t.Error("Bea's stream does not tell her of her notice")
	}
	if newNotice(t, streams["Bea"]) {
		t.Error("Bea's stream tells her of the same notice twice")
	}
	for _, name := range []string{"Ana", "Carol"} {
		if newNotice(t, streams[name]) {
			t.Errorf("%s's stream tells them of a notice they were not given", name)
		}
	}

	// Deleted for good, the notice names it by a masked hint of its
	// address, never the address.
	if err := f.svc.PurgeAccount(ctx, tm.ana, tm.shared, service.PurgeAccountRequest{Confirm: tm.shared}); err != nil {
		t.Fatal(err)
	}
	got = f.notices(t, tm.bea)
	if len(got) != 1 || got[0].State != "deleted" || got[0].Mailbox != "s…@mail.example" || got[0].RecoverableUntil != 0 {
		t.Errorf("Bea's notice once the mailbox is deleted for good: %+v", got)
	}
	// A deletion for good tells nobody anything new.
	if newNotice(t, streams["Bea"]) {
		t.Error("Bea's stream tells her of a notice the deletion for good did not give")
	}
	// Dismissed, it is gone; a key has no notices.
	if err := f.svc.DismissNotice(ctx, tm.bea, got[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := f.notices(t, tm.bea); len(got) != 0 {
		t.Errorf("a dismissed notice is still listed: %+v", got)
	}
	wantCode(t, "dismissing it again", f.svc.DismissNotice(ctx, tm.bea, got[0].ID), service.CodeNotFound)
	wantCode(t, "Ana dismissing Bea's notice", f.svc.DismissNotice(ctx, tm.ana, got[0].ID), service.CodeNotFound)
	_, err := f.svc.Notices(ctx, admin())
	wantCode(t, "a key reading notices", err, service.CodeNotAuthorized)
}

func TestRecoveringIsAnOwnersWithinThirtyDays(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()
	f.connect(t, tm.shared)
	tm.grant(t, tm.shared, workspace.Flags{Read: true})
	f.removed(t, tm.ana, tm.shared)
	created, err := f.svc.CreateWorkspaceKey(ctx, tm.ana, tm.id, keyRequest("assistant", "read"))
	if err != nil {
		t.Fatal(err)
	}
	k := f.authenticate(t, created.Key)

	// Its owner sees it, with who would read it again; nobody else does.
	listed, err := f.svc.ListRemovedMailboxes(ctx, tm.ana, tm.id)
	if err != nil || len(listed) != 1 || listed[0].ID != tm.shared || listed[0].Email != "support@mail.example" ||
		listed[0].RemovedBy != tm.ana.UserID || listed[0].Readers != 2 {
		t.Fatalf("Ana's removed mailboxes: %+v (%v)", listed, err)
	}
	for name, p := range map[string]service.Principal{"Carol, an admin": tm.carol, "Bea, a member": tm.bea, "the team's key": k} {
		_, err := f.svc.ListRemovedMailboxes(ctx, p, tm.id)
		wantCode(t, name+" listing the removed mailboxes", err, service.CodeNotAuthorized)
		_, err = f.svc.RecoverAccount(ctx, p, tm.shared)
		if p.IsSession() {
			wantCode(t, name+" recovering it", err, service.CodeNotFound)
		} else {
			wantCode(t, name+" recovering it", err, service.CodeNotAuthorized)
		}
	}
	// The operator lists and deletes any removed mailbox for good, and never
	// recovers one of a person or a team.
	if listed, err := f.svc.ListRemovedMailboxes(ctx, admin(), tm.id); err != nil || len(listed) != 1 {
		t.Errorf("the operator's list of the team's removed mailboxes: %+v (%v)", listed, err)
	}
	_, err = f.svc.RecoverAccount(ctx, admin(), tm.shared)
	wantCode(t, "the operator recovering it", err, service.CodeNotAuthorized)

	// In a closed team, nothing is recovered.
	f.closeTeam(t, tm.id)
	_, err = f.svc.RecoverAccount(ctx, tm.ana, tm.shared)
	wantClosed(t, "recovering it in the closed team", err)
	f.reopenTeam(t, tm.id)

	// Past its 30 days, it is not recovered either.
	f.repo.WithClock(func() time.Time { return time.Now().Add(30 * 24 * time.Hour) })
	_, err = f.svc.RecoverAccount(ctx, tm.ana, tm.shared)
	wantCode(t, "recovering it past its 30 days", err, service.CodeConflict)
	f.repo.WithClock(time.Now)

	recovered, err := f.svc.RecoverAccount(ctx, tm.ana, tm.shared)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID != tm.shared || !recovered.Sync.Held || recovered.Sync.Enabled {
		t.Errorf("the recovered mailbox: %+v, want it held", recovered)
	}
	if _, err := f.svc.GetAccount(ctx, tm.bea, tm.shared); err != nil {
		t.Errorf("Bea does not see the recovered mailbox: %v", err)
	}
	_, err = f.svc.RecoverAccount(ctx, tm.ana, tm.shared)
	wantCode(t, "recovering it twice", err, service.CodeNotFound)

	// A personal mailbox is its person's to recover, and an operator
	// mailbox the operator's.
	own := f.mailbox(t, tm.bea, "bea@mail.example")
	ops := f.mailbox(t, admin(), "ops@mail.example")
	f.removed(t, tm.bea, own)
	f.removed(t, admin(), ops)
	_, err = f.svc.RecoverAccount(ctx, tm.ana, own)
	wantCode(t, "Ana recovering Bea's own mailbox", err, service.CodeNotFound)
	_, err = f.svc.ListRemovedMailboxes(ctx, tm.ana, authPersonal(t, f, tm.bea))
	wantCode(t, "Ana listing Bea's removed mailboxes", err, service.CodeNotFound)
	if _, err := f.svc.RecoverAccount(ctx, tm.bea, own); err != nil {
		t.Errorf("Bea recovering her own mailbox: %v", err)
	}
	if _, err := f.svc.RecoverAccount(ctx, admin(), ops); err != nil {
		t.Errorf("the operator recovering its own mailbox: %v", err)
	}
}

func TestARecoveredMailboxStaysOffUntilSomeoneWhoMayTurnsItOn(t *testing.T) {
	m := newMailFixture(t)
	tm := newSupportTeam(t, m.fixture)
	carol := tm.join(t, m.fixture, "carol@example.com", workspace.RoleAdmin)
	ctx := t.Context()
	shared, box := m.ownedBoxIn(t, tm.ana, tm.id, "support@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("a", "Refund for order 4471"))
	m.index(t, shared, box)
	m.connect(t, shared)
	tm.grant(t, shared, workspace.Flags{Read: true, Manage: true})
	own, ownBox := m.ownedBox(t, tm.ana, "ana@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	ownBox.Deliver("INBOX", message("b", "Dentist on Tuesday"))
	m.index(t, own, ownBox)
	m.connect(t, own)
	ops, opsBox := m.fakeAccount(t, service.Principal{}, "ops@mail.example")
	m.index(t, ops, opsBox)
	m.connect(t, ops)
	kept := map[string]int{}
	for _, id := range []string{shared, own, ops} {
		if !m.eligible(t, id) {
			t.Fatalf("%s does not sync to begin with", id)
		}
		kept[id] = indexed(t, m.fixture, id)
	}
	m.removed(t, tm.ana, shared)
	m.removed(t, tm.ana, own)
	m.removed(t, admin(), ops)
	for _, recover := range []struct {
		p  service.Principal
		id string
	}{{tm.ana, shared}, {tm.ana, own}, {admin(), ops}} {
		if _, err := m.svc.RecoverAccount(ctx, recover.p, recover.id); err != nil {
			t.Fatal(err)
		}
	}

	// Recovered, each keeps its index and syncs nothing until it is turned
	// on, whatever its consent says.
	for _, id := range []string{shared, own, ops} {
		if m.eligible(t, id) {
			t.Errorf("%s syncs as soon as it is recovered", id)
		}
		if n := indexed(t, m.fixture, id); n != kept[id] {
			t.Errorf("%s keeps %d messages of its index, want %d", id, n, kept[id])
		}
	}
	st, err := m.svc.SyncStatus(ctx, tm.bea, shared)
	if err != nil || st.Enabled || !st.Held || st.HeldSince == 0 {
		t.Errorf("its sync, as Bea sees it: %+v (%v), want it held", st, err)
	}
	err = m.svc.TriggerSync(ctx, tm.ana, shared)
	if service.CodeOf(err) != service.CodeConflict || !strings.Contains(service.MessageOf(err), "recovered") {
		t.Errorf("asking a held mailbox to sync: %v", err)
	}

	// A team's: its owners and admins turn it on, to the current text; a
	// member, whatever they manage, does not.
	on := true
	_, err = m.svc.SetMailboxSync(ctx, tm.bea, shared, service.MailboxSyncRequest{Enabled: &on, Version: m.consent().Sync})
	wantCode(t, "Bea turning it on", err, service.CodeNotAuthorized)
	st, err = m.svc.SetMailboxSync(ctx, carol, shared, service.MailboxSyncRequest{Enabled: &on, Version: m.consent().Sync})
	if err != nil || !st.Enabled || st.Held {
		t.Errorf("Carol turning it on: %+v (%v)", st, err)
	}
	// A personal one: its person, with no text.
	_, err = m.svc.SetMailboxSync(ctx, tm.ana, own, service.MailboxSyncRequest{Enabled: &on, Version: m.consent().Sync})
	wantCode(t, "Ana turning hers on with a text", err, service.CodeBadRequest)
	off := false
	_, err = m.svc.SetMailboxSync(ctx, tm.ana, own, service.MailboxSyncRequest{Enabled: &off})
	wantCode(t, "Ana turning hers off here", err, service.CodeBadRequest)
	st, err = m.svc.SetMailboxSync(ctx, tm.ana, own, service.MailboxSyncRequest{Enabled: &on})
	if err != nil || !st.Enabled || st.Held {
		t.Errorf("Ana turning hers on: %+v (%v)", st, err)
	}
	// An operator mailbox: the operator.
	st, err = m.svc.SetMailboxSync(ctx, admin(), ops, switchSync(true))
	if err != nil || !st.Enabled || st.Held {
		t.Errorf("the operator turning its own on: %+v (%v)", st, err)
	}
	for _, id := range []string{shared, own, ops} {
		if !m.eligible(t, id) {
			t.Errorf("%s does not sync once turned on", id)
		}
		if n := indexed(t, m.fixture, id); n != kept[id] {
			t.Errorf("%s catches up from %d messages, want the %d it kept", id, n, kept[id])
		}
	}
}

func TestRecoveryRestoresQualifyingReadersButNoKeyAccess(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()
	f.connect(t, tm.shared)
	tm.grant(t, tm.shared, workspace.Flags{Read: true, Act: true})
	dan := tm.join(t, f, "dan@example.com", workspace.RoleMember)
	if _, err := f.svc.SetAccess(ctx, tm.ana, tm.shared, dan.UserID, grantRequest(true, false, false, false)); err != nil {
		t.Fatal(err)
	}
	created, err := f.svc.CreateWorkspaceKey(ctx, tm.ana, tm.id, keyRequest("assistant", "read", reads(tm.shared)))
	if err != nil {
		t.Fatal(err)
	}
	k := f.authenticate(t, created.Key)
	if !slices.Contains(ids(t, f, k), tm.shared) {
		t.Fatal("the key does not reach the mailbox to begin with")
	}

	f.removed(t, tm.ana, tm.shared)
	// Removed, it protects no reader: Dan leaves the team meanwhile.
	if err := f.svc.RemoveMember(ctx, tm.ana, tm.id, dan.UserID); err != nil {
		t.Fatalf("removing Dan while the mailbox is removed: %v", err)
	}
	if _, err := f.svc.RecoverAccount(ctx, tm.ana, tm.shared); err != nil {
		t.Fatal(err)
	}
	// Bea still qualifies, and reads it again with what she held; Dan does
	// not; the key reaches nothing, and nothing gives it the mailbox back.
	if a := accessOf(t, f, tm.bea, tm.shared); !a.Access.Read || !a.Access.Act {
		t.Errorf("Bea holds %+v on the recovered mailbox", a.Access)
	}
	if slices.Contains(ids(t, f, dan), tm.shared) {
		t.Error("Dan, who left the team, sees the recovered mailbox")
	}
	if got := ids(t, f, k); len(got) != 0 {
		t.Errorf("the key reaches %v after the recovery", got)
	}

	// A team mailbox nobody who could read it is left to read is not
	// recovered: it is deleted for good and linked again.
	alone := "acc_00000000000000dd"
	tm.link(t, f, alone, "alone@mail.example")
	f.connect(t, alone)
	tm.grant(t, alone, workspace.Flags{Read: true})
	f.exec(t, `DELETE FROM mailbox_access WHERE account_id = ? AND user_id = ?`, alone, tm.ana.UserID)
	f.removed(t, tm.ana, alone)
	if err := f.svc.RemoveMember(ctx, tm.ana, tm.id, tm.bea.UserID); err != nil {
		t.Fatalf("removing Bea, its last reader, while it is removed: %v", err)
	}
	listed, err := f.svc.ListRemovedMailboxes(ctx, tm.ana, tm.id)
	if err != nil || len(listed) != 1 || listed[0].Readers != 0 {
		t.Errorf("the removed mailboxes: %+v (%v), want one nobody would read", listed, err)
	}
	_, err = f.svc.RecoverAccount(ctx, tm.ana, alone)
	wantCode(t, "recovering the mailbox nobody can read", err, service.CodeConflict)
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE id = ? AND removed_at <> 0`, alone); n != 1 {
		t.Error("the refused recovery changed the mailbox")
	}
	if err := f.svc.PurgeAccount(ctx, tm.ana, alone, service.PurgeAccountRequest{Confirm: alone}); err != nil {
		t.Errorf("deleting it for good: %v", err)
	}
}

func TestTurningOffATeamMailboxsSyncTellsItsOtherReaders(t *testing.T) {
	m := newMailFixture(t)
	tm := newSupportTeam(t, m.fixture)
	carol := tm.join(t, m.fixture, "carol@example.com", workspace.RoleAdmin)
	ctx := t.Context()
	shared, box := m.ownedBoxIn(t, tm.ana, tm.id, "support@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("a", "Refund for order 4471"))
	m.index(t, shared, box)
	m.connect(t, shared)
	tm.grant(t, shared, workspace.Flags{Read: true})
	off := false

	// Carol, an admin who reads nothing of it, turns it off: both its
	// readers are told.
	if _, err := m.svc.SetMailboxSync(ctx, carol, shared, service.MailboxSyncRequest{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]service.Principal{"Ana": tm.ana, "Bea": tm.bea} {
		got := m.notices(t, p)
		if len(got) != 1 || got[0].Kind != "sync_off" || got[0].State != "live" || got[0].Mailbox != "support@mail.example" {
			t.Errorf("%s's notices: %+v", name, got)
		}
	}
	// Off already, turning it off again tells nobody anything.
	if _, err := m.svc.SetMailboxSync(ctx, carol, shared, service.MailboxSyncRequest{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if got := m.notices(t, tm.bea); len(got) != 1 {
		t.Errorf("Bea is told %d times", len(got))
	}
	// Ana turns it on, and off: Bea is told, Ana is not.
	tm.syncOn(t, m.fixture, shared)
	if _, err := m.svc.SetMailboxSync(ctx, tm.ana, shared, service.MailboxSyncRequest{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if got := m.notices(t, tm.bea); len(got) != 2 {
		t.Errorf("Bea's notices after Ana turned it off: %+v", got)
	}
	if got := m.notices(t, tm.ana); len(got) != 1 {
		t.Errorf("Ana is told of what she did: %+v", got)
	}

	// A consent still bound to a person, which they withdraw, tells the
	// mailbox's other readers too.
	migrated(t, m.fixture, shared, tm.bea.UserID)
	stream := m.stream(t, tm.ana)
	if _, err := m.svc.WithdrawSyncConsent(ctx, tm.bea); err != nil {
		t.Fatal(err)
	}
	if got := m.notices(t, tm.ana); len(got) != 2 || got[0].Kind != "sync_off" {
		t.Errorf("Ana's notices after Bea withdrew the consent bound to her: %+v", got)
	}
	if got := m.notices(t, tm.bea); len(got) != 2 {
		t.Errorf("Bea is told of her own withdrawal: %+v", got)
	}
	if !newNotice(t, stream) {
		t.Error("Ana's stream does not tell her of the notice")
	}

	// Removed, its owners still turn it off, which deletes the index it
	// kept; an admin does not.
	tm.syncOn(t, m.fixture, shared)
	m.index(t, shared, box)
	m.removed(t, tm.ana, shared)
	_, err := m.svc.SetMailboxSync(ctx, carol, shared, service.MailboxSyncRequest{Enabled: &off})
	wantCode(t, "Carol turning the removed mailbox off", err, service.CodeNotFound)
	on := true
	_, err = m.svc.SetMailboxSync(ctx, tm.ana, shared, service.MailboxSyncRequest{Enabled: &on, Version: m.consent().Sync})
	wantCode(t, "Ana turning the removed mailbox on", err, service.CodeNotFound)
	if _, err := m.svc.SetMailboxSync(ctx, tm.ana, shared, service.MailboxSyncRequest{Enabled: &off}); err != nil {
		t.Fatalf("Ana turning the removed mailbox off: %v", err)
	}
	if n := indexed(t, m.fixture, shared); n != 0 {
		t.Errorf("the removed mailbox keeps %d messages", n)
	}
	listed, err := m.svc.ListRemovedMailboxes(ctx, tm.ana, tm.id)
	if err != nil || len(listed) != 1 || listed[0].IndexKept {
		t.Errorf("the removed mailboxes: %+v (%v), want its index gone", listed, err)
	}
}

func TestClosingAPersonPurgesTheirRemovedMailboxesAtOnce(t *testing.T) {
	f := newFixture(t)
	f.person(t, "keeper@example.com", auth.RoleOwner)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	own := f.mailbox(t, ana, "ana@mail.example")
	f.seedMailbox(t, own, "Quarterly pelican census")
	// A team she is alone in goes with her, its removed mailbox too.
	ws := workspace.NewRepository(f.db, nil)
	team, err := ws.CreateTeam(t.Context(), "Solo", ana.UserID, nil)
	if err != nil {
		t.Fatal(err)
	}
	const solo = "acc_00000000000000e1"
	(supportTeam{ws: ws, db: f.db, ana: ana, id: team.ID}).link(t, f, solo, "solo@mail.example")
	f.connect(t, solo)
	f.removed(t, ana, own)
	f.removed(t, ana, solo)

	deleted, err := f.svc.DeleteUser(t.Context(), admin(), service.CloseUserRequest{Email: "ana@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if deleted.AccountsRemoved != 2 {
		t.Errorf("deleting Ana deleted %d mailboxes, want her two removed ones", deleted.AccountsRemoved)
	}
	for _, id := range []string{own, solo} {
		if n := f.count(t, `SELECT count(*) FROM accounts WHERE id = ?`, id) +
			f.count(t, `SELECT count(*) FROM mailbox_events WHERE account_id = ?`, id); n != 0 {
			t.Errorf("%s, removed, and its journal outlive her (%d rows)", id, n)
		}
	}
	if found := f.onDisk(t, "ana@mail.example", "pelican"); len(found) > 0 {
		t.Errorf("deleted with her, but still on disk:\n  %s", strings.Join(found, "\n  "))
	}
}

func TestDeletingForGoodIsForAnOwnerOrTheOperatorAndOnlyOfARemovedMailbox(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()
	f.connect(t, tm.shared)
	purge := func(p service.Principal, id string) error {
		return f.svc.PurgeAccount(ctx, p, id, service.PurgeAccountRequest{Confirm: id})
	}
	// A live mailbox is removed first.
	wantCode(t, "Ana deleting the live mailbox for good", purge(tm.ana, tm.shared), service.CodeConflict)
	wantCode(t, "Bea deleting the live mailbox for good", purge(tm.bea, tm.shared), service.CodeNotFound)
	f.removed(t, tm.ana, tm.shared)
	wantCode(t, "Carol, an admin", purge(tm.carol, tm.shared), service.CodeNotFound)
	wantCode(t, "Bea, a member", purge(tm.bea, tm.shared), service.CodeNotFound)
	// The operator deletes any removed mailbox for good, a closed team's
	// included: its owners no longer can.
	f.closeTeam(t, tm.id)
	wantClosed(t, "Ana in the closed team", purge(tm.ana, tm.shared))
	if err := purge(admin(), tm.shared); err != nil {
		t.Fatalf("the operator deleting it for good: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE id = ?`, tm.shared); n != 0 {
		t.Error("the mailbox is still there")
	}
	if n := f.count(t, `SELECT count(*) FROM mailbox_events WHERE account_id = ? AND kind = 'purged' AND actor = ?`,
		tm.shared, admin().Actor()); n != 1 {
		t.Error("the deletion for good is not journaled as the operator's")
	}
}

// authPersonal is a person's personal workspace.
func authPersonal(t *testing.T, f *fixture, p service.Principal) string {
	t.Helper()
	w, err := workspace.NewRepository(f.db, nil).PersonalOf(t.Context(), p.UserID)
	if err != nil {
		t.Fatal(err)
	}
	return w.ID
}
