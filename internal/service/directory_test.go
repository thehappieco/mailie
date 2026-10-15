package service_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Following an external directory (directory.go): what its documents do to
// people, workspaces and memberships, and what the service keeps of it.

const (
	dirIssuer = "https://directory.example.org"
	dirGen    = "generation-1"
	dirTeamID = "3c9a0b1d-0000-4000-8000-0000000000d1"
	// dirMargin is the margin an extension adds to a credentials revocation.
	dirMargin = 5 * time.Second
)

// newDirectoryFixture is a daemon whose workspaces come from a directory an
// extension follows.
func newDirectoryFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWith(t, fixtureOptions{source: workspace.Platform(), registry: account.RegistryOptions{
		Google:    account.OAuthClient{ClientID: "google-client"},
		Microsoft: account.OAuthClient{ClientID: "ms-client", Tenant: "common"},
	}})
}

// subjectOf is the directory's subject for an address's person.
func subjectOf(email string) string {
	local, _, _ := strings.Cut(email, "@")
	return "sub-" + local
}

// personalOf is the directory's id of an address's person's personal
// workspace.
func personalOf(email string) string { return "personal-" + subjectOf(email) }

// followed makes a person who signs in with an identity of the directory,
// with the local personal workspace signing up made, which their first
// document replaces, and returns their session.
func (f *fixture) followed(t *testing.T, email string) service.Principal {
	t.Helper()
	p := f.person(t, email, auth.RoleMember)
	f.exec(t, `INSERT INTO user_identities(issuer, subject, user_id, created_at) VALUES (?, ?, ?, 100)`,
		dirIssuer, subjectOf(email), p.UserID)
	return p
}

// document is what the directory says of an address's person at version:
// active, their personal workspace, and the teams given.
func document(email string, version int64, teams ...service.DirectoryWorkspace) service.DirectoryDocument {
	return service.DirectoryDocument{
		Issuer: dirIssuer, Subject: subjectOf(email), Generation: dirGen, Version: version, Status: service.DirectoryActive,
		Workspaces: append([]service.DirectoryWorkspace{{ID: personalOf(email), Kind: workspace.KindPersonal,
			Role: workspace.RoleOwner}}, teams...),
	}
}

// inTeam is a membership of the directory's team, at the team's version.
func inTeam(role workspace.Role, version int64) service.DirectoryWorkspace {
	return service.DirectoryWorkspace{ID: dirTeamID, Kind: workspace.KindTeam, Name: "Support", Role: role, Version: version}
}

// apply applies a document as an extension does, and fails the test on a
// refusal.
func (f *fixture) apply(t *testing.T, doc service.DirectoryDocument) service.DirectoryApplied {
	t.Helper()
	got, err := f.svc.ApplyDirectoryDocument(t.Context(), doc, service.ApplyOptions{CredentialsMargin: dirMargin})
	if err != nil {
		t.Fatalf("applying %s's document v%d: %v", doc.Subject, doc.Version, err)
	}
	return got
}

var boxes atomic.Int64

// teamBox links an active password mailbox into a workspace as linker, who
// reads it, and returns its id.
func (f *fixture) teamBox(t *testing.T, workspaceID, linker, email string) string {
	t.Helper()
	id := fmt.Sprintf("acc_%016x", 0xd1d0000+boxes.Add(1))
	if _, err := f.repo.Create(t.Context(), account.Account{
		ID: id, WorkspaceID: workspaceID, Email: email, Provider: provider.KindIMAP,
		AuthKind: "password", IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example",
		SMTPPort: 465, SMTPTLS: "implicit", LoginUser: email, State: account.StateActive,
	}, linker); err != nil {
		t.Fatalf("linking %s: %v", email, err)
	}
	return id
}

// giveRead gives a person read on a mailbox without a key.
func (f *fixture) giveRead(t *testing.T, accountID, userID string) {
	t.Helper()
	if _, err := workspace.NewRepository(f.db, nil).SetGrant(t.Context(), accountID, userID,
		workspace.Flags{Read: true}, "usr_test", nil); err != nil {
		t.Fatal(err)
	}
}

// takeRead takes a person's read on a mailbox.
func (f *fixture) takeRead(t *testing.T, accountID, userID string) {
	t.Helper()
	if _, err := workspace.NewRepository(f.db, nil).Revoke(t.Context(), accountID, userID,
		workspace.Flags{Read: true}, nil); err != nil {
		t.Fatal(err)
	}
}

// readsNow reports whether a person reads a mailbox now, by the one rule.
func (f *fixture) readsNow(t *testing.T, accountID, userID string) bool {
	t.Helper()
	return f.count(t, `SELECT count(*) FROM mailbox_access g WHERE g.account_id = ? AND g.user_id = ? AND `+
		store.ReaderSQL("g"), accountID, userID) == 1
}

