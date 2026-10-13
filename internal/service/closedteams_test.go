package service_test

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Closing a team (docs/workspaces.md, "Closed teams"): nothing in it is used
// until it reopens, and nothing it holds is lost meanwhile.

// closeTeam closes a team as an extension does, and fails the test unless it
// was open.
func (f *fixture) closeTeam(t *testing.T, id string) {
	t.Helper()
	if changed, err := f.svc.CloseTeam(t.Context(), id); err != nil || !changed {
		t.Fatalf("closing %s: changed %v, %v", id, changed, err)
	}
}

// reopenTeam reopens a team as an extension does, and fails the test unless
// it was closed.
func (f *fixture) reopenTeam(t *testing.T, id string) {
	t.Helper()
	if changed, err := f.svc.ReopenTeam(t.Context(), id); err != nil || !changed {
		t.Fatalf("reopening %s: changed %v, %v", id, changed, err)
	}
}

// wantClosed fails the test unless err is the refusal of a closed team.
func wantClosed(t *testing.T, what string, err error) {
	t.Helper()
	if service.CodeOf(err) != service.CodeConflict || !strings.Contains(service.MessageOf(err), "team is closed") {
		t.Errorf("%s = %v, want the closed team's conflict", what, err)
	}
}

// listedTeam is the team as p's listing of workspaces shows it.
func listedTeam(t *testing.T, f *fixture, p service.Principal, id string) service.Workspace {
	t.Helper()
	listed, err := f.svc.ListWorkspaces(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(listed, func(w service.Workspace) bool { return w.ID == id })
	if i < 0 {
		t.Fatalf("the listing has no %s: %+v", id, listed)
	}
	return listed[i]
}

func TestAClosedTeamsMailboxesReachNobodyOwnersIncluded(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	ctx := t.Context()
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
	tm.grant(t, shared, workspace.Flags{Read: true, Act: true, Send: true})
	own := f.mailbox(t, tm.ana, "ana@mail.example")
	for name, p := range map[string]service.Principal{"Ana, its owner": tm.ana, "Bea, who reads it": tm.bea} {
		if !slices.Contains(ids(t, f, p), shared) {
			t.Fatalf("before: %s does not see the team's mailbox", name)
		}
	}

	f.closeTeam(t, tm.id)

	for name, p := range map[string]service.Principal{"Ana, its owner": tm.ana, "Bea, who reads it": tm.bea} {
		if slices.Contains(ids(t, f, p), shared) {
			t.Errorf("%s lists a closed team's mailbox", name)
		}
		_, err := f.svc.GetAccount(ctx, p, shared)
		wantCode(t, name+" reading a closed team's mailbox", err, service.CodeNotFound)
		if held, err := tm.ws.Access(ctx, p.UserID, []string{shared}); err != nil || len(held) != 0 {
			t.Errorf("%s holds %+v on it (%v)", name, held, err)
		}
		// The team is listed, and nothing in it shown.
		_, err = f.svc.ListMembers(ctx, p, tm.id)
		wantClosed(t, name+" listing its members", err)
		_, err = f.svc.AccessDirectory(ctx, p, tm.id)
		wantClosed(t, name+" listing who holds what", err)
		_, err = f.svc.ListWorkspaceKeys(ctx, p, tm.id)
		wantClosed(t, name+" listing its keys", err)
	}
	// Her own mailbox is not the team's.
	if !slices.Contains(ids(t, f, tm.ana), own) {
		t.Error("closing a team hid Ana's personal mailbox")
	}
	// Someone outside it is told it does not exist, as before.
	outsider := f.person(t, "out@example.com", auth.RoleOwner)
	_, err := f.svc.ListMembers(ctx, outsider, tm.id)
	wantCode(t, "someone outside the closed team", err, service.CodeNotFound)

	// What it holds is kept, and who reads it is what it was: closing is a
	// gate on using it, never the reading rule. The operator still sees it.
	if n := f.count(t, `SELECT count(*) FROM mailbox_access WHERE account_id = ? AND read = 1`, shared); n != 2 {
		t.Errorf("%d people hold read on it, want Ana's and Bea's kept", n)
	}
	directory, err := f.svc.AccessDirectory(ctx, admin(), tm.id)
	if err != nil {
		t.Fatalf("the operator listing who holds what: %v", err)
	}
	if len(directory) != 1 || directory[0].Readers != 2 || directory[0].NoReader {
		t.Errorf("the operator's directory of the closed team: %+v", directory)
	}
	if _, err := f.svc.ListMembers(ctx, admin(), tm.id); err != nil {
		t.Errorf("the operator listing its members: %v", err)
	}
}

func TestAClosedTeamsKeysReachNothingAndWorkAgainOnReopening(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()
	created, err := f.svc.CreateWorkspaceKey(ctx, tm.ana, tm.id, keyRequest("assistant", "write",
		service.KeyMailboxRequest{AccountID: tm.shared, Read: true, Act: true}))
	if err != nil {
		t.Fatal(err)
	}
	k := f.authenticate(t, created.Key)
	if !slices.Contains(ids(t, f, k), tm.shared) {
		t.Fatal("before: the key does not reach the team's mailbox")
	}

	f.closeTeam(t, tm.id)

	// The key still authenticates, is not revoked, and reaches nothing.
	if _, err := f.svc.Authenticate(ctx, created.Key, nil); err != nil {
		t.Errorf("a closed team's key no longer authenticates: %v", err)
	}
	if stopped, reason := f.stopped(t, created.Prefix); stopped {
		t.Errorf("closing the team stopped its key, for %q", reason)
	}
	if listed := ids(t, f, k); len(listed) != 0 {
		t.Errorf("a closed team's key lists %v", listed)
	}
	_, err = f.svc.GetAccount(ctx, k, tm.shared)
	wantCode(t, "a closed team's key reading its mailbox", err, service.CodeNotFound)
	if held, err := tm.ws.KeyAccessOf(ctx, created.Prefix, []string{tm.shared}); err != nil || len(held) != 0 {
		t.Errorf("the key holds %+v (%v)", held, err)
	}
	// What it holds is kept.
	if n := f.count(t, `SELECT count(*) FROM key_access WHERE key_prefix = ? AND read = 1 AND act = 1`,
		created.Prefix); n != 1 {
		t.Errorf("the key holds %d mailboxes, want the one it held kept", n)
	}
	if listed := listedTeam(t, f, k, tm.id); listed.ClosedAt == 0 {
		t.Errorf("the key lists its team as %+v, want it closed", listed)
	}

	f.reopenTeam(t, tm.id)
	if !slices.Contains(ids(t, f, k), tm.shared) {
		t.Error("reopened, the key does not reach the team's mailbox again")
	}
	if a := accessOf(t, f, k, tm.shared); !a.Access.Read || !a.Access.Act {
		t.Errorf("reopened, the key holds %+v, want what it held", a.Access)
	}
}

func TestAClosedTeamSyncsNothingAndKeepsItsIndexAndConsent(t *testing.T) {
	f, engine := newSyncFixture(t)
	tm := newSupportTeam(t, f)
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
	tm.syncOn(t, f, shared)
	f.seedIndex(t, shared, "Quarterly")
	if !f.eligible(t, shared) {
		t.Fatal("before: the team's mailbox does not sync")
	}
	reconciled := len(engine.reconciledIDs())

	f.closeTeam(t, tm.id)

	if f.eligible(t, shared) {
		t.Error("a closed team's mailbox may still store what it syncs")
	}
	eligible, err := f.db.SyncEligibleAccounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(eligible, shared) {
		t.Error("the engine is told to sync a closed team's mailbox")
	}
	if got := engine.reconciledIDs()[reconciled:]; !slices.Contains(got, shared) {
		t.Errorf("closing the team told the engine of %v, want its mailbox", got)
	}
	// Its index and its consent are kept.
	if n := f.count(t, `SELECT count(*) FROM messages WHERE account_id = ?`, shared); n != 1 {
		t.Errorf("a closed team's mailbox keeps %d messages, want its index kept", n)
	}
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE id = ? AND sync_enabled_at <> 0 AND sync_consent_version = ?`,
		shared, f.consent().Sync); n != 1 {
		t.Error("closing the team took its consent to sync away")
	}
	// Nobody switches it on or off meanwhile.
	off := false
	_, err = f.svc.SetMailboxSync(t.Context(), tm.ana, shared, service.MailboxSyncRequest{Enabled: &off})
	wantCode(t, "the owner turning a closed team's sync off", err, service.CodeNotFound)

	reconciled = len(engine.reconciledIDs())
	f.reopenTeam(t, tm.id)
	if !f.eligible(t, shared) {
		t.Error("reopened, the team's mailbox does not sync")
	}
	if got := engine.reconciledIDs()[reconciled:]; !slices.Contains(got, shared) {
		t.Errorf("reopening the team told the engine of %v, want its mailbox", got)
	}
}

func TestNothingIsLinkedKeyedGrantedOrConsentedInAClosedTeam(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()
	created, err := f.svc.CreateWorkspaceKey(ctx, tm.ana, tm.id, keyRequest("assistant", "write", reads(tm.shared)))
	if err != nil {
		t.Fatal(err)
	}

	f.closeTeam(t, tm.id)

	// Linking.
	req := f.passwordAccount(t, "new@mail.example")
	req.WorkspaceID = tm.id
	_, err = f.svc.AddAccount(ctx, tm.ana, keyed(tm.ana, req))
	wantClosed(t, "Ana linking a mailbox into it", err)
	// Granting: its mailbox does not exist for its owner, and the operator,
	// who still sees it, changes nothing on it.
	_, err = f.svc.SetAccess(ctx, tm.ana, tm.shared, tm.bea.UserID, grantRequest(true, false, false, false))
	wantCode(t, "Ana giving Bea read", err, service.CodeNotFound)
	_, err = f.svc.SetAccess(ctx, admin(), tm.shared, tm.bea.UserID, grantRequest(false, false, false, true))
	wantClosed(t, "the operator giving Bea manage", err)
	wantClosed(t, "the operator taking Ana's send",
		f.svc.RevokeAccess(ctx, admin(), tm.shared, tm.ana.UserID, workspace.Flags{Send: true}))
	// Consenting.
	on := true
	_, err = f.svc.SetMailboxSync(ctx, tm.ana, tm.shared, service.MailboxSyncRequest{Enabled: &on, Version: f.consent().Sync})
	wantCode(t, "Ana giving the team's consent to sync", err, service.CodeNotFound)
	// Keying: no key, nothing given to one, no mailbox key.
	_, err = f.svc.CreateWorkspaceKey(ctx, tm.ana, tm.id, keyRequest("another", "read"))
	wantClosed(t, "Ana creating a key", err)
	_, err = f.svc.SetKeyAccess(ctx, tm.ana, tm.id, created.Prefix, tm.shared, keyAccess(true, true, false))
	wantClosed(t, "Ana giving the key act", err)
	wantClosed(t, "Ana revoking the key", f.svc.RevokeWorkspaceKey(ctx, tm.ana, tm.id, created.Prefix))
	key := newBrowserKey(t, "", 1)
	_, err = f.svc.WriteFirstKey(ctx, tm.ana, tm.shared, service.FirstKeyRequest{
		PublicKey: b64(key.public), Namespace: key.namespace,
		Grants: []service.GrantToRequest{key.grantTo(t, f, tm.ana.UserID)},
	})
	wantCode(t, "Ana writing the mailbox's first key", err, service.CodeNotFound)
	// Inviting, and changing who is in it, by anyone.
	_, err = f.svc.CreateTeamInvite(ctx, tm.ana, tm.id, service.TeamInviteRequest{Email: "dan@example.com"})
	wantClosed(t, "Ana inviting someone", err)
	role := string(workspace.RoleAdmin)
	_, err = f.svc.SetMember(ctx, tm.ana, tm.id, tm.bea.UserID, service.MemberRequest{Role: &role})
	wantClosed(t, "Ana making Bea an admin", err)
	_, err = f.svc.SetMember(ctx, admin(), tm.id, tm.bea.UserID, service.MemberRequest{Role: &role})
	wantClosed(t, "the operator making Bea an admin", err)
	wantClosed(t, "Ana removing Bea", f.svc.RemoveMember(ctx, tm.ana, tm.id, tm.bea.UserID))
	wantClosed(t, "the operator removing Bea", f.svc.RemoveMember(ctx, admin(), tm.id, tm.bea.UserID))
	_, err = f.svc.RenameWorkspace(ctx, admin(), tm.id, service.RenameWorkspaceRequest{Name: "Help desk"})
	wantClosed(t, "the operator renaming it", err)

	// The repository and the schema refuse what reaches past the service.
	closed := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, workspace.ErrClosed) {
			t.Errorf("%s: %v, want ErrClosed", what, err)
		}
	}
	_, err = tm.ws.SetGrant(ctx, tm.shared, tm.bea.UserID, workspace.Flags{Send: true}, tm.ana.UserID, nil)
	closed("a grant", err)
	_, err = tm.ws.SetKeyAccess(ctx, created.Prefix, tm.shared, workspace.Flags{Read: true, Act: true}, tm.ana.UserID, nil)
	closed("what a key holds", err)
	_, err = tm.ws.WriteFirstKey(ctx, tm.shared, tm.ana.UserID, workspace.FirstKey{
		PublicKey: key.public, Namespace: key.namespace, Grants: []workspace.GrantTo{
			{UserID: tm.ana.UserID, Grant: grantAt(1), SealedTo: authtest.AccountPublicKey(t, f.db, tm.ana.UserID)},
		},
	}, nil)
	closed("a first mailbox key", err)
	_, err = tm.ws.SupplyGrant(ctx, tm.shared, tm.bea.UserID, tm.ana.UserID, 1,
		workspace.Sealed{Grant: grantAt(1), SealedTo: authtest.AccountPublicKey(t, f.db, tm.bea.UserID)}, nil)
	closed("a key supplied", err)
	_, _, err = f.keys.Issue(ctx, auth.NewKeyRequest{Name: "k", Scope: auth.ScopeRead, WorkspaceID: tm.id,
		CreatedBy: tm.ana.UserID, TermsVersion: service.DefaultKeyTermsVersion})
	closed("a key issued", err)
	_, _, err = f.users.CreateInvite(ctx, auth.NewInvite{Email: "dan@example.com", WorkspaceID: tm.id,
		WorkspaceRole: workspace.RoleMember, CreatedBy: tm.ana.UserID})
	closed("an invite", err)
	if err := f.db.Write(ctx, func(tx *sql.Tx) error {
		return workspace.GrantLinkTx(ctx, tx, tm.shared, tm.id, tm.carol.UserID, time.Now())
	}); !errors.Is(err, workspace.ErrClosed) {
		t.Errorf("a linker's grant: %v, want ErrClosed", err)
	}

	// Nothing changed: every refusal left the team as it was.
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE workspace_id = ?`, tm.id); n != 1 {
		t.Errorf("the closed team holds %d mailboxes", n)
	}
	if n := f.count(t, `SELECT count(*) FROM api_keys WHERE workspace_id = ? AND revoked_at = 0`, tm.id); n != 1 {
		t.Errorf("the closed team holds %d live keys", n)
	}
	if n := f.count(t, `SELECT count(*) FROM mailbox_access WHERE account_id = ?`, tm.shared); n != 1 {
		t.Errorf("%d people hold something on its mailbox, want Ana alone", n)
	}
	if n := f.count(t, `SELECT count(*) FROM invites WHERE workspace_id = ?`, tm.id); n != 0 {
		t.Errorf("%d invites into the closed team", n)
	}
}

