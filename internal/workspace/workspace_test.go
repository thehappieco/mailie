package workspace_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
	"github.com/thehappieco/mailie/internal/workspace"
)

type fixture struct {
	t        *testing.T
	db       *store.Store
	ws       *workspace.Repository
	accounts *account.Repository
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := storetest.New(t)
	return &fixture{t: t, db: db, ws: workspace.NewRepository(db, nil), accounts: account.NewRepository(db, nil)}
}

func (f *fixture) person(email string) auth.User {
	f.t.Helper()
	return authtest.NewUser(f.t, f.db, email, auth.RoleMember)
}

// team makes a team owned by owner, with the others as members of the roles
// given ("bea:admin").
func (f *fixture) team(name string, owner auth.User, others map[string]workspace.Role) workspace.Workspace {
	f.t.Helper()
	w, err := f.ws.CreateTeam(f.t.Context(), name, owner.ID, nil)
	if err != nil {
		f.t.Fatalf("CreateTeam: %v", err)
	}
	for id, role := range others {
		f.join(w.ID, id, role)
	}
	return w
}

func (f *fixture) join(workspaceID, userID string, role workspace.Role) {
	f.t.Helper()
	err := f.db.Write(f.t.Context(), func(tx *sql.Tx) error {
		return f.ws.AddMemberTx(f.t.Context(), tx, workspaceID, userID, role, f.db.Now())
	})
	if err != nil {
		f.t.Fatalf("AddMemberTx: %v", err)
	}
}

// link links a mailbox into a workspace as linker; an empty linker is the
// operator's.
func (f *fixture) link(workspaceID, linker, email string) account.Account {
	f.t.Helper()
	a, err := f.accounts.Create(f.t.Context(), account.Account{
		ID:    "acc_" + strings.NewReplacer("@", "_", ".", "_").Replace(email) + "_" + workspaceID[len(workspaceID)-4:],
		Email: email, Provider: provider.KindGmail, AuthKind: "oauth2",
		IMAPHost: "imap.example.com", IMAPPort: 993, SMTPHost: "smtp.example.com", SMTPPort: 465, SMTPTLS: "implicit",
		LoginUser: email, WorkspaceID: workspaceID,
	}, linker)
	if err != nil {
		f.t.Fatalf("link %s: %v", email, err)
	}
	return a
}

func (f *fixture) grant(accountID, userID string, flags workspace.Flags) workspace.Grant {
	f.t.Helper()
	g, err := f.ws.SetGrant(f.t.Context(), accountID, userID, flags, "usr_test", nil)
	if err != nil {
		f.t.Fatalf("SetGrant: %v", err)
	}
	return g
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.Reader().QueryRowContext(f.t.Context(), query, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func role(r workspace.Role) *workspace.Role         { return &r }
func status(s workspace.Status) *workspace.Status   { return &s }
func readOnly() workspace.Flags                     { return workspace.Flags{Read: true} }
func readAct() workspace.Flags                      { return workspace.Flags{Read: true, Act: true} }
func manageOnly() workspace.Flags                   { return workspace.Flags{Manage: true} }
func want(t *testing.T, what string, err, is error) { t.Helper(); wantIs(t, what, err, is) }

func wantIs(t *testing.T, what string, err, is error) {
	t.Helper()
	if !errors.Is(err, is) {
		t.Errorf("%s: %v, want %v", what, err, is)
	}
}

func TestAPersonsPersonalWorkspaceIsCreatedWithThem(t *testing.T) {
	f := newFixture(t)
	ana := f.person("ana@example.org")
	personal, err := f.ws.PersonalOf(t.Context(), ana.ID)
	if err != nil {
		t.Fatal(err)
	}
	if personal.Kind != workspace.KindPersonal || personal.Source != workspace.SourceLocal || personal.Name != "" ||
		personal.PersonID != ana.ID || !strings.HasPrefix(personal.ID, "wsp_") || len(personal.ID) != 20 {
		t.Errorf("personal workspace = %+v", personal)
	}
	mine, err := f.ws.ForPerson(t.Context(), ana.ID)
	if err != nil || len(mine) != 1 || mine[0].ID != personal.ID || mine[0].Role != workspace.RoleOwner ||
		mine[0].Status != workspace.StatusActive {
		t.Errorf("ForPerson = %+v, %v", mine, err)
	}
}

func TestAPersonListsOnlyTheWorkspacesTheyAreAnActiveMemberOf(t *testing.T) {
	f := newFixture(t)
	ana, bea, cid := f.person("ana@example.org"), f.person("bea@example.org"), f.person("cid@example.org")
	support := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleAdmin})
	f.team("Sales", cid, nil)

	mine, err := f.ws.ForPerson(t.Context(), bea.ID)
	if err != nil || len(mine) != 2 || mine[0].Kind != workspace.KindPersonal || mine[1].ID != support.ID ||
		mine[1].Role != workspace.RoleAdmin || mine[1].Name != "Support" {
		t.Fatalf("Bea's workspaces = %+v, %v", mine, err)
	}
	if _, err := f.ws.SetMember(t.Context(), support.ID, bea.ID, workspace.MemberChange{Status: status(workspace.StatusDisabled)}, nil); err != nil {
		t.Fatal(err)
	}
	if mine, _ := f.ws.ForPerson(t.Context(), bea.ID); len(mine) != 1 {
		t.Errorf("a disabled membership is still listed as Bea's: %+v", mine)
	}

	all, err := f.ws.All(t.Context())
	if err != nil || len(all) != 6 || all[0].ID != workspace.OperatorID {
		t.Fatalf("All = %+v, %v", all, err)
	}
	for _, w := range all {
		if w.ID == support.ID && (w.Members != 2 || w.Mailboxes != 0) {
			t.Errorf("Support counts %d members and %d mailboxes", w.Members, w.Mailboxes)
		}
	}
}

