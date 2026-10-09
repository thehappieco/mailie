package service_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
)

// identityProvider is the issuer an extension signs people in through, in the
// tests below.
const identityProvider = "https://accounts.example.com"

// vouched is a person the provider vouches for, address verified, for a day.
func vouched(subject, email string) service.ExternalSignIn {
	return service.ExternalSignIn{
		Issuer: identityProvider, Subject: subject, Email: email, EmailVerified: true,
		Name: "Cy Lima", UserAgent: "extension", TTL: 24 * time.Hour,
	}
}

func TestExternalSignInOnlyRefusesEveryPasswordUseCase(t *testing.T) {
	// The same calls on two daemons: one where people sign in only through
	// an extension, which refuses every one of them with one answer, and one
	// as `serve` runs it, where none of them gets that answer.
	for _, only := range []bool{true, false} {
		f := newFixtureWith(t, fixtureOptions{publicURL: "http://localhost:5174", externalSignInOnly: only})
		ctx := t.Context()
		owner := f.person(t, "owner@example.com", auth.RoleOwner)
		member := f.person(t, "member@example.com", auth.RoleMember)
		team, err := f.svc.CreateWorkspace(ctx, owner, service.CreateWorkspaceRequest{Name: "Support"})
		if err != nil {
			t.Fatal(err)
		}
		// Invitations made before, from the command line: neither signs
		// anyone up nor joins anyone to a team while passwords are off.
		signUpCode, _, err := f.users.CreateInvite(ctx, auth.NewInvite{Email: "new@example.com", Role: auth.RoleMember, CreatedBy: "cli"})
		if err != nil {
			t.Fatal(err)
		}
		joinCode, _, err := f.users.CreateInvite(ctx, auth.NewInvite{
			Email: "member@example.com", WorkspaceID: team.ID, WorkspaceRole: "member", CreatedBy: "cli",
		})
		if err != nil {
			t.Fatal(err)
		}
		invites := f.count(t, `SELECT count(*) FROM invites`)

		calls := map[string]error{}
		_, calls["answer a challenge"] = f.svc.Challenge(ctx, service.ChallengeRequest{Email: "owner@example.com"})
		_, calls["sign in with a password"] = f.svc.Login(ctx, service.LoginRequest{Email: "owner@example.com", AuthKey: authtest.AuthKey}, "test")
		_, calls["sign up with an invitation"] = f.svc.SignUp(ctx, service.SignUpRequest{
			Invite: signUpCode, Email: "new@example.com", Name: "New", Enrolment: wireEnrolment(t)}, "test")
		_, calls["accept a team invitation"] = f.svc.AcceptInvite(ctx, member, service.AcceptInviteRequest{Invite: joinCode})
		_, calls["begin a password change"] = f.svc.BeginPasswordChange(ctx, owner,
			service.PasswordBeginRequest{CurrentAuthKey: authtest.AuthKey})
		_, _, calls["finish a password change"] = f.svc.FinishPasswordChange(ctx, owner, service.PasswordFinishRequest{}, "test")
		_, calls["open a recovery"] = f.svc.OpenRecovery(ctx, service.RecoverOpenRequest{
			Email: "owner@example.com", RecoveryProof: authtest.RecoveryProof})
		calls["finish a recovery"] = f.svc.FinishRecovery(ctx, service.RecoverFinishRequest{})
		calls["replace the recovery code"] = f.svc.ReplaceRecovery(ctx, owner, service.RecoveryRequest{})
		_, calls["step up with the password"] = f.svc.StepUp(ctx, owner, service.StepUpRequest{AuthKey: authtest.AuthKey})
		_, calls["the upgrade's sign-in"] = f.svc.UpgradeLogin(ctx, service.UpgradeLoginRequest{
			Email: "owner@example.com", Password: authtest.Password})
		_, calls["the upgrade's enrolment"] = f.svc.UpgradeEnrol(ctx, service.UpgradeEnrolRequest{Enrolment: wireEnrolment(t)}, "test")
		_, calls["a reset invitation"] = f.svc.CompleteReset(ctx, service.ResetRequest{
			Email: "owner@example.com", Enrolment: wireEnrolment(t)}, "test")
		_, calls["invite to the instance, signed in"] = f.svc.CreateInvite(ctx, owner, service.InviteRequest{Email: "x@example.com"})
		_, calls["invite to the instance, with a key"] = f.svc.CreateInvite(ctx, admin(), service.InviteRequest{Email: "y@example.com"})
		_, calls["invite into a team"] = f.svc.CreateTeamInvite(ctx, owner, team.ID, service.TeamInviteRequest{Email: "z@example.com"})
		_, calls["invite into a team, with a key"] = f.svc.CreateTeamInvite(ctx, admin(), team.ID, service.TeamInviteRequest{Email: "w@example.com"})

		for name, err := range calls {
			refused := service.CodeOf(err) == service.CodeNotAuthorized && strings.Contains(service.MessageOf(err), "another way")
			if only && !refused {
				t.Errorf("passwords off: %s: %v, want not_authorized", name, err)
			}
			if !only && refused {
				t.Errorf("passwords on: %s was refused as if they were off: %v", name, err)
			}
		}
		if !only {
			continue
		}
		// Refused before anything changed.
		if n := f.count(t, `SELECT count(*) FROM invites`); n != invites {
			t.Errorf("%d invitations, want the %d there were", n, invites)
		}
		if n := f.count(t, `SELECT count(*) FROM invites WHERE used_at <> 0`); n != 0 {
			t.Errorf("%d invitations were spent", n)
		}
		if n := f.count(t, `SELECT count(*) FROM users WHERE email = 'new@example.com'`); n != 0 {
			t.Error("an invitation signed somebody up")
		}
		if _, err := f.users.Login(ctx, "owner@example.com", authtest.AuthKey, "test"); err != nil {
			t.Errorf("the owner's password changed: %v", err)
		}

		// What is not about passwords or invitations works as it does
		// anywhere: the person's own account, and signing in through the
		// extension.
		if _, err := f.svc.Me(ctx, owner); err != nil {
			t.Errorf("Me: %v", err)
		}
		if _, err := f.svc.UpdateProfile(ctx, owner, service.ProfileRequest{Name: "Owner"}); err != nil {
			t.Errorf("UpdateProfile: %v", err)
		}
		if _, err := f.svc.SignInExternal(ctx, vouched("subject-of-cy", "cy@example.com")); err != nil {
			t.Errorf("SignInExternal: %v", err)
		}
		if _, _, err := f.svc.PinIdentityKey(ctx, identityProvider, "subject-of-cy", "k1", []byte("key")); err != nil {
			t.Errorf("PinIdentityKey: %v", err)
		}
		if err := f.svc.SignOut(ctx, owner, service.SignOutRequest{}); err != nil {
			t.Errorf("SignOut: %v", err)
		}
	}
}