func TestOnlyATeamCloses(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	ctx := t.Context()
	personal := authtest.Personal(t, f.db, tm.ana.UserID)

	for name, id := range map[string]string{"a personal workspace": personal, "the operator workspace": workspace.OperatorID} {
		_, err := f.svc.CloseWorkspace(ctx, admin(), id)
		wantCode(t, "the operator closing "+name, err, service.CodeBadRequest)
		_, err = f.svc.CloseTeam(ctx, id)
		wantCode(t, "an extension closing "+name, err, service.CodeBadRequest)
	}
	_, err := f.svc.CloseWorkspace(ctx, admin(), "wsp_0000000000000000")
	wantCode(t, "closing a workspace nobody knows", err, service.CodeNotFound)
	_, err = f.svc.CloseTeam(ctx, "wsp_0000000000000000")
	wantCode(t, "an extension closing a workspace nobody knows", err, service.CodeNotFound)
	_, err = f.svc.CloseTeam(ctx, " ")
	wantCode(t, "an extension closing no team", err, service.CodeBadRequest)

	// Only the operator closes a local team: no person, its owner and an
	// owner of the instance included, and no other key.
	owner := f.person(t, "olga@example.com", auth.RoleOwner)
	restricted := service.Principal{KeyPrefix: "cccccccc", Scope: auth.ScopeAdmin, WorkspaceID: workspace.OperatorID,
		AccountIDs: []string{"acc_00000000000000aa"}}
	key := f.authenticate(t, authtest.NewWorkspaceKey(t, f.db, auth.ScopeRead, tm.id, tm.ana.UserID))
	for name, p := range map[string]service.Principal{
		"its owner": tm.ana, "an owner of the instance": owner, "an instance read key": reader(),
		"a restricted admin key": restricted, "a key of the team": key,
	} {
		_, err := f.svc.CloseWorkspace(ctx, p, tm.id)
		wantCode(t, name+" closing the team", err, service.CodeNotAuthorized)
		_, err = f.svc.ReopenWorkspace(ctx, p, tm.id)
		wantCode(t, name+" reopening the team", err, service.CodeNotAuthorized)
	}
	if w, err := tm.ws.Get(ctx, tm.id); err != nil || !w.ClosedAt.IsZero() {
		t.Fatalf("a refusal closed the team: %+v (%v)", w, err)
	}

	// A team whose source is not local closes where it is managed, which an
	// extension follows; the operator is refused it.
	const elsewhere = "0b4a3c5e-0000-4000-8000-000000000001"
	f.exec(t, `INSERT INTO workspaces(id, kind, source, name, created_at, updated_at)
		VALUES (?, 'team', 'platform', 'Elsewhere', 500, 500)`, elsewhere)
	_, err = f.svc.CloseWorkspace(ctx, admin(), elsewhere)
	wantCode(t, "the operator closing a team from another source", err, service.CodeConflict)
	f.closeTeam(t, elsewhere)
	_, err = f.svc.ReopenWorkspace(ctx, admin(), elsewhere)
	wantCode(t, "the operator reopening a team from another source", err, service.CodeConflict)
	f.reopenTeam(t, elsewhere)

	// The operator's own: closing, again, and reopening.
	closed, err := f.svc.CloseWorkspace(ctx, admin(), tm.id)
	if err != nil || closed.ClosedAt == 0 || closed.ID != tm.id || closed.Kind != "team" {
		t.Fatalf("the operator closing the team: %+v (%v)", closed, err)
	}
	again, err := f.svc.CloseWorkspace(ctx, admin(), tm.id)
	if err != nil || again.ClosedAt != closed.ClosedAt {
		t.Errorf("closing it again: %+v (%v), want it closed since the first time", again, err)
	}
	if changed, err := f.svc.CloseTeam(ctx, tm.id); err != nil || changed {
		t.Errorf("an extension closing a closed team: changed %v (%v)", changed, err)
	}
	reopened, err := f.svc.ReopenWorkspace(ctx, admin(), tm.id)
	if err != nil || reopened.ClosedAt != 0 {
		t.Errorf("the operator reopening the team: %+v (%v)", reopened, err)
	}
	if changed, err := f.svc.ReopenTeam(ctx, tm.id); err != nil || changed {
		t.Errorf("an extension reopening an open team: changed %v (%v)", changed, err)
	}
}

