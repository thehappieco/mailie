package auth_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Invites and sign-up with workspaces: every person gets a personal
// workspace, and a team invite brings a new or an existing person into a
// team.

func signUp(t *testing.T, users *auth.Users, code, email string) auth.User {
	t.Helper()
	_, _, user, err := users.SignUp(t.Context(), auth.SignUpRequest{Invite: code, Email: email, Password: "long enough password"})
	if err != nil {
		t.Fatalf("sign up %s: %v", email, err)
	}
	return user
}

func teamOf(t *testing.T, db *store.Store, owner string) workspace.Workspace {
	t.Helper()
	w, err := workspace.NewRepository(db, nil).CreateTeam(t.Context(), "Support", owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// teamInvite is a team invite the operator made, which may sign a new person
// up.
func teamInvite(t *testing.T, users *auth.Users, email, team string, role workspace.Role) (string, auth.Invite) {
	t.Helper()
	return teamInviteBy(t, users, "cli", email, team, role)
}

// teamInviteBy is a team invite createdBy made.
func teamInviteBy(t *testing.T, users *auth.Users, createdBy, email, team string, role workspace.Role) (string, auth.Invite) {
	t.Helper()
	code, inv, err := users.CreateInvite(t.Context(), auth.NewInvite{
		Email: email, WorkspaceID: team, WorkspaceRole: role, CreatedBy: createdBy,
	})
	if err != nil {
		t.Fatalf("team invite: %v", err)
	}
	return code, inv
}

func TestATeamInviteSignsUpANewPersonOnlyWhenTheOperatorOrAnInstanceOwnerMadeIt(t *testing.T) {
	// Any person may create a team and invite into it. Were that enough to
	// create an account, Mallory, a member, could make one for Alice with a
	// password of her own choosing, and spend the owner invite the operator
	// had waiting for Alice on the way.
	cheapKDF(t)
	users, db, _ := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.org", auth.RoleOwner)
	mallory := authtest.NewUser(t, db, "mallory@example.org", auth.RoleMember)
	authtest.NewUser(t, db, "bea@example.org", auth.RoleMember)
	team := teamOf(t, db, mallory.ID)
	signUpWith := func(code, email string) error {
		_, _, _, err := users.SignUp(t.Context(), auth.SignUpRequest{Invite: code, Email: email, Password: "long enough password"})
		return err
	}
	exists := func(email string) bool {
		t.Helper()
		var n int
		if err := db.Reader().QueryRow(`SELECT count(*) FROM users WHERE email = ?`, email).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}

	toOwner := invite(t, users, "alice@example.org", auth.RoleOwner)
	squat, _ := teamInviteBy(t, users, mallory.ID, "alice@example.org", team.ID, workspace.RoleMember)
	if err := signUpWith(squat, "alice@example.org"); !errors.Is(err, auth.ErrInviteJoinsOnly) {
		t.Fatalf("a member's team invite signed a new person up: %v", err)
	}
	if exists("alice@example.org") {
		t.Fatal("an account was created through a member's team invite")
	}
	if pending, err := users.PendingInvites(t.Context(), ""); err != nil || len(pending) != 1 || pending[0].Email != "alice@example.org" {
		t.Errorf("the operator's invite for Alice after the refusal: %+v, %v", pending, err)
	}
	// The same answer for an address that has an account: the link does
	// not tell its holder which ones do.
	probe, _ := teamInviteBy(t, users, mallory.ID, "bea@example.org", team.ID, workspace.RoleMember)
	if err := signUpWith(probe, "bea@example.org"); !errors.Is(err, auth.ErrInviteJoinsOnly) {
		t.Errorf("a member's team invite for an address with an account: %v", err)
	}

	// Alice arrives as the operator meant, and accepts Mallory's invite,
	// which was not spent, signed in.
	alice := signUp(t, users, toOwner, "alice@example.org")
	if alice.Role != auth.RoleOwner {
		t.Errorf("Alice is %q, want the owner her invite named", alice.Role)
	}
	if joined, err := users.AcceptInvite(t.Context(), alice.ID, squat); err != nil || joined.ID != team.ID {
		t.Errorf("Alice accepting the team invite signed in: %+v, %v", joined, err)
	}

	// The operator's team invites and an active instance owner's sign
	// people up.
	instanceKey, _, _ := strings.Cut(authtest.NewKey(t, db, auth.ScopeAdmin, ""), ".")
	personKey, _, _ := strings.Cut(authtest.NewPersonalKey(t, db, auth.ScopeWrite, mallory.ID), ".")
	for _, c := range []struct{ by, email string }{
		{"cli", "cid@example.org"}, {"key:" + instanceKey, "dan@example.org"}, {ana.ID, "eve@example.org"},
	} {
		code, _ := teamInviteBy(t, users, c.by, c.email, team.ID, workspace.RoleMember)
		if err := signUpWith(code, c.email); err != nil {
			t.Errorf("a team invite made by %s: %v", c.by, err)
		}
	}
	for _, by := range []string{"key:" + personKey, "", "usr_nobody"} {
		code, _ := teamInviteBy(t, users, by, "fay@example.org", team.ID, workspace.RoleMember)
		if err := signUpWith(code, "fay@example.org"); !errors.Is(err, auth.ErrInviteJoinsOnly) {
			t.Errorf("a team invite made by %q signed a new person up: %v", by, err)
		}
	}

	// An owner vouches only while they are one: switched off, the invites
	// they made no longer sign anyone up.
	late, _ := teamInviteBy(t, users, ana.ID, "gil@example.org", team.ID, workspace.RoleMember)
	if _, err := users.Disable(t.Context(), ana.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := signUpWith(late, "gil@example.org"); !errors.Is(err, auth.ErrInviteJoinsOnly) {
		t.Errorf("a disabled owner's team invite signed a new person up: %v", err)
	}
	if exists("gil@example.org") || exists("fay@example.org") {
		t.Error("an account was created through an invite nobody could vouch for")
	}
}

func TestASignUpCreatesThePersonsPersonalWorkspace(t *testing.T) {
	cheapKDF(t)
	users, db, _ := newUsers(t)
	ana := signUp(t, users, invite(t, users, "ana@example.org", auth.RoleMember), "ana@example.org")
	mine, err := workspace.NewRepository(db, nil).ForPerson(t.Context(), ana.ID)
	if err != nil || len(mine) != 1 || mine[0].Kind != workspace.KindPersonal || mine[0].PersonID != ana.ID ||
		mine[0].Role != workspace.RoleOwner {
		t.Fatalf("Ana's workspaces = %+v, %v", mine, err)
	}
}

func TestThePlatformSourceCreatesNoPersonalWorkspaceAtSignUp(t *testing.T) {
	cheapKDF(t)
	users, db, _ := newUsers(t)
	users = users.WithWorkspaceSource(workspace.Platform())
	ana := signUp(t, users, invite(t, users, "ana@example.org", auth.RoleMember), "ana@example.org")
	if _, err := workspace.NewRepository(db, nil).PersonalOf(t.Context(), ana.ID); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("a personal workspace the platform did not send: %v", err)
	}
}

func TestATeamInviteMakesANewPersonWithTheirPersonalWorkspace(t *testing.T) {
	cheapKDF(t)
	users, db, _ := newUsers(t)
	owner := authtest.NewUser(t, db, "owner@example.org", auth.RoleOwner)
	team := teamOf(t, db, owner.ID)
	code, inv := teamInvite(t, users, "Bea@Example.org", team.ID, workspace.RoleAdmin)
	if inv.Email != "bea@example.org" || inv.Role != auth.RoleMember || inv.WorkspaceID != team.ID ||
		inv.WorkspaceRole != workspace.RoleAdmin || !strings.HasPrefix(inv.ID, "inv_") || len(inv.ID) != 20 {
		t.Fatalf("invite = %+v", inv)
	}

	bea := signUp(t, users, code, "bea@example.org")
	if bea.Role != auth.RoleMember {
		t.Errorf("a person a team brought in is %q on the instance, want member", bea.Role)
	}
	mine, err := workspace.NewRepository(db, nil).ForPerson(t.Context(), bea.ID)
	if err != nil || len(mine) != 2 || mine[0].Kind != workspace.KindPersonal || mine[1].ID != team.ID ||
		mine[1].Role != workspace.RoleAdmin {
		t.Fatalf("Bea's workspaces = %+v, %v", mine, err)
	}
}

func TestAnExistingPersonJoinsByAcceptingATeamInvite(t *testing.T) {
	users, db, _ := newUsers(t)
	owner := authtest.NewUser(t, db, "owner@example.org", auth.RoleOwner)
	bea := authtest.NewUser(t, db, "bea@example.org", auth.RoleMember)
	team := teamOf(t, db, owner.ID)
	code, _ := teamInvite(t, users, "bea@example.org", team.ID, workspace.RoleMember)

	// Signing up again is refused: the address has an account.
	if _, _, _, err := users.SignUp(t.Context(), auth.SignUpRequest{Invite: code, Email: "bea@example.org",
		Password: "long enough password"}); !errors.Is(err, auth.ErrEmailTaken) {
		t.Fatalf("signing up with an address that has an account: %v", err)
	}
	joined, err := users.AcceptInvite(t.Context(), bea.ID, code)
	if err != nil || joined.ID != team.ID || joined.Role != workspace.RoleMember || joined.Status != workspace.StatusActive {
		t.Fatalf("AcceptInvite = %+v, %v", joined, err)
	}
	if _, err := users.AcceptInvite(t.Context(), bea.ID, code); !errors.Is(err, auth.ErrInviteInvalid) {
		t.Errorf("the invite worked twice: %v", err)
	}
	// Joining grants no mailbox.
	var grants int
	if err := db.Reader().QueryRow(`SELECT count(*) FROM mailbox_access WHERE user_id = ?`, bea.ID).Scan(&grants); err != nil || grants != 0 {
		t.Errorf("joining gave %d grants (%v)", grants, err)
	}
}

func TestARefusedAcceptanceDoesNotSpendTheInvite(t *testing.T) {
	users, db, clock := newUsers(t)
	owner := authtest.NewUser(t, db, "owner@example.org", auth.RoleOwner)
	bea := authtest.NewUser(t, db, "bea@example.org", auth.RoleMember)
	mallory := authtest.NewUser(t, db, "mallory@example.org", auth.RoleMember)
	team := teamOf(t, db, owner.ID)
	code, inv := teamInvite(t, users, "bea@example.org", team.ID, workspace.RoleMember)

	if _, err := users.AcceptInvite(t.Context(), mallory.ID, code); !errors.Is(err, auth.ErrInviteInvalid) {
		t.Errorf("another address accepted the invite: %v", err)
	}
	if _, err := users.AcceptInvite(t.Context(), owner.ID, code); !errors.Is(err, auth.ErrInviteInvalid) {
		t.Errorf("a member who is not its address accepted the invite: %v", err)
	}
	instance := invite(t, users, "carol@example.org", auth.RoleMember)
	carol := authtest.NewUser(t, db, "carol2@example.org", auth.RoleMember)
	if _, err := users.AcceptInvite(t.Context(), carol.ID, instance); !errors.Is(err, auth.ErrInviteInvalid) {
		t.Errorf("an instance invite was accepted as a team's: %v", err)
	}
	if pending, err := users.PendingInvites(t.Context(), team.ID); err != nil || len(pending) != 1 || pending[0].ID != inv.ID {
		t.Fatalf("after the refusals the team's pending invites are %+v, %v", pending, err)
	}

	if _, err := users.AcceptInvite(t.Context(), bea.ID, code); err != nil {
		t.Fatalf("the person it was for, after the refusals: %v", err)
	}

	// Already a member: refused, and the invite is still there.
	second, _ := teamInvite(t, users, "dan@example.org", team.ID, workspace.RoleMember)
	dan := authtest.NewUser(t, db, "dan@example.org", auth.RoleMember)
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		return workspace.NewRepository(db, nil).AddMemberTx(t.Context(), tx, team.ID, dan.ID, workspace.RoleMember, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := users.AcceptInvite(t.Context(), dan.ID, second); !errors.Is(err, workspace.ErrAlreadyMember) {
		t.Errorf("accepting as a member already: %v", err)
	}
	if pending, _ := users.PendingInvites(t.Context(), team.ID); len(pending) != 1 {
		t.Errorf("a refused acceptance spent the invite: %+v", pending)
	}

	// Expired: refused.
	eve := authtest.NewUser(t, db, "eve@example.org", auth.RoleMember)
	late, _ := teamInvite(t, users, "eve@example.org", team.ID, workspace.RoleMember)
	*clock = clock.Add(auth.InviteTTL + time.Minute)
	if _, err := users.AcceptInvite(t.Context(), eve.ID, late); !errors.Is(err, auth.ErrInviteInvalid) {
		t.Errorf("an expired invite was accepted: %v", err)
	}
}

func TestATeamInviteIsOnlyForATeamAndForSomeoneNotInItYet(t *testing.T) {
	users, db, _ := newUsers(t)
	owner := authtest.NewUser(t, db, "owner@example.org", auth.RoleOwner)
	team := teamOf(t, db, owner.ID)
	personal := authtest.Personal(t, db, owner.ID)

	for _, c := range []struct {
		what string
		in   auth.NewInvite
		want error
	}{
		{"into a personal workspace", auth.NewInvite{Email: "x@example.org", WorkspaceID: personal, WorkspaceRole: workspace.RoleMember}, workspace.ErrPersonal},
		{"into the operator workspace", auth.NewInvite{Email: "x@example.org", WorkspaceID: workspace.OperatorID, WorkspaceRole: workspace.RoleMember}, workspace.ErrOperator},
		{"into a workspace nobody knows", auth.NewInvite{Email: "x@example.org", WorkspaceID: "wsp_nobody", WorkspaceRole: workspace.RoleMember}, workspace.ErrNotFound},
		{"for a member already", auth.NewInvite{Email: "OWNER@example.org", WorkspaceID: team.ID, WorkspaceRole: workspace.RoleAdmin}, workspace.ErrAlreadyMember},
		{"without a role in the team", auth.NewInvite{Email: "x@example.org", WorkspaceID: team.ID}, auth.ErrInvalidWorkspaceRole},
		{"with a team role and no team", auth.NewInvite{Email: "x@example.org", Role: auth.RoleMember, WorkspaceRole: workspace.RoleAdmin}, auth.ErrInvalidWorkspaceRole},
		{"for an address with an account, to the instance", auth.NewInvite{Email: "owner@example.org", Role: auth.RoleMember}, auth.ErrEmailTaken},
	} {
		if _, _, err := users.CreateInvite(t.Context(), c.in); !errors.Is(err, c.want) {
			t.Errorf("an invite %s: %v, want %v", c.what, err, c.want)
		}
	}
	platform := auth.NewUsers(db).WithWorkspaceSource(workspace.Platform())
	if _, _, err := platform.CreateInvite(t.Context(), auth.NewInvite{Email: "x@example.org", WorkspaceID: team.ID,
		WorkspaceRole: workspace.RoleMember}); !errors.Is(err, workspace.ErrManagedElsewhere) {
		t.Errorf("a team invite under the platform source: %v", err)
	}
}

func TestPendingInvitesAreListedAndRevokedPerWorkspace(t *testing.T) {
	users, db, _ := newUsers(t)
	owner := authtest.NewUser(t, db, "owner@example.org", auth.RoleOwner)
	team := teamOf(t, db, owner.ID)
	_, toTeam := teamInvite(t, users, "bea@example.org", team.ID, workspace.RoleMember)
	_, toInstance, err := users.CreateInvite(t.Context(), auth.NewInvite{Email: "cid@example.org", Role: auth.RoleMember})
	if err != nil {
		t.Fatal(err)
	}

	if got, err := users.PendingInvites(t.Context(), team.ID); err != nil || len(got) != 1 || got[0].ID != toTeam.ID {
		t.Errorf("the team's invites: %+v, %v", got, err)
	}
	if got, err := users.PendingInvites(t.Context(), ""); err != nil || len(got) != 1 || got[0].ID != toInstance.ID {
		t.Errorf("the instance's invites: %+v, %v", got, err)
	}
	// A team revokes only its own.
	if err := users.RevokeInvite(t.Context(), team.ID, toInstance.ID, nil); !errors.Is(err, auth.ErrInviteNotFound) {
		t.Errorf("a team revoked the instance's invite: %v", err)
	}
	if _, err := users.Invite(t.Context(), team.ID, toTeam.ID); err != nil {
		t.Errorf("reading the team's invite: %v", err)
	}
	if err := users.RevokeInvite(t.Context(), team.ID, toTeam.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := users.PendingInvites(t.Context(), team.ID); len(got) != 0 {
		t.Errorf("a revoked invite is still pending: %+v", got)
	}
	if err := users.RevokeInvite(t.Context(), team.ID, toTeam.ID, nil); !errors.Is(err, auth.ErrInviteNotFound) {
		t.Errorf("revoking twice: %v", err)
	}
}

func TestTheBootstrapInviteIsAnOwnersOnlyWhileTheServerHasNone(t *testing.T) {
	users, db, clock := newUsers(t)
	needs := func() bool {
		t.Helper()
		ok, err := users.NeedsFirstOwner(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !needs() {
		t.Fatal("an empty server needs no owner")
	}
	// A member invite changes nothing; an owner invite on the way does.
	invite(t, users, "member@example.org", auth.RoleMember)
	if !needs() {
		t.Error("a member invite stands in for an owner")
	}
	invite(t, users, "owner@example.org", auth.RoleOwner)
	if needs() {
		t.Error("an owner invite waiting is not counted")
	}
	// Once it expires it is nobody on the way.
	*clock = clock.Add(auth.InviteTTL + time.Minute)
	if !needs() {
		t.Error("an expired owner invite still counts")
	}
	owner := authtest.NewUser(t, db, "owner2@example.org", auth.RoleOwner)
	if needs() {
		t.Error("an active owner is not counted")
	}
	if _, err := users.Disable(t.Context(), owner.ID, true); err != nil {
		t.Fatal(err)
	}
	if !needs() {
		t.Error("a disabled owner still counts")
	}
}

// linkMailbox creates a password mailbox linked by userID in workspaceID,
// which gives them every flag on it.
func linkMailbox(t *testing.T, db *store.Store, id, workspaceID, userID, email string) {
	t.Helper()
	if _, err := account.NewRepository(db, nil).Create(t.Context(), account.Account{
		ID: id, WorkspaceID: workspaceID, OwnerUserID: userID, Email: email, Provider: provider.KindIMAP,
		AuthKind: "password", IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example",
		SMTPPort: 465, SMTPTLS: "implicit", LoginUser: email,
	}); err != nil {
		t.Fatalf("link %s: %v", email, err)
	}
}

func TestAHeldPrincipalNeverReachesAMailboxItsKeyLostWhenItsPersonLostRead(t *testing.T) {
	// What a caller holds for long — a stdio MCP session, a subscription,
	// an event stream — was authenticated once. Bea's key was made for a
	// team mailbox and her own; losing read on the team's takes it out of
	// the key, and the principal held since must stop at its next re-check,
	// not keep the mailbox, and not reach it again when read comes back.
	keys, db, _ := newKeys(t)
	ana := authtest.NewUser(t, db, "ana@example.org", auth.RoleMember)
	bea := authtest.NewUser(t, db, "bea@example.org", auth.RoleMember)
	team := teamOf(t, db, ana.ID)
	ws := workspace.NewRepository(db, nil)
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		return ws.AddMemberTx(t.Context(), tx, team.ID, bea.ID, workspace.RoleMember, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	const shared, own = "acc_00000000000000aa", "acc_00000000000000bb"
	linkMailbox(t, db, shared, team.ID, ana.ID, "support@mail.example")
	linkMailbox(t, db, own, authtest.Personal(t, db, bea.ID), bea.ID, "bea@mail.example")
	if _, err := ws.SetGrant(t.Context(), shared, bea.ID, workspace.Flags{Read: true}, ana.ID, nil); err != nil {
		t.Fatal(err)
	}
	secret, _ := issue(t, keys, auth.NewKeyRequest{Scope: auth.ScopeRead, UserID: bea.ID,
		AccountIDs: []string{shared, own}, TermsVersion: "terms"})
	held, err := keys.Authenticate(t.Context(), secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.Recheck(t.Context(), held); err != nil {
		t.Fatalf("Recheck while nothing changed: %v", err)
	}

	if _, err := ws.Revoke(t.Context(), shared, bea.ID, workspace.Flags{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := keys.Recheck(t.Context(), held); !errors.Is(err, auth.ErrKeyNarrowed) {
		t.Errorf("a principal still naming the mailbox its key lost passes the re-check: %v", err)
	}
	if _, err := ws.SetGrant(t.Context(), shared, bea.ID, workspace.Flags{Read: true}, ana.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := keys.Recheck(t.Context(), held); !errors.Is(err, auth.ErrKeyNarrowed) {
		t.Errorf("read granted back revived the held principal: %v", err)
	}

	// The key itself works on, for what it still names: authenticating
	// again gives a principal without the lost mailbox.
	fresh, err := keys.Authenticate(t.Context(), secret, nil)
	if err != nil {
		t.Fatalf("the key stopped working for its other mailbox: %v", err)
	}
	if len(fresh.AccountIDs) != 1 || fresh.AccountIDs[0] != own || fresh.MayAccess(shared) {
		t.Errorf("the key names %v after losing the team's mailbox", fresh.AccountIDs)
	}
	if err := keys.Recheck(t.Context(), fresh); err != nil {
		t.Errorf("a fresh principal fails the re-check: %v", err)
	}
}

func TestAHeldPrincipalOutlivesTheRemovalOfOneOfItsKeysMailboxes(t *testing.T) {
	// A removed mailbox leaves its key's restriction too, but its id is
	// never reused: a principal still naming it reaches nothing more, and a
	// stdio session must not end over a mailbox somebody removed.
	keys, db, _ := newKeys(t)
	ana := authtest.NewUser(t, db, "ana@example.org", auth.RoleMember)
	personal := authtest.Personal(t, db, ana.ID)
	const first, second = "acc_00000000000000a1", "acc_00000000000000a2"
	linkMailbox(t, db, first, personal, ana.ID, "ana@mail.example")
	linkMailbox(t, db, second, personal, ana.ID, "ana@work.example")
	secret, _ := issue(t, keys, auth.NewKeyRequest{Scope: auth.ScopeRead, UserID: ana.ID,
		AccountIDs: []string{first, second}, TermsVersion: "terms"})
	held, err := keys.Authenticate(t.Context(), secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	repo := account.NewRepository(db, nil)
	if err := repo.Delete(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if err := keys.Recheck(t.Context(), held); err != nil {
		t.Errorf("removing one of the key's mailboxes ended a principal held for the other: %v", err)
	}
	// The last one gone revokes the key, as it always did.
	if err := repo.Delete(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if err := keys.Recheck(t.Context(), held); !errors.Is(err, auth.ErrInvalidKey) {
		t.Errorf("a key with none of its mailboxes left passes the re-check: %v", err)
	}
}

func TestARemovedMemberCannotRejoinWithALeftoverInvite(t *testing.T) {
	// Two invites for Bea, as admin, made one after the other: accepting
	// one spends the other, so once she is removed nothing left over lets
	// her back in. A new invite is a new decision, and works.
	users, db, _ := newUsers(t)
	owner := authtest.NewUser(t, db, "owner@example.org", auth.RoleOwner)
	bea := authtest.NewUser(t, db, "bea@example.org", auth.RoleMember)
	team := teamOf(t, db, owner.ID)
	ws := workspace.NewRepository(db, nil)
	first, _ := teamInvite(t, users, "bea@example.org", team.ID, workspace.RoleAdmin)
	second, _ := teamInvite(t, users, "bea@example.org", team.ID, workspace.RoleAdmin)

	if _, err := users.AcceptInvite(t.Context(), bea.ID, first); err != nil {
		t.Fatal(err)
	}
	if pending, err := users.PendingInvites(t.Context(), team.ID); err != nil || len(pending) != 0 {
		t.Fatalf("after accepting one invite the team still has %+v pending (%v)", pending, err)
	}
	if err := ws.RemoveMember(t.Context(), team.ID, bea.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := users.AcceptInvite(t.Context(), bea.ID, second); !errors.Is(err, auth.ErrInviteInvalid) {
		t.Fatalf("a removed member rejoined with the leftover invite: %v", err)
	}
	if _, err := ws.Member(t.Context(), team.ID, bea.ID); !errors.Is(err, workspace.ErrNotMember) {
		t.Errorf("Bea's membership after the refusal: %v", err)
	}

	third, _ := teamInvite(t, users, "bea@example.org", team.ID, workspace.RoleMember)
	if joined, err := users.AcceptInvite(t.Context(), bea.ID, third); err != nil || joined.Role != workspace.RoleMember {
		t.Fatalf("a new invite after the removal: %+v, %v", joined, err)
	}
}

func TestLeavingATeamDeletesItsInvitesStillWaitingForThePerson(t *testing.T) {
	// Dan was invited, then added to the team some other way, so his
	// invite never ran: removing him, or disabling his membership, takes it
	// with his place.
	users, db, _ := newUsers(t)
	owner := authtest.NewUser(t, db, "owner@example.org", auth.RoleOwner)
	team := teamOf(t, db, owner.ID)
	ws := workspace.NewRepository(db, nil)
	for _, leave := range []struct {
		name string
		do   func(userID string) error
	}{
		{"removing", func(userID string) error { return ws.RemoveMember(t.Context(), team.ID, userID, nil) }},
		{"disabling", func(userID string) error {
			disabled := workspace.StatusDisabled
			_, err := ws.SetMember(t.Context(), team.ID, userID, workspace.MemberChange{Status: &disabled}, nil)
			return err
		}},
	} {
		email := leave.name + "@example.org"
		dan := authtest.NewUser(t, db, email, auth.RoleMember)
		code, _ := teamInvite(t, users, email, team.ID, workspace.RoleAdmin)
		if err := db.Write(t.Context(), func(tx *sql.Tx) error {
			return ws.AddMemberTx(t.Context(), tx, team.ID, dan.ID, workspace.RoleMember, time.Now())
		}); err != nil {
			t.Fatal(err)
		}
		if err := leave.do(dan.ID); err != nil {
			t.Fatalf("%s: %v", leave.name, err)
		}
		if pending, err := users.PendingInvites(t.Context(), team.ID); err != nil || len(pending) != 0 {
			t.Errorf("%s left %+v pending (%v)", leave.name, pending, err)
		}
		if leave.name == "removing" {
			if _, err := users.AcceptInvite(t.Context(), dan.ID, code); !errors.Is(err, auth.ErrInviteInvalid) {
				t.Errorf("after %s, the leftover invite brought him back: %v", leave.name, err)
			}
		}
	}
}

func TestSigningUpSpendsTheOtherInvitesWaitingForTheAddress(t *testing.T) {
	cheapKDF(t)
	users, db, _ := newUsers(t)
	owner := authtest.NewUser(t, db, "owner@example.org", auth.RoleMember)
	support := teamOf(t, db, owner.ID)
	sales, err := workspace.NewRepository(db, nil).CreateTeam(t.Context(), "Sales", owner.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := teamInvite(t, users, "cid@example.org", support.ID, workspace.RoleMember)
	second, _ := teamInvite(t, users, "cid@example.org", support.ID, workspace.RoleAdmin)
	elsewhere, _ := teamInvite(t, users, "cid@example.org", sales.ID, workspace.RoleMember)
	invite(t, users, "cid@example.org", auth.RoleOwner)

	cid := signUp(t, users, first, "cid@example.org")
	if pending, _ := users.PendingInvites(t.Context(), support.ID); len(pending) != 0 {
		t.Errorf("the team's other invite for the address is still pending: %+v", pending)
	}
	if pending, _ := users.PendingInvites(t.Context(), ""); len(pending) != 0 {
		t.Errorf("an instance invite for an address that has an account is still pending: %+v", pending)
	}
	if needs, err := users.NeedsFirstOwner(t.Context()); err != nil || !needs {
		t.Errorf("an owner invite nobody can redeem still counts as an owner on the way (%v)", err)
	}
	if _, err := users.AcceptInvite(t.Context(), cid.ID, second); !errors.Is(err, auth.ErrInviteInvalid) {
		t.Errorf("the team's second invite still works: %v", err)
	}
	// Another team's invite is that team's decision, and still stands.
	if joined, err := users.AcceptInvite(t.Context(), cid.ID, elsewhere); err != nil || joined.ID != sales.ID {
		t.Errorf("accepting the other team's invite: %+v, %v", joined, err)
	}
}
