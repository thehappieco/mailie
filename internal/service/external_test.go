package service_test

import (
	"bytes"
	"errors"
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

// pinned is in asking for the product key the provider delivers, which the
// extension pinned for the identity first.
func (f *fixture) pinned(t *testing.T, in service.ExternalSignIn) service.ExternalSignIn {
	t.Helper()
	if _, _, err := f.svc.PinIdentityKey(t.Context(), in.Issuer, in.Subject, authtest.ProductKeyID,
		authtest.ProductKey(in.Subject)); err != nil {
		t.Fatal(err)
	}
	in.WantsKey, in.ProductKeyID = true, authtest.ProductKeyID
	return in
}

// wireExternalEnrolment is what a page sends with ticket: a fresh public key
// and a platform wrap's shape, under the provider's product key id.
func wireExternalEnrolment(t *testing.T, ticket string) service.ExternalEnrolment {
	t.Helper()
	return service.ExternalEnrolment{
		Ticket: ticket, PublicKey: b64(authtest.PublicKey(t)), PlatformWrap: b64(authtest.PlatformWrap(t)),
		ProductKeyID: authtest.ProductKeyID, UserAgent: "extension",
	}
}

// signedIn signs a person in through the provider as an extension and its
// page do: for the identity alone, and for a person without an account key,
// again asking for the product key, then enrolling with the ticket. It
// returns the session.
func (f *fixture) signedIn(t *testing.T, in service.ExternalSignIn) service.Session {
	t.Helper()
	ctx := t.Context()
	signed, err := f.svc.SignInExternal(ctx, in)
	if errors.Is(err, auth.ErrAccountKeyNeeded) {
		signed, err = f.svc.SignInExternal(ctx, f.pinned(t, in))
	}
	switch {
	case err != nil:
		t.Fatalf("signing in as %s: %v", in.Subject, err)
	case signed.Session != nil:
		return *signed.Session
	case signed.Enrolment == nil:
		t.Fatalf("signing in as %s answered %+v", in.Subject, signed)
	}
	session, err := f.svc.EnrolExternal(ctx, wireExternalEnrolment(t, signed.Enrolment.Ticket))
	if err != nil {
		t.Fatalf("enrolling %s: %v", in.Subject, err)
	}
	return session
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
		_, calls["open an invitation"] = f.svc.OpenSignUp(ctx, service.SignUpOpenRequest{Invite: signUpCode, Email: "new@example.com"})
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
		f.signedIn(t, vouched("subject-of-cy", "cy@example.com"))
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
	session := f.signedIn(t, in)
	if session.User.Email != "cy@example.com" || session.User.Role != "member" || session.User.Name != "Cy Lima" ||
		session.User.HasPassword || len(session.Token) != 43 || strings.Contains(session.Token, ".") ||
		session.User.PublicKey == "" || session.User.SealID == "" {
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
	if err != nil || again.Session == nil || again.Session.User.ID != session.User.ID || again.Session.Token == session.Token ||
		again.PlatformWrap != "" || again.Enrolment != nil {
		t.Errorf("signing in again: %+v, %v", again, err)
	}
}

func TestExternalSignInRefusalsUseTheFixedCodes(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	// Cy and Dee came through the provider, and Dee was disabled since; the
	// owner and Gone signed up with a password, and Gone was disabled.
	for _, name := range []string{"cy", "dee"} {
		f.signedIn(t, vouched("subject-of-"+name, name+"@example.com"))
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

func TestASignInThroughAProviderNeverGivesAPersonWithoutAnAccountKeyASession(t *testing.T) {
	f := newFixtureWith(t, fixtureOptions{externalSignInOnly: true})
	ctx := t.Context()

	// For the identity alone: a conflict the extension tells apart, and
	// nobody created.
	_, err := f.svc.SignInExternal(ctx, vouched("subject-of-cy", "cy@example.com"))
	if service.CodeOf(err) != service.CodeConflict || !errors.Is(err, auth.ErrAccountKeyNeeded) {
		t.Fatalf("a first sign-in for the identity alone: %v, want a conflict caused by ErrAccountKeyNeeded", err)
	}
	if n := f.count(t, `SELECT count(*) FROM users`) + f.count(t, `SELECT count(*) FROM sessions`); n != 0 {
		t.Fatalf("it wrote %d people and sessions", n)
	}

	// Asking for the key: a ticket and the seal id to bind the wrap to, and
	// still no session.
	signed, err := f.svc.SignInExternal(ctx, f.pinned(t, vouched("subject-of-cy", "cy@example.com")))
	if err != nil || signed.Session != nil || signed.PlatformWrap != "" || signed.Enrolment == nil ||
		signed.Enrolment.Ticket == "" || signed.Enrolment.SealID == "" || signed.Enrolment.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("a first sign-in asking for the key: %+v (%v)", signed, err)
	}
	if n := f.count(t, `SELECT count(*) FROM sessions`); n != 0 {
		t.Fatalf("%d sessions for a person without an account key", n)
	}

	// The ticket enrols her: the public key she made, written once, and
	// the session, answered as a password sign-in's is.
	req := wireExternalEnrolment(t, signed.Enrolment.Ticket)
	session, err := f.svc.EnrolExternal(ctx, req)
	if err != nil || session.User.PublicKey != req.PublicKey || session.User.SealID != signed.Enrolment.SealID ||
		session.User.HasPassword || len(session.Token) != 43 {
		t.Fatalf("EnrolExternal: %+v, %v", session, err)
	}
	if _, err := f.svc.EnrolExternal(ctx, wireExternalEnrolment(t, signed.Enrolment.Ticket)); service.CodeOf(err) != service.CodeNotAuthorized {
		t.Errorf("a ticket used twice: %v, want not_authorized", err)
	}

	// From now on a sign-in asking for the key answers her wrap with the
	// session, and one for the identity alone the session only.
	again, err := f.svc.SignInExternal(ctx, f.pinned(t, vouched("subject-of-cy", "cy@example.com")))
	if err != nil || again.Session == nil || again.PlatformWrap != req.PlatformWrap || again.Enrolment != nil ||
		again.Session.User.PublicKey != req.PublicKey {
		t.Errorf("a sign-in asking for the key: %+v (%v), want the session and her wrap", again, err)
	}
	alone, err := f.svc.SignInExternal(ctx, vouched("subject-of-cy", "cy@example.com"))
	if err != nil || alone.Session == nil || alone.PlatformWrap != "" {
		t.Errorf("a sign-in for the identity alone: %+v (%v)", alone, err)
	}
}

func TestAnEnrolmentRefusesWhatIsNotAPublicKeyAndAPlatformWrap(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	signed, err := f.svc.SignInExternal(ctx, f.pinned(t, vouched("subject-of-cy", "cy@example.com")))
	if err != nil || signed.Enrolment == nil {
		t.Fatalf("%+v, %v", signed, err)
	}
	ticket := signed.Enrolment.Ticket
	accountWrap := b64(authtest.Wrap(t))
	for name, c := range map[string]struct {
		edit func(*service.ExternalEnrolment)
		want service.Code
	}{
		"a public key of 31 bytes":      {func(e *service.ExternalEnrolment) { e.PublicKey = b64(make([]byte, 31)) }, service.CodeBadRequest},
		"a public key of low order":     {func(e *service.ExternalEnrolment) { e.PublicKey = b64(make([]byte, 32)) }, service.CodeBadRequest},
		"a public key padded":           {func(e *service.ExternalEnrolment) { e.PublicKey += "=" }, service.CodeBadRequest},
		"an account wrap":               {func(e *service.ExternalEnrolment) { e.PlatformWrap = accountWrap }, service.CodeBadRequest},
		"no wrap":                       {func(e *service.ExternalEnrolment) { e.PlatformWrap = "" }, service.CodeBadRequest},
		"a product key id not Mailie's": {func(e *service.ExternalEnrolment) { e.ProductKeyID = "wappie:1" }, service.CodeBadRequest},
		"another product key id":        {func(e *service.ExternalEnrolment) { e.ProductKeyID = "mailie:2" }, service.CodeNotAuthorized},
		"no ticket":                     {func(e *service.ExternalEnrolment) { e.Ticket = "" }, service.CodeNotAuthorized},
	} {
		req := wireExternalEnrolment(t, ticket)
		c.edit(&req)
		if _, err := f.svc.EnrolExternal(ctx, req); service.CodeOf(err) != c.want {
			t.Errorf("%s: %v, want %s", name, err, c.want)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM platform_wraps`) + f.count(t, `SELECT count(*) FROM sessions`); n != 0 {
		t.Fatalf("refusals wrote %d wraps and sessions", n)
	}
	if _, err := f.svc.EnrolExternal(ctx, wireExternalEnrolment(t, ticket)); err != nil {
		t.Fatalf("the ticket after the refusals: %v", err)
	}
}

func TestASignInAskingForAKeyNoWrapIsStoredUnderIsRefusedAsSuch(t *testing.T) {
	// A person whose account key a reset invitation made has no platform
	// wrap (docs/key-scheme.md section 17.2): a sign-in asking for the key
	// is a conflict the extension tells apart, and starts no session.
	f := newFixture(t)
	ctx := t.Context()
	f.person(t, "ana@example.com", auth.RoleMember)
	ana, err := f.users.GetByEmail(ctx, "ana@example.com")
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO user_identities(issuer, subject, user_id, created_at) VALUES (?, 'subject-of-ana', ?, 0)`,
		identityProvider, ana.ID)
	sessions := f.count(t, `SELECT count(*) FROM sessions`)
	_, err = f.svc.SignInExternal(ctx, f.pinned(t, vouched("subject-of-ana", "ana@example.com")))
	if service.CodeOf(err) != service.CodeConflict || !errors.Is(err, auth.ErrNoPlatformWrap) {
		t.Errorf("a sign-in asking for the key: %v, want a conflict caused by ErrNoPlatformWrap", err)
	}
	if n := f.count(t, `SELECT count(*) FROM sessions`); n != sessions {
		t.Errorf("the refusal started %d sessions", n-sessions)
	}
	// An id the extension did not pin, and one that is not an id.
	in := f.pinned(t, vouched("subject-of-ana", "ana@example.com"))
	in.ProductKeyID = "mailie:2"
	if _, err := f.svc.SignInExternal(ctx, in); service.CodeOf(err) != service.CodeNotAuthorized {
		t.Errorf("an unpinned id: %v, want not_authorized", err)
	}
	in.ProductKeyID = "mailie:01"
	if _, err := f.svc.SignInExternal(ctx, in); service.CodeOf(err) != service.CodeBadRequest {
		t.Errorf("an id in another spelling: %v, want bad_request", err)
	}
}

func TestAnExternalStepUpRefusesAProductKeyOtherThanThePinnedOneAndSaysSo(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	session := f.signedIn(t, vouched("subject-of-cy", "cy@example.com"))
	p, err := f.svc.Authenticate(ctx, session.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	mark, err := f.svc.MarkExternalStepUp(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	// The provider authenticated the person a second after the mark, which
	// the server's clock must have reached.
	time.Sleep(time.Until(mark.Add(1100 * time.Millisecond)))
	proof := service.ExternalStepUpProof{
		Issuer: identityProvider, Subject: "subject-of-cy", AuthTime: mark.Add(time.Second),
		ProductKeyID: authtest.ProductKeyID, ProductKey: authtest.ProductKey("somebody else"),
	}
	_, err = f.svc.ExternalStepUp(ctx, p, proof)
	if service.CodeOf(err) != service.CodeConflict || !errors.Is(err, auth.ErrProductKeyChanged) {
		t.Fatalf("another product key: %v, want a conflict caused by ErrProductKeyChanged", err)
	}
	if !strings.Contains(f.logs.String(), "product key differs from the one pinned") {
		t.Errorf("the refusal was not logged as an error:\n%s", f.logs.String())
	}
	// The mark is still good for the pinned key, which steps up at the
	// provider's time.
	proof.ProductKey = authtest.ProductKey("subject-of-cy")
	at, err := f.svc.ExternalStepUp(ctx, p, proof)
	if err != nil || !at.Equal(mark.Add(time.Second)) {
		t.Fatalf("the pinned key: %v, %v", at, err)
	}
	me, err := f.svc.Me(ctx, p)
	if err != nil || me.Session.AuthenticatedAt != at.Unix() {
		t.Errorf("the session's step-up time is %d (%v), want %d", me.Session.AuthenticatedAt, err, at.Unix())
	}
}
