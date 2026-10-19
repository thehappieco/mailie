package service_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
)

// The name of a person who signs in through an extension, on a daemon whose
// people's names follow their identity (Deps.NameFollowsIdentity), and on one
// that keeps them as `serve` does.

// named is vouched under another name the provider gives the person now.
func named(subject, email, name string) service.ExternalSignIn {
	in := vouched(subject, email)
	in.Name = name
	return in
}

// nameOf reads the person's name as the server keeps it.
func (f *fixture) nameOf(t *testing.T, userID string) string {
	t.Helper()
	user, err := f.users.Get(t.Context(), userID)
	if err != nil {
		t.Fatal(err)
	}
	return user.Name
}

func TestWhereNamesFollowTheIdentityEverySignInNamesThePersonAsTheProviderDoesThen(t *testing.T) {
	f := newFixtureWith(t, fixtureOptions{externalSignInOnly: true, nameFollowsIdentity: true})
	ctx := t.Context()
	first := f.signedIn(t, named("subject-of-cy", "cy@example.com", "Cy Lima"))
	if first.User.Name != "Cy Lima" {
		t.Fatalf("the first sign-in named her %q", first.User.Name)
	}
	cy := first.User.ID

	for _, c := range []struct{ provider, want string }{
		// Renamed at the provider: the next sign-in carries it, and its
		// session says so.
		{"Cy Souza", "Cy Souza"},
		// Made into a name the server takes, as a new person's is.
		{"  Cy\tSouza\x00Lima ", "Cy Souza Lima"},
		// None at the provider now: none here either, not the last one.
		{"", ""},
		{" \n ", ""},
		// And a name again.
		{"Cy", "Cy"},
	} {
		signed, err := f.svc.SignInExternal(ctx, named("subject-of-cy", "cy@example.com", c.provider))
		if err != nil || signed.Session == nil {
			t.Fatalf("signing in as %q: %+v, %v", c.provider, signed, err)
		}
		if signed.Session.User.Name != c.want || f.nameOf(t, cy) != c.want {
			t.Errorf("the provider's %q: the session says %q and the server keeps %q, want %q",
				c.provider, signed.Session.User.Name, f.nameOf(t, cy), c.want)
		}
		p, err := f.svc.Authenticate(ctx, signed.Session.Token, nil)
		if err != nil {
			t.Fatal(err)
		}
		if me, err := f.svc.Me(ctx, p); err != nil || me.User.Name != c.want {
			t.Errorf("Me after the provider's %q: %q (%v), want %q", c.provider, me.User.Name, err, c.want)
		}
	}

	// A sign-in asking for the product key names her the same way.
	signed, err := f.svc.SignInExternal(ctx, f.pinned(t, named("subject-of-cy", "cy@example.com", "Cy Lima")))
	if err != nil || signed.Session == nil || signed.Session.User.Name != "Cy Lima" {
		t.Errorf("a sign-in asking for the key: %+v, %v, want her named Cy Lima", signed, err)
	}
}