// strandedRows lists the journal's strandings as "account cause actor lost".
func (f *fixture) strandedRows(t *testing.T) []string {
	t.Helper()
	rows, err := f.db.Reader().QueryContext(t.Context(), `SELECT account_id || ' ' || cause || ' ' || actor || ' ' || lost_user
		FROM mailbox_events WHERE kind = 'stranded' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// live reports whether a session still works.
func (f *fixture) live(t *testing.T, p service.Principal) bool {
	t.Helper()
	return f.svc.Recheck(t.Context(), p) == nil
}

// state is what the users row says of a person: disabled (here or
// suspended), and suspended.
func (f *fixture) state(t *testing.T, userID string) (disabled, suspended bool) {
	t.Helper()
	u, err := f.users.Get(t.Context(), userID)
	if err != nil {
		t.Fatal(err)
	}
	return u.Disabled, u.Suspended
}

// keyOf makes a live key a person created in a workspace and returns its
// prefix.
func (f *fixture) keyOf(t *testing.T, workspaceID, userID string) string {
	t.Helper()
	return authtest.Prefix(authtest.NewWorkspaceKey(t, f.db, auth.ScopeRead, workspaceID, userID))
}

// wantIs fails the test unless err is a *service.Error of code whose cause is
// cause.
func wantIs(t *testing.T, what string, err error, code service.Code, cause error) {
	t.Helper()
	if service.CodeOf(err) != code || !errors.Is(err, cause) {
		t.Errorf("%s: %v, want %s caused by %v", what, err, code, cause)
	}
}

func TestADirectoryIsAppliedOnlyUnderTheSourceThatMirrorsIt(t *testing.T) {
	// The local source, which a self-hosted server and an edition that does
	// not follow its directory yet run with: nothing of a directory applies.
	f := newFixture(t)
	ctx := t.Context()
	bea := f.followed(t, "bea@example.com")
	_, err := f.svc.ApplyDirectoryDocument(ctx, document("bea@example.com", 1), service.ApplyOptions{})
	wantIs(t, "a document under the local source", err, service.CodeConflict, service.ErrNotFollowing)
	_, err = f.svc.ResetDirectory(ctx, dirIssuer, dirGen)
	wantIs(t, "a reset", err, service.CodeConflict, service.ErrNotFollowing)
	_, err = f.svc.ConfirmDirectoryGeneration(ctx, dirIssuer, dirGen)
	wantIs(t, "a confirmation", err, service.CodeConflict, service.ErrNotFollowing)
	err = f.svc.SetDirectoryCursor(ctx, dirIssuer, dirGen, 0, 1, time.Time{})
	wantIs(t, "a cursor", err, service.CodeConflict, service.ErrNotFollowing)
	closed := document("bea@example.com", 2)
	closed.Status, closed.Workspaces = service.DirectoryClosed, nil
	_, err = f.svc.ForgetExternalPerson(ctx, closed)
	wantIs(t, "a forget", err, service.CodeConflict, service.ErrNotFollowing)
	in := vouched(subjectOf("cy@example.com"), "cy@example.com")
	in.Issuer = dirIssuer
	doc := document("cy@example.com", 1)
	in.Directory = &doc
	_, err = f.svc.SignInExternal(ctx, f.pinned(t, in))
	wantIs(t, "a sign-in with a document", err, service.CodeConflict, service.ErrNotFollowing)
	if n := f.count(t, `SELECT count(*) FROM directories`) + f.count(t, `SELECT count(*) FROM directory_people`); n != 0 {
		t.Errorf("the local source kept %d rows of a directory", n)
	}
	if w, err := f.svc.ListWorkspaces(ctx, bea); err != nil || len(w) != 1 || w[0].ID == personalOf("bea@example.com") {
		t.Errorf("Bea's workspaces under the local source: %+v (%v)", w, err)
	}

	// The source that mirrors a directory takes it.
	g := newDirectoryFixture(t)
	g.followed(t, "bea@example.com")
	if got := g.apply(t, document("bea@example.com", 1)); !got.Applied || got.ReplacedPersonal == "" {
		t.Errorf("a document under the source that mirrors it: %+v", got)
	}
}

func TestARisingDisableSuspendsEvenWhenTheAccountIsActiveAgain(t *testing.T) {
	// The directory disabled Bea and enabled her again before the extension
	// read either: the document it reads says active, with a disable newer
	// than the one kept. What a disable ends ends; she is active after.
	f := newDirectoryFixture(t)
	bea := f.followed(t, "bea@example.com")
	f.apply(t, document("bea@example.com", 1))
	key := f.keyOf(t, personalOf("bea@example.com"), bea.UserID)

	doc := document("bea@example.com", 2)
	doc.DisabledAt = time.Now().Add(-time.Minute)
	got := f.apply(t, doc)
	if !got.Suspended || !got.Resumed || got.SuspensionKept {
		t.Errorf("a missed disable and enable: %+v", got)
	}
	if f.live(t, bea) {
		t.Error("Bea's session outlived a disable")
	}
	if stopped, reason := f.stopped(t, key); !stopped || reason != workspace.KeyStopCreatorSuspended {
		t.Errorf("Bea's key: stopped %v for %q", stopped, reason)
	}
	if disabled, suspended := f.state(t, bea.UserID); disabled || suspended {
		t.Errorf("Bea is disabled %v, suspended %v; want her active again", disabled, suspended)
	}
}

func TestALocalDisableStandsOverADocument(t *testing.T) {
	f := newDirectoryFixture(t)
	bea := f.followed(t, "bea@example.com")
	f.apply(t, document("bea@example.com", 1))
	if _, err := f.svc.DisableUser(t.Context(), admin(), service.CloseUserRequest{Email: "bea@example.com"}); err != nil {
		t.Fatal(err)
	}
	disabled := document("bea@example.com", 2)
	disabled.Status, disabled.DisabledAt = service.DirectoryDisabled, time.Now()
	if got := f.apply(t, disabled); got.Suspended {
		t.Errorf("a disabled document suspended a person disabled here: %+v", got)
	}
	active := document("bea@example.com", 3)
	active.DisabledAt = disabled.DisabledAt
	if got := f.apply(t, active); got.Resumed || !got.Applied {
		t.Errorf("an active document on a person disabled here: %+v", got)
	}
	if isDisabled, suspended := f.state(t, bea.UserID); !isDisabled || suspended {
		t.Errorf("Bea is disabled %v, suspended %v; want her disabled here still", isDisabled, suspended)
	}
}

func TestADocumentThatDoesNotKnowTheDisableNeverResumes(t *testing.T) {
	f := newDirectoryFixture(t)
	bea := f.followed(t, "bea@example.com")
	disabledAt := time.Now().Add(-time.Hour)
	disabled := document("bea@example.com", 4)
	disabled.Status, disabled.DisabledAt = service.DirectoryDisabled, disabledAt
	if got := f.apply(t, disabled); !got.Suspended {
		t.Fatalf("a disabled document: %+v", got)
	}
	// After a reset any version applies: one that knows of no disable is
	// older than the disable kept, and keeps the suspension.
	if _, err := f.svc.ResetDirectory(t.Context(), dirIssuer, dirGen); err != nil {
		t.Fatal(err)
	}
	if got := f.apply(t, document("bea@example.com", 1)); !got.Applied || got.Resumed || !got.SuspensionKept {
		t.Errorf("an active document without the disable: %+v, want the suspension kept", got)
	}
	if _, suspended := f.state(t, bea.UserID); !suspended {
		t.Fatal("an active document that does not know of the disable resumed Bea")
	}
	// One that knows of it resumes her.
	knows := document("bea@example.com", 2)
	knows.DisabledAt = disabledAt
	if got := f.apply(t, knows); !got.Resumed {
		t.Errorf("an active document that knows of the disable: %+v", got)
	}
}

func TestAnActiveDocumentResumesOnlyASuspension(t *testing.T) {
	f := newDirectoryFixture(t)
	ana := f.followed(t, "ana@example.com")
	cy := f.followed(t, "cy@example.com")
	dan := f.followed(t, "dan@example.com")
	f.suspend(t, cy.UserID)
	if _, err := f.svc.DisableUser(t.Context(), admin(), service.CloseUserRequest{Email: "dan@example.com"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		who    service.Principal
		email  string
		resume bool
	}{{ana, "ana@example.com", false}, {cy, "cy@example.com", true}, {dan, "dan@example.com", false}} {
		got := f.apply(t, document(c.email, 1))
		if got.Resumed != c.resume {
			t.Errorf("%s: resumed %v, want %v", c.email, got.Resumed, c.resume)
		}
		disabled, _ := f.state(t, c.who.UserID)
		if disabled == (c.email != "dan@example.com") {
			t.Errorf("%s is disabled %v after an active document", c.email, disabled)
		}
	}
}

func TestADocumentThatResumesAndRevokesRunsInOneWrite(t *testing.T) {
	f := newDirectoryFixture(t)
	bea := f.followed(t, "bea@example.com")
	disabledAt := time.Now().Add(-time.Hour)
	disabled := document("bea@example.com", 1)
	disabled.Status, disabled.DisabledAt = service.DirectoryDisabled, disabledAt
	f.apply(t, disabled)

	doc := document("bea@example.com", 2, inTeam(workspace.RoleOwner, 1))
	doc.DisabledAt, doc.SessionsRevokedAt, doc.CredentialsRevokedAt = disabledAt, time.Now(), time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	got, err := f.svc.ApplyDirectoryDocument(ctx, doc, service.ApplyOptions{CredentialsMargin: dirMargin})
	if err != nil {
		t.Fatalf("a document that resumes and revokes: %v (a write inside the write would hang until the deadline)", err)
	}
	if !got.Resumed || !slices.Contains(got.Changed, dirTeamID) {
		t.Errorf("applied %+v", got)
	}
	if disabled, _ := f.state(t, bea.UserID); disabled {
		t.Error("Bea is not active")
	}
}

func TestADemotionFromADocumentDropsPendingConsentsAndInvitesAndKeepsKeys(t *testing.T) {
	f := newDirectoryFixture(t)
	ana := f.followed(t, "ana@example.com")
	bea := f.followed(t, "bea@example.com")
	f.apply(t, document("ana@example.com", 1, inTeam(workspace.RoleOwner, 1)))
	f.apply(t, document("bea@example.com", 1, inTeam(workspace.RoleAdmin, 1)))
	box := f.teamBox(t, dirTeamID, ana.UserID, "support@mail.example")
	f.exec(t, `INSERT INTO oauth_pending(state, account_id, owner_user_id, flow, redirect_uri, expires_at)
		VALUES ('state-of-bea', ?, ?, 'web', 'https://console.example/oauth/return', 99999999999)`, box, bea.UserID)
	f.exec(t, `INSERT INTO invites(code_hash, email, role, created_by, created_at, expires_at, id, workspace_id, workspace_role)
		VALUES (randomblob(32), 'dan@example.com', 'member', ?, 100, 99999999999, 'inv_00000000000000b1', ?, 'member')`,
		bea.UserID, dirTeamID)
	key := f.keyOf(t, dirTeamID, bea.UserID)

	if got := f.apply(t, document("bea@example.com", 2, inTeam(workspace.RoleMember, 1))); !slices.Contains(got.Changed, dirTeamID) {
		t.Errorf("demoting Bea changed %v", got.Changed)
	}
	if n := f.count(t, `SELECT count(*) FROM oauth_pending WHERE owner_user_id = ?`, bea.UserID); n != 0 {
		t.Errorf("Bea's consent attempt on a mailbox she no longer manages outlived her demotion")
	}
	if n := f.count(t, `SELECT count(*) FROM invites WHERE created_by = ? AND expires_at > ?`, bea.UserID, time.Now().Unix()); n != 0 {
		t.Errorf("the invite Bea made as an admin is still waiting")
	}
	if stopped, _ := f.stopped(t, key); stopped {
		t.Error("a demotion stopped the key Bea created: it is the workspace's")
	}
	if m, err := workspace.NewRepository(f.db, nil).Member(t.Context(), dirTeamID, bea.UserID); err != nil ||
		m.Role != workspace.RoleMember || !m.Active() {
		t.Errorf("Bea's membership: %+v (%v)", m, err)
	}
}

func TestTheSameDocumentTwiceWithASubSecondTimeEndsNothingTheSecondTime(t *testing.T) {
	f := newDirectoryFixture(t)
	bea := f.followed(t, "bea@example.com")
	doc := document("bea@example.com", 3)
	revoked := time.Now().Add(-30 * time.Second).Truncate(time.Second).Add(123456 * time.Microsecond)
	doc.SessionsRevokedAt, doc.CredentialsRevokedAt = revoked, revoked
	f.apply(t, doc)
	again := f.signInAgain(t, "bea@example.com")
	key := f.keyOf(t, personalOf("bea@example.com"), bea.UserID)
	f.exec(t, `UPDATE api_keys SET created_at = ? WHERE prefix = ?`, revoked.Unix()-100, key)

	// The same document: not newer.
	if got := f.apply(t, doc); got.Applied || got.SessionsEnded+got.KeysStopped != 0 {
		t.Errorf("the same document again: %+v", got)
	}
	// After a reset it applies again, and its moments, kept to the
	// microsecond, have not risen: nothing more ends.
	if _, err := f.svc.ResetDirectory(t.Context(), dirIssuer, dirGen); err != nil {
		t.Fatal(err)
	}
	if got := f.apply(t, doc); !got.Applied || got.SessionsEnded+got.KeysStopped != 0 {
		t.Errorf("the same document after a reset: %+v, want nothing ended", got)
	}
	if !f.live(t, again) {
		t.Error("a session started after the revocation ended")
	}
	if stopped, _ := f.stopped(t, key); stopped {
		t.Error("a key the revocation spared was stopped by the same moment again")
	}
}

func TestRisingSessionRevocationEndsOnlySessionsStartedBefore(t *testing.T) {
	f := newDirectoryFixture(t)
	bea := f.followed(t, "bea@example.com")
	f.apply(t, document("bea@example.com", 1))
	later := f.signInAgain(t, "bea@example.com")
	f.exec(t, `UPDATE sessions SET created_at = created_at - 100 WHERE id = ?`, bea.SessionID)

	doc := document("bea@example.com", 2)
	doc.SessionsRevokedAt = time.Now().Add(-50 * time.Second)
	if got := f.apply(t, doc); got.SessionsEnded != 1 {
		t.Errorf("the revocation ended %d sessions, want the one started before it", got.SessionsEnded)
	}
	if f.live(t, bea) {
		t.Error("the session started before the revocation still works")
	}
	if !f.live(t, later) {
		t.Error("the session started after the revocation ended")
	}
}

func TestRisingCredentialRevocationStopsKeysCreatedBeforeItAndItsMarginOnly(t *testing.T) {
	f := newDirectoryFixture(t)
	bea := f.followed(t, "bea@example.com")
	f.apply(t, document("bea@example.com", 1))
	personal := personalOf("bea@example.com")
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	keys := map[string]string{}
	for name, created := range map[string]int64{"before": at.Unix() - 100, "within the margin": at.Unix() + 3,
		"after the margin": at.Unix() + 10} {
		keys[name] = f.keyOf(t, personal, bea.UserID)
		f.exec(t, `UPDATE api_keys SET created_at = ? WHERE prefix = ?`, created, keys[name])
	}
	doc := document("bea@example.com", 2)
	doc.CredentialsRevokedAt = at
	if got := f.apply(t, doc); got.KeysStopped != 2 {
		t.Errorf("the revocation stopped %d keys, want 2", got.KeysStopped)
	}
	for name, prefix := range keys {
		stopped, reason := f.stopped(t, prefix)
		if want := name != "after the margin"; stopped != want {
			t.Errorf("the key created %s: stopped %v, want %v", name, stopped, want)
		}
		if stopped && reason != workspace.KeyStopAccountRecovered {
			t.Errorf("the key created %s stopped for %q", name, reason)
		}
	}
}

func TestATeamIsRenamedOrClosedOnlyByAHigherWorkspaceVersion(t *testing.T) {
	f := newDirectoryFixture(t)
	f.followed(t, "ana@example.com")
	ws := workspace.NewRepository(f.db, nil)
	team := func() workspace.Workspace {
		t.Helper()
		w, err := ws.Get(t.Context(), dirTeamID)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	f.apply(t, document("ana@example.com", 1, inTeam(workspace.RoleOwner, 2)))
	same := inTeam(workspace.RoleOwner, 2)
	same.Name, same.Closed = "Renamed", true
	f.apply(t, document("ana@example.com", 2, same))
	if w := team(); w.Name != "Support" || !w.ClosedAt.IsZero() {
		t.Errorf("the same team version renamed or closed it: %+v", w)
	}
	higher := same
	higher.Version = 3
	f.apply(t, document("ana@example.com", 3, higher))
	if w := team(); w.Name != "Renamed" || w.ClosedAt.IsZero() {
		t.Errorf("a higher team version did not rename and close it: %+v", w)
	}
	older := inTeam(workspace.RoleOwner, 1)
	older.Name = "Old"
	f.apply(t, document("ana@example.com", 4, older))
	if w := team(); w.Name != "Renamed" || w.ClosedAt.IsZero() {
		t.Errorf("an older team version undid a newer one: %+v", w)
	}
}

func TestATeamClosedByADocumentStopsEverythingAndKeepsItsData(t *testing.T) {
	f := newDirectoryFixture(t)
	ctx := t.Context()
	ana := f.followed(t, "ana@example.com")
	bea := f.followed(t, "bea@example.com")
	f.apply(t, document("ana@example.com", 1, inTeam(workspace.RoleOwner, 1)))
	f.apply(t, document("bea@example.com", 1, inTeam(workspace.RoleMember, 1)))
	box := f.teamBox(t, dirTeamID, ana.UserID, "support@mail.example")
	f.giveRead(t, box, bea.UserID)

	closed := inTeam(workspace.RoleOwner, 2)
	closed.Closed = true
	f.apply(t, document("ana@example.com", 2, closed))
	for _, p := range []service.Principal{ana, bea} {
		if _, err := f.svc.GetAccount(ctx, p, box); service.CodeOf(err) != service.CodeNotFound {
			t.Errorf("a closed team's mailbox reached %s: %v", p.UserID, err)
		}
	}
	if f.count(t, `SELECT count(*) FROM accounts WHERE id = ?`, box) != 1 ||
		f.count(t, `SELECT count(*) FROM mailbox_access WHERE account_id = ?`, box) != 2 {
		t.Error("closing the team lost its mailbox or who holds what on it")
	}
	// A member of a closed team still changes: a document lists Cy in it.
	cy := f.followed(t, "cy@example.com")
	f.apply(t, document("cy@example.com", 1, inTeam(workspace.RoleMember, 2)))
	if _, err := workspace.NewRepository(f.db, nil).Member(ctx, dirTeamID, cy.UserID); err != nil {
		t.Errorf("Cy joining the closed team: %v", err)
	}

	open := inTeam(workspace.RoleOwner, 3)
	f.apply(t, document("ana@example.com", 3, open))
	if _, err := f.svc.GetAccount(ctx, bea, box); err != nil {
		t.Errorf("the reopened team's mailbox: %v", err)
	}
}

func TestATeamClosedOrReopenedAcrossAResetTakesItsStateFromTheNextDocument(t *testing.T) {
	f := newDirectoryFixture(t)
	f.followed(t, "ana@example.com")
	closed := inTeam(workspace.RoleOwner, 9)
	closed.Closed = true
	f.apply(t, document("ana@example.com", 9, closed))
	if _, err := f.svc.ResetDirectory(t.Context(), dirIssuer, "generation-2"); err != nil {
		t.Fatal(err)
	}
	doc := document("ana@example.com", 1, inTeam(workspace.RoleOwner, 1))
	doc.Generation = "generation-2"
	f.apply(t, doc)
	if w, err := workspace.NewRepository(f.db, nil).Get(t.Context(), dirTeamID); err != nil || !w.ClosedAt.IsZero() {
		t.Errorf("after a reset, the next document's team: %+v (%v), want it reopened", w, err)
	}
}

func TestAnOlderOrEqualDocumentChangesNothing(t *testing.T) {
	f := newDirectoryFixture(t)
	f.followed(t, "ana@example.com")
	f.apply(t, document("ana@example.com", 3, inTeam(workspace.RoleOwner, 1)))
	before := f.count(t, `SELECT count(*) FROM workspace_members`)
	for _, version := range []int64{3, 2} {
		doc := document("ana@example.com", version)
		doc.SessionsRevokedAt = time.Now()
		if got := f.apply(t, doc); got.Applied || len(got.Changed) != 0 || got.SessionsEnded != 0 {
			t.Errorf("version %d after 3: %+v", version, got)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM workspace_members`); n != before {
		t.Errorf("an older document changed the memberships: %d, want %d", n, before)
	}
}

