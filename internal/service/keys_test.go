package service_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// keyOf makes a key of a workspace — p's personal one unless named — that p
// created, holding every mailbox of it p reads, with read, act where the
// scope allows and p holds act, send where the scope allows and p holds
// send; and returns the principal it authenticates as. The scene is set as
// the test needs it: who may give what is the service's routes' to check,
// and these are tested on their own. A workspace key has at most the send
// scope; the admin scope asked for is the send scope.
func keyOf(t *testing.T, f *fixture, p service.Principal, scope auth.Scope, workspaceID ...string) service.Principal {
	t.Helper()
	ws := authtest.Personal(t, f.db, p.UserID)
	if len(workspaceID) > 0 {
		ws = workspaceID[0]
	}
	if scope == auth.ScopeAdmin {
		scope = auth.ScopeSend
	}
	accounts, err := f.svc.ListAccounts(t.Context(), p, ws)
	if err != nil {
		t.Fatal(err)
	}
	var grants []workspace.KeyGrant
	for _, a := range accounts {
		if !a.Access.Read {
			continue
		}
		grants = append(grants, workspace.KeyGrant{AccountID: a.ID, Flags: workspace.Flags{
			Read: true, Act: a.Access.Act && scope.Covers(auth.ScopeWrite), Send: a.Access.Send && scope.Covers(auth.ScopeSend),
		}})
	}
	return f.authenticate(t, authtest.NewWorkspaceKey(t, f.db, scope, ws, p.UserID, grants...))
}

func (f *fixture) authenticate(t *testing.T, presented string) service.Principal {
	t.Helper()
	p, err := f.svc.Authenticate(t.Context(), presented, nil)
	if err != nil {
		t.Fatalf("authenticate a key: %v", err)
	}
	return p
}

// keyRequest is a key of the current terms holding what mailboxes name.
func keyRequest(name, scope string, mailboxes ...service.KeyMailboxRequest) service.WorkspaceKeyRequest {
	return service.WorkspaceKeyRequest{Name: name, Scope: scope, TermsVersion: service.DefaultKeyTermsVersion, Mailboxes: mailboxes}
}

func reads(accountID string) service.KeyMailboxRequest {
	return service.KeyMailboxRequest{AccountID: accountID, Read: true}
}

func keyAccess(read, act, send bool) service.KeyAccessRequest {
	return service.KeyAccessRequest{Read: &read, Act: &act, Send: &send}
}

// keysTeam is the Support team with its mailbox, which Ana, its owner,
// reads; Carol, an admin who reads nothing; and Bea, a member.
type keysTeam struct {
	supportTeam
	carol  service.Principal
	shared string
}

func newKeysTeam(t *testing.T, f *fixture) keysTeam {
	t.Helper()
	tm := keysTeam{supportTeam: newSupportTeam(t, f), shared: "acc_00000000000000aa"}
	tm.carol = tm.join(t, f, "carol@example.com", workspace.RoleAdmin)
	tm.link(t, f, tm.shared, "support@mail.example")
	return tm
}

