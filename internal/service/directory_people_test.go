package service_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// A directory's people: a first sign-in with their document, and a person
// the directory closed, forgotten, then released.

// followedAs is followed with a role on the instance.
func (f *fixture) followedAs(t *testing.T, email string, role auth.Role) service.Principal {
	t.Helper()
	p := f.person(t, email, role)
	f.exec(t, `INSERT INTO user_identities(issuer, subject, user_id, created_at) VALUES (?, ?, ?, 100)`,
		dirIssuer, subjectOf(email), p.UserID)
	return p
}

// signInWith signs a person in through the directory's identity provider, as
// an extension does with the document it read, asking for the product key.
func (f *fixture) signInWith(t *testing.T, ctx context.Context, email string, doc *service.DirectoryDocument) (service.ExternalSignedIn, error) {
	t.Helper()
	in := vouched(subjectOf(email), email)
	in.Issuer, in.Directory, in.DirectoryApply = dirIssuer, doc, service.ApplyOptions{CredentialsMargin: dirMargin}
	if _, _, err := f.svc.PinIdentityKey(ctx, in.Issuer, in.Subject, authtest.ProductKeyID,
		authtest.ProductKey(in.Subject)); err != nil {
		t.Fatal(err)
	}
	in.WantsKey, in.ProductKeyID = true, authtest.ProductKeyID
	return f.svc.SignInExternal(ctx, in)
}

// closing is the directory's document closing an address's person.
func closing(email string, version int64) service.DirectoryDocument {
	doc := document(email, version)
	doc.Status, doc.Workspaces = service.DirectoryClosed, nil
	return doc
}

// forget forgets a person the directory closed, on its document at version.
func (f *fixture) forget(t *testing.T, email string, version int64) service.ForgottenPerson {
	t.Helper()
	got, err := f.svc.ForgetExternalPerson(t.Context(), closing(email, version))
	if err != nil {
		t.Fatalf("forgetting %s: %v", email, err)
	}
	return got
}

func TestANewPersonsDocumentIsAppliedInTheTransactionThatCreatesThem(t *testing.T) {
	f := newDirectoryFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	doc := document("cy@example.com", 1, inTeam(workspace.RoleOwner, 1))
	signed, err := f.signInWith(t, ctx, "cy@example.com", &doc)
	if err != nil || signed.Enrolment == nil {
		t.Fatalf("Cy's first sign-in with her document: %+v, %v (a write inside the write would hang until the deadline)", signed, err)
	}
	userID, ok, err := f.svc.IdentityUser(ctx, dirIssuer, subjectOf("cy@example.com"))
	if err != nil || !ok {
		t.Fatalf("Cy's identity: %v (%v)", ok, err)
	}
	ws := workspace.NewRepository(f.db, nil)
	personal, err := ws.PersonalOf(ctx, userID)
	if err != nil || personal.ID != personalOf("cy@example.com") || personal.Source != workspace.SourcePlatform {
		t.Errorf("Cy's personal workspace: %+v (%v)", personal, err)
	}
	if m, err := ws.Member(ctx, dirTeamID, userID); err != nil || m.Role != workspace.RoleOwner {
		t.Errorf("Cy in the directory's team: %+v (%v)", m, err)
	}
	people, err := f.svc.DirectoryPeople(ctx, dirIssuer)
	if err != nil || len(people) != 1 || people[0].UserID != userID || people[0].Version != 1 || !people[0].Held {
		t.Errorf("the directory's people: %+v (%v)", people, err)
	}
	if n := f.count(t, `SELECT count(*) FROM workspaces WHERE person_id = ?`, userID); n != 1 {
		t.Errorf("Cy has %d personal workspaces", n)
	}
}