func TestTheLastOwnerCannotLeaveOrBeDemoted(t *testing.T) {
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember})
	ctx := t.Context()

	want(t, "demoting the last owner", func() error {
		_, err := f.ws.SetMember(ctx, team.ID, ana.ID, workspace.MemberChange{Role: role(workspace.RoleAdmin)}, nil)
		return err
	}(), workspace.ErrLastOwner)
	want(t, "disabling the last owner", func() error {
		_, err := f.ws.SetMember(ctx, team.ID, ana.ID, workspace.MemberChange{Status: status(workspace.StatusDisabled)}, nil)
		return err
	}(), workspace.ErrLastOwner)
	want(t, "the last owner leaving", f.ws.RemoveMember(ctx, team.ID, ana.ID, nil), workspace.ErrLastOwner)

	members, err := f.ws.Members(ctx, team.ID)
	if err != nil || len(members) != 2 || !members[0].LastOwner || members[1].LastOwner {
		t.Fatalf("the listing does not mark the last owner in advance: %+v, %v", members, err)
	}

	// Another active owner first, and then the first may go.
	if _, err := f.ws.SetMember(ctx, team.ID, bea.ID, workspace.MemberChange{Role: role(workspace.RoleOwner)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.ws.RemoveMember(ctx, team.ID, ana.ID, nil); err != nil {
		t.Fatalf("an owner leaving with another owner left: %v", err)
	}
	// An owner disabled on the instance is no owner the team can count on.
	if _, err := f.db.Writer().ExecContext(ctx, `UPDATE users SET status = 'disabled' WHERE id = ?`, bea.ID); err != nil {
		t.Fatal(err)
	}
	cid := f.person("cid@example.org")
	f.join(team.ID, cid.ID, workspace.RoleOwner)
	want(t, "demoting the only owner left active", func() error {
		_, err := f.ws.SetMember(ctx, team.ID, cid.ID, workspace.MemberChange{Role: role(workspace.RoleMember)}, nil)
		return err
	}(), workspace.ErrLastOwner)
}

func TestATeamMailboxNamesNoPersonAndItsLinkerHoldsReadActAndSend(t *testing.T) {
	// A team mailbox is the team's: no person's, whoever linked it. They
	// hold read, act and send, and manage it by their role, as every owner
	// and admin of the team does; who linked it is attribution only.
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleAdmin})
	shared := f.link(team.ID, bea.ID, "support@example.org")
	mine := f.link(f.personal(ana.ID), ana.ID, "ana@gmail.com")
	ctx := t.Context()

	if shared.OwnerUserID != "" || shared.LinkedBy != bea.ID || shared.WorkspaceID != team.ID {
		t.Errorf("the team mailbox: owner %q, linked by %q, in %s", shared.OwnerUserID, shared.LinkedBy, shared.WorkspaceID)
	}
	if mine.OwnerUserID != ana.ID || mine.LinkedBy != ana.ID {
		t.Errorf("the personal mailbox: owner %q, linked by %q", mine.OwnerUserID, mine.LinkedBy)
	}
	if g, err := f.ws.Grant(ctx, shared.ID, bea.ID); err != nil || g.Flags != workspace.LinkFlags() || g.GrantedBy != bea.ID {
		t.Errorf("the linker's grant = %+v, %v; want read, act and send, granted by themselves", g, err)
	}
	access, err := f.ws.Access(ctx, ana.ID, []string{shared.ID, mine.ID})
	if err != nil {
		t.Fatal(err)
	}
	if access[shared.ID] != manageOnly() {
		t.Errorf("the team's owner, holding no grant, may %+v on its mailbox; want manage by the role, and nothing else", access[shared.ID])
	}
	if access[mine.ID] != (workspace.Flags{Read: true, Act: true, Send: true, Manage: true}) {
		t.Errorf("Ana may %+v on her own mailbox", access[mine.ID])
	}
	// The linker is an ordinary member of the team: their grant changes,
	// and they go, as anyone's does, while someone else reads the mailbox.
	f.grant(shared.ID, ana.ID, readOnly())
	if _, err := f.ws.SetGrant(ctx, shared.ID, bea.ID, workspace.Flags{Send: true}, ana.ID, nil); err != nil {
		t.Errorf("changing the linker's grant: %v", err)
	}
	if err := f.ws.RemoveMember(ctx, team.ID, bea.ID, nil); err != nil {
		t.Errorf("removing the linker: %v", err)
	}
	if a, err := f.accounts.Get(ctx, shared.ID); err != nil || a.LinkedBy != bea.ID || a.OwnerUserID != "" {
		t.Errorf("the mailbox once its linker left: %+v, %v", a, err)
	}
}