func TestADocumentOfAnotherGenerationIsRefused(t *testing.T) {
	f := newDirectoryFixture(t)
	f.followed(t, "ana@example.com")
	f.apply(t, document("ana@example.com", 1))
	doc := document("ana@example.com", 2, inTeam(workspace.RoleOwner, 1))
	doc.Generation = "generation-2"
	_, err := f.svc.ApplyDirectoryDocument(t.Context(), doc, service.ApplyOptions{})
	wantIs(t, "a document of another generation", err, service.CodeConflict, service.ErrDirectoryGeneration)
	if n := f.count(t, `SELECT count(*) FROM workspaces WHERE id = ?`, dirTeamID); n != 0 {
		t.Error("a document of another generation was applied")
	}
}

func TestAResetForgetsVersionsAndCursorAndKeepsTheHighestMarks(t *testing.T) {
	f := newDirectoryFixture(t)
	ctx := t.Context()
	bea := f.followed(t, "bea@example.com")
	doc := document("bea@example.com", 5, inTeam(workspace.RoleOwner, 7))
	doc.SessionsRevokedAt = time.UnixMicro(1_700_000_000_123_456)
	f.apply(t, doc)
	published := time.Now().Truncate(time.Microsecond)
	if err := f.svc.SetDirectoryCursor(ctx, dirIssuer, dirGen, 0, 42, published); err != nil {
		t.Fatal(err)
	}
	if st, ok, err := f.svc.Directory(ctx, dirIssuer); err != nil || !ok || st.Cursor != 42 || !st.PublishedThrough.Equal(published) {
		t.Fatalf("the directory before the reset: %+v, %v (%v)", st, ok, err)
	}

	st, err := f.svc.ResetDirectory(ctx, dirIssuer, "generation-2")
	if err != nil {
		t.Fatal(err)
	}
	if st.Generation != "generation-2" || st.Confirmed || st.Reset != 1 || st.Cursor != 0 || !st.PublishedThrough.IsZero() {
		t.Errorf("the directory after the reset: %+v", st)
	}
	people, err := f.svc.DirectoryPeople(ctx, dirIssuer)
	if err != nil || len(people) != 1 || people[0].UserID != bea.UserID || people[0].Version != 0 {
		t.Errorf("the directory's people after the reset: %+v (%v)", people, err)
	}
	if us := f.count(t, `SELECT sessions_revoked_at_us FROM directory_people WHERE user_id = ?`, bea.UserID); int64(us) !=
		doc.SessionsRevokedAt.UnixMicro() {
		t.Errorf("the revocation kept is %d, want the one the document said", us)
	}
	if v := f.count(t, `SELECT source_version FROM workspaces WHERE id = ?`, dirTeamID); v != 0 {
		t.Errorf("the team's version after the reset is %d", v)
	}
}