func TestANewPersonIsNotCreatedWithoutADocumentWhereTheSourceCreatesNoPersonalWorkspace(t *testing.T) {
	f := newDirectoryFixture(t)
	ctx := t.Context()
	_, err := f.signInWith(t, ctx, "cy@example.com", nil)
	wantIs(t, "a first sign-in without a document", err, service.CodeInternal, service.ErrDirectoryNeeded)
	if after := service.RetryAfterOf(err); after != 10*time.Second {
		t.Errorf("a first sign-in without a document says to wait %v", after)
	}
	// Nor with one whose workspace part is refused.
	doc := document("cy@example.com", 1)
	doc.Workspaces = doc.Workspaces[1:]
	_, err = f.signInWith(t, ctx, "cy@example.com", &doc)
	wantCode(t, "a first sign-in whose document names no personal workspace", err, service.CodeBadRequest)
	if n := f.count(t, `SELECT count(*) FROM users WHERE email = 'cy@example.com'`) +
		f.count(t, `SELECT count(*) FROM user_identities`); n != 0 {
		t.Errorf("%d rows of Cy were created", n)
	}

	// The local source creates her, with the personal workspace it makes.
	g := newFixture(t)
	in := vouched(subjectOf("cy@example.com"), "cy@example.com")
	if _, err := g.svc.SignInExternal(ctx, g.pinned(t, in)); err != nil {
		t.Fatalf("a first sign-in under the local source: %v", err)
	}
}

func TestANewPersonWhoseDocumentSaysDisabledIsNotCreated(t *testing.T) {
	f := newDirectoryFixture(t)
	doc := document("cy@example.com", 1)
	doc.Status, doc.DisabledAt = service.DirectoryDisabled, time.Now()
	_, err := f.signInWith(t, t.Context(), "cy@example.com", &doc)
	wantIs(t, "a first sign-in whose document disables her", err, service.CodeUnauthorized, auth.ErrUserDisabled)
	for what, n := range map[string]int{
		"the person":          f.count(t, `SELECT count(*) FROM users WHERE email = 'cy@example.com'`),
		"the identity":        f.count(t, `SELECT count(*) FROM user_identities`),
		"the directory":       f.count(t, `SELECT count(*) FROM directories`),
		"what it said of her": f.count(t, `SELECT count(*) FROM directory_people`),
		"a workspace":         f.count(t, `SELECT count(*) FROM workspaces WHERE source <> 'local'`),
	} {
		if n != 0 {
			t.Errorf("%s was created: %d rows", what, n)
		}
	}
}

