package service_test

import (
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/service"
)

func TestAnAPIKeyCannotUseTheSessionUseCases(t *testing.T) {
	f := newFixture(t)
	key := admin()
	checks := map[string]error{}
	_, checks["Me"] = f.svc.Me(t.Context(), key)
	checks["SignOut"] = f.svc.SignOut(t.Context(), key, service.SignOutRequest{})
	_, checks["ChangePassword"] = f.svc.ChangePassword(t.Context(), key, service.PasswordRequest{Current: "a", Next: "b"}, "")
	_, checks["UpdateProfile"] = f.svc.UpdateProfile(t.Context(), key, service.ProfileRequest{Name: "x"})
	for name, err := range checks {
		if service.CodeOf(err) != service.CodeNotAuthorized {
			t.Errorf("%s with an API key: %v, want not_authorized", name, err)
		}
	}
}

func TestOnlyAnOwnerOrAnInstanceAdminKeyCanInvite(t *testing.T) {
	f := newFixtureWith(t, fixtureOptions{publicURL: "https://console.mailie.example"})
	owner := f.person(t, "owner@example.com", auth.RoleOwner)
	member := f.person(t, "member@example.com", auth.RoleMember)

	for name, p := range map[string]service.Principal{
		"member":             member,
		"read key":           reader(),
		"restricted admin":   {KeyPrefix: "cccccccc", Scope: auth.ScopeAdmin, AccountIDs: []string{"acc_x"}},
		"key acting as user": {KeyPrefix: "dddddddd", Scope: auth.ScopeAdmin, UserID: owner.UserID, UserRole: auth.RoleOwner},
	} {
		if _, err := f.svc.CreateInvite(t.Context(), p, service.InviteRequest{Email: "x@example.com"}); service.CodeOf(err) != service.CodeNotAuthorized {
			t.Errorf("%s could invite: %v", name, err)
		}
	}

	for name, p := range map[string]service.Principal{"owner": owner, "instance key": admin()} {
		invitee := strings.ReplaceAll(name, " ", "-") + "-invitee@example.com"
		invite, err := f.svc.CreateInvite(t.Context(), p, service.InviteRequest{Email: invitee, Role: "owner"})
		if err != nil {
			t.Fatalf("%s could not invite: %v", name, err)
		}
		if invite.Role != "owner" || !strings.HasPrefix(invite.URL, "https://console.mailie.example/#invite=") {
			t.Errorf("%s: invite = %+v", name, invite)
		}
	}
	invite, err := f.svc.CreateInvite(t.Context(), owner, service.InviteRequest{Email: "plain@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if invite.Role != "member" {
		t.Errorf("an invite with no role gave %q, want member", invite.Role)
	}
	if _, err := f.svc.CreateInvite(t.Context(), owner, service.InviteRequest{Email: "member@example.com"}); service.CodeOf(err) != service.CodeConflict {
		t.Errorf("inviting an address that already has an account: %v", err)
	}
}

func TestAnInviteNeedsThePublicURL(t *testing.T) {
	// A link built from whichever Host header the request came in with
	// would let anybody who can reach the daemon mint links to their own
	// site.
	f := newFixture(t)
	_, err := f.svc.CreateInvite(t.Context(), admin(), service.InviteRequest{Email: "a@example.com"})
	if service.CodeOf(err) != service.CodeConflict || !strings.Contains(service.MessageOf(err), "MAIL_PUBLIC_URL") {
		t.Fatalf("err = %v", err)
	}
}

func TestARestrictedAdminKeyCannotMintAWiderKey(t *testing.T) {
	f := newFixture(t)
	a := f.mailbox(t, admin(), "a@mail.example")
	b := f.mailbox(t, admin(), "b@mail.example")
	restricted := service.Principal{KeyPrefix: "cccccccc", Scope: auth.ScopeAdmin, AccountIDs: []string{a}}

	for name, ids := range map[string][]string{"every account": nil, "another account": {b}, "one more": {a, b}} {
		_, err := f.svc.CreateAPIKey(t.Context(), restricted, service.CreateAPIKeyRequest{
			Name: "wider", Scope: "read", AccountIDs: ids,
		})
		if service.CodeOf(err) != service.CodeNotAuthorized {
			t.Errorf("%s: a key restricted to %s minted %v: %v", name, a, ids, err)
		}
	}
	created, err := f.svc.CreateAPIKey(t.Context(), restricted, service.CreateAPIKeyRequest{
		Name: "narrower", Scope: "read", AccountIDs: []string{a},
	})
	if err != nil {
		t.Fatalf("a key could not mint one within its own reach: %v", err)
	}
	minted, err := f.svc.Authenticate(t.Context(), created.Key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(t, f, minted); len(got) != 1 || got[0] != a {
		t.Errorf("the minted key sees %v", got)
	}

	// Nor higher, and never from a person.
	send := service.Principal{KeyPrefix: "eeeeeeee", Scope: auth.ScopeSend}
	if _, err := f.svc.CreateAPIKey(t.Context(), send, service.CreateAPIKeyRequest{Name: "x", Scope: "admin"}); service.CodeOf(err) != service.CodeNotAuthorized {
		t.Errorf("a send key minted an admin key: %v", err)
	}
	owner := f.person(t, "owner@example.com", auth.RoleOwner)
	if _, err := f.svc.CreateAPIKey(t.Context(), owner, service.CreateAPIKeyRequest{Name: "x", Scope: "read"}); service.CodeOf(err) != service.CodeNotAuthorized {
		t.Errorf("a console session minted a key: %v", err)
	}
	if _, err := f.svc.CreateAPIKey(t.Context(), admin(), service.CreateAPIKeyRequest{
		Name: "x", Scope: "read", AccountIDs: []string{"acc_nope"},
	}); service.CodeOf(err) != service.CodeBadRequest {
		t.Errorf("a key restricted to a missing account: %v", err)
	}
}