func TestAResetWithTheSameGenerationLetsALowerVersionApply(t *testing.T) {
	f := newDirectoryFixture(t)
	f.followed(t, "ana@example.com")
	f.apply(t, document("ana@example.com", 5))
	if got := f.apply(t, document("ana@example.com", 2)); got.Applied {
		t.Fatal("a lower version applied before a reset")
	}
	if _, err := f.svc.ResetDirectory(t.Context(), dirIssuer, dirGen); err != nil {
		t.Fatal(err)
	}
	if got := f.apply(t, document("ana@example.com", 2, inTeam(workspace.RoleOwner, 1))); !got.Applied {
		t.Error("a lower version did not apply after a reset of the same generation")
	}
}

func TestAResetIsOneTransaction(t *testing.T) {
	f := newDirectoryFixture(t)
	ctx := t.Context()
	f.followed(t, "ana@example.com")
	f.apply(t, document("ana@example.com", 5, inTeam(workspace.RoleOwner, 7)))
	// Its last step refused: nothing of it stays.
	f.exec(t, `CREATE TRIGGER test_refuse_versions BEFORE UPDATE OF source_version ON workspaces
		BEGIN SELECT RAISE(ABORT, 'refused by the test'); END`)
	if _, err := f.svc.ResetDirectory(ctx, dirIssuer, "generation-2"); err == nil {
		t.Fatal("a reset whose last step failed succeeded")
	}
	st, _, err := f.svc.Directory(ctx, dirIssuer)
	if err != nil || st.Generation != dirGen || st.Reset != 0 {
		t.Errorf("after a failed reset the directory is %+v (%v)", st, err)
	}
	if v := f.count(t, `SELECT version FROM directory_people`); v != 5 {
		t.Errorf("after a failed reset the person's version is %d", v)
	}
	f.exec(t, `DROP TRIGGER test_refuse_versions`)
	if _, err := f.svc.ResetDirectory(ctx, dirIssuer, "generation-2"); err != nil {
		t.Fatal(err)
	}
	if v := f.count(t, `SELECT version + (SELECT source_version FROM workspaces WHERE id = ?) FROM directory_people`, dirTeamID); v != 0 {
		t.Errorf("after the reset the versions sum to %d", v)
	}
}

