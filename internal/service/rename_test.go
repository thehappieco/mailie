package service_test

import (
	"strings"
	"testing"

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