func TestOnlyAnOwnerOrAdminSignedInCreatesAWorkspacesKeys(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()

	_, err := f.svc.CreateWorkspaceKey(ctx, tm.bea, tm.id, keyRequest("bea's", "read"))
	wantCode(t, "a member creating a key", err, service.CodeNotAuthorized)
	_, err = f.svc.ListWorkspaceKeys(ctx, tm.bea, tm.id)
	wantCode(t, "a member listing the keys", err, service.CodeNotAuthorized)
	outsider := f.person(t, "out@example.com", auth.RoleOwner)
	_, err = f.svc.CreateWorkspaceKey(ctx, outsider, tm.id, keyRequest("out", "read"))
	wantCode(t, "someone outside the team, an owner of the instance", err, service.CodeNotFound)
	_, err = f.svc.CreateWorkspaceKey(ctx, admin(), tm.id, keyRequest("op", "read"))
	wantCode(t, "the operator", err, service.CodeNotAuthorized)

	for name, p := range map[string]service.Principal{"Ana, the owner": tm.ana, "Carol, an admin": tm.carol} {
		created, err := f.svc.CreateWorkspaceKey(ctx, p, tm.id, keyRequest(name, "read"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.HasPrefix(created.Key, created.Prefix+".") || created.WorkspaceID != tm.id ||
			created.CreatedBy != p.UserID || created.TermsVersion != service.DefaultKeyTermsVersion || !created.Live {
			t.Errorf("%s created %+v", name, created.WorkspaceKey)
		}
		if days := time.Unix(created.ExpiresAt, 0).Sub(time.Unix(created.CreatedAt, 0)).Hours() / 24; days != 90 {
			t.Errorf("a key without a lifetime lives %v days, want 90", days)
		}
		k, err := f.svc.Authenticate(ctx, created.Key, nil)
		if err != nil || k.UserID != "" || k.WorkspaceID != tm.id || k.IsInstance() {
			t.Errorf("%s's key authenticates as %+v (%v): a key of the team, acting as nobody", name, k, err)
		}
	}
	// In a personal workspace, its person.
	if _, err := f.svc.CreateWorkspaceKey(ctx, tm.bea, authtest.Personal(t, f.db, tm.bea.UserID), keyRequest("bea's own", "read")); err != nil {
		t.Errorf("Bea creating a key in her personal workspace: %v", err)
	}
	_, err = f.svc.CreateWorkspaceKey(ctx, tm.ana, authtest.Personal(t, f.db, tm.bea.UserID), keyRequest("x", "read"))
	wantCode(t, "Ana in Bea's personal workspace", err, service.CodeNotFound)
}

func TestAKeyNeverMintsListsOrChangesAKey(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()
	created, err := f.svc.CreateWorkspaceKey(ctx, tm.ana, tm.id, keyRequest("assistant", "send", reads(tm.shared)))
	if err != nil {
		t.Fatal(err)
	}
	k := f.authenticate(t, created.Key)
	_, err = f.svc.CreateWorkspaceKey(ctx, k, tm.id, keyRequest("minted", "read"))
	wantCode(t, "a key creating a key", err, service.CodeNotAuthorized)
	_, err = f.svc.ListWorkspaceKeys(ctx, k, tm.id)
	wantCode(t, "a key listing keys", err, service.CodeNotAuthorized)
	wantCode(t, "a key revoking a key", f.svc.RevokeWorkspaceKey(ctx, k, tm.id, created.Prefix), service.CodeNotAuthorized)
	_, err = f.svc.SetKeyAccess(ctx, k, tm.id, created.Prefix, tm.shared, keyAccess(true, true, true))
	wantCode(t, "a key widening a key", err, service.CodeNotAuthorized)
	_, err = f.svc.ListMyAPIKeys(ctx, k)
	wantCode(t, "a key listing its creator's keys", err, service.CodeNotAuthorized)
	wantCode(t, "a key revoking its creator's key", f.svc.RevokeMyAPIKey(ctx, k, created.Prefix), service.CodeNotAuthorized)
	// Nor through the operator's route, which takes an instance admin key.
	_, err = f.svc.CreateAPIKey(ctx, k, service.CreateAPIKeyRequest{Name: "x", Scope: "read"})
	wantCode(t, "a key using the operator's route", err, service.CodeNotAuthorized)
	if _, err := f.svc.Authenticate(ctx, created.Key, nil); err != nil {
		t.Errorf("the key stopped working: %v", err)
	}
}

func TestAKeyReadsOnlyMailboxesAReaderGaveItAndAnAdminWhoReadsNothingCannotReadThroughOne(t *testing.T) {
	m := newMailFixture(t)
	tm := newKeysTeam(t, m.fixture)
	tm.syncOn(t, m.fixture, tm.shared)
	box := providertest.NewFakeMailbox(providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("a", "Refund for order 4471"))
	m.index(t, tm.shared, box)
	ctx := t.Context()

	// Carol administers the team and reads nothing: she cannot give a key
	// read, nor act, which needs read; send she may give.
	_, err := m.svc.CreateWorkspaceKey(ctx, tm.carol, tm.id, keyRequest("carol's", "write", reads(tm.shared)))
	wantCode(t, "Carol giving a new key read", err, service.CodeNotAuthorized)
	_, err = m.svc.CreateWorkspaceKey(ctx, tm.carol, tm.id, keyRequest("carol's", "write",
		service.KeyMailboxRequest{AccountID: tm.shared, Act: true}))
	wantCode(t, "Carol giving act without read", err, service.CodeBadRequest)
	created, err := m.svc.CreateWorkspaceKey(ctx, tm.carol, tm.id, keyRequest("carol's", "send",
		service.KeyMailboxRequest{AccountID: tm.shared, Send: true}))
	if err != nil {
		t.Fatalf("Carol giving a key send: %v", err)
	}
	_, err = m.svc.SetKeyAccess(ctx, tm.carol, tm.id, created.Prefix, tm.shared, keyAccess(true, false, true))
	wantCode(t, "Carol adding read to her key", err, service.CodeNotAuthorized)

	// Her key reads nothing, on any path.
	k := m.authenticate(t, created.Key)
	if _, err := m.svc.GetAccount(ctx, k, tm.shared); err != nil {
		t.Errorf("the key does not see the mailbox it may send from: %v", err)
	}
	_, err = m.svc.SearchMessages(ctx, k, service.SearchRequest{AccountID: tm.shared})
	wantCode(t, "searching through her key", err, service.CodeNotAuthorized)
	if page, err := m.svc.SearchMessages(ctx, k, service.SearchRequest{Query: "refund"}); err != nil || len(page.Messages) != 0 {
		t.Errorf("searching everything through her key finds %d messages (%v)", len(page.Messages), err)
	}
	_, err = m.svc.GetMessage(ctx, k, service.GetMessageRequest{ID: m.messageID(t, tm.shared, "INBOX", 1)})
	wantCode(t, "reading a message through her key", err, service.CodeNotFound)
	_, err = m.svc.ListFolders(ctx, k, tm.shared)
	wantCode(t, "listing folders through her key", err, service.CodeNotAuthorized)
	if st, err := m.svc.Storage(ctx, k, ""); err != nil || len(st.Mailboxes) != 0 {
		t.Errorf("her key's storage: %+v (%v)", st, err)
	}
	_, err = m.svc.Subscribe(ctx, k, 0, service.EventFilter{AccountIDs: []string{tm.shared}})
	wantCode(t, "following the mailbox's events through her key", err, service.CodeNotAuthorized)
	_, err = m.svc.WaitForNewMail(ctx, k, 0, time.Second, service.EventFilter{AccountIDs: []string{tm.shared}})
	wantCode(t, "waiting for its mail through her key", err, service.CodeNotAuthorized)
	wantCode(t, "subscribing to its inbox through her key", m.svc.MayFollow(ctx, k, tm.shared), service.CodeNotAuthorized)

	// Ana reads the mailbox: she gives the key read, and it reads.
	if _, err := m.svc.SetKeyAccess(ctx, tm.ana, tm.id, created.Prefix, tm.shared, keyAccess(true, false, true)); err != nil {
		t.Fatalf("Ana, who reads it, giving the key read: %v", err)
	}
	if page, err := m.svc.SearchMessages(ctx, k, service.SearchRequest{AccountID: tm.shared}); err != nil || len(page.Messages) != 1 {
		t.Errorf("the key once a reader gave it read finds %d messages (%v)", len(page.Messages), err)
	}
	// Carol, who still reads nothing, does not through the key she
	// created either: its secret is the key's, and her own session reads
	// nothing.
	_, err = m.svc.SearchMessages(ctx, tm.carol, service.SearchRequest{AccountID: tm.shared})
	wantCode(t, "Carol searching", err, service.CodeNotAuthorized)
	// And keys never count as readers.
	dir, err := m.svc.AccessDirectory(ctx, tm.ana, tm.id)
	if err != nil {
		t.Fatal(err)
	}
	for _, mb := range dir {
		if mb.AccountID == tm.shared && (mb.Readers != 1 || len(mb.Keys) != 1 || !mb.Keys[0].Read || mb.Keys[0].Prefix != created.Prefix) {
			t.Errorf("the directory says %d readers and keys %+v; want Ana alone, and the key listed apart", mb.Readers, mb.Keys)
		}
	}
}

func TestAKeyKeepsItsMailboxWhenItsCreatorLosesReadAndEveryOwnerOrAdminSeesAndRevokesIt(t *testing.T) {
	m := newMailFixture(t)
	tm := newKeysTeam(t, m.fixture)
	tm.syncOn(t, m.fixture, tm.shared)
	box := providertest.NewFakeMailbox(providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("a", "Refund for order 4471"))
	m.index(t, tm.shared, box)
	ctx := t.Context()
	// Carol is given read, creates a key with it, and loses it again.
	if _, err := m.svc.SetAccess(ctx, tm.ana, tm.shared, tm.carol.UserID, grantRequest(true, false, false, false)); err != nil {
		t.Fatal(err)
	}
	created, err := m.svc.CreateWorkspaceKey(ctx, tm.carol, tm.id, keyRequest("carol's", "read", reads(tm.shared)))
	if err != nil {
		t.Fatal(err)
	}
	k := m.authenticate(t, created.Key)
	if err := m.svc.RevokeAccess(ctx, tm.ana, tm.shared, tm.carol.UserID, workspace.Flags{}); err != nil {
		t.Fatal(err)
	}
	if page, err := m.svc.SearchMessages(ctx, k, service.SearchRequest{AccountID: tm.shared}); err != nil || len(page.Messages) != 1 {
		t.Errorf("the key lost the mailbox with its creator: %d messages (%v)", len(page.Messages), err)
	}
	// Every owner and admin sees it, revoked or not, and may revoke it.
	for _, p := range []service.Principal{tm.ana, tm.carol} {
		listed, err := m.svc.ListWorkspaceKeys(ctx, p, tm.id)
		if err != nil || len(listed) != 1 || listed[0].Prefix != created.Prefix || len(listed[0].Mailboxes) != 1 {
			t.Errorf("%s lists %+v (%v)", p.UserID, listed, err)
		}
	}
	if err := m.svc.RevokeWorkspaceKey(ctx, tm.ana, tm.id, created.Prefix); err != nil {
		t.Fatalf("Ana revoking the key Carol created: %v", err)
	}
	wantCode(t, "the revoked key, held", m.svc.Recheck(ctx, k), service.CodeUnauthorized)
	if _, err := m.svc.Authenticate(ctx, created.Key, nil); service.CodeOf(err) != service.CodeUnauthorized {
		t.Errorf("a revoked key authenticates: %v", err)
	}
	listed, err := m.svc.ListWorkspaceKeys(ctx, tm.carol, tm.id)
	if err != nil || len(listed) != 1 || listed[0].RevokedAt == 0 || listed[0].Live {
		t.Errorf("the revoked key is listed as %+v (%v); it stays listed, revoked", listed, err)
	}
	if err := m.svc.RevokeWorkspaceKey(ctx, tm.carol, tm.id, created.Prefix); err != nil {
		t.Errorf("revoking twice: %v", err)
	}
}

func TestAKeyActsOnlyWithActAndTheWriteScopeUnderItsKeyTerms(t *testing.T) {
	b := genericBox(t)
	ctx := t.Context()
	a := b.row(t, "INBOX", "a")
	ws := authtest.Personal(t, b.m.db, b.owner.UserID)
	mark := func(p service.Principal) error {
		_, err := b.m.svc.SetFlags(ctx, p, service.SetFlagsRequest{IDs: []int64{a}, Seen: yes()})
		return err
	}
	readOnly := b.m.authenticate(t, authtest.NewWorkspaceKey(t, b.m.db, auth.ScopeWrite, ws, b.owner.UserID,
		workspace.KeyGrant{AccountID: b.id, Flags: workspace.Flags{Read: true}}))
	wantCode(t, "a write key holding read alone", mark(readOnly), service.CodeNotAuthorized)
	readKey := keyOf(t, b.m.fixture, b.owner, auth.ScopeRead)
	wantCode(t, "a read key", mark(readKey), service.CodeNotAuthorized)
	if n := b.box.Opens(provider.RoleInteractive); n != 0 {
		t.Fatalf("refused actions opened %d connections", n)
	}
	// The key terms its creator agreed to cover what it does: no person's
	// consent to actions is asked, not even its creator's.
	if _, err := b.m.svc.WithdrawActionsConsent(ctx, b.owner); err != nil {
		t.Fatal(err)
	}
	actor := keyOf(t, b.m.fixture, b.owner, auth.ScopeWrite)
	if err := mark(actor); err != nil {
		t.Fatalf("a write key holding act: %v", err)
	}
	// Taking act away stops it at once, the principal held.
	if _, err := b.m.svc.SetKeyAccess(ctx, b.owner, ws, actor.KeyPrefix, b.id, keyAccess(true, false, false)); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "the key once act is taken away", mark(actor), service.CodeNotAuthorized)
}

func TestWhereKeysActUnderTheirCreatorsConsentAKeyStopsWhenItsCreatorWithdraws(t *testing.T) {
	// MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT: an edition whose key terms say a
	// key acts only while its person allows actions holds every workspace
	// key to its creator's own actions consent, not to the key terms alone.
	m := newMailFixtureWith(t, fixtureOptions{keysActUnderCreator: true})
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	id, box := m.ownedBox(t, ana, "ana@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("a", "Lunch on Friday"))
	m.index(t, id, box)
	b := &actionBox{m: m, owner: ana, id: id, box: box}
	b.allow(t)
	ctx := t.Context()
	a := b.row(t, "INBOX", "a")
	key := m.authenticate(t, authtest.NewWorkspaceKey(t, m.db, auth.ScopeWrite, authtest.Personal(t, m.db, ana.UserID),
		ana.UserID, workspace.KeyGrant{AccountID: id, Flags: workspace.Flags{Read: true, Act: true}}))
	if _, err := m.svc.SetFlags(ctx, key, service.SetFlagsRequest{IDs: []int64{a}, Seen: yes()}); err != nil {
		t.Fatalf("with its creator allowing actions: %v", err)
	}
	if _, err := m.svc.WithdrawActionsConsent(ctx, ana); err != nil {
		t.Fatal(err)
	}
	_, err := m.svc.SetFlags(ctx, key, service.SetFlagsRequest{IDs: []int64{a}, Seen: yes()})
	wantCode(t, "once its creator withdrew", err, service.CodeConflict)
	if n := box.Opens(provider.RoleInteractive); n != 1 {
		t.Fatalf("the refused action opened a connection: %d interactive opens, want the first action's 1", n)
	}
}

func TestAPersonsKeyCarriedOverActsOnlyWhileItsCreatorAllowsActionsAndNeverSends(t *testing.T) {
	b := genericBox(t)
	ctx := t.Context()
	a := b.row(t, "INBOX", "a")
	presented := authtest.NewWorkspaceKey(t, b.m.db, auth.ScopeWrite, authtest.Personal(t, b.m.db, b.owner.UserID),
		b.owner.UserID, workspace.KeyGrant{AccountID: b.id, Flags: workspace.Flags{Read: true, Act: true}})
	b.m.exec(t, `UPDATE api_keys SET origin = 'person' WHERE prefix = ?`, authtest.Prefix(presented))
	earlier := b.m.authenticate(t, presented)
	if _, err := b.m.svc.SetFlags(ctx, earlier, service.SetFlagsRequest{IDs: []int64{a}, Seen: yes()}); err != nil {
		t.Fatalf("with its creator allowing actions: %v", err)
	}
	if _, err := b.m.svc.WithdrawActionsConsent(ctx, b.owner); err != nil {
		t.Fatal(err)
	}
	_, err := b.m.svc.SetFlags(ctx, earlier, service.SetFlagsRequest{IDs: []int64{a}, Seen: yes()})
	wantCode(t, "once its creator withdrew", err, service.CodeConflict)
}

func TestAKeySendsOnlyWithTheSendFlagTheSendScopeAndConfirmAndUnderTheAddressAlone(t *testing.T) {
	b := anaSends(t)
	m := b.m
	ctx := t.Context()
	ws := authtest.Personal(t, m.db, b.owner.UserID)
	if _, err := m.svc.UpdateProfile(ctx, b.owner, service.ProfileRequest{Name: "Ana Lima"}); err != nil {
		t.Fatal(err)
	}
	// A key needs the send flag on the mailbox, and the send scope.
	reader := m.authenticate(t, authtest.NewWorkspaceKey(t, m.db, auth.ScopeSend, ws, b.owner.UserID,
		workspace.KeyGrant{AccountID: b.id, Flags: workspace.Flags{Read: true}}))
	_, err := b.send(t, reader, "", b.compose("bea@example.org"))
	wantCode(t, "a send key holding read alone", err, service.CodeNotAuthorized)
	writer := keyOf(t, m.fixture, b.owner, auth.ScopeWrite)
	_, err = b.send(t, writer, "", b.compose("bea@example.org"))
	wantCode(t, "a write key", err, service.CodeNotAuthorized)
	sender := keyOf(t, m.fixture, b.owner, auth.ScopeSend)
	unconfirmed := b.compose("bea@example.org")
	unconfirmed.Confirm = false
	_, err = b.send(t, sender, "", unconfirmed)
	wantCode(t, "a send key without confirm=true", err, service.CodeBadRequest)
	if n := b.smtp.DataCommands(); n != 0 {
		t.Fatalf("%d DATA commands before anything could send", n)
	}

	// Its creator's own consent to sending is not asked: the key terms
	// cover what the key does.
	if _, err := m.svc.WithdrawSendConsent(ctx, b.owner); err != nil {
		t.Fatal(err)
	}
	since := b.lastSeq(t)
	res, err := b.send(t, sender, "by-key", b.compose("bea@example.org"))
	if err != nil || res.State != service.SendStateSent {
		t.Fatalf("a send key holding send: %+v, %v", res, err)
	}
	sent := b.smtp.Messages()
	if len(sent) != 1 {
		t.Fatalf("%d messages reached the server", len(sent))
	}
	if from := header(t, sent[0].Raw, "From"); len(from) != 1 || from[0] != "<ana@mail.example>" {
		t.Errorf("a key's message: From = %q, want the address alone", from)
	}
	if a, err := m.svc.GetAccount(ctx, sender, b.id); err != nil || !a.Send.Available || a.Send.FromName != "" {
		t.Errorf("the key's card shows %+v (%v)", a.Send, err)
	}
	// The record and its notice name the key, not a person.
	var by, user string
	if err := m.db.Reader().QueryRowContext(ctx, `SELECT created_by, user_id FROM sends WHERE idempotency_key = 'by-key'`).
		Scan(&by, &user); err != nil || by != "key:"+sender.KeyPrefix || user != "" {
		t.Errorf("the record names %q and %q (%v)", by, user, err)
	}
	if finished := b.finished(t, since); len(finished) != 1 || finished[0].SentBy != "key:"+sender.KeyPrefix || finished[0].UserID != "" {
		t.Errorf("send.finished = %+v", finished)
	}
	// Idempotent: the same request is answered from the record.
	again, err := b.send(t, sender, "by-key", b.compose("bea@example.org"))
	if err != nil || !again.Replayed || len(b.smtp.Messages()) != 1 {
		t.Errorf("the same send again: %+v, %v, %d messages", again, err, len(b.smtp.Messages()))
	}
	// Only that key reads its record; the person, or another key, never.
	if st, err := m.svc.SendStatus(ctx, sender, b.id, "by-key"); err != nil || st.State != store.SendSent {
		t.Errorf("the key reading its send: %+v, %v", st, err)
	}
	other := keyOf(t, m.fixture, b.owner, auth.ScopeSend)
	_, err = m.svc.SendStatus(ctx, other, b.id, "by-key")
	wantCode(t, "another key reading it", err, service.CodeNotFound)
	_, err = m.svc.SendStatus(ctx, b.owner, b.id, "by-key")
	wantCode(t, "the person reading it", err, service.CodeNotFound)
	_, err = b.send(t, other, "by-key", b.compose("bea@example.org"))
	wantCode(t, "another key reusing it", err, service.CodeConflict)

	// The workspace's owners and admins list the key's sends.
	sends, err := m.svc.ListKeySends(ctx, b.owner, ws, sender.KeyPrefix)
	if err != nil || len(sends) != 1 || sends[0].IdempotencyKey != "by-key" || sends[0].State != store.SendSent {
		t.Errorf("the key's sends: %+v (%v)", sends, err)
	}
	// Taking send away stops the key at once.
	if _, err := m.svc.SetKeyAccess(ctx, b.owner, ws, sender.KeyPrefix, b.id, keyAccess(true, false, false)); err != nil {
		t.Fatal(err)
	}
	_, err = b.send(t, sender, "after", b.compose("bea@example.org"))
	wantCode(t, "the key once send is taken away", err, service.CodeNotAuthorized)
}

func TestAKeysSendThatMayHaveBeenDeliveredIsNeverRetried(t *testing.T) {
	b := anaSends(t)
	sender := keyOf(t, b.m.fixture, b.owner, auth.ScopeSend)
	b.smtp.DropAfterData(1)
	res, err := b.send(t, sender, "lost", b.compose("bea@example.org"))
	if err != nil || res.State != service.SendStateUnknown {
		t.Fatalf("result = %+v, %v; want unknown", res, err)
	}
	_, err = b.send(t, sender, "lost", b.compose("bea@example.org"))
	wantCode(t, "retrying an unknown send", err, service.CodeConflict)
	if n := b.smtp.DataCommands(); n != 1 {
		t.Fatalf("DATA %d times; a message that may have been delivered is never sent again", n)
	}
}

func TestTwoKeysSendingTheSameMessageWithoutAKeyInOneMinuteEachSendTheirs(t *testing.T) {
	// Two tools of one workspace send the same alert from one mailbox, with
	// no Idempotency-Key, in the same minute: neither is refused as the
	// other's, and each one's retry is still answered from its own record.
	b := anaSends(t)
	one := keyOf(t, b.m.fixture, b.owner, auth.ScopeSend)
	two := keyOf(t, b.m.fixture, b.owner, auth.ScopeSend)
	first, err := b.send(t, one, "", b.compose("bea@example.org"))
	if err != nil || first.State != service.SendStateSent {
		t.Fatalf("the first key's send: %+v, %v", first, err)
	}
	second, err := b.send(t, two, "", b.compose("bea@example.org"))
	if err != nil || second.State != service.SendStateSent || second.Replayed || second.MessageID == first.MessageID {
		t.Fatalf("the second key's send of the same message: %+v, %v; want its own", second, err)
	}
	for name, p := range map[string]service.Principal{"first": one, "second": two} {
		again, err := b.send(t, p, "", b.compose("bea@example.org"))
		if err != nil || !again.Replayed {
			t.Errorf("the %s key's retry: %+v, %v; want it replayed", name, again, err)
		}
	}
	if n := b.smtp.DataCommands(); n != 2 {
		t.Fatalf("DATA %d times for two keys' sends, want 2", n)
	}
}

func TestNoKeySendsWhereTheServersKeysMayNotSend(t *testing.T) {
	m := newMailFixtureWith(t, fixtureOptions{keysMayNotSend: true})
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	b := m.sendingBox(t, ana, "ana@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	ws := authtest.Personal(t, m.db, ana.UserID)
	ctx := t.Context()
	_, err := m.svc.CreateWorkspaceKey(ctx, ana, ws, keyRequest("sender", "send"))
	wantCode(t, "a key with the send scope", err, service.CodeBadRequest)
	created, err := m.svc.CreateWorkspaceKey(ctx, ana, ws, keyRequest("writer", "write", reads(b.id)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.svc.SetKeyAccess(ctx, ana, ws, created.Prefix, b.id, keyAccess(true, false, true))
	wantCode(t, "giving a key send", err, service.CodeNotAuthorized)
	// One made before the switch was turned off sends nothing either.
	before := m.authenticate(t, authtest.NewWorkspaceKey(t, m.db, auth.ScopeSend, ws, ana.UserID,
		workspace.KeyGrant{AccountID: b.id, Flags: workspace.Flags{Read: true, Send: true}}))
	_, err = b.send(t, before, "k", b.compose("bea@example.org"))
	wantCode(t, "a send key", err, service.CodeNotAuthorized)
	if a, err := m.svc.GetAccount(ctx, before, b.id); err != nil || a.Send.Available {
		t.Errorf("the key's card offers sending: %+v (%v)", a.Send, err)
	}
	if n := b.smtp.DataCommands(); n != 0 {
		t.Fatalf("%d DATA commands", n)
	}
	// People still send.
	if _, err := b.send(t, ana, "k-ana", b.compose("bea@example.org")); err != nil {
		t.Errorf("Ana sending: %v", err)
	}
}

func TestAKeySendsAtMostItsDailyLimit(t *testing.T) {
	b := anaSends(t)
	sender := keyOf(t, b.m.fixture, b.owner, auth.ScopeSend)
	for i := range service.DailyKeySendLimit {
		b.m.exec(t, `INSERT INTO sends(account_id, idempotency_key, compose_hash, message_id_hdr, state, created_by,
			user_id, created_at, updated_at) VALUES (?, ?, 'h', 'm@x', 'sent', ?, '', ?, ?)`,
			b.id, "earlier-"+string(rune('a'+i%26))+strings.Repeat("x", i/26), "key:"+sender.KeyPrefix,
			time.Now().Unix(), time.Now().Unix())
	}
	_, err := b.send(t, sender, "one-more", b.compose("bea@example.org"))
	wantCode(t, "a key past its daily limit", err, service.CodeRateLimited)
	// Its creator's own sends are theirs to count.
	if _, err := b.send(t, b.owner, "by-ana", b.compose("bea@example.org")); err != nil {
		t.Errorf("Ana sending: %v", err)
	}
}

func TestAKeyNeverReachesAnotherWorkspace(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()
	own := f.mailbox(t, tm.ana, "ana@mail.example")
	created, err := f.svc.CreateWorkspaceKey(ctx, tm.ana, tm.id, keyRequest("team", "read", reads(tm.shared)))
	if err != nil {
		t.Fatal(err)
	}
	// A mailbox of another workspace, even one its creator reads, is no
	// mailbox of the team's.
	_, err = f.svc.SetKeyAccess(ctx, tm.ana, tm.id, created.Prefix, own, keyAccess(true, false, false))
	wantCode(t, "a mailbox of Ana's personal workspace", err, service.CodeNotFound)
	_, err = f.svc.CreateWorkspaceKey(ctx, tm.ana, tm.id, keyRequest("both", "read", reads(tm.shared), reads(own)))
	wantCode(t, "a new key for both", err, service.CodeNotFound)
	k := f.authenticate(t, created.Key)
	if got := ids(t, f, k); !slices.Equal(got, []string{tm.shared}) {
		t.Errorf("the team's key sees %v", got)
	}
	_, err = f.svc.GetAccount(ctx, k, own)
	wantCode(t, "the team's key on Ana's own mailbox", err, service.CodeNotFound)
	_, err = f.svc.ListAccounts(ctx, k, authtest.Personal(t, f.db, tm.ana.UserID))
	wantCode(t, "the team's key narrowed to another workspace", err, service.CodeNotFound)
	if listed, err := f.svc.ListWorkspaces(ctx, k); err != nil || len(listed) != 1 || listed[0].ID != tm.id {
		t.Errorf("the team's key lists workspaces %+v (%v)", listed, err)
	}
	// Another workspace's owners neither list nor revoke it.
	personal := authtest.Personal(t, f.db, tm.ana.UserID)
	wantCode(t, "revoking it from another workspace", f.svc.RevokeWorkspaceKey(ctx, tm.ana, personal, created.Prefix),
		service.CodeNotFound)
}

func TestRemovingDisablingOrDeletingTheCreatorRevokesTheirKeysButADemotionDoesNot(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()
	key := func(p service.Principal) string {
		t.Helper()
		created, err := f.svc.CreateWorkspaceKey(ctx, p, tm.id, keyRequest("k", "read"))
		if err != nil {
			t.Fatal(err)
		}
		return created.Key
	}
	live := func(presented string) bool {
		_, err := f.svc.Authenticate(ctx, presented, nil)
		return err == nil
	}
	// Demoted, and her membership disabled: Carol's key stays.
	carols := key(tm.carol)
	if _, err := f.svc.SetMember(ctx, tm.ana, tm.id, tm.carol.UserID, service.MemberRequest{Role: ptr("member")}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetMember(ctx, tm.ana, tm.id, tm.carol.UserID, service.MemberRequest{Status: ptr("disabled")}); err != nil {
		t.Fatal(err)
	}
	if !live(carols) {
		t.Error("a demotion or a disabled membership revoked the key Carol created")
	}
	// Removed from the team: revoked.
	if err := f.svc.RemoveMember(ctx, tm.ana, tm.id, tm.carol.UserID); err != nil {
		t.Fatal(err)
	}
	if live(carols) {
		t.Error("the key Carol created works after she left the team")
	}

	// Disabled on the instance, and deleted: revoked.
	owner := f.person(t, "olga@example.com", auth.RoleOwner)
	dan := tm.join(t, f, "dan@example.com", workspace.RoleAdmin)
	dans := key(dan)
	if _, err := f.svc.DisableUser(ctx, owner, service.CloseUserRequest{Email: "dan@example.com"}); err != nil {
		t.Fatal(err)
	}
	if live(dans) {
		t.Error("the key Dan created works after he was disabled")
	}
	eve := tm.join(t, f, "eve@example.com", workspace.RoleAdmin)
	eves := key(eve)
	if _, err := f.svc.DeleteUser(ctx, owner, service.CloseUserRequest{Email: "eve@example.com"}); err != nil {
		t.Fatal(err)
	}
	if live(eves) {
		t.Error("the key Eve created works after she was deleted")
	}
	listed, err := f.svc.ListWorkspaceKeys(ctx, tm.ana, tm.id)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range listed {
		if k.Live {
			t.Errorf("key %s is live", k.Prefix)
		}
		if k.Prefix == authtest.Prefix(eves) && k.CreatedBy != "" {
			t.Errorf("the key Eve created still names her: %q", k.CreatedBy)
		}
	}
}

func TestAWorkspaceHoldsAtMostTwentyLiveKeys(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()
	// Nineteen through the store, cheaply, and one through the service:
	// the limit counts every live key of the workspace, whoever created it.
	for range service.MaxWorkspaceKeys - 1 {
		authtest.NewWorkspaceKey(t, f.db, auth.ScopeRead, tm.id, tm.ana.UserID)
	}
	last, err := f.svc.CreateWorkspaceKey(ctx, tm.carol, tm.id, keyRequest("twentieth", "read"))
	if err != nil {
		t.Fatalf("the twentieth key: %v", err)
	}
	_, err = f.svc.CreateWorkspaceKey(ctx, tm.ana, tm.id, keyRequest("one more", "read"))
	wantCode(t, "a twenty-first live key", err, service.CodeConflict)
	if err := f.svc.RevokeWorkspaceKey(ctx, tm.ana, tm.id, last.Prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateWorkspaceKey(ctx, tm.ana, tm.id, keyRequest("replacement", "read")); err != nil {
		t.Fatalf("after revoking one: %v", err)
	}
	// Another workspace's keys are its own to count.
	if _, err := f.svc.CreateWorkspaceKey(ctx, tm.ana, authtest.Personal(t, f.db, tm.ana.UserID), keyRequest("own", "read")); err != nil {
		t.Fatalf("a key of another workspace: %v", err)
	}
}

func TestThePersonsKeysTheUpgradeMovedIntoATeamDoNotCountTowardItsTwentyLiveKeys(t *testing.T) {
	// Before migration 0012 each person could hold twenty live keys; a team
	// whose members' keys it moved in may hold many more than twenty. Its
	// owners and admins still create keys of their own, up to twenty, while
	// those expire on their own.
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()
	for i := range service.MaxWorkspaceKeys + 5 {
		prefix := authtest.Prefix(authtest.NewWorkspaceKey(t, f.db, auth.ScopeRead, tm.id, tm.bea.UserID))
		origin := []string{auth.OriginPerson, auth.OriginPersonAll}[i%2]
		f.exec(t, `UPDATE api_keys SET origin = ? WHERE prefix = ?`, origin, prefix)
	}
	for i := range service.MaxWorkspaceKeys {
		if _, err := f.svc.CreateWorkspaceKey(ctx, tm.ana, tm.id, keyRequest("new", "read")); err != nil {
			t.Fatalf("key %d of the team's own, beside the upgrade's: %v", i+1, err)
		}
	}
	_, err := f.svc.CreateWorkspaceKey(ctx, tm.ana, tm.id, keyRequest("one more", "read"))
	wantCode(t, "a twenty-first key of the team's own", err, service.CodeConflict)
}

func TestKeysAreReadWriteOrSendLiveThirtyNinetyOrThreeHundredSixtyFiveDaysAndNameTheCurrentTerms(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleOwner)
	ws := authtest.Personal(t, f.db, ana.UserID)
	ctx := t.Context()
	for _, scope := range []string{"admin", ""} {
		_, err := f.svc.CreateWorkspaceKey(ctx, ana, ws, keyRequest("x", scope))
		wantCode(t, "scope "+scope, err, service.CodeBadRequest)
	}
	for days, ok := range map[int]bool{30: true, 90: true, 365: true, 7: false, 366: false, -1: false} {
		req := keyRequest("x", "send")
		req.TTLDays = days
		if _, err := f.svc.CreateWorkspaceKey(ctx, ana, ws, req); (err == nil) != ok {
			t.Errorf("ttl_days %d: %v", days, err)
		}
	}
	for _, version := range []string{"", "2026-10-open-api-keys"} {
		req := keyRequest("x", "read")
		req.TermsVersion = version
		_, err := f.svc.CreateWorkspaceKey(ctx, ana, ws, req)
		wantCode(t, "terms "+version, err, service.CodeConflict)
	}
	_, err := f.svc.CreateWorkspaceKey(ctx, ana, ws, keyRequest("  ", "read"))
	wantCode(t, "no name", err, service.CodeBadRequest)
}

func TestAPersonListsAndRevokesTheKeysTheyCreatedInEveryWorkspace(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()
	inTeam, err := f.svc.CreateWorkspaceKey(ctx, tm.carol, tm.id, keyRequest("team", "read"))
	if err != nil {
		t.Fatal(err)
	}
	inOwn, err := f.svc.CreateWorkspaceKey(ctx, tm.carol, authtest.Personal(t, f.db, tm.carol.UserID), keyRequest("own", "read"))
	if err != nil {
		t.Fatal(err)
	}
	anas, err := f.svc.CreateWorkspaceKey(ctx, tm.ana, tm.id, keyRequest("ana's", "read"))
	if err != nil {
		t.Fatal(err)
	}
	listed, err := f.svc.ListMyAPIKeys(ctx, tm.carol)
	if err != nil || len(listed) != 2 {
		t.Fatalf("Carol lists %+v (%v), want her two keys", listed, err)
	}
	for _, other := range []string{anas.Prefix, authtest.Prefix(authtest.NewKey(t, f.db, auth.ScopeAdmin)), "ffffffff"} {
		wantCode(t, "Carol revoking "+other, f.svc.RevokeMyAPIKey(ctx, tm.carol, other), service.CodeNotFound)
	}
	for _, k := range []service.CreatedWorkspaceKey{inTeam, inOwn} {
		if err := f.svc.RevokeMyAPIKey(ctx, tm.carol, k.Prefix); err != nil {
			t.Fatalf("Carol revoking %s: %v", k.Name, err)
		}
		if _, err := f.svc.Authenticate(ctx, k.Key, nil); service.CodeOf(err) != service.CodeUnauthorized {
			t.Errorf("a revoked key authenticates: %v", err)
		}
	}
	// A key is created in a workspace, never for oneself.
	wantCode(t, "creating a key of one's own", f.svc.CreateMyAPIKey(ctx, tm.carol), service.CodeBadRequest)
	if msg := service.MessageOf(f.svc.CreateMyAPIKey(ctx, tm.carol)); !strings.Contains(msg, "created in a workspace by its owners and admins") {
		t.Errorf("message = %q", msg)
	}
}

func TestAHeldKeySeesWhatItHoldsChangeAtOnce(t *testing.T) {
	// A stream, a long poll or a stdio session holds the principal for
	// long: what the key holds is read live, so a mailbox given or taken
	// shows on it without reconnecting.
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()
	created, err := f.svc.CreateWorkspaceKey(ctx, tm.ana, tm.id, keyRequest("watcher", "read"))
	if err != nil {
		t.Fatal(err)
	}
	k := f.authenticate(t, created.Key)
	stream, err := f.svc.Subscribe(ctx, k, 0, service.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := f.svc.SetKeyAccess(ctx, tm.ana, tm.id, created.Prefix, tm.shared, keyAccess(true, false, false)); err != nil {
		t.Fatal(err)
	}
	if changes, err := stream.CheckAccess(ctx); err != nil || len(changes) != 1 ||
		changes[0] != (service.AccessChange{AccountID: tm.shared, Read: true}) {
		t.Fatalf("after the key was given the mailbox: %+v %v", changes, err)
	}
	if err := f.svc.RevokeKeyAccess(ctx, tm.ana, tm.id, created.Prefix, tm.shared); err != nil {
		t.Fatal(err)
	}
	if changes, err := stream.CheckAccess(ctx); err != nil || len(changes) != 1 ||
		changes[0] != (service.AccessChange{AccountID: tm.shared, Read: false}) {
		t.Fatalf("after the mailbox was taken out of the key: %+v %v", changes, err)
	}
	if got := ids(t, f, k); len(got) != 0 {
		t.Errorf("the held key still lists %v", got)
	}
	// Revoked, it stops at its next re-check.
	if err := f.svc.RevokeWorkspaceKey(ctx, tm.carol, tm.id, created.Prefix); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "the held key once revoked", f.svc.Recheck(ctx, k), service.CodeUnauthorized)
}