func TestACursorWrittenAgainstAnOlderResetIsRefused(t *testing.T) {
	f := newDirectoryFixture(t)
	ctx := t.Context()
	if _, err := f.svc.ResetDirectory(ctx, dirIssuer, dirGen); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ResetDirectory(ctx, dirIssuer, dirGen); err != nil {
		t.Fatal(err)
	}
	err := f.svc.SetDirectoryCursor(ctx, dirIssuer, dirGen, 0, 10, time.Time{})
	wantIs(t, "a cursor read before the reset", err, service.CodeConflict, service.ErrDirectoryMoved)
	err = f.svc.SetDirectoryCursor(ctx, "https://other.example.org", dirGen, 1, 10, time.Time{})
	wantIs(t, "a cursor for another directory", err, service.CodeConflict, service.ErrDirectoryMoved)
	if err := f.svc.SetDirectoryCursor(ctx, dirIssuer, dirGen, 1, 10, time.Time{}); err != nil {
		t.Fatalf("a cursor read since the reset: %v", err)
	}
	err = f.svc.SetDirectoryCursor(ctx, dirIssuer, dirGen, 1, 5, time.Time{})
	wantIs(t, "a cursor going back", err, service.CodeConflict, service.ErrDirectoryMoved)
	if st, _, _ := f.svc.Directory(ctx, dirIssuer); st.Cursor != 10 {
		t.Errorf("the cursor is %d, want 10", st.Cursor)
	}
}

