package service_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Renaming a mailbox: whoever manages it, by the one name rule, never a
// removed one, never a key; the freshness hook is never asked.

// renameTo is a request to name a mailbox name.
func renameTo(name string) service.RenameAccountRequest {
	return service.RenameAccountRequest{DisplayName: &name}
}

// storedName is a mailbox's name as the database holds it.
func storedName(t *testing.T, f *fixture, id string) string {
	t.Helper()
	var name string
	if err := f.db.Reader().QueryRowContext(t.Context(), `SELECT display_name FROM accounts WHERE id = ?`, id).Scan(&name); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestAPersonRenamesTheirOwnPersonalMailbox(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	id := f.mailbox(t, ana, "ana@mail.example")
	got, err := f.svc.RenameAccount(t.Context(), ana, id, renameTo("Vendas"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != id || got.DisplayName != "Vendas" || got.Email != "ana@mail.example" {
		t.Errorf("the answer is %+v", got)
	}
	if want, err := f.svc.GetAccount(t.Context(), ana, id); err != nil || want.DisplayName != "Vendas" {
		t.Errorf("read again: %+v, %v", want, err)
	}
	// Someone else does not see it at all.
	bob := f.person(t, "bob@example.com", auth.RoleMember)
	_, err = f.svc.RenameAccount(t.Context(), bob, id, renameTo("Mine"))
	wantCode(t, "Bob renaming Ana's mailbox", err, service.CodeNotFound)
}

func TestATeamsOwnersAndAdminsRenameItsMailboxesByTheirRole(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	carol := tm.join(t, f, "carol@example.com", workspace.RoleAdmin)
	dan := tm.join(t, f, "dan@example.com", workspace.RoleOwner)
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
	for name, p := range map[string]service.Principal{"Ana, who linked it": tm.ana, "Carol, an admin": carol, "Dan, an owner holding nothing": dan} {
		if _, err := f.svc.RenameAccount(t.Context(), p, shared, renameTo(name)); err != nil {
			t.Errorf("%s renaming it: %v", name, err)
		}
		if got := storedName(t, f, shared); got != name {
			t.Errorf("after %s's rename it is named %q", name, got)
		}
	}
}

func TestAMemberRenamesATeamMailboxOnlyWithAStoredManage(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
	tm.grant(t, shared, workspace.Flags{Manage: true})
	if _, err := f.svc.RenameAccount(t.Context(), tm.bea, shared, renameTo("Suporte")); err != nil {
		t.Fatalf("Bea, holding manage, renaming it: %v", err)
	}
	if got := storedName(t, f, shared); got != "Suporte" {
		t.Errorf("it is named %q", got)
	}
}

func TestAMemberWhoSeesAMailboxWithoutManageIsNotAuthorizedToRenameIt(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
	tm.grant(t, shared, workspace.Flags{Read: true, Act: true, Send: true})
	_, err := f.svc.RenameAccount(t.Context(), tm.bea, shared, renameTo("Suporte"))
	wantCode(t, "Bea, reading it without manage, renaming it", err, service.CodeNotAuthorized)
	if got := storedName(t, f, shared); got != "" {
		t.Errorf("it is named %q after a refused rename", got)
	}
}

func TestAMailboxTheCallerCannotSeeIsNotFoundToARename(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
	bob := f.person(t, "bob@example.com", auth.RoleMember)
	for name, p := range map[string]service.Principal{"Bea, a member holding nothing": tm.bea, "Bob, outside the team": bob} {
		_, err := f.svc.RenameAccount(t.Context(), p, shared, renameTo("Mine"))
		wantCode(t, name, err, service.CodeNotFound)
	}
	_, err := f.svc.RenameAccount(t.Context(), tm.ana, "acc_0000000000000000", renameTo("Nothing"))
	wantCode(t, "an unknown mailbox", err, service.CodeNotFound)
}

func TestARemovedMailboxIsNeverRenamed(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	id := f.mailbox(t, ana, "ana@mail.example")
	if _, err := f.svc.RemoveAccount(t.Context(), ana, id, service.RemoveAccountRequest{Confirm: id}); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.RenameAccount(t.Context(), ana, id, renameTo("Vendas"))
	wantCode(t, "renaming a removed mailbox", err, service.CodeNotFound)
	if got := storedName(t, f, id); got != "" {
		t.Errorf("the removed mailbox is named %q", got)
	}
}

func TestNothingInAClosedOrWithheldTeamIsRenamed(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
	f.closeTeam(t, tm.id)
	_, err := f.svc.RenameAccount(t.Context(), tm.ana, shared, renameTo("Closed"))
	wantCode(t, "renaming a closed team's mailbox", err, service.CodeNotFound)
	f.reopenTeam(t, tm.id)

	withheld := newFixtureWith(t, fixtureOptions{teamsWithheld: true})
	wt := newSupportTeam(t, withheld)
	wt.link(t, withheld, shared, "support@mail.example")
	_, err = withheld.svc.RenameAccount(t.Context(), wt.ana, shared, renameTo("Withheld"))
	wantCode(t, "renaming a withheld team's mailbox", err, service.CodeNotAuthorized)
	if got := storedName(t, f, shared) + storedName(t, withheld, shared); got != "" {
		t.Errorf("a closed or withheld team's mailbox is named %q", got)
	}
}

func TestAnAPIKeyNeverRenamesAMailbox(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	id := f.mailbox(t, ana, "ana@mail.example")
	key := f.authenticate(t, authtest.NewWorkspaceKey(t, f.db, auth.ScopeSend, authtest.Personal(t, f.db, ana.UserID), ana.UserID,
		workspace.KeyGrant{AccountID: id, Flags: workspace.Flags{Read: true, Act: true, Send: true}}))
	_, err := f.svc.RenameAccount(t.Context(), key, id, renameTo("Mine"))
	wantCode(t, "a key renaming a mailbox it holds", err, service.CodeNotAuthorized)
	if got := storedName(t, f, id); got != "" {
		t.Errorf("a key's refused rename named it %q", got)
	}
}

func TestTheOperatorRenamesOperatorMailboxesAndNoOther(t *testing.T) {
	f := newFixture(t)
	ops, err := f.repo.Create(t.Context(), account.Account{
		ID: "acc_00000000000000e1", WorkspaceID: workspace.OperatorID, Email: "ops@mail.example", Provider: provider.KindIMAP,
		AuthKind: "password", IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example", SMTPPort: 465,
		SMTPTLS: "implicit", LoginUser: "ops@mail.example", State: account.StateActive,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.RenameAccount(t.Context(), admin(), ops.ID, renameTo("Operations")); err != nil {
		t.Fatalf("the operator renaming an operator mailbox: %v", err)
	}
	if got := storedName(t, f, ops.ID); got != "Operations" {
		t.Errorf("it is named %q", got)
	}
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	id := f.mailbox(t, ana, "ana@mail.example")
	_, err = f.svc.RenameAccount(t.Context(), admin(), id, renameTo("Ours"))
	wantCode(t, "the operator renaming a person's mailbox", err, service.CodeNotFound)
}

func TestARenameTrimsTheNameAndAnEmptyOneClearsIt(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	id := f.mailbox(t, ana, "ana@mail.example")
	if got, err := f.svc.RenameAccount(t.Context(), ana, id, renameTo("  Vendas \t")); err != nil || got.DisplayName != "Vendas" {
		t.Fatalf("renamed: %+v, %v", got, err)
	}
	if got, err := f.svc.RenameAccount(t.Context(), ana, id, renameTo("   ")); err != nil || got.DisplayName != "" {
		t.Fatalf("cleared: %+v, %v", got, err)
	}
	if got := storedName(t, f, id); got != "" {
		t.Errorf("it is named %q after clearing it", got)
	}
	_, err := f.svc.RenameAccount(t.Context(), ana, id, service.RenameAccountRequest{})
	wantCode(t, "a rename without a name", err, service.CodeBadRequest)
}

func TestANameTooLongOrWithAControlCharacterIsRefusedAndNothingChanges(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	id := f.mailbox(t, ana, "ana@mail.example")
	if _, err := f.svc.RenameAccount(t.Context(), ana, id, renameTo("Vendas")); err != nil {
		t.Fatal(err)
	}
	// 200 characters pass, an emoji sequence's joiner (a format character) too.
	if _, err := f.svc.RenameAccount(t.Context(), ana, id, renameTo(strings.Repeat("é", 200))); err != nil {
		t.Errorf("200 characters: %v", err)
	}
	if _, err := f.svc.RenameAccount(t.Context(), ana, id, renameTo("Equipe 👩‍💻")); err != nil {
		t.Errorf("an emoji sequence: %v", err)
	}
	for name, bad := range map[string]string{
		"201 characters": strings.Repeat("é", 201), "a line break": "Ven\ndas", "a tab inside": "Ven\tdas", "a NUL": "Ven\x00das",
		"a DEL": "Ven\x7fdas", "a C1 control": "Ven\u0085das",
	} {
		_, err := f.svc.RenameAccount(t.Context(), ana, id, renameTo(bad))
		wantCode(t, name, err, service.CodeBadRequest)
	}
	if got := storedName(t, f, id); got != "Equipe 👩‍💻" {
		t.Errorf("after refused renames it is named %q", got)
	}
}

func TestARenameMarksAFollowedWorkspaceChangedAndAsksTheFreshnessHookNothing(t *testing.T) {
	// A name widens nobody's access: no freshness question (freshness.go).
	// The platform hears it as the item's label, from the team's change
	// mark, which rises in the rename's transaction.
	w := newFreshWorld(t)
	box := w.box(t, "vendas@example.com")
	mark := func() int {
		return w.count(t, `SELECT coalesce((SELECT seq FROM workspace_changes WHERE workspace_id = ?), 0)`, dirTeamID)
	}
	before := mark()
	w.hook.take()
	if _, err := w.svc.RenameAccount(t.Context(), w.ana, box, renameTo("Vendas")); err != nil {
		t.Fatal(err)
	}
	if asked := w.hook.take(); len(asked) != 0 {
		t.Errorf("a rename asked the freshness hook %+v", asked)
	}
	if after := mark(); after <= before {
		t.Errorf("the team's change mark is %d after a rename, %d before", after, before)
	}
}

func TestALinkOrAnAddCarryingANameTheRenameRefusesIsABadRequestAndStoresNothing(t *testing.T) {
	w := newLinkWorld(t)
	for name, bad := range map[string]string{"201 characters": strings.Repeat("a", 201), "a line break": "Ven\ndas"} {
		add := keyed(w.ana, w.passwordAccount(t, "ana@mail.example"))
		add.DisplayName = bad
		_, err := w.svc.AddAccount(t.Context(), w.ana, add)
		wantCode(t, "an add named with "+name, err, service.CodeBadRequest)
		_, err = w.svc.StartLink(t.Context(), w.ana, service.LinkRequest{Provider: "gmail", DisplayName: bad})
		wantCode(t, "a link named with "+name, err, service.CodeBadRequest)
	}
	if n := w.count(t, `SELECT count(*) FROM accounts`); n != 0 {
		t.Errorf("%d mailboxes stored after refused names", n)
	}
	if n := w.count(t, `SELECT count(*) FROM oauth_links`); n != 0 {
		t.Errorf("%d links stored after refused names", n)
	}
}

func TestAMailboxsNameIsStoredTrimmedWhereverItIsGiven(t *testing.T) {
	w := newLinkWorld(t)
	add := keyed(w.ana, w.passwordAccount(t, "ana@mail.example"))
	add.DisplayName = "  Vendas  "
	added, err := w.svc.AddAccount(t.Context(), w.ana, add)
	if err != nil {
		t.Fatal(err)
	}
	if added.Account.DisplayName != "Vendas" || storedName(t, w.fixture, added.Account.ID) != "Vendas" {
		t.Errorf("an add stored %q, answered %q", storedName(t, w.fixture, added.Account.ID), added.Account.DisplayName)
	}
	started := w.start(t, w.ana, service.LinkRequest{DisplayName: "\tSuporte "})
	w.googleNames("ana.lima@gmail.com")
	linked, err := w.finish(t, w.ana, started)
	if err != nil {
		t.Fatal(err)
	}
	if linked.DisplayName != "Suporte" || storedName(t, w.fixture, linked.ID) != "Suporte" {
		t.Errorf("a link stored %q, answered %q", storedName(t, w.fixture, linked.ID), linked.DisplayName)
	}
	if got, err := w.svc.RenameAccount(t.Context(), w.ana, added.Account.ID, renameTo(" Compras\n")); err != nil || got.DisplayName != "Compras" {
		t.Errorf("a rename answered %+v, %v", got, err)
	}
}

// A rename an extension carries for a person (PersonActs.Rename): the same
// act, run once, a name held already recorded as a no-op.

// renamedThrough is a rename of accountID to name that an extension carries
// for p in workspaceID, as request id.
func renamedThrough(t *testing.T, f *fixture, p service.Principal, id, workspaceID, accountID, name string) (service.ActOutcome, error) {
	t.Helper()
	return f.actsFor(t, delegated(p, id, workspaceID, time.Now())).Rename(t.Context(), accountID, name, nil)
}

// recorded reports whether the extension's request id ran, and what it did.
func recorded(t *testing.T, f *fixture, id string) (service.ActOutcome, bool) {
	t.Helper()
	out, ran, err := f.svc.RequestOutcome(t.Context(), "ext", id)
	if err != nil {
		t.Fatal(err)
	}
	return out, ran
}

func TestARenameThroughAnExtensionRunsOnceAndAnswersItsFirstOutcomeAfter(t *testing.T) {
	f := newFixture(t)
	tm := syncedTeam(t, f)
	out, err := renamedThrough(t, f, tm.carol, "r1", tm.id, tm.shared, "  Suporte\t")
	if err != nil || !out.Changed || out.Ran {
		t.Fatalf("Carol, an admin, renaming the team's mailbox: %+v, %v", out, err)
	}
	if got := storedName(t, f, tm.shared); got != "Suporte" {
		t.Errorf("it is named %q, want the name trimmed", got)
	}
	if out, ran := recorded(t, f, "r1"); !ran || !out.Changed {
		t.Errorf("the request's record: %+v, ran %v", out, ran)
	}
	// Renamed since by a session: the request again answers what it did,
	// and names nothing again.
	if _, err := f.svc.RenameAccount(t.Context(), tm.ana, tm.shared, renameTo("Atendimento")); err != nil {
		t.Fatal(err)
	}
	out, err = renamedThrough(t, f, tm.carol, "r1", tm.id, tm.shared, "Suporte")
	if err != nil || !out.Ran || !out.Changed {
		t.Errorf("the request again: %+v, %v; want its first outcome", out, err)
	}
	if got := storedName(t, f, tm.shared); got != "Atendimento" {
		t.Errorf("the request again named it %q", got)
	}
}

func TestARenameThroughAnExtensionToTheNameAMailboxHoldsIsRecordedAsANoOp(t *testing.T) {
	f := newFixture(t)
	tm := syncedTeam(t, f)
	if _, err := f.svc.RenameAccount(t.Context(), tm.ana, tm.shared, renameTo("Suporte")); err != nil {
		t.Fatal(err)
	}
	out, err := renamedThrough(t, f, tm.carol, "r1", tm.id, tm.shared, " Suporte ")
	if err != nil || out.Changed || out.Ran {
		t.Fatalf("a rename to the name it holds: %+v, %v; want done with nothing changed", out, err)
	}
	if out, ran := recorded(t, f, "r1"); !ran || out.Changed {
		t.Errorf("the request's record: %+v, ran %v; want a no-op recorded", out, ran)
	}
	if _, err := f.svc.RenameAccount(t.Context(), tm.ana, tm.shared, renameTo("Atendimento")); err != nil {
		t.Fatal(err)
	}
	out, err = renamedThrough(t, f, tm.carol, "r1", tm.id, tm.shared, "Suporte")
	if err != nil || !out.Ran || out.Changed {
		t.Errorf("the no-op again, renamed since: %+v, %v; want its first outcome", out, err)
	}
	if got := storedName(t, f, tm.shared); got != "Atendimento" {
		t.Errorf("the no-op again named it %q", got)
	}
}

func TestARenameThroughAnExtensionRefusesWhatASessionsRenameRefusesAndRecordsNothing(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	refused := func(what, id string, err error, refusal service.Refusal) {
		t.Helper()
		wantRefusal(t, what, err, refusal)
		if _, ran := recorded(t, f, id); ran {
			t.Errorf("%s was recorded", what)
		}
	}
	// Bea reads it, and manages nothing.
	_, err := renamedThrough(t, f, tm.bea, "r1", tm.id, tm.shared, "Mine")
	refused("a member reading it without manage", "r1", err, service.RefusalNotAuthorized)
	other := "acc_00000000000000bb"
	tm.link(t, f, other, "other@mail.example")
	_, err = renamedThrough(t, f, tm.bea, "r2", tm.id, other, "Mine")
	refused("a member holding nothing on it", "r2", err, service.RefusalNotFound)
	_, err = renamedThrough(t, f, tm.ana, "r3", tm.id, tm.shared, strings.Repeat("é", 201))
	wantCode(t, "a name too long", err, service.CodeBadRequest)
	refused("a name too long", "r3", err, service.RefusalNone)
	_, err = renamedThrough(t, f, tm.ana, "r4", authtest.Personal(t, f.db, tm.ana.UserID), tm.shared, "Elsewhere")
	refused("a mailbox named in another workspace", "r4", err, service.RefusalNotFound)

	// Suspended, the person renames nothing: a name is no narrowing of
	// their own.
	f.suspend(t, tm.carol.UserID)
	_, err = renamedThrough(t, f, tm.carol, "r5", tm.id, tm.shared, "Suspended")
	refused("a suspended admin", "r5", err, service.RefusalActorInactive)
	f.resume(t, tm.carol.UserID)

	// A closed team: said as closed, tried again once it reopens.
	f.closeTeam(t, tm.id)
	_, err = renamedThrough(t, f, tm.ana, "r6", tm.id, tm.shared, "Closed")
	wantCode(t, "a closed team's mailbox", err, service.CodeConflict)
	refused("a closed team's mailbox", "r6", err, service.RefusalTeamClosed)
	f.reopenTeam(t, tm.id)

	// A removed mailbox is not found, to its owners too.
	f.removed(t, tm.ana, tm.shared)
	_, err = renamedThrough(t, f, tm.ana, "r7", tm.id, tm.shared, "Removed")
	refused("a removed mailbox", "r7", err, service.RefusalNotFound)
	if got := storedName(t, f, tm.shared) + storedName(t, f, other); got != "" {
		t.Errorf("refused renames named the mailboxes %q", got)
	}

	// Teams withheld: nothing of a team is renamed.
	withheld := newFixtureWith(t, fixtureOptions{teamsWithheld: true})
	wt := newSupportTeam(t, withheld)
	const shared = "acc_00000000000000aa"
	wt.link(t, withheld, shared, "support@mail.example")
	_, err = withheld.actsFor(t, delegated(wt.ana, "w1", wt.id, time.Now())).Rename(ctx, shared, "Withheld", nil)
	wantRefusal(t, "a withheld team's mailbox", err, service.RefusalTeamWithheld)
	if got := storedName(t, withheld, shared); got != "" {
		t.Errorf("a withheld team's mailbox is named %q", got)
	}
}

func TestAMemberHoldingManageRenamesThroughAnExtensionAsInASession(t *testing.T) {
	f := newFixture(t)
	tm := syncedTeam(t, f)
	_, err := renamedThrough(t, f, tm.bea, "r1", tm.id, tm.shared, "Suporte")
	wantRefusal(t, "Bea without manage", err, service.RefusalNotAuthorized)
	tm.grant(t, tm.shared, workspace.Flags{Read: true, Manage: true})
	// The refused request recorded nothing: the same request runs now.
	if out, err := renamedThrough(t, f, tm.bea, "r1", tm.id, tm.shared, "Suporte"); err != nil || !out.Changed || out.Ran {
		t.Errorf("Bea, holding manage now: %+v, %v", out, err)
	}
	// Her personal mailbox is hers to rename.
	own := f.mailbox(t, tm.bea, "bea@mail.example")
	if out, err := renamedThrough(t, f, tm.bea, "r2", authtest.Personal(t, f.db, tm.bea.UserID), own, "Minha"); err != nil ||
		!out.Changed {
		t.Errorf("Bea renaming her own mailbox: %+v, %v", out, err)
	}
	if got := storedName(t, f, tm.shared) + "/" + storedName(t, f, own); got != "Suporte/Minha" {
		t.Errorf("the names are %q", got)
	}
}

func TestARenameThroughAnExtensionMarksAFollowedWorkspaceChangedAndAsksTheFreshnessHookNothing(t *testing.T) {
	w := newFreshWorld(t)
	box := w.box(t, "vendas@example.com")
	mark := func() int {
		return w.count(t, `SELECT coalesce((SELECT seq FROM workspace_changes WHERE workspace_id = ?), 0)`, dirTeamID)
	}
	before := mark()
	w.hook.take()
	out, err := w.actsFor(t, delegated(w.ana, "r1", dirTeamID, time.Now())).Rename(t.Context(), box, "Vendas", nil)
	if err != nil || !out.Changed {
		t.Fatalf("Ana renaming the team's mailbox through an extension: %+v, %v", out, err)
	}
	if asked := w.hook.take(); len(asked) != 0 {
		t.Errorf("a rename through an extension asked the freshness hook %+v", asked)
	}
	after := mark()
	if after <= before {
		t.Errorf("the team's change mark is %d after a rename, %d before", after, before)
	}
	// A no-op changes nothing a report reads.
	if out, err := w.actsFor(t, delegated(w.ana, "r2", dirTeamID, time.Now())).Rename(t.Context(), box, "Vendas", nil); err != nil ||
		out.Changed {
		t.Fatalf("the same name again: %+v, %v", out, err)
	}
	if again := mark(); again != after {
		t.Errorf("a rename to the name it held moved the team's change mark from %d to %d", after, again)
	}
}

// drawnFrom is what a rename was drawn from: the label want, "" for none.
func drawnFrom(want string) func(label string) bool {
	return func(label string) bool { return label == want }
}

func TestARenameThroughAnExtensionDrawnFromALabelRenamedSinceIsStateChangedAndWritesNothing(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f) // support@mail.example
	if _, err := f.svc.RenameAccount(ctx, tm.ana, tm.shared, renameTo("Suporte")); err != nil {
		t.Fatal(err)
	}
	// Drawn from the label it holds: renamed.
	acts := func(id string) *service.PersonActs { return f.actsFor(t, delegated(tm.carol, id, tm.id, time.Now())) }
	if out, err := acts("r1").Rename(ctx, tm.shared, "Atendimento", drawnFrom("Suporte")); err != nil || !out.Changed {
		t.Fatalf("a rename drawn from the label it holds: %+v, %v", out, err)
	}
	// A session renamed it since a request was drawn from "Atendimento": the
	// write compares, refuses, and neither names it nor records the request.
	if _, err := f.svc.RenameAccount(ctx, tm.ana, tm.shared, renameTo("Vendas")); err != nil {
		t.Fatal(err)
	}
	_, err := acts("r2").Rename(ctx, tm.shared, "Compras", drawnFrom("Atendimento"))
	wantCode(t, "a rename drawn from a label renamed since", err, service.CodeConflict)
	wantRefusal(t, "a rename drawn from a label renamed since", err, service.RefusalStateChanged)
	if got := storedName(t, f, tm.shared); got != "Vendas" {
		t.Errorf("a refused rename left it named %q", got)
	}
	if _, ran := recorded(t, f, "r2"); ran {
		t.Error("a refused rename recorded its request")
	}
	// The name the request asks, held already, is no exception: what it was
	// drawn from is what it compares.
	_, err = acts("r3").Rename(ctx, tm.shared, "Vendas", drawnFrom("Atendimento"))
	wantRefusal(t, "a rename to the name it holds, drawn from another label", err, service.RefusalStateChanged)
	// What it compares is the label a snapshot reports: none for a name that
	// says the address's local part, whatever the name is.
	if _, err := f.svc.RenameAccount(ctx, tm.ana, tm.shared, renameTo("Support team")); err != nil {
		t.Fatal(err)
	}
	_, err = acts("r4").Rename(ctx, tm.shared, "Suporte", drawnFrom("Support team"))
	wantRefusal(t, "a rename drawn from the name, not the label", err, service.RefusalStateChanged)
	if out, err := acts("r5").Rename(ctx, tm.shared, "Suporte", drawnFrom("")); err != nil || !out.Changed {
		t.Errorf("a rename drawn from no label, of a mailbox reported with none: %+v, %v", out, err)
	}
	if got := storedName(t, f, tm.shared); got != "Suporte" {
		t.Errorf("it is named %q", got)
	}
}

// The label a rename would leave (PersonActs.RenamedLabel): what a snapshot
// reports of the mailbox once renamed, read as the rename authorizes it,
// writing nothing.

// labelIfRenamed is the label accountID would be reported under once p
// renamed it name through an extension, as request id in workspaceID.
func labelIfRenamed(t *testing.T, f *fixture, p service.Principal, id, workspaceID, accountID, name string) (string, error) {
	t.Helper()
	return f.actsFor(t, delegated(p, id, workspaceID, time.Now())).RenamedLabel(t.Context(), accountID, name)
}

func TestTheLabelARenameWouldLeaveIsTheNameButOneThatSaysTheAddressAndWritesNothing(t *testing.T) {
	f := newFixture(t)
	tm := syncedTeam(t, f) // support@mail.example
	for i, c := range []struct{ name, want string }{
		{"Suporte", "Suporte"},
		{"  Suporte SP\t", "Suporte SP"},
		{"Support team", ""}, // the address's local part, in any case
		{"SUPPORT", ""},
		{"suporte@loja", ""}, // an address
		{"Sup", "Sup"},
		{"", ""},
		{"Atendimento 24h", "Atendimento 24h"},
	} {
		id := fmt.Sprintf("l%d", i)
		got, err := labelIfRenamed(t, f, tm.carol, id, tm.id, tm.shared, c.name)
		if err != nil || got != c.want {
			t.Errorf("renamed %q, it would be reported as %q (%v), want %q", c.name, got, err, c.want)
		}
		if _, ran := recorded(t, f, id); ran {
			t.Errorf("asking the label of %q recorded the request", c.name)
		}
	}
	if got := storedName(t, f, tm.shared); got != "" {
		t.Errorf("asking labels named the mailbox %q", got)
	}
	// What it says is what the rename then leaves.
	if _, err := renamedThrough(t, f, tm.carol, "r1", tm.id, tm.shared, "Suporte SP"); err != nil {
		t.Fatal(err)
	}
	if got, err := labelIfRenamed(t, f, tm.carol, "l2", tm.id, tm.shared, storedName(t, f, tm.shared)); err != nil ||
		got != "Suporte SP" {
		t.Errorf("the label of the name it holds is %q, %v", got, err)
	}
}

func TestTheLabelARenameWouldLeaveIsRefusedWhereTheRenameIs(t *testing.T) {
	f := newFixture(t)
	tm := syncedTeam(t, f)
	// Bea reads it, and manages nothing.
	_, err := labelIfRenamed(t, f, tm.bea, "l1", tm.id, tm.shared, "Mine")
	wantRefusal(t, "a member reading it without manage", err, service.RefusalNotAuthorized)
	other := "acc_00000000000000bb"
	tm.link(t, f, other, "other@mail.example")
	_, err = labelIfRenamed(t, f, tm.bea, "l2", tm.id, other, "Mine")
	wantRefusal(t, "a member holding nothing on it", err, service.RefusalNotFound)
	_, err = labelIfRenamed(t, f, tm.ana, "l3", tm.id, tm.shared, strings.Repeat("é", 201))
	wantCode(t, "a name too long", err, service.CodeBadRequest)
	_, err = labelIfRenamed(t, f, tm.ana, "l4", authtest.Personal(t, f.db, tm.ana.UserID), tm.shared, "Elsewhere")
	wantRefusal(t, "a mailbox named in another workspace", err, service.RefusalNotFound)

	f.suspend(t, tm.carol.UserID)
	_, err = labelIfRenamed(t, f, tm.carol, "l5", tm.id, tm.shared, "Suspended")
	wantRefusal(t, "a suspended admin", err, service.RefusalActorInactive)
	f.resume(t, tm.carol.UserID)

	f.closeTeam(t, tm.id)
	_, err = labelIfRenamed(t, f, tm.ana, "l6", tm.id, tm.shared, "Closed")
	wantRefusal(t, "a closed team's mailbox", err, service.RefusalTeamClosed)
	f.reopenTeam(t, tm.id)

	// A request that ran already: its record answers.
	if _, err := renamedThrough(t, f, tm.ana, "r1", tm.id, tm.shared, "Suporte"); err != nil {
		t.Fatal(err)
	}
	_, err = labelIfRenamed(t, f, tm.ana, "r1", tm.id, tm.shared, "Suporte")
	wantRefusal(t, "a request that ran", err, service.RefusalRequestDone)

	f.removed(t, tm.ana, tm.shared)
	_, err = labelIfRenamed(t, f, tm.ana, "l7", tm.id, tm.shared, "Removed")
	wantRefusal(t, "a removed mailbox", err, service.RefusalNotFound)

	withheld := newFixtureWith(t, fixtureOptions{teamsWithheld: true})
	wt := newSupportTeam(t, withheld)
	const shared = "acc_00000000000000aa"
	wt.link(t, withheld, shared, "support@mail.example")
	_, err = withheld.actsFor(t, delegated(wt.ana, "w1", wt.id, time.Now())).RenamedLabel(t.Context(), shared, "Withheld")
	wantRefusal(t, "a withheld team's mailbox", err, service.RefusalTeamWithheld)
}

func TestTheLabelARenameWouldLeaveOfATeamClosedBetweenItsReadsIsSaidClosedAsTheRenameSaysIt(t *testing.T) {
	// The request's item is read while the team is open, the rename's
	// authorization once it closed, which hides its mailboxes: said as
	// closed, which an extension tries again once it reopens, never as not
	// found, which it would not.
	f := newFixture(t)
	tm := syncedTeam(t, f)
	acts := f.actsFor(t, delegated(tm.ana, "l1", tm.id, time.Now()))
	_, err := service.RenamedLabelMeanwhileForTest(t.Context(), acts, tm.shared, "Suporte", func() { f.closeTeam(t, tm.id) })
	wantCode(t, "a team closed between the label's reads", err, service.CodeConflict)
	wantRefusal(t, "a team closed between the label's reads", err, service.RefusalTeamClosed)
	if _, ran := recorded(t, f, "l1"); ran {
		t.Error("asking the label recorded the request")
	}
	f.reopenTeam(t, tm.id)
	if got, err := labelIfRenamed(t, f, tm.ana, "l1", tm.id, tm.shared, "Suporte"); err != nil || got != "Suporte" {
		t.Errorf("the same request once the team reopened: %q, %v", got, err)
	}
}