func TestWhereNamesFollowTheIdentityASignInThatIsRefusedRenamesNobody(t *testing.T) {
	f := newFixtureWith(t, fixtureOptions{externalSignInOnly: true, nameFollowsIdentity: true})
	ctx := t.Context()
	cy := f.signedIn(t, named("subject-of-cy", "cy@example.com", "Cy Lima")).User.ID

	// A product key id nothing is pinned under: refused, and the
	// transaction that would have renamed her rolled back.
	in := f.pinned(t, named("subject-of-cy", "cy@example.com", "Cy Souza"))
	in.ProductKeyID = "mailie:2"
	if _, err := f.svc.SignInExternal(ctx, in); service.CodeOf(err) != service.CodeNotAuthorized {
		t.Fatalf("an unpinned product key id: %v, want not_authorized", err)
	}
	if got := f.nameOf(t, cy); got != "Cy Lima" {
		t.Errorf("a refused sign-in renamed her %q", got)
	}

	// Disabled: refused as any disabled person is, still named as she was.
	if _, err := f.svc.DisableUser(ctx, admin(), service.CloseUserRequest{Email: "cy@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SignInExternal(ctx, named("subject-of-cy", "cy@example.com", "")); service.CodeOf(err) != service.CodeUnauthorized {
		t.Fatalf("a disabled person's sign-in: %v, want unauthorized", err)
	}
	if got := f.nameOf(t, cy); got != "Cy Lima" {
		t.Errorf("a disabled person's refused sign-in renamed her %q", got)
	}
}

func TestWhereNamesFollowTheIdentityAPersonLinkedToOneNeverRenamesThemselves(t *testing.T) {
	f := newFixtureWith(t, fixtureOptions{nameFollowsIdentity: true})
	ctx := t.Context()
	session := f.signedIn(t, named("subject-of-cy", "cy@example.com", "Cy Lima"))
	cy, err := f.svc.Authenticate(ctx, session.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Whatever she asks for, a name, none, or one the server would refuse
	// anyway: one answer, which says where the name is changed, and her name
	// stays the provider's.
	for _, name := range []string{"Cy Souza", "", strings.Repeat("y", auth.MaxNameLength+1)} {
		_, err := f.svc.UpdateProfile(ctx, cy, service.ProfileRequest{Name: name})
		if service.CodeOf(err) != service.CodeConflict || !errors.Is(err, auth.ErrNameFromIdentity) ||
			!strings.Contains(service.MessageOf(err), "where you sign in") {
			t.Errorf("renaming herself %q: %v, want a conflict saying where the name is changed", name, err)
		}
	}
	if got := f.nameOf(t, cy.UserID); got != "Cy Lima" {
		t.Errorf("she is named %q, want the provider's Cy Lima", got)
	}

	// Somebody who signs in with a password on the same daemon names
	// themselves as ever: there is no identity their name could follow.
	owner := f.person(t, "owner@example.com", auth.RoleOwner)
	user, err := f.svc.UpdateProfile(ctx, owner, service.ProfileRequest{Name: "  Ana Lima "})
	if err != nil || user.Name != "Ana Lima" {
		t.Errorf("a person with a password renaming herself: %+v, %v", user, err)
	}
	if _, err := f.svc.UpdateProfile(ctx, owner, service.ProfileRequest{Name: "a\x00b"}); service.CodeOf(err) != service.CodeBadRequest {
		t.Errorf("a name with a control character: %v, want bad_request as ever", err)
	}
}

func TestAStepUpNeverRenamesThePerson(t *testing.T) {
	// Only a sign-in names a person after their identity: what a step-up
	// presents carries no name at all, and the one it finishes leaves hers
	// as her last sign-in left it, wherever names follow the identity.
	for _, field := range reflect.VisibleFields(reflect.TypeFor[service.ExternalStepUpProof]()) {
		if strings.Contains(strings.ToLower(field.Name), "name") {
			t.Errorf("ExternalStepUpProof.%s: a step-up carries no name", field.Name)
		}
	}
	f := newFixtureWith(t, fixtureOptions{externalSignInOnly: true, nameFollowsIdentity: true})
	ctx := t.Context()
	session := f.signedIn(t, named("subject-of-cy", "cy@example.com", "Cy Lima"))
	p, err := f.svc.Authenticate(ctx, session.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	mark, err := f.svc.MarkExternalStepUp(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	// The provider authenticated her a second after the mark, which the
	// server's clock must have reached.
	time.Sleep(time.Until(mark.Add(1100 * time.Millisecond)))
	if _, err := f.svc.ExternalStepUp(ctx, p, service.ExternalStepUpProof{
		Issuer: identityProvider, Subject: "subject-of-cy", AuthTime: mark.Add(time.Second),
		ProductKeyID: authtest.ProductKeyID, ProductKey: authtest.ProductKey("subject-of-cy"),
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.nameOf(t, p.UserID); got != "Cy Lima" {
		t.Errorf("after a step-up she is named %q, want Cy Lima", got)
	}
}

func TestByDefaultAPersonKeepsTheNameTheirFirstSignInGaveThemAndChangesIt(t *testing.T) {
	// As `serve` runs it, and an embedding binary that leaves the option
	// off: the provider's later names do not reach the person, who renames
	// themselves here.
	f := newFixtureWith(t, fixtureOptions{externalSignInOnly: true})
	ctx := t.Context()
	cy := f.signedIn(t, named("subject-of-cy", "cy@example.com", "Cy Lima")).User.ID
	for _, provider := range []string{"Cy Souza", ""} {
		signed, err := f.svc.SignInExternal(ctx, named("subject-of-cy", "cy@example.com", provider))
		if err != nil || signed.Session == nil || signed.Session.User.Name != "Cy Lima" {
			t.Errorf("signing in under the provider's %q: %+v, %v, want her still Cy Lima", provider, signed, err)
		}
	}
	session := f.signedIn(t, named("subject-of-cy", "cy@example.com", "Cy Souza"))
	p, err := f.svc.Authenticate(ctx, session.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	if user, err := f.svc.UpdateProfile(ctx, p, service.ProfileRequest{Name: "Cy Q"}); err != nil || user.Name != "Cy Q" {
		t.Errorf("renaming herself: %+v, %v", user, err)
	}
	if got := f.nameOf(t, cy); got != "Cy Q" {
		t.Errorf("she is named %q, want the Cy Q she chose", got)
	}
}