func TestACursorIsNeverWrittenForADirectoryWithoutARow(t *testing.T) {
	f := newDirectoryFixture(t)
	err := f.svc.SetDirectoryCursor(t.Context(), dirIssuer, dirGen, 0, 3, time.Now())
	wantIs(t, "a cursor for a directory never followed", err, service.CodeConflict, service.ErrDirectoryMoved)
	if _, ok, err := f.svc.Directory(t.Context(), dirIssuer); err != nil || ok {
		t.Errorf("a cursor created the directory: %v (%v)", ok, err)
	}
}

func TestAResetCreatesAnAbsentDirectoryAtItsGeneration(t *testing.T) {
	f := newDirectoryFixture(t)
	st, err := f.svc.ResetDirectory(t.Context(), dirIssuer, dirGen)
	if err != nil {
		t.Fatal(err)
	}
	if st.Issuer != dirIssuer || st.Generation != dirGen || st.Confirmed || st.Reset != 0 || st.Cursor != 0 {
		t.Errorf("a reset of a directory never followed: %+v", st)
	}
	if got, ok, err := f.svc.Directory(t.Context(), dirIssuer); err != nil || !ok || got.Generation != dirGen {
		t.Errorf("the directory read back: %+v, %v (%v)", got, ok, err)
	}
}