func TestAReturningPersonsDocumentIsAppliedBeforeTheirSession(t *testing.T) {
	f := newDirectoryFixture(t)
	ctx := t.Context()
	first := document("cy@example.com", 1)
	signed, err := f.signInWith(t, ctx, "cy@example.com", &first)
	if err != nil || signed.Enrolment == nil {
		t.Fatalf("Cy's first sign-in: %+v, %v", signed, err)
	}
	if _, err := f.svc.EnrolExternal(ctx, wireExternalEnrolment(t, signed.Enrolment.Ticket)); err != nil {
		t.Fatal(err)
	}
	userID, _, _ := f.svc.IdentityUser(ctx, dirIssuer, subjectOf("cy@example.com"))

	// The directory disabled her: applied, and she is refused.
	disabledAt := time.Now()
	disabled := document("cy@example.com", 2)
	disabled.Status, disabled.DisabledAt = service.DirectoryDisabled, disabledAt
	_, err = f.signInWith(t, ctx, "cy@example.com", &disabled)
	wantIs(t, "a sign-in whose document disables her", err, service.CodeUnauthorized, auth.ErrUserDisabled)
	if _, suspended := f.state(t, userID); !suspended {
		t.Fatal("the refused sign-in did not apply the document that suspends her")
	}
	// Enabled again, and in a team: her session starts with both.
	active := document("cy@example.com", 3, inTeam(workspace.RoleMember, 1))
	active.DisabledAt = disabledAt
	signed, err = f.signInWith(t, ctx, "cy@example.com", &active)
	if err != nil || signed.Session == nil {
		t.Fatalf("Cy's sign-in once enabled: %+v, %v", signed, err)
	}
	session, err := f.svc.Authenticate(ctx, signed.Session.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := f.svc.ListWorkspaces(ctx, session)
	if err != nil || !slices.ContainsFunc(listed, func(w service.Workspace) bool { return w.ID == dirTeamID }) {
		t.Errorf("her session's workspaces %+v (%v) do not have the team her document named", listed, err)
	}
	// Her document must be hers.
	other := document("dan@example.com", 1)
	_, err = f.signInWith(t, ctx, "cy@example.com", &other)
	wantCode(t, "a sign-in with another person's document", err, service.CodeBadRequest)
}

func TestAPersonDeletedWhileSigningInIsCreatedAgainOnlyWithTheirDocument(t *testing.T) {
	// Bea's sign-in applies her document to her; meanwhile, before its own
	// write, the directory has her forgotten, and a second sign-in of hers
	// pins her key again. The first sign-in's write finds her identity
	// linked to nobody and creates her anew: with the personal workspace
	// her document names, or, a document that closes her, not at all.
	for _, c := range []struct {
		what   string
		doc    service.DirectoryDocument
		forget int64
	}{
		{"an active document", document("bea@example.com", 2), 3},
		{"a document that closes her", closing("bea@example.com", 2), 2},
	} {
		t.Run(c.what, func(t *testing.T) {
			f := newDirectoryFixture(t)
			ctx := t.Context()
			bea := f.followed(t, "bea@example.com")
			f.apply(t, document("bea@example.com", 1))
			in := vouched(subjectOf("bea@example.com"), "bea@example.com")
			doc := c.doc
			in.Issuer, in.Directory, in.DirectoryApply = dirIssuer, &doc, service.ApplyOptions{CredentialsMargin: dirMargin}
			in = f.pinned(t, in)
			signed, err := service.SignInExternalMeanwhileForTest(ctx, f.svc, in, func() {
				f.forget(t, "bea@example.com", c.forget)
				f.pinned(t, in)
			})

			userID, linked, lookErr := f.svc.IdentityUser(ctx, dirIssuer, subjectOf("bea@example.com"))
			if lookErr != nil {
				t.Fatal(lookErr)
			}
			if c.doc.Status == service.DirectoryClosed {
				wantIs(t, "her sign-in once her document closes her", err, service.CodeUnauthorized, auth.ErrUserDisabled)
				if linked || f.count(t, `SELECT count(*) FROM users WHERE email = 'bea@example.com'`) != 0 {
					t.Errorf("a person was created with a document that closes her: %s", userID)
				}
				return
			}
			if err != nil || signed.Enrolment == nil {
				t.Fatalf("her sign-in: %+v, %v", signed, err)
			}
			if !linked || userID == bea.UserID {
				t.Fatalf("her identity signs in %q (%v), want a new person", userID, linked)
			}
			personal, err := workspace.NewRepository(f.db, nil).PersonalOf(ctx, userID)
			if err != nil || personal.ID != personalOf("bea@example.com") || personal.Source != workspace.SourcePlatform {
				t.Errorf("the new person's personal workspace: %+v (%v), want her document's", personal, err)
			}
			people, err := f.svc.DirectoryPeople(ctx, dirIssuer)
			if err != nil || !slices.ContainsFunc(people, func(p service.DirectoryPerson) bool {
				return p.UserID == userID && p.Version == 2
			}) {
				t.Errorf("the directory's people %+v (%v) do not hold the new person's document", people, err)
			}
		})
	}
}

func TestForgettingAClosedPersonDeletesThemAndTheirPersonalWorkspaceWithoutRefusals(t *testing.T) {
	// Bea is the instance's only owner, and the only reader of the team's
	// mailbox: closing her here would be refused without force. A
	// directory that closed her forgets her all the same.
	f := newDirectoryFixture(t)
	ctx := t.Context()
	ana := f.followed(t, "ana@example.com")
	bea := f.followedAs(t, "bea@example.com", auth.RoleOwner)
	f.apply(t, document("ana@example.com", 1, inTeam(workspace.RoleOwner, 1)))
	f.apply(t, document("bea@example.com", 1, inTeam(workspace.RoleMember, 1)))
	box := f.teamBox(t, dirTeamID, ana.UserID, "support@mail.example")
	f.giveRead(t, box, bea.UserID)
	f.takeRead(t, box, ana.UserID)
	own := f.teamBox(t, personalOf("bea@example.com"), bea.UserID, "bea@mail.example")
	if _, err := f.svc.DeleteUser(ctx, admin(), service.CloseUserRequest{Email: "bea@example.com"}); service.CodeOf(err) != service.CodeConflict {
		t.Fatalf("closing Bea here without force: %v", err)
	}

	for _, c := range []struct {
		what  string
		doc   service.DirectoryDocument
		code  service.Code
		cause error
	}{
		{"a document that does not close her", document("bea@example.com", 2), service.CodeConflict, service.ErrNotClosed},
		{"a document of another generation", func() service.DirectoryDocument {
			d := closing("bea@example.com", 2)
			d.Generation = "generation-2"
			return d
		}(), service.CodeConflict, service.ErrDirectoryGeneration},
		{"nobody's document", closing("nobody@example.com", 2), service.CodeNotFound, service.ErrNoSuchPerson},
	} {
		_, err := f.svc.ForgetExternalPerson(ctx, c.doc)
		wantIs(t, c.what, err, c.code, c.cause)
	}
	f.apply(t, closing("bea@example.com", 3))
	_, err := f.svc.ForgetExternalPerson(ctx, closing("bea@example.com", 2))
	wantIs(t, "an older document than the one applied", err, service.CodeConflict, service.ErrDirectoryMoved)

	got := f.forget(t, "bea@example.com", 3)
	if got.UserID != bea.UserID || got.PersonalWorkspaceID != personalOf("bea@example.com") || got.AccountsRemoved != 1 ||
		len(got.Stranded) != 1 || got.Stranded[0].AccountID != box {
		t.Errorf("forgetting Bea: %+v", got)
	}
	for what, n := range map[string]int{
		"her":                     f.count(t, `SELECT count(*) FROM users WHERE id = ?`, bea.UserID),
		"her personal workspace":  f.count(t, `SELECT count(*) FROM workspaces WHERE id = ?`, personalOf("bea@example.com")),
		"her personal mailbox":    f.count(t, `SELECT count(*) FROM accounts WHERE id = ?`, own),
		"her identity":            f.count(t, `SELECT count(*) FROM user_identities WHERE user_id = ?`, bea.UserID),
		"her team's membership":   f.count(t, `SELECT count(*) FROM workspace_members WHERE user_id = ?`, bea.UserID),
		"her flags on the team's": f.count(t, `SELECT count(*) FROM mailbox_access WHERE user_id = ?`, bea.UserID),
	} {
		if n != 0 {
			t.Errorf("%s outlived her being forgotten: %d", what, n)
		}
	}
	if f.count(t, `SELECT count(*) FROM workspaces WHERE id = ?`, dirTeamID) != 1 ||
		f.count(t, `SELECT count(*) FROM accounts WHERE id = ?`, box) != 1 {
		t.Error("the team or its mailbox went with her")
	}
}

func TestForgettingTheLastReaderOfATeamMailboxStrandsItAndKeepsTheTeam(t *testing.T) {
	f := newDirectoryFixture(t)
	ana, bea, box := heldSupport(t, f)
	if _, err := f.svc.ConfirmDirectoryGeneration(t.Context(), dirIssuer, dirGen); err != nil {
		t.Fatal(err)
	}
	got := f.forget(t, "bea@example.com", 2)
	if len(got.Stranded) != 1 || got.Stranded[0].AccountID != box || got.Stranded[0].WorkspaceID != dirTeamID {
		t.Errorf("forgetting the mailbox's only reader stranded %+v", got.Stranded)
	}
	if rows := f.strandedRows(t); !slices.Equal(rows, []string{box + " forgotten directory " + bea.UserID}) {
		t.Errorf("the journal holds %v: a followed person's name stays until released", rows)
	}
	if _, err := workspace.NewRepository(f.db, nil).Member(t.Context(), dirTeamID, ana.UserID); err != nil {
		t.Errorf("the team lost Ana: %v", err)
	}
}

func TestForgettingASuspendedPersonStrandsWhatWaitedForThem(t *testing.T) {
	f := newDirectoryFixture(t)
	_, bea, box := heldSupport(t, f)
	f.apply(t, closing("bea@example.com", 2))
	if _, suspended := f.state(t, bea.UserID); !suspended {
		t.Fatal("the closed document did not suspend Bea")
	}
	if got := f.forget(t, "bea@example.com", 2); len(got.Stranded) != 1 || got.Stranded[0].AccountID != box {
		t.Errorf("forgetting the mailbox's only reader once resumed stranded %+v", got.Stranded)
	}
}

func TestForgettingAPersonWhoseMembershipIsHeldStrandsWhatTheHoldKept(t *testing.T) {
	// Bea, Cy and Dan each alone read one of the team's mailboxes, and are
	// removed from it before the directory confirmed its generation: their
	// memberships are held. Forgetting Bea, disabling Cy and deleting Dan,
	// each with force, strands what each hold kept.
	f := newDirectoryFixture(t)
	ctx := t.Context()
	ana := f.followed(t, "ana@example.com")
	f.apply(t, document("ana@example.com", 1, inTeam(workspace.RoleOwner, 1)))
	boxes := map[string]string{}
	ids := map[string]string{}
	for _, email := range []string{"bea@example.com", "cy@example.com", "dan@example.com"} {
		ids[email] = f.followed(t, email).UserID
		f.apply(t, document(email, 1, inTeam(workspace.RoleMember, 1)))
		boxes[email] = f.teamBox(t, dirTeamID, ana.UserID, "box-"+email)
		f.giveRead(t, boxes[email], ids[email])
		f.takeRead(t, boxes[email], ana.UserID)
		if got := f.apply(t, document(email, 2)); !slices.Equal(got.Held, []string{dirTeamID}) {
			t.Fatalf("%s's removal was not held: %+v", email, got)
		}
	}

	f.forget(t, "bea@example.com", 2)
	if _, err := f.svc.DisableUser(ctx, admin(), service.CloseUserRequest{Email: "cy@example.com", Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.DeleteUser(ctx, admin(), service.CloseUserRequest{Email: "dan@example.com", Force: true}); err != nil {
		t.Fatal(err)
	}
	actor := admin().Actor()
	want := []string{
		boxes["bea@example.com"] + " forgotten directory " + ids["bea@example.com"],
		boxes["cy@example.com"] + " disabled " + actor + " " + ids["cy@example.com"],
		boxes["dan@example.com"] + " deleted " + actor + " " + ids["dan@example.com"],
	}
	if got := f.strandedRows(t); !slices.Equal(got, want) {
		t.Errorf("the journal holds %v, want %v", got, want)
	}
}

func TestAForgottenPersonsSubjectIsKeptUntilReleasedOrSwept(t *testing.T) {
	f := newDirectoryFixture(t)
	ctx := t.Context()
	bea := f.followed(t, "bea@example.com")
	f.apply(t, document("bea@example.com", 1))
	f.forget(t, "bea@example.com", 2)

	people, err := f.svc.DirectoryPeople(ctx, dirIssuer)
	if err != nil || len(people) != 1 || !people[0].Gone || people[0].Held || people[0].UserID != bea.UserID ||
		people[0].Subject != subjectOf("bea@example.com") || people[0].Status != service.DirectoryClosed {
		t.Fatalf("the directory's people once Bea is forgotten: %+v (%v)", people, err)
	}
	subjects, err := f.svc.DirectorySubjects(ctx, dirIssuer, []string{bea.UserID, "usr_nobody"})
	if err != nil || len(subjects) != 1 || subjects[bea.UserID] != subjectOf("bea@example.com") {
		t.Errorf("Bea's subject once forgotten: %v (%v)", subjects, err)
	}
	// The same subject signing in again is a new person beside her row.
	doc := document("bea@example.com", 3)
	if _, err := f.signInWith(t, ctx, "bea@example.com", &doc); err != nil {
		t.Fatalf("the subject signing in again: %v", err)
	}
	if n, err := f.svc.SweepDirectory(ctx, time.Now()); err != nil || n != 0 {
		t.Errorf("sweeping before 90 days released %d (%v)", n, err)
	}
	if n, err := f.svc.SweepDirectory(ctx, time.Now().Add(store.MailboxJournalRetention+time.Hour)); err != nil || n != 1 {
		t.Errorf("sweeping after 90 days released %d (%v), want Bea's row", n, err)
	}
	people, err = f.svc.DirectoryPeople(ctx, dirIssuer)
	if err != nil || len(people) != 1 || people[0].Gone || people[0].UserID == bea.UserID {
		t.Errorf("the directory's people once swept: %+v (%v), want the new person alone", people, err)
	}
}

// removedByBea is the directory's team, Bea its owner, with a mailbox she
// removed and one she alone reads; it returns both.
func removedByBea(t *testing.T, f *fixture) (bea service.Principal, removed, read string) {
	t.Helper()
	bea = f.followed(t, "bea@example.com")
	f.apply(t, document("bea@example.com", 1, inTeam(workspace.RoleOwner, 1)))
	removed = f.teamBox(t, dirTeamID, bea.UserID, "old@mail.example")
	// Connected: removing it keeps it 30 days, its deletion pending.
	if err := f.repo.SavePassword(t.Context(), removed, "a password"); err != nil {
		t.Fatal(err)
	}
	read = f.teamBox(t, dirTeamID, bea.UserID, "support@mail.example")
	if _, err := f.svc.RemoveAccount(t.Context(), bea, removed, service.RemoveAccountRequest{Confirm: removed}); err != nil {
		t.Fatal(err)
	}
	return bea, removed, read
}

// journalOf lists the journal as "kind actor lost".
func (f *fixture) journalOf(t *testing.T, accountID string) []string {
	t.Helper()
	rows, err := f.db.Reader().QueryContext(t.Context(), `SELECT kind || ' ' || actor || ' ' || lost_user FROM mailbox_events
		WHERE account_id = ? ORDER BY seq`, accountID)
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

func TestAPendingDeletionKeepsItsActorAfterThePersonIsForgotten(t *testing.T) {
	f := newDirectoryFixture(t)
	bea, removed, _ := removedByBea(t, f)
	f.forget(t, "bea@example.com", 2)
	if got := f.journalOf(t, removed); !slices.Equal(got, []string{"removed " + bea.UserID + " "}) {
		t.Errorf("the removal Bea made says %v once she is forgotten, want her name kept until released", got)
	}
}

func TestReleasingAPersonBlanksTheirActorAndLostUser(t *testing.T) {
	f := newDirectoryFixture(t)
	ctx := t.Context()
	bea, removed, read := removedByBea(t, f)
	err := f.svc.ReleaseDirectoryPerson(ctx, dirIssuer, bea.UserID)
	wantIs(t, "releasing a person still here", err, service.CodeConflict, service.ErrPersonStillHere)

	f.forget(t, "bea@example.com", 2)
	if got := f.journalOf(t, read); !slices.Equal(got, []string{"stranded directory " + bea.UserID}) {
		t.Fatalf("the stranding of the mailbox Bea read says %v", got)
	}
	if err := f.svc.ReleaseDirectoryPerson(ctx, dirIssuer, bea.UserID); err != nil {
		t.Fatal(err)
	}
	if got := append(f.journalOf(t, removed), f.journalOf(t, read)...); !slices.Equal(got,
		[]string{"removed  ", "stranded directory "}) {
		t.Errorf("once Bea is released the journal says %v", got)
	}
	if people, err := f.svc.DirectoryPeople(ctx, dirIssuer); err != nil || len(people) != 0 {
		t.Errorf("the directory's people once Bea is released: %+v (%v)", people, err)
	}
	// Releasing again changes nothing.
	if err := f.svc.ReleaseDirectoryPerson(ctx, dirIssuer, bea.UserID); err != nil {
		t.Errorf("releasing again: %v", err)
	}
}

func TestAForcedDeleteJournalsEveryMailboxItStrands(t *testing.T) {
	// No directory follows anyone: Bea alone reads a mailbox of a local team
	// and one of a team from another source. Deleting her with force
	// journals both stranded, and forgets her there at once.
	f := newFixture(t)
	ctx := t.Context()
	tm := newSupportTeam(t, f)
	const local = "acc_00000000000000d1"
	tm.link(t, f, local, "support@mail.example")
	f.giveRead(t, local, tm.bea.UserID)
	f.takeRead(t, local, tm.ana.UserID)
	elsewhere := elsewhereTeam(t, f, "0b4a3c5e-0000-4000-8000-0000000000d3", tm.ana.UserID)
	f.exec(t, `INSERT INTO workspace_members(workspace_id, user_id, role, status, created_at, updated_at)
		VALUES (?, ?, 'member', 'active', 100, 100)`, elsewhere, tm.bea.UserID)
	otherBox := f.teamBox(t, elsewhere, tm.ana.UserID, "elsewhere@mail.example")
	f.giveRead(t, otherBox, tm.bea.UserID)
	f.takeRead(t, otherBox, tm.ana.UserID)

	if _, err := f.svc.DeleteUser(ctx, admin(), service.CloseUserRequest{Email: "bea@example.com"}); service.CodeOf(err) != service.CodeConflict {
		t.Fatalf("deleting the last reader without force: %v", err)
	}
	if rows := f.strandedRows(t); len(rows) != 0 {
		t.Fatalf("a refused deletion journaled %v", rows)
	}
	if _, err := f.svc.DeleteUser(ctx, admin(), service.CloseUserRequest{Email: "bea@example.com", Force: true}); err != nil {
		t.Fatal(err)
	}
	actor := admin().Actor()
	got := f.strandedRows(t)
	want := []string{local + " deleted " + actor + " ", otherBox + " deleted " + actor + " "}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the journal holds %v, want %v: a person no directory follows is forgotten at once", got, want)
	}
}

func TestNoTransportCallsWhatOnlyAnExtensionMay(t *testing.T) {
	// What an extension that follows a directory calls takes no principal:
	// the directory decides. A transport that called one would let a caller
	// stand in for the directory.
	extensionOnly := []string{
		"ApplyDirectoryDocument", "Directory", "ResetDirectory", "ConfirmDirectoryGeneration", "SetDirectoryCursor",
		"ForgetExternalPerson", "ReleaseDirectoryPerson", "DirectoryPeople", "DirectorySubjects", "IdentityUser",
		"SweepDirectory", "PersonalReplacementBlocks", "AccessSnapshot", "MailboxJournal", "MailboxJournalHead",
		"FollowedWorkspaces", "WorkspaceChanges", "ExtensionState", "ExtensionStateKeys", "WriteExtensionState",
	}
	for _, dir := range []string{"../api", "../mcp"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && slices.Contains(extensionOnly, sel.Sel.Name) {
						t.Errorf("%s calls %s, which only an extension may", path, sel.Sel.Name)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