func TestTheLastReaderCannotLoseReadBeDisabledOrRemoved(t *testing.T) {
	f := newFixture(t)
	ana, bea, cid := f.person("ana@example.org"), f.person("bea@example.org"), f.person("cid@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleAdmin, cid.ID: workspace.RoleMember})
	shared := f.link(team.ID, bea.ID, "support@example.org")
	ctx := t.Context()

	// Bea linked it and is its only reader; Ana owns the team and reads
	// nothing, which no role makes up for.
	want(t, "revoking the last reader's read", func() error {
		_, err := f.ws.Revoke(ctx, shared.ID, bea.ID, readOnly(), nil)
		return err
	}(), workspace.ErrLastReader)
	want(t, "setting the last reader's grant without read", func() error {
		_, err := f.ws.SetGrant(ctx, shared.ID, bea.ID, workspace.Flags{Send: true}, ana.ID, nil)
		return err
	}(), workspace.ErrLastReader)
	want(t, "disabling the last reader", func() error {
		_, err := f.ws.SetMember(ctx, team.ID, bea.ID, workspace.MemberChange{Status: status(workspace.StatusDisabled)}, nil)
		return err
	}(), workspace.ErrLastReader)
	want(t, "removing the last reader", f.ws.RemoveMember(ctx, team.ID, bea.ID, nil), workspace.ErrLastReader)
	// A role change is no loss of read.
	if _, err := f.ws.SetMember(ctx, team.ID, bea.ID, workspace.MemberChange{Role: role(workspace.RoleMember)}, nil); err != nil {
		t.Errorf("demoting the last reader: %v", err)
	}
	members, err := f.ws.Members(ctx, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		wantLast := m.UserID == bea.ID
		if (len(m.LastReaderOf) == 1 && m.LastReaderOf[0] == shared.ID) != wantLast {
			t.Errorf("%s is marked the last reader of %v", m.Email, m.LastReaderOf)
		}
	}
	if m, err := f.ws.Member(ctx, team.ID, bea.ID); err != nil || len(m.LastReaderOf) != 1 {
		t.Errorf("Bea, read alone: %+v, %v", m, err)
	}
	if b, err := f.ws.Blocks(ctx, bea.ID); err != nil || len(b.LastReaderOf) != 1 || b.LastReaderOf[0] != shared.ID {
		t.Errorf("closing Bea would be blocked by %+v, %v", b, err)
	}

	// A reader disabled on the instance reads nothing: Cid with read does
	// not count while she is off.
	f.grant(shared.ID, cid.ID, readOnly())
	if _, err := f.db.Writer().ExecContext(ctx, `UPDATE users SET status = 'disabled' WHERE id = ?`, cid.ID); err != nil {
		t.Fatal(err)
	}
	want(t, "removing the last reader beside one disabled on the instance", f.ws.RemoveMember(ctx, team.ID, bea.ID, nil),
		workspace.ErrLastReader)
	if _, err := f.db.Writer().ExecContext(ctx, `UPDATE users SET status = 'active' WHERE id = ?`, cid.ID); err != nil {
		t.Fatal(err)
	}
	// With another reader, Bea goes; and then Cid is the last.
	if err := f.ws.RemoveMember(ctx, team.ID, bea.ID, nil); err != nil {
		t.Fatalf("removing a reader while another reads: %v", err)
	}
	want(t, "revoking the new last reader", func() error {
		_, err := f.ws.Revoke(ctx, shared.ID, cid.ID, workspace.Flags{}, nil)
		return err
	}(), workspace.ErrLastReader)
	// A team mailbox nobody reads was never guarded (removing it is the
	// way out), and a personal mailbox is no team's.
	if g, err := f.ws.Grant(ctx, shared.ID, cid.ID); err != nil || !g.Read {
		t.Errorf("the refusals changed Cid's grant: %+v, %v", g, err)
	}
}

func TestConcurrentRemovalsAlwaysKeepAReader(t *testing.T) {
	// Two readers, and at the same time each is taken off the mailbox by a
	// different way: one write has to be refused, whichever commits first.
	for round := range 8 {
		f := newFixture(t)
		ana, bea, cid := f.person("ana@example.org"), f.person("bea@example.org"), f.person("cid@example.org")
		team := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember, cid.ID: workspace.RoleMember})
		shared := f.link(team.ID, bea.ID, "support@example.org")
		f.grant(shared.ID, cid.ID, readOnly())
		ctx := t.Context()

		ways := []func() error{
			func() error { _, err := f.ws.Revoke(ctx, shared.ID, bea.ID, readOnly(), nil); return err },
			func() error { return f.ws.RemoveMember(ctx, team.ID, cid.ID, nil) },
		}
		if round%2 == 1 {
			ways[1] = func() error {
				_, err := f.ws.SetMember(ctx, team.ID, cid.ID, workspace.MemberChange{Status: status(workspace.StatusDisabled)}, nil)
				return err
			}
		}
		errs := make(chan error, len(ways))
		start := make(chan struct{})
		for _, way := range ways {
			go func() { <-start; errs <- way() }()
		}
		close(start)
		var refused, done int
		for range ways {
			switch err := <-errs; {
			case err == nil:
				done++
			case errors.Is(err, workspace.ErrLastReader):
				refused++
			default:
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if done != 1 || refused != 1 {
			t.Errorf("round %d: %d went through and %d were refused, want one each", round, done, refused)
		}
		if n := f.count(`SELECT count(*) FROM mailbox_access g JOIN workspace_members m
			ON m.workspace_id = g.workspace_id AND m.user_id = g.user_id
			WHERE g.account_id = ? AND g.read = 1 AND m.status = 'active'`, shared.ID); n != 1 {
			t.Errorf("round %d: %d readers left, want 1", round, n)
		}
	}
}

func TestManageIsStoredForMembersOnlyAndOwnersAndAdminsManageByRole(t *testing.T) {
	f := newFixture(t)
	ana, bea, cid := f.person("ana@example.org"), f.person("bea@example.org"), f.person("cid@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleAdmin, cid.ID: workspace.RoleMember})
	shared := f.link(team.ID, ana.ID, "support@example.org")
	ctx := t.Context()

	want(t, "manage stored for an admin", func() error {
		_, err := f.ws.SetGrant(ctx, shared.ID, bea.ID, workspace.Flags{Read: true, Manage: true}, ana.ID, nil)
		return err
	}(), workspace.ErrManageByRole)
	want(t, "manage stored for the owner", func() error {
		_, err := f.ws.SetGrant(ctx, shared.ID, ana.ID, workspace.Flags{Read: true, Manage: true}, ana.ID, nil)
		return err
	}(), workspace.ErrManageByRole)
	// Both manage it all the same, and so a consent attempt of theirs
	// stores its grant; a member does once they hold manage.
	err := f.db.Write(ctx, func(tx *sql.Tx) error {
		for _, who := range []string{ana.ID, bea.ID} {
			if err := workspace.ManagesTx(ctx, tx, shared.ID, who); err != nil {
				t.Errorf("%s manages the mailbox: %v", who, err)
			}
		}
		if err := workspace.ManagesTx(ctx, tx, shared.ID, cid.ID); !errors.Is(err, workspace.ErrNoGrant) {
			t.Errorf("a member holding nothing manages it: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f.grant(shared.ID, cid.ID, manageOnly())
	if a, err := f.ws.Access(ctx, cid.ID, []string{shared.ID}); err != nil || a[shared.ID] != manageOnly() {
		t.Errorf("Cid, holding manage, may %+v, %v", a, err)
	}

	// Made an admin, Cid manages by the role: the stored flag goes, and a
	// grant that held nothing else with it.
	if _, err := f.ws.SetMember(ctx, team.ID, cid.ID, workspace.MemberChange{Role: role(workspace.RoleAdmin)}, nil); err != nil {
		t.Fatal(err)
	}
	want(t, "Cid's grant once an admin", func() error { _, err := f.ws.Grant(ctx, shared.ID, cid.ID); return err }(), workspace.ErrNoGrant)
	if a, err := f.ws.Access(ctx, cid.ID, []string{shared.ID}); err != nil || a[shared.ID] != manageOnly() {
		t.Errorf("Cid, an admin, may %+v, %v", a, err)
	}

	// A consent attempt an admin started ends when they stop being one:
	// they manage none of the team's mailboxes then.
	if _, err := f.db.Writer().ExecContext(ctx, `INSERT INTO oauth_pending(state, account_id, owner_user_id, flow,
		redirect_uri, expires_at) VALUES ('st-cid', ?, ?, 'web', 'https://c.example/oauth/return', 9999999999)`,
		shared.ID, cid.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ws.SetMember(ctx, team.ID, cid.ID, workspace.MemberChange{Role: role(workspace.RoleMember)}, nil); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM oauth_pending WHERE owner_user_id = ?`, cid.ID); n != 0 {
		t.Errorf("a demoted admin's consent attempt survived: %d", n)
	}
}

func TestARoleOrStatusChangeExpiresTheInvitesThePersonCreated(t *testing.T) {
	f := newFixture(t)
	ana, bea, cid := f.person("ana@example.org"), f.person("bea@example.org"), f.person("cid@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleAdmin, cid.ID: workspace.RoleAdmin})
	other := f.team("Sales", bea, nil)
	ctx := t.Context()
	invite := func(id, workspaceID, createdBy string) {
		t.Helper()
		if _, err := f.db.Writer().ExecContext(ctx, `INSERT INTO invites(id, code_hash, email, role, created_by,
			created_at, expires_at, workspace_id, workspace_role)
			VALUES (?, randomblob(32), ? || '@example.org', 'member', ?, 1, 9999999999, ?, 'member')`,
			id, id, createdBy, workspaceID); err != nil {
			t.Fatal(err)
		}
	}
	live := func(id string) bool {
		t.Helper()
		return f.count(`SELECT count(*) FROM invites WHERE id = ? AND expires_at > unixepoch()`, id) == 1
	}
	invite("inv_by_bea", team.ID, bea.ID)
	invite("inv_by_bea_sales", other.ID, bea.ID)
	invite("inv_by_cid", team.ID, cid.ID)
	invite("inv_by_ana", team.ID, ana.ID)

	if _, err := f.ws.SetMember(ctx, team.ID, bea.ID, workspace.MemberChange{Role: role(workspace.RoleMember)}, nil); err != nil {
		t.Fatal(err)
	}
	if live("inv_by_bea") {
		t.Error("an invite survived its creator's demotion")
	}
	if !live("inv_by_bea_sales") || !live("inv_by_ana") {
		t.Error("a demotion expired invites made elsewhere or by someone else")
	}
	if err := f.ws.RemoveMember(ctx, team.ID, cid.ID, nil); err != nil {
		t.Fatal(err)
	}
	if live("inv_by_cid") {
		t.Error("an invite survived its creator's removal")
	}
	// Changing nothing expires nothing.
	if _, err := f.ws.SetMember(ctx, team.ID, ana.ID, workspace.MemberChange{Role: role(workspace.RoleOwner)}, nil); err != nil {
		t.Fatal(err)
	}
	if !live("inv_by_ana") {
		t.Error("a change that changed nothing expired an invite")
	}
}

func TestAGrantOnlyGoesToAnActiveMemberOfTheMailboxesWorkspace(t *testing.T) {
	f := newFixture(t)
	ana, bea, cid, dan := f.person("ana@example.org"), f.person("bea@example.org"), f.person("cid@example.org"), f.person("dan@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember, cid.ID: workspace.RoleMember})
	other := f.team("Sales", dan, nil)
	shared := f.link(team.ID, ana.ID, "support@example.org")
	ctx := t.Context()

	if _, err := f.ws.SetMember(ctx, team.ID, cid.ID, workspace.MemberChange{Status: status(workspace.StatusDisabled)}, nil); err != nil {
		t.Fatal(err)
	}
	grantTo := func(user string) error {
		_, err := f.ws.SetGrant(ctx, shared.ID, user, readOnly(), ana.ID, nil)
		return err
	}
	want(t, "a grant to someone of another workspace", grantTo(dan.ID), workspace.ErrNotMember)
	want(t, "a grant to a disabled member", grantTo(cid.ID), workspace.ErrNotMember)
	if _, err := f.db.Writer().ExecContext(ctx, `UPDATE users SET status = 'disabled' WHERE id = ?`, bea.ID); err != nil {
		t.Fatal(err)
	}
	want(t, "a grant to a person disabled on the instance", grantTo(bea.ID), workspace.ErrNotMember)
	if _, err := f.db.Writer().ExecContext(ctx, `UPDATE users SET status = 'active' WHERE id = ?`, bea.ID); err != nil {
		t.Fatal(err)
	}
	if err := grantTo(bea.ID); err != nil {
		t.Fatalf("a grant to an active member: %v", err)
	}
	want(t, "a grant on another workspace's mailbox to its member", func() error {
		_, err := f.ws.SetGrant(ctx, f.link(other.ID, dan.ID, "sales@example.org").ID, bea.ID, readOnly(), dan.ID, nil)
		return err
	}(), workspace.ErrNotMember)
	want(t, "a grant on a mailbox nobody knows", func() error {
		_, err := f.ws.SetGrant(ctx, "acc_nobody", bea.ID, readOnly(), ana.ID, nil)
		return err
	}(), workspace.ErrNoMailbox)
}

func TestAGrantNeedsAFlagAndActNeedsRead(t *testing.T) {
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember})
	shared := f.link(team.ID, ana.ID, "support@example.org")
	ctx := t.Context()

	want(t, "a grant with no flag", func() error {
		_, err := f.ws.SetGrant(ctx, shared.ID, bea.ID, workspace.Flags{}, ana.ID, nil)
		return err
	}(), workspace.ErrNoFlags)
	want(t, "act without read", func() error {
		_, err := f.ws.SetGrant(ctx, shared.ID, bea.ID, workspace.Flags{Act: true}, ana.ID, nil)
		return err
	}(), workspace.ErrActWithoutRead)

	g := f.grant(shared.ID, bea.ID, workspace.Flags{Read: true, Act: true, Send: true})
	if g.Flags != (workspace.Flags{Read: true, Act: true, Send: true}) || g.WorkspaceID != team.ID || g.GrantedBy != "usr_test" {
		t.Fatalf("grant = %+v", g)
	}
	// Revoking read takes act with it; send stays, a send-only member.
	left, err := f.ws.Revoke(ctx, shared.ID, bea.ID, readOnly(), nil)
	if err != nil || left.Flags != (workspace.Flags{Send: true}) {
		t.Fatalf("after revoking read: %+v, %v", left, err)
	}
	left, err = f.ws.Revoke(ctx, shared.ID, bea.ID, workspace.Flags{Send: true}, nil)
	if err != nil || left.Any() {
		t.Fatalf("after revoking the last flag: %+v, %v", left, err)
	}
	want(t, "the grant left with nothing", func() error { _, err := f.ws.Grant(ctx, shared.ID, bea.ID); return err }(), workspace.ErrNoGrant)
	want(t, "revoking a grant that is not there", func() error {
		_, err := f.ws.Revoke(ctx, shared.ID, bea.ID, workspace.Flags{}, nil)
		return err
	}(), workspace.ErrNoGrant)
}

func TestRemovingAMemberRemovesTheirGrantsAndTheirKeysLoseTheMailbox(t *testing.T) {
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember})
	shared := f.link(team.ID, ana.ID, "support@example.org")
	own := f.link(f.personal(bea.ID), bea.ID, "bea@gmail.com")
	f.grant(shared.ID, bea.ID, readAct())
	ctx := t.Context()

	keys := auth.NewKeys(f.db)
	_, onlyShared, err := keys.Issue(ctx, auth.NewKeyRequest{Name: "shared", Scope: auth.ScopeRead, UserID: bea.ID,
		AccountIDs: []string{shared.ID}, TermsVersion: "t"})
	if err != nil {
		t.Fatal(err)
	}
	_, both, err := keys.Issue(ctx, auth.NewKeyRequest{Name: "both", Scope: auth.ScopeRead, UserID: bea.ID,
		AccountIDs: []string{shared.ID, own.ID}, TermsVersion: "t"})
	if err != nil {
		t.Fatal(err)
	}

	if err := f.ws.RemoveMember(ctx, team.ID, bea.ID, nil); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM mailbox_access WHERE user_id = ? AND workspace_id = ?`, bea.ID, team.ID); n != 0 {
		t.Errorf("a removed member kept %d grants", n)
	}
	listed, err := keys.ListFor(ctx, bea.ID)
	if err != nil {
		t.Fatal(err)
	}
	byPrefix := map[string]auth.Key{}
	for _, k := range listed {
		byPrefix[k.Prefix] = k
	}
	if k := byPrefix[onlyShared.Prefix]; !k.Revoked() {
		t.Errorf("the key made for the lost mailbox alone is %+v, want revoked", k)
	}
	if k := byPrefix[both.Prefix]; k.Revoked() || len(k.AccountIDs) != 1 || k.AccountIDs[0] != own.ID {
		t.Errorf("the key for both mailboxes is %+v, want it reaching only Bea's own", k)
	}
	if _, err := f.ws.Member(ctx, team.ID, bea.ID); !errors.Is(err, workspace.ErrNotMember) {
		t.Errorf("Bea is still a member: %v", err)
	}
}

func (f *fixture) personal(userID string) string {
	f.t.Helper()
	return authtest.Personal(f.t, f.db, userID)
}

func TestDisablingAMemberRemovesTheirGrantsAndEnablingDoesNotRestoreThem(t *testing.T) {
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember})
	shared := f.link(team.ID, ana.ID, "support@example.org")
	f.grant(shared.ID, bea.ID, readOnly())
	ctx := t.Context()

	m, err := f.ws.SetMember(ctx, team.ID, bea.ID, workspace.MemberChange{Status: status(workspace.StatusDisabled)}, nil)
	if err != nil || m.Status != workspace.StatusDisabled || m.Active() {
		t.Fatalf("disable: %+v, %v", m, err)
	}
	if n := f.count(`SELECT count(*) FROM mailbox_access WHERE user_id = ?`, bea.ID); n != 0 {
		t.Errorf("a disabled member kept %d grants", n)
	}
	m, err = f.ws.SetMember(ctx, team.ID, bea.ID, workspace.MemberChange{Status: status(workspace.StatusActive)}, nil)
	if err != nil || !m.Active() {
		t.Fatalf("enable: %+v, %v", m, err)
	}
	if n := f.count(`SELECT count(*) FROM mailbox_access WHERE user_id = ?`, bea.ID); n != 0 {
		t.Errorf("enabling restored %d grants", n)
	}
}

func TestPersonalAndOperatorWorkspacesTakeNoMembersOrGrants(t *testing.T) {
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	personal := f.personal(ana.ID)
	mine := f.link(personal, ana.ID, "ana@gmail.com")
	ops := f.link(workspace.OperatorID, "", "ops@example.org")
	ctx := t.Context()

	for _, c := range []struct {
		ws  string
		err error
	}{{personal, workspace.ErrPersonal}, {workspace.OperatorID, workspace.ErrOperator}} {
		want(t, "joining "+c.ws, f.db.Write(ctx, func(tx *sql.Tx) error {
			return f.ws.AddMemberTx(ctx, tx, c.ws, bea.ID, workspace.RoleMember, f.db.Now())
		}), c.err)
		want(t, "changing a member of "+c.ws, func() error {
			_, err := f.ws.SetMember(ctx, c.ws, ana.ID, workspace.MemberChange{Role: role(workspace.RoleAdmin)}, nil)
			return err
		}(), c.err)
		want(t, "removing a member of "+c.ws, f.ws.RemoveMember(ctx, c.ws, ana.ID, nil), c.err)
		want(t, "renaming "+c.ws, func() error { _, err := f.ws.Rename(ctx, c.ws, "Mine", nil); return err }(), c.err)
	}
	want(t, "a grant on a personal mailbox", func() error {
		_, err := f.ws.SetGrant(ctx, mine.ID, bea.ID, readOnly(), ana.ID, nil)
		return err
	}(), workspace.ErrPersonal)
	want(t, "a grant on an operator mailbox", func() error {
		_, err := f.ws.SetGrant(ctx, ops.ID, ana.ID, manageOnly(), "cli", nil)
		return err
	}(), workspace.ErrOperator)
	// A linked mailbox never goes into the operator workspace, and one
	// nobody linked never anywhere else.
	if _, err := f.accounts.Create(ctx, account.Account{ID: "acc_x", Email: "x@example.org", Provider: provider.KindGmail,
		AuthKind: "oauth2", IMAPHost: "h", IMAPPort: 993, SMTPHost: "h", SMTPPort: 465, SMTPTLS: "implicit", LoginUser: "x",
		WorkspaceID: workspace.OperatorID}, ana.ID); !errors.Is(err, account.ErrNoWorkspace) {
		t.Errorf("a linked mailbox in the operator workspace: %v", err)
	}
	if _, err := f.accounts.Create(ctx, account.Account{ID: "acc_y", Email: "y@example.org", Provider: provider.KindGmail,
		AuthKind: "oauth2", IMAPHost: "h", IMAPPort: 993, SMTPHost: "h", SMTPPort: 465, SMTPTLS: "implicit", LoginUser: "y",
		WorkspaceID: personal}, ""); !errors.Is(err, account.ErrNoWorkspace) {
		t.Errorf("a mailbox nobody linked in a personal workspace: %v", err)
	}
	// And nobody links into a workspace they are not an active member of.
	if _, err := f.accounts.Create(ctx, account.Account{ID: "acc_z", Email: "z@example.org", Provider: provider.KindGmail,
		AuthKind: "oauth2", IMAPHost: "h", IMAPPort: 993, SMTPHost: "h", SMTPPort: 465, SMTPTLS: "implicit", LoginUser: "z",
		WorkspaceID: personal}, bea.ID); !errors.Is(err, workspace.ErrNotMember) {
		t.Errorf("Bea linking into Ana's personal workspace: %v", err)
	}
}

func TestThePlatformSourceRefusesLocalChanges(t *testing.T) {
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	local := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember})
	shared := f.link(local.ID, ana.ID, "support@example.org")
	platform := workspace.NewRepository(f.db, workspace.Platform())
	ctx := t.Context()

	if platform.Source().Name() != workspace.SourcePlatform {
		t.Errorf("source = %q", platform.Source().Name())
	}
	want(t, "creating a team", func() error { _, err := platform.CreateTeam(ctx, "Sales", ana.ID, nil); return err }(),
		workspace.ErrManagedElsewhere)
	want(t, "renaming a team", func() error { _, err := platform.Rename(ctx, local.ID, "Help", nil); return err }(),
		workspace.ErrManagedElsewhere)
	want(t, "changing a member", func() error {
		_, err := platform.SetMember(ctx, local.ID, bea.ID, workspace.MemberChange{Role: role(workspace.RoleAdmin)}, nil)
		return err
	}(), workspace.ErrManagedElsewhere)
	want(t, "removing a member", platform.RemoveMember(ctx, local.ID, bea.ID, nil), workspace.ErrManagedElsewhere)
	// Grants are Mailie's, whatever the source.
	if _, err := platform.SetGrant(ctx, shared.ID, bea.ID, readOnly(), ana.ID, nil); err != nil {
		t.Errorf("a grant under the platform source: %v", err)
	}
	// It creates no personal workspace for a new person.
	if err := f.db.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO users(id, email, password_hash, role, password_changed_at, created_at, updated_at)
			VALUES ('usr_platform', 'p@example.org', 'x', 'member', 1, 1, 1)`); err != nil {
			return err
		}
		return workspace.Platform().PersonCreatedTx(ctx, tx, "usr_platform", f.db.Now())
	}); err != nil {
		t.Fatal(err)
	}
	want(t, "the personal workspace of a person the platform source saw created", func() error {
		_, err := f.ws.PersonalOf(ctx, "usr_platform")
		return err
	}(), workspace.ErrNotFound)

	// A workspace the platform sent is never changed locally, whatever the
	// configured source.
	if _, err := f.db.Writer().ExecContext(ctx, `INSERT INTO workspaces(id, kind, source, name, created_at, updated_at)
		VALUES ('0b4a3c5e-0000-4000-8000-000000000001', 'team', 'platform', 'From the platform', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	want(t, "renaming a platform workspace locally", func() error {
		_, err := f.ws.Rename(ctx, "0b4a3c5e-0000-4000-8000-000000000001", "Mine", nil)
		return err
	}(), workspace.ErrManagedElsewhere)
}

func TestACheckRunsInsideTheWriteAndItsRefusalChangesNothing(t *testing.T) {
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember})
	shared := f.link(team.ID, ana.ID, "support@example.org")
	ctx := t.Context()
	refused := errors.New("not yours to do")
	sawMember := false
	check := func(tx *sql.Tx) error {
		// The check reads inside the write's transaction.
		m, err := workspace.MemberTx(ctx, tx, team.ID, ana.ID)
		sawMember = err == nil && m.Role == workspace.RoleOwner
		return refused
	}
	want(t, "SetGrant", func() error { _, err := f.ws.SetGrant(ctx, shared.ID, bea.ID, readOnly(), ana.ID, check); return err }(), refused)
	want(t, "Rename", func() error { _, err := f.ws.Rename(ctx, team.ID, "Other", check); return err }(), refused)
	want(t, "RemoveMember", f.ws.RemoveMember(ctx, team.ID, bea.ID, check), refused)
	want(t, "CreateTeam", func() error { _, err := f.ws.CreateTeam(ctx, "Sales", ana.ID, check); return err }(), refused)
	if !sawMember {
		t.Error("the check could not read the membership inside the transaction")
	}
	if n := f.count(`SELECT count(*) FROM mailbox_access WHERE user_id = ?`, bea.ID); n != 0 {
		t.Errorf("a refused grant left %d rows", n)
	}
	if w, _ := f.ws.Get(ctx, team.ID); w.Name != "Support" {
		t.Errorf("a refused rename left %q", w.Name)
	}
	if n := f.count(`SELECT count(*) FROM workspaces WHERE kind = 'team'`); n != 1 {
		t.Errorf("a refused creation left %d teams", n)
	}
}

func TestATeamNameIsChecked(t *testing.T) {
	f := newFixture(t)
	ana := f.person("ana@example.org")
	for _, name := range []string{"", "   ", strings.Repeat("x", workspace.MaxNameLength+1), "tab\there"} {
		if _, err := f.ws.CreateTeam(t.Context(), name, ana.ID, nil); !errors.Is(err, workspace.ErrInvalidName) {
			t.Errorf("CreateTeam(%q): %v", name, err)
		}
	}
	w, err := f.ws.CreateTeam(t.Context(), "  Café ☕  ", ana.ID, nil)
	if err != nil || w.Name != "Café ☕" {
		t.Errorf("CreateTeam = %+v, %v", w, err)
	}
	if _, err := f.ws.CreateTeam(t.Context(), "Ghosts", "usr_nobody", nil); !errors.Is(err, workspace.ErrNoSuchPerson) {
		t.Errorf("a team for nobody: %v", err)
	}
}

func TestTheDirectoryShowsAddressesGrantsReadersAndTheTeamsConsent(t *testing.T) {
	f := newFixture(t)
	ana, bea, cid := f.person("ana@example.org"), f.person("bea@example.org"), f.person("cid@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember, cid.ID: workspace.RoleMember})
	first := f.link(team.ID, ana.ID, "support@example.org")
	second := f.link(team.ID, ana.ID, "billing@example.org")
	f.grant(first.ID, bea.ID, workspace.Flags{Read: true, Manage: true})
	f.grant(second.ID, cid.ID, readOnly())
	ctx := t.Context()
	if _, err := f.db.SetMailboxSync(ctx, first.ID, true, ana.ID, "sync-3", nil); err != nil {
		t.Fatal(err)
	}

	list, err := f.ws.Directory(ctx, team.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("the directory = %+v, %v", list, err)
	}
	all := map[string]workspace.MailboxAccess{}
	for _, mb := range list {
		all[mb.AccountID] = mb
		if (mb.AccountID != first.ID && mb.AccountID != second.ID) || mb.LinkedBy != ana.ID || len(mb.Grants) != 2 ||
			mb.Readers != 2 || mb.NoReader {
			t.Errorf("in the directory: %+v", mb)
		}
	}
	if c := all[first.ID].Sync; c.At.IsZero() || c.By != ana.ID || c.Version != "sync-3" || c.Migrated {
		t.Errorf("the first mailbox's consent: %+v", c)
	}
	if c := all[second.ID].Sync; !c.At.IsZero() || c.By != "" {
		t.Errorf("the second mailbox's consent: %+v", c)
	}
	access, err := f.ws.Access(ctx, bea.ID, []string{first.ID, second.ID})
	if err != nil || len(access) != 1 || access[first.ID] != (workspace.Flags{Read: true, Manage: true}) {
		t.Errorf("Bea's access = %+v, %v", access, err)
	}
	// Nobody reading a team mailbox shows, in advance.
	if _, err := f.db.Writer().ExecContext(ctx, `DELETE FROM mailbox_access WHERE account_id = ?`, second.ID); err != nil {
		t.Fatal(err)
	}
	list, err = f.ws.Directory(ctx, team.ID)
	for _, mb := range list {
		if mb.AccountID == second.ID && (err != nil || mb.Readers != 0 || !mb.NoReader) {
			t.Errorf("a mailbox nobody reads: %+v, %v", mb, err)
		}
	}
}

func TestBlocksNameWhatAPersonWouldLeaveBehindInTheirTeams(t *testing.T) {
	f := newFixture(t)
	ana, bea, cid := f.person("ana@example.org"), f.person("bea@example.org"), f.person("cid@example.org")
	support := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember})
	alone := f.team("Alone", ana, nil)
	shared := f.link(support.ID, ana.ID, "support@example.org")
	f.link(alone.ID, ana.ID, "alone@example.org")
	f.link(f.personal(ana.ID), ana.ID, "ana@gmail.com")
	f.team("Sales", cid, map[string]workspace.Role{ana.ID: workspace.RoleMember})
	ctx := t.Context()

	// She owns Support, others are in it, and she alone reads its mailbox;
	// the team she is alone in and her personal workspace block nothing.
	b, err := f.ws.Blocks(ctx, ana.ID)
	if err != nil || !b.Any() || len(b.LastOwnerOf) != 1 || b.LastOwnerOf[0] != support.ID ||
		len(b.LastReaderOf) != 1 || b.LastReaderOf[0] != shared.ID {
		t.Fatalf("blocks = %+v, %v", b, err)
	}
	f.grant(shared.ID, bea.ID, readOnly())
	b, err = f.ws.Blocks(ctx, ana.ID)
	if err != nil || len(b.LastReaderOf) != 0 || len(b.LastOwnerOf) != 1 {
		t.Fatalf("blocks once Bea reads it = %+v, %v", b, err)
	}
	if b, err := f.ws.Blocks(ctx, bea.ID); err != nil || b.Any() {
		t.Errorf("Bea blocks %+v, %v", b, err)
	}
}

