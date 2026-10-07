package account_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Which mailboxes a caller may see, as the repository applies the rule in
// SQL: active membership of the mailbox's workspace and a grant on it.

type visibilityFixture struct {
	t    *testing.T
	repo *account.Repository
	db   *store.Store
	ws   *workspace.Repository
}

func newVisibility(t *testing.T) *visibilityFixture {
	t.Helper()
	repo, db := newRepo(t)
	return &visibilityFixture{t: t, repo: repo, db: db, ws: workspace.NewRepository(db, nil)}
}

func (f *visibilityFixture) person(email string) auth.User {
	f.t.Helper()
	return authtest.NewUser(f.t, f.db, email, auth.RoleMember)
}

func (f *visibilityFixture) link(id, workspaceID, linker, email string) account.Account {
	f.t.Helper()
	a, err := f.repo.Create(f.t.Context(), account.Account{
		ID: id, Email: email, Provider: provider.KindGmail, AuthKind: "oauth2",
		IMAPHost: "imap.example.com", IMAPPort: 993, SMTPHost: "smtp.example.com", SMTPPort: 465, SMTPTLS: "implicit",
		LoginUser: email, WorkspaceID: workspaceID,
	}, linker)
	if err != nil {
		f.t.Fatalf("link %s: %v", id, err)
	}
	return a
}

func (f *visibilityFixture) sees(v account.Visibility) []string {
	f.t.Helper()
	list, err := f.repo.ListVisible(f.t.Context(), v)
	if err != nil {
		f.t.Fatal(err)
	}
	var ids []string
	for _, a := range list {
		ids = append(ids, a.ID)
		// Fetching one applies the same rule as listing.
		if _, err := f.repo.GetVisible(f.t.Context(), a.ID, v); err != nil {
			f.t.Errorf("%s is listed for %+v and not fetched: %v", a.ID, v, err)
		}
	}
	slices.Sort(ids)
	return ids
}

func (f *visibilityFixture) hidden(id string, v account.Visibility) {
	f.t.Helper()
	if _, err := f.repo.GetVisible(f.t.Context(), id, v); !errors.Is(err, account.ErrNotFound) {
		f.t.Errorf("%s is visible to %+v: %v", id, v, err)
	}
}