// heldSupport is the directory's team: Ana its owner, reading nothing, Bea a
// member who alone reads its mailbox. It returns the mailbox.
func heldSupport(t *testing.T, f *fixture) (ana, bea service.Principal, box string) {
	t.Helper()
	ana = f.followed(t, "ana@example.com")
	bea = f.followed(t, "bea@example.com")
	f.apply(t, document("ana@example.com", 1, inTeam(workspace.RoleOwner, 1)))
	f.apply(t, document("bea@example.com", 1, inTeam(workspace.RoleMember, 1)))
	box = f.teamBox(t, dirTeamID, ana.UserID, "support@mail.example")
	f.giveRead(t, box, bea.UserID)
	f.takeRead(t, box, ana.UserID)
	return ana, bea, box
}

func TestAStrandingRemovalIsHeldUntilTheGenerationIsConfirmed(t *testing.T) {
	f := newDirectoryFixture(t)
	ctx := t.Context()
	_, bea, box := heldSupport(t, f)

	got := f.apply(t, document("bea@example.com", 2))
	if !slices.Equal(got.Held, []string{dirTeamID}) || len(got.Stranded) != 0 {
		t.Fatalf("removing the mailbox's only reader before the generation is confirmed: %+v", got)
	}
	if f.readsNow(t, box, bea.UserID) || len(f.strandedRows(t)) != 0 {
		t.Error("a held member reads, or the hold journaled a stranding")
	}
	stranded, err := f.svc.ConfirmDirectoryGeneration(ctx, dirIssuer, dirGen)
	if err != nil || len(stranded) != 1 || stranded[0].AccountID != box {
		t.Fatalf("confirming the generation: %+v (%v)", stranded, err)
	}
	if got := f.strandedRows(t); !slices.Equal(got, []string{box + " membership_ended directory " + bea.UserID}) {
		t.Errorf("the journal holds %v", got)
	}
	if n := f.count(t, `SELECT count(*) FROM workspace_members WHERE user_id = ? AND workspace_id = ?`, bea.UserID, dirTeamID); n != 0 {
		t.Error("the confirmation left Bea's membership")
	}
	// Confirmed, a removal that strands ends at once.
	cy := f.followed(t, "cy@example.com")
	f.apply(t, document("cy@example.com", 1, inTeam(workspace.RoleMember, 1)))
	f.giveRead(t, box, cy.UserID)
	if got := f.apply(t, document("cy@example.com", 2)); len(got.Held) != 0 || len(got.Stranded) != 1 {
		t.Errorf("a removal once the generation is confirmed: %+v", got)
	}
}

func TestConfirmingTheGenerationEndsEveryHeldMembershipAndStrands(t *testing.T) {
	// In the directory's team, Bea alone reads one mailbox, Cy another,
	// both of them a third, and Dan, who stays, a fourth. Both are removed
	// before the generation is confirmed, and held; the confirmation strands
	// each mailbox a hold kept, once.
	f := newDirectoryFixture(t)
	ctx := t.Context()
	ana := f.followed(t, "ana@example.com")
	people := map[string]service.Principal{}
	f.apply(t, document("ana@example.com", 1, inTeam(workspace.RoleOwner, 1)))
	for _, email := range []string{"bea@example.com", "cy@example.com", "dan@example.com"} {
		people[email] = f.followed(t, email)
		f.apply(t, document(email, 1, inTeam(workspace.RoleMember, 1)))
	}
	bea, cy, dan := people["bea@example.com"].UserID, people["cy@example.com"].UserID, people["dan@example.com"].UserID
	beas, cys, both, dans := f.teamBox(t, dirTeamID, ana.UserID, "one@mail.example"),
		f.teamBox(t, dirTeamID, ana.UserID, "two@mail.example"), f.teamBox(t, dirTeamID, ana.UserID, "three@mail.example"),
		f.teamBox(t, dirTeamID, ana.UserID, "four@mail.example")
	for box, readers := range map[string][]string{beas: {bea}, cys: {cy}, both: {bea, cy}, dans: {dan}} {
		for _, r := range readers {
			f.giveRead(t, box, r)
		}
		f.takeRead(t, box, ana.UserID)
	}
	f.apply(t, document("bea@example.com", 2))
	f.apply(t, document("cy@example.com", 2))
	if n := f.count(t, `SELECT count(*) FROM workspace_members WHERE held_at <> 0`); n != 2 {
		t.Fatalf("%d memberships held, want Bea's and Cy's", n)
	}

	stranded, err := f.svc.ConfirmDirectoryGeneration(ctx, dirIssuer, dirGen)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range stranded {
		got = append(got, s.AccountID)
	}
	slices.Sort(got)
	want := []string{beas, cys, both}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the confirmation stranded %v, want %v: each mailbox a hold kept, once", got, want)
	}
	if rows := f.strandedRows(t); len(rows) != 3 {
		t.Errorf("the journal holds %v", rows)
	}
	if n := f.count(t, `SELECT count(*) FROM workspace_members WHERE held_at <> 0`); n != 0 {
		t.Errorf("%d memberships still held", n)
	}
}

func TestConfirmingTwiceChangesNothing(t *testing.T) {
	f := newDirectoryFixture(t)
	ctx := t.Context()
	heldSupport(t, f)
	f.apply(t, document("bea@example.com", 2))
	if _, err := f.svc.ConfirmDirectoryGeneration(ctx, dirIssuer, dirGen); err != nil {
		t.Fatal(err)
	}
	journal := f.count(t, `SELECT count(*) FROM mailbox_events`)
	stranded, err := f.svc.ConfirmDirectoryGeneration(ctx, dirIssuer, dirGen)
	if err != nil || len(stranded) != 0 {
		t.Errorf("confirming again: %+v (%v)", stranded, err)
	}
	if n := f.count(t, `SELECT count(*) FROM mailbox_events`); n != journal {
		t.Errorf("confirming again journaled %d entries", n-journal)
	}
	_, err = f.svc.ConfirmDirectoryGeneration(ctx, dirIssuer, "generation-2")
	wantIs(t, "confirming another generation", err, service.CodeConflict, service.ErrDirectoryGeneration)
}