func TestDeletingAPersonTakesTheirPersonalWorkspaceAndTheTeamsTheyWereAloneIn(t *testing.T) {
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	alone := f.team("Alone", ana, nil)
	support := f.team("Support", bea, map[string]workspace.Role{ana.ID: workspace.RoleAdmin})
	shared := f.link(support.ID, bea.ID, "support@example.org")
	f.grant(shared.ID, ana.ID, readOnly())
	ctx := t.Context()
	// A mailbox Ana linked into Support, and the team's consent to sync it
	// she gave: both stay, without her name.
	linked := f.link(support.ID, ana.ID, "ana-linked@example.org")
	if _, err := f.db.SetMailboxSync(ctx, linked.ID, true, ana.ID, "sync-3", nil); err != nil {
		t.Fatal(err)
	}
	f.grant(linked.ID, bea.ID, readOnly())
	// Ana grants something to someone else, which keeps but forgets her.
	cid := f.person("cid@example.org")
	f.join(support.ID, cid.ID, workspace.RoleMember)
	if _, err := f.ws.SetGrant(ctx, shared.ID, cid.ID, readOnly(), ana.ID, nil); err != nil {
		t.Fatal(err)
	}
	inAlone := f.link(alone.ID, ana.ID, "alone@example.org")

	// A workspace that still holds a mailbox is not deleted.
	err := f.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := workspace.DeletePersonTx(ctx, tx, ana.ID)
		return err
	})
	wantIs(t, "deleting with a mailbox left", err, workspace.ErrHoldsMailboxes)

	if err := f.accounts.Delete(ctx, inAlone.ID); err != nil {
		t.Fatal(err)
	}
	var teams []string
	err = f.db.Write(ctx, func(tx *sql.Tx) error {
		var err error
		if teams, err = workspace.DeletePersonTx(ctx, tx, ana.ID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, ana.ID)
		return err
	})
	if err != nil || len(teams) != 1 || teams[0] != alone.ID {
		t.Fatalf("DeletePersonTx = %v, %v", teams, err)
	}
	if n := f.count(`SELECT count(*) FROM workspaces WHERE id = ? OR person_id = ?`, alone.ID, ana.ID); n != 0 {
		t.Errorf("%d of Ana's workspaces are left", n)
	}
	if n := f.count(`SELECT count(*) FROM workspace_members WHERE user_id = ?`, ana.ID); n != 0 {
		t.Errorf("%d of Ana's memberships are left", n)
	}
	if n := f.count(`SELECT count(*) FROM mailbox_access WHERE user_id = ? OR granted_by = ?`, ana.ID, ana.ID); n != 0 {
		t.Errorf("%d grants still name Ana", n)
	}
	if g, err := f.ws.Grant(ctx, shared.ID, cid.ID); err != nil || !g.Read || g.GrantedBy != "" {
		t.Errorf("the grant Ana gave = %+v, %v; want it kept, without her name", g, err)
	}
	if a, err := f.accounts.Get(ctx, linked.ID); err != nil || a.LinkedBy != "" || a.SyncConsent.By != "" ||
		a.SyncConsent.At == 0 || a.SyncConsent.Version != "sync-3" {
		t.Errorf("the mailbox Ana linked = %+v, %v; want it kept, its consent standing, without her name", a, err)
	}
}