func TestClosingATeamEndsItsWaitingInvitesAndConsentAttempts(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	ctx := t.Context()
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
	own := f.mailbox(t, tm.ana, "ana@mail.example")
	if _, _, err := f.users.CreateInvite(ctx, auth.NewInvite{Email: "dan@example.com", WorkspaceID: tm.id,
		WorkspaceRole: workspace.RoleMember, CreatedBy: tm.ana.UserID}); err != nil {
		t.Fatal(err)
	}
	// Consent attempts Ana started, on the team's mailbox and on her own.
	f.exec(t, `INSERT INTO oauth_pending(state, account_id, owner_user_id, flow, pkce_verifier, redirect_uri,
		  device_code, expires_at)
		VALUES ('st-team', ?1, ?3, 'web', 'v1', 'https://console.example/oauth/return', '', 9999999999),
		       ('st-own', ?2, ?3, 'web', 'v2', 'https://console.example/oauth/return', '', 9999999999)`,
		shared, own, tm.ana.UserID)
	manages := func() error {
		return f.db.Write(ctx, func(tx *sql.Tx) error { return workspace.ManagesTx(ctx, tx, shared, tm.ana.UserID) })
	}
	if err := manages(); err != nil {
		t.Fatalf("before: Ana does not manage the team's mailbox: %v", err)
	}

	f.closeTeam(t, tm.id)

	if pending, err := f.users.PendingInvites(ctx, tm.id); err != nil || len(pending) != 0 {
		t.Errorf("invites into the closed team still wait: %+v (%v)", pending, err)
	}
	if n := f.count(t, `SELECT count(*) FROM oauth_pending WHERE state = 'st-team'`); n != 0 {
		t.Error("a consent attempt on the closed team's mailbox is still pending")
	}
	if n := f.count(t, `SELECT count(*) FROM oauth_pending WHERE state = 'st-own'`); n != 1 {
		t.Error("closing the team ended a consent attempt on Ana's own mailbox")
	}
	// An attempt already past its row stores nothing: nobody manages a
	// closed team's mailbox.
	if err := manages(); !errors.Is(err, workspace.ErrNoGrant) {
		t.Errorf("Ana still manages the closed team's mailbox: %v", err)
	}

	f.reopenTeam(t, tm.id)
	// What ended stays ended; what she held comes back.
	if pending, err := f.users.PendingInvites(ctx, tm.id); err != nil || len(pending) != 0 {
		t.Errorf("reopening the team brought back the invites %+v (%v)", pending, err)
	}
	if n := f.count(t, `SELECT count(*) FROM oauth_pending WHERE state = 'st-team'`); n != 0 {
		t.Error("reopening the team brought back a consent attempt")
	}
	if err := manages(); err != nil {
		t.Errorf("reopened, Ana does not manage the team's mailbox: %v", err)
	}
}