func TestAMemberWithoutAGrantCannotSeeTheMailbox(t *testing.T) {
	f := newVisibility(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	team, err := f.ws.CreateTeam(t.Context(), "Support", ana.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Write(t.Context(), func(tx *sql.Tx) error {
		return f.ws.AddMemberTx(t.Context(), tx, team.ID, bea.ID, workspace.RoleMember, f.db.Now())
	}); err != nil {
		t.Fatal(err)
	}
	shared := f.link("acc_shared", team.ID, ana.ID, "support@example.org")

	if got := f.sees(account.Visibility{UserID: bea.ID}); len(got) != 0 {
		t.Errorf("a member without a grant sees %v", got)
	}
	f.hidden(shared.ID, account.Visibility{UserID: bea.ID})
	if _, err := f.ws.SetGrant(t.Context(), shared.ID, bea.ID, workspace.Flags{Send: true}, ana.ID, nil); err != nil {
		t.Fatal(err)
	}
	// Any grant shows the mailbox; Need narrows to the flags asked.
	if got := f.sees(account.Visibility{UserID: bea.ID}); !slices.Equal(got, []string{shared.ID}) {
		t.Errorf("a send-only member sees %v", got)
	}
	f.hidden(shared.ID, account.Visibility{UserID: bea.ID, Need: workspace.Flags{Read: true}})
	if got := f.sees(account.Visibility{UserID: bea.ID, Need: workspace.Flags{Send: true}}); !slices.Equal(got, []string{shared.ID}) {
		t.Errorf("a send-only member sees %v among what they may send from", got)
	}
	// A person disabled on the instance sees nothing, grant or not.
	if _, err := f.db.Writer().ExecContext(t.Context(), `UPDATE users SET status = 'disabled' WHERE id = ?`, bea.ID); err != nil {
		t.Fatal(err)
	}
	f.hidden(shared.ID, account.Visibility{UserID: bea.ID})
}

func TestOwnersAndAdminsSeeEveryTeamMailboxsCardAndReadNone(t *testing.T) {
	f := newVisibility(t)
	ana, bea, cid, dan := f.person("ana@example.org"), f.person("bea@example.org"), f.person("cid@example.org"),
		f.person("dan@example.org")
	team, err := f.ws.CreateTeam(t.Context(), "Support", ana.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	for user, r := range map[string]workspace.Role{bea.ID: workspace.RoleAdmin, cid.ID: workspace.RoleMember, dan.ID: workspace.RoleMember} {
		if err := f.db.Write(t.Context(), func(tx *sql.Tx) error {
			return f.ws.AddMemberTx(t.Context(), tx, team.ID, user, r, f.db.Now())
		}); err != nil {
			t.Fatal(err)
		}
	}
	box := f.link("acc_box", team.ID, cid.ID, "cid-team@example.org")
	// The owner and the admin manage it by their role: its card, and
	// manage; never read, act or send.
	for _, who := range []auth.User{ana, bea} {
		if got := f.sees(account.Visibility{UserID: who.ID}); !slices.Equal(got, []string{box.ID}) {
			t.Errorf("%s sees %v, want the team mailbox's card", who.Email, got)
		}
		if got := f.sees(account.Visibility{UserID: who.ID, Need: workspace.Flags{Manage: true}}); !slices.Equal(got, []string{box.ID}) {
			t.Errorf("%s manages %v", who.Email, got)
		}
		for _, need := range []workspace.Flags{{Read: true}, {Act: true}, {Send: true}, {Read: true, Manage: true}} {
			f.hidden(box.ID, account.Visibility{UserID: who.ID, Need: need})
		}
	}
	// A member holding nothing sees nothing; the instance's owner role is
	// no workspace role.
	f.hidden(box.ID, account.Visibility{UserID: dan.ID})
	boss := authtest.NewUser(t, f.db, "boss@example.org", auth.RoleOwner)
	f.hidden(box.ID, account.Visibility{UserID: boss.ID})
	// The member who linked it reads it, and manages it only with manage
	// stored for them.
	if got := f.sees(account.Visibility{UserID: cid.ID, Need: workspace.Flags{Read: true}}); !slices.Equal(got, []string{box.ID}) {
		t.Errorf("the linker reads %v", got)
	}
	f.hidden(box.ID, account.Visibility{UserID: cid.ID, Need: workspace.Flags{Manage: true}})
	// An admin disabled in the team sees nothing by the role any more.
	if _, err := f.db.Writer().ExecContext(t.Context(),
		`UPDATE workspace_members SET status = 'disabled' WHERE workspace_id = ? AND user_id = ?`, team.ID, bea.ID); err != nil {
		t.Fatal(err)
	}
	f.hidden(box.ID, account.Visibility{UserID: bea.ID})
}

func TestTheOperatorWorkspaceIsWhatUnownedReaches(t *testing.T) {
	f := newVisibility(t)
	ana := f.person("ana@example.org")
	mine := f.link("acc_mine", "", ana.ID, "ana@gmail.com")
	ops := f.link("acc_ops", "", "", "ops@example.org")
	if mine.WorkspaceID != authtest.Personal(t, f.db, ana.ID) || ops.WorkspaceID != workspace.OperatorID {
		t.Fatalf("default workspaces: %s, %s", mine.WorkspaceID, ops.WorkspaceID)
	}
	if got := f.sees(account.Visibility{Unowned: true}); !slices.Equal(got, []string{ops.ID}) {
		t.Errorf("the operator's view is %v", got)
	}
	if got := f.sees(account.Visibility{UserID: ana.ID}); !slices.Equal(got, []string{mine.ID}) {
		t.Errorf("Ana's view is %v", got)
	}
	if got := f.sees(account.Visibility{All: true}); !slices.Equal(got, []string{mine.ID, ops.ID}) {
		t.Errorf("everything is %v", got)
	}
	if got := f.sees(account.Visibility{All: true, Workspace: workspace.OperatorID}); !slices.Equal(got, []string{ops.ID}) {
		t.Errorf("narrowed to the operator workspace: %v", got)
	}
	if got := f.sees(account.Visibility{UserID: ana.ID, Workspace: workspace.OperatorID}); len(got) != 0 {
		t.Errorf("Ana narrowed to the operator workspace sees %v", got)
	}
}

func TestTheSameAddressInTwoWorkspacesIsTwoIndependentMailboxes(t *testing.T) {
	const address, password = "shared@icloud.com", "abcd-efgh-ijkl-mnop"
	repo, db := newRepo(t)
	registry := account.NewRegistry(t.Context(), repo, account.RegistryOptions{
		SpoolDir: t.TempDir(), AllowInsecureAuth: true, AllowPrivate: true,
	})
	t.Cleanup(func() { _ = registry.Close() })
	srv := providertest.NewIMAPServer(t, providertest.IMAPOptions{User: address, Password: password})
	registry.RedirectForTest(appleIMAP, srv.Addr)
	ana := authtest.NewUser(t, db, "ana@example.org", auth.RoleMember)
	ws := workspace.NewRepository(db, nil)
	team, err := ws.CreateTeam(t.Context(), "Support", ana.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	add := func(owner, workspaceID string) (account.Account, error) {
		a, _, err := registry.Add(t.Context(), account.AddRequest{
			Email: address, Provider: provider.KindIMAP, ICloud: true, Password: password,
			LinkerID: owner, WorkspaceID: workspaceID,
		})
		return a, err
	}
	personal, err := add(ana.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	inTeam, err := add(ana.ID, team.ID)
	if err != nil {
		t.Fatalf("the same address in a team: %v", err)
	}
	operator, err := add("", "")
	if err != nil {
		t.Fatalf("the same address for the operator: %v", err)
	}
	if personal.ID == inTeam.ID || inTeam.ID == operator.ID {
		t.Fatalf("one mailbox for two links: %s %s %s", personal.ID, inTeam.ID, operator.ID)
	}
	if _, err := add(ana.ID, team.ID); !errors.Is(err, account.ErrDuplicate) {
		t.Errorf("the same address twice in the team: %v, want ErrDuplicate", err)
	}

	// Each has its own credentials; removing one leaves the others whole.
	for _, a := range []account.Account{personal, inTeam, operator} {
		if pw, err := repo.Password(t.Context(), a.ID); err != nil || pw != password {
			t.Errorf("%s's password: %q, %v", a.ID, pw, err)
		}
	}
	if err := registry.Remove(t.Context(), inTeam.ID); err != nil {
		t.Fatal(err)
	}
	for _, a := range []account.Account{personal, operator} {
		if _, err := repo.Password(t.Context(), a.ID); err != nil {
			t.Errorf("removing the team's link took %s's credentials: %v", a.ID, err)
		}
	}
	// And nothing says to one workspace that the address is linked in
	// another: each person sees their own.
	got, err := repo.ListVisible(context.Background(), account.Visibility{UserID: ana.ID})
	if err != nil || len(got) != 1 || got[0].ID != personal.ID {
		t.Errorf("Ana sees %+v, %v", got, err)
	}
}