func TestAPersonWithoutAPersonalWorkspaceGetsOneAtStart(t *testing.T) {
	// Only a binary from before migration 0008, run on a migrated database,
	// creates a person without a personal workspace; nothing could link a
	// mailbox for them afterwards. The daemon gives them one when it starts,
	// as old as they are, under the local source only.
	f := newFixture(t)
	ctx := t.Context()
	housed := f.person("ana@example.org")
	stray := func(id, email string) {
		t.Helper()
		if _, err := f.db.Writer().ExecContext(ctx, `INSERT INTO users(id, email, name, password_hash, role, status,
			password_changed_at, created_at, updated_at) VALUES (?, ?, '', '$argon2id$x', 'member', 'active', 100, 100, 100)`,
			id, email); err != nil {
			t.Fatal(err)
		}
	}
	stray("usr_00000000000000b1", "bea@example.org")

	if made, err := workspace.NewRepository(f.db, workspace.Platform()).RepairPersonal(ctx); err != nil || made != 0 {
		t.Errorf("the platform source made %d (%v): its workspaces are the platform's to send", made, err)
	}
	if made, err := f.ws.RepairPersonal(ctx); err != nil || made != 1 {
		t.Fatalf("RepairPersonal made %d (%v), want 1", made, err)
	}
	mine, err := f.ws.ForPerson(ctx, "usr_00000000000000b1")
	if err != nil || len(mine) != 1 || mine[0].Kind != workspace.KindPersonal || mine[0].Role != workspace.RoleOwner ||
		mine[0].Status != workspace.StatusActive || mine[0].CreatedAt.Unix() != 100 {
		t.Errorf("the repaired person's workspaces: %+v (%v)", mine, err)
	}
	if theirs, err := f.ws.ForPerson(ctx, housed.ID); err != nil || len(theirs) != 1 {
		t.Errorf("a person who had one: %+v (%v)", theirs, err)
	}
	if made, err := f.ws.RepairPersonal(ctx); err != nil || made != 0 {
		t.Errorf("a second start made %d (%v)", made, err)
	}
}