func TestAnExternalSessionIsAnsweredAsAPasswordSignInIs(t *testing.T) {
	f := newFixtureWith(t, fixtureOptions{externalSignInOnly: true})
	ctx := t.Context()
	in := vouched("subject-of-cy", "Cy@Example.com")
	in.TTL = 8 * time.Hour
	session, err := f.svc.SignInExternal(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if session.User.Email != "cy@example.com" || session.User.Role != "member" || session.User.Name != "Cy Lima" ||
		session.User.HasPassword || len(session.Token) != 43 || strings.Contains(session.Token, ".") {
		t.Fatalf("session = %+v", session)
	}

	// The token is a console session like any other.
	p, err := f.svc.Authenticate(ctx, session.Token, nil)
	if err != nil || !p.IsSession() || p.UserID != session.User.ID {
		t.Fatalf("the token authenticated %+v (%v)", p, err)
	}
	me, err := f.svc.Me(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if me.User != session.User || me.User.HasPassword || me.Session.ExpiresAt != session.ExpiresAt ||
		me.Session.ExpiresAt-me.Session.CreatedAt != int64((8*time.Hour).Seconds()) {
		t.Errorf("me = %+v, want the person and an eight-hour session", me)
	}
	// Signing in again is the same person, with a session of its own.
	again, err := f.svc.SignInExternal(ctx, vouched("subject-of-cy", "cy@example.com"))
	if err != nil || again.User.ID != session.User.ID || again.Token == session.Token {
		t.Errorf("signing in again: %+v, %v", again, err)
	}
}

func TestExternalSignInRefusalsUseTheFixedCodes(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	// Cy and Dee came through the provider, and Dee was disabled since; the
	// owner and Gone signed up with a password, and Gone was disabled.
	for _, name := range []string{"cy", "dee"} {
		if _, err := f.svc.SignInExternal(ctx, vouched("subject-of-"+name, name+"@example.com")); err != nil {
			t.Fatal(err)
		}
	}
	f.person(t, "gone@example.com", auth.RoleMember)
	f.person(t, "owner@example.com", auth.RoleOwner)
	for _, email := range []string{"dee@example.com", "gone@example.com"} {
		if _, err := f.svc.DisableUser(ctx, admin(), service.CloseUserRequest{Email: email}); err != nil {
			t.Fatal(err)
		}
	}

	edit := func(change func(*service.ExternalSignIn)) service.ExternalSignIn {
		in := vouched("subject-of-someone", "someone@example.com")
		change(&in)
		return in
	}
	for name, c := range map[string]struct {
		in   service.ExternalSignIn
		want service.Code
	}{
		"an issuer with a path":         {edit(func(in *service.ExternalSignIn) { in.Issuer = identityProvider + "/realm" }), service.CodeBadRequest},
		"an issuer over http":           {edit(func(in *service.ExternalSignIn) { in.Issuer = "http://accounts.example.com" }), service.CodeBadRequest},
		"no subject":                    {edit(func(in *service.ExternalSignIn) { in.Subject = "" }), service.CodeBadRequest},
		"a malformed address":           {edit(func(in *service.ExternalSignIn) { in.Email = "someone" }), service.CodeBadRequest},
		"an address lower case changes": {edit(func(in *service.ExternalSignIn) { in.Email = "\u212Aaren@example.com" }), service.CodeBadRequest},
		"a session of no time":          {edit(func(in *service.ExternalSignIn) { in.TTL = 0 }), service.CodeBadRequest},
		"a session past 14 days":        {edit(func(in *service.ExternalSignIn) { in.TTL = auth.SessionTTL + time.Hour }), service.CodeBadRequest},
		"an unverified address":         {edit(func(in *service.ExternalSignIn) { in.EmailVerified = false }), service.CodeNotAuthorized},
		"an unverified address somebody has": {edit(func(in *service.ExternalSignIn) {
			in.Email, in.EmailVerified = "owner@example.com", false
		}), service.CodeNotAuthorized},
		"another subject for a provider's person's address": {vouched("subject-of-another-cy", "cy@example.com"), service.CodeConflict},
		"the address of a person with a password":           {vouched("subject-of-owner", "Owner@example.com"), service.CodeConflict},
		"a disabled person's address":                       {vouched("subject-of-gone", "gone@example.com"), service.CodeConflict},
		"a disabled person's own identity":                  {vouched("subject-of-dee", "dee@example.com"), service.CodeUnauthorized},
	} {
		_, err := f.svc.SignInExternal(ctx, c.in)
		if got := service.CodeOf(err); got != c.want {
			t.Errorf("%s: %s (%v), want %s", name, got, err, c.want)
		}
		// A conflict here is no invitation to sign in with a password.
		if c.want == service.CodeConflict && !strings.Contains(service.MessageOf(err), "never takes over") {
			t.Errorf("%s: %q", name, service.MessageOf(err))
		}
	}
	// No refusal linked anything: the identities are Cy's and Dee's, each
	// from the sign-in that created its person.
	if n := f.count(t, `SELECT count(*) FROM user_identities`); n != 2 {
		t.Errorf("%d identities, want Cy's and Dee's", n)
	}

	if _, _, err := f.svc.PinIdentityKey(ctx, identityProvider, "subject-of-cy", "", []byte("k")); service.CodeOf(err) != service.CodeBadRequest {
		t.Errorf("a pin without a key id: %v", err)
	}
	if _, _, err := f.svc.PinIdentityKey(ctx, "accounts.example.com", "subject-of-cy", "k1", []byte("k")); service.CodeOf(err) != service.CodeBadRequest {
		t.Errorf("a pin for an issuer that is no origin: %v", err)
	}
}

func TestAPinnedKeyComesBackAsTheCallersOwnCopy(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	offered := []byte("the key first seen")
	pinned, inserted, err := f.svc.PinIdentityKey(ctx, identityProvider, "subject-of-cy", "k1", offered)
	if err != nil || !inserted || !bytes.Equal(pinned, offered) {
		t.Fatalf("first pin: %q, %v, %v", pinned, inserted, err)
	}
	offered[0], pinned[0] = 'X', 'Y'
	again, inserted, err := f.svc.PinIdentityKey(ctx, identityProvider, "subject-of-cy", "k1", []byte("a different key"))
	if err != nil || inserted || string(again) != "the key first seen" {
		t.Errorf("another key under the same id: %q, inserted %v, %v", again, inserted, err)
	}
}