func TestADocumentThatChangesAWorkspacesKindIsRefusedButItsRevocationsApply(t *testing.T) {
	f := newDirectoryFixture(t)
	bea := f.followed(t, "bea@example.com")
	f.apply(t, document("bea@example.com", 1, inTeam(workspace.RoleMember, 1)))
	// Its personal workspace is the team's id.
	doc := document("bea@example.com", 2)
	doc.Workspaces[0].ID = dirTeamID
	doc.SessionsRevokedAt = time.Now()
	got, err := f.svc.ApplyDirectoryDocument(t.Context(), doc, service.ApplyOptions{})
	wantIs(t, "a document that changes a workspace's kind", err, service.CodeConflict, service.ErrKindChanged)
	if got.SessionsEnded != 1 || got.UserID != bea.UserID {
		t.Errorf("what the refused document applied: %+v", got)
	}
	if f.live(t, bea) {
		t.Error("the revocation of a refused document did not apply")
	}
	if v := f.count(t, `SELECT version FROM directory_people WHERE user_id = ?`, bea.UserID); v != 1 {
		t.Errorf("the refused document's version was recorded: %d", v)
	}
	if us := f.count(t, `SELECT sessions_revoked_at_us FROM directory_people WHERE user_id = ?`, bea.UserID); int64(us) !=
		doc.SessionsRevokedAt.UnixMicro() {
		t.Errorf("the revocation kept is %d", us)
	}
	if w, err := f.svc.ListWorkspaces(t.Context(), f.signInAgain(t, "bea@example.com")); err != nil || len(w) != 2 {
		t.Errorf("Bea's workspaces: %+v (%v), want them as they were", w, err)
	}
	// A shape outside the rules is refused the same way.
	shape := document("bea@example.com", 3)
	shape.Workspaces = append(shape.Workspaces, service.DirectoryWorkspace{ID: "wsp_0011223344556677",
		Kind: workspace.KindTeam, Name: "Local", Role: workspace.RoleMember, Version: 1})
	_, err = f.svc.ApplyDirectoryDocument(t.Context(), shape, service.ApplyOptions{})
	wantCode(t, "a document naming a local-looking id", err, service.CodeBadRequest)
}

func TestAClosedDocumentEndsSessionsAndKeysAndSuspendsBeforeItsConfirmation(t *testing.T) {
	f := newDirectoryFixture(t)
	bea := f.followed(t, "bea@example.com")
	f.apply(t, document("bea@example.com", 1, inTeam(workspace.RoleOwner, 1)))
	key := f.keyOf(t, dirTeamID, bea.UserID)

	closed := document("bea@example.com", 2)
	closed.Status, closed.Workspaces = service.DirectoryClosed, nil
	closed.SessionsRevokedAt, closed.CredentialsRevokedAt = time.Now(), time.Now()
	got := f.apply(t, closed)
	if !got.Applied || !got.Suspended || len(got.Changed) != 0 {
		t.Errorf("a closed document: %+v", got)
	}
	if f.live(t, bea) {
		t.Error("Bea's session outlived a closed document")
	}
	if stopped, _ := f.stopped(t, key); !stopped {
		t.Error("Bea's key outlived a closed document")
	}
	if _, suspended := f.state(t, bea.UserID); !suspended {
		t.Error("Bea is not suspended")
	}
	// Her memberships and her person wait for a fresh answer.
	if n := f.count(t, `SELECT count(*) FROM workspace_members WHERE user_id = ?`, bea.UserID); n != 2 {
		t.Errorf("Bea has %d memberships, want both kept until the closing is confirmed", n)
	}
}

func TestTheStartLineListsWhatWouldKeepAPersonalWorkspace(t *testing.T) {
	f := newDirectoryFixture(t)
	ana := f.followed(t, "ana@example.com")
	bea := f.followed(t, "bea@example.com")
	f.followed(t, "cy@example.com")
	dan := f.person(t, "dan@example.com", auth.RoleMember)
	f.teamBox(t, authtest.Personal(t, f.db, ana.UserID), ana.UserID, "ana@mail.example")
	f.keyOf(t, authtest.Personal(t, f.db, bea.UserID), bea.UserID)
	stopped := f.keyOf(t, authtest.Personal(t, f.db, bea.UserID), bea.UserID)
	f.exec(t, `UPDATE api_keys SET revoked_at = 1, stop_reason = 'manual' WHERE prefix = ?`, stopped)
	f.teamBox(t, authtest.Personal(t, f.db, dan.UserID), dan.UserID, "dan@mail.example")

	blocks, err := f.svc.PersonalReplacementBlocks(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []service.PersonalBlock{
		{UserID: ana.UserID, WorkspaceID: authtest.Personal(t, f.db, ana.UserID), Accounts: 1},
		{UserID: bea.UserID, WorkspaceID: authtest.Personal(t, f.db, bea.UserID), LiveKeys: 1, StoppedKeys: 1},
	}
	slices.SortFunc(blocks, func(a, b service.PersonalBlock) int { return strings.Compare(a.UserID, b.UserID) })
	slices.SortFunc(want, func(a, b service.PersonalBlock) int { return strings.Compare(a.UserID, b.UserID) })
	if !slices.Equal(blocks, want) {
		t.Errorf("the start line lists %+v, want %+v", blocks, want)
	}
}