func TestReopeningATeamRestoresAccessAndSync(t *testing.T) {
	f, _ := newSyncFixture(t)
	tm := newSupportTeam(t, f)
	ctx := t.Context()
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
	tm.syncOn(t, f, shared)
	tm.grant(t, shared, workspace.Flags{Read: true, Send: true})
	before := map[string]service.AccountAccess{
		"Ana": accessOf(t, f, tm.ana, shared).Access, "Bea": accessOf(t, f, tm.bea, shared).Access,
	}

	f.closeTeam(t, tm.id)
	f.reopenTeam(t, tm.id)

	for name, p := range map[string]service.Principal{"Ana": tm.ana, "Bea": tm.bea} {
		if got := accessOf(t, f, p, shared).Access; got != before[name] {
			t.Errorf("reopened, %s holds %+v, want %+v as before", name, got, before[name])
		}
		if listed := listedTeam(t, f, p, tm.id); listed.ClosedAt != 0 {
			t.Errorf("reopened, %s lists the team as %+v", name, listed)
		}
	}
	if !f.eligible(t, shared) {
		t.Error("reopened, the team's mailbox does not sync")
	}
	if _, err := f.svc.ListMembers(ctx, tm.ana, tm.id); err != nil {
		t.Errorf("reopened, Ana listing its members: %v", err)
	}
	if _, err := f.svc.CreateWorkspaceKey(ctx, tm.ana, tm.id, keyRequest("after", "read", reads(shared))); err != nil {
		t.Errorf("reopened, Ana creating a key: %v", err)
	}
}

func TestAClosedTeamIsListedAsClosed(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	key := f.authenticate(t, authtest.NewWorkspaceKey(t, f.db, auth.ScopeRead, tm.id, tm.ana.UserID))
	at := time.Now().Unix()

	f.closeTeam(t, tm.id)

	for name, p := range map[string]service.Principal{
		"Ana, its owner": tm.ana, "Bea, a member": tm.bea, "the operator": admin(), "a key of the team": key,
	} {
		listed := listedTeam(t, f, p, tm.id)
		if listed.ClosedAt < at || listed.Name != "Support" {
			t.Errorf("%s lists the closed team as %+v", name, listed)
		}
	}
	// Nothing else is closed.
	listed, err := f.svc.ListWorkspaces(t.Context(), tm.ana)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range listed {
		if w.ID != tm.id && w.ClosedAt != 0 {
			t.Errorf("Ana's %s workspace is listed closed", w.Kind)
		}
	}
}
