package service_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
)

func personalKey(name, scope string, accounts ...string) service.PersonalKeyRequest {
	return service.PersonalKeyRequest{Name: name, Scope: scope, AccountIDs: accounts, TermsVersion: service.DefaultKeyTermsVersion}
}

func TestAPersonMakesKeysOnlyForMailboxesTheyOwn(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	bob := f.person(t, "bob@example.com", auth.RoleMember)
	anas := f.mailbox(t, ana, "ana@mail.example")
	bobs := f.mailbox(t, bob, "bob@mail.example")

	_, err := f.svc.CreateMyAPIKey(t.Context(), ana, personalKey("assistant", "read", anas, bobs))
	if service.CodeOf(err) != service.CodeNotFound {
		t.Fatalf("a key for somebody else's mailbox: %v, want not_found", err)
	}
	if keys, _ := f.svc.ListMyAPIKeys(t.Context(), ana); len(keys) != 0 {
		t.Fatalf("the refused key was stored: %+v", keys)
	}
	created, err := f.svc.CreateMyAPIKey(t.Context(), ana, personalKey("assistant", "read", anas))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Key, created.Prefix+".") || created.TermsVersion != service.DefaultKeyTermsVersion ||
		len(created.AccountIDs) != 1 || created.AccountIDs[0] != anas {
		t.Fatalf("created %+v", created.PersonalKey)
	}
	if days := time.Unix(created.ExpiresAt, 0).Sub(time.Unix(created.CreatedAt, 0)).Hours() / 24; days != 90 {
		t.Errorf("a key without a lifetime lives %v days, want 90", days)
	}
	p, err := f.svc.Authenticate(t.Context(), created.Key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.UserID != ana.UserID || p.Scope != auth.ScopeRead {
		t.Errorf("the key acts as %+v", p)
	}
	if got := ids(t, f, p); len(got) != 1 || got[0] != anas {
		t.Errorf("the key sees %v, want only %s", got, anas)
	}
}

func TestAKeyNamesTheKeyTermsThePersonAgreedTo(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	for _, version := range []string{"", "2026-01-api-keys"} {
		req := personalKey("assistant", "read")
		req.TermsVersion = version
		if _, err := f.svc.CreateMyAPIKey(t.Context(), ana, req); service.CodeOf(err) != service.CodeConflict {
			t.Errorf("terms %q: %v, want conflict", version, err)
		}
	}
	keys, err := f.keys.ListFor(t.Context(), ana.UserID)
	if err != nil || len(keys) != 0 {
		t.Fatalf("keys stored without the current terms: %+v (%v)", keys, err)
	}
	if _, err := f.svc.CreateMyAPIKey(t.Context(), ana, personalKey("assistant", "write")); err != nil {
		t.Fatal(err)
	}
	keys, _ = f.keys.ListFor(t.Context(), ana.UserID)
	if len(keys) != 1 || keys[0].TermsVersion != service.DefaultKeyTermsVersion || keys[0].CreatedBy != ana.UserID {
		t.Errorf("stored %+v; the key records the text agreed to and who agreed", keys)
	}
}

func TestPersonalKeysAreReadOrWriteAndLiveThirtyNinetyOrThreeHundredSixtyFiveDays(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleOwner)
	for _, scope := range []string{"send", "admin", ""} {
		if _, err := f.svc.CreateMyAPIKey(t.Context(), ana, personalKey("x", scope)); service.CodeOf(err) != service.CodeBadRequest {
			t.Errorf("scope %q: %v, want bad_request", scope, err)
		}
	}
	for days, ok := range map[int]bool{30: true, 90: true, 365: true, 7: false, 366: false, -1: false} {
		req := personalKey("x", "read")
		req.TTLDays = days
		_, err := f.svc.CreateMyAPIKey(t.Context(), ana, req)
		if (err == nil) != ok {
			t.Errorf("ttl_days %d: %v", days, err)
		}
	}
}

func TestAPersonHoldsAtMostTwentyLiveKeys(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	// Nineteen through the store, cheaply, and one through the service:
	// the limit counts every live key acting as the person.
	for range service.MaxPersonalKeys - 1 {
		authtest.NewKey(t, f.db, auth.ScopeRead, ana.UserID)
	}
	last, err := f.svc.CreateMyAPIKey(t.Context(), ana, personalKey("twentieth", "read"))
	if err != nil {
		t.Fatalf("the twentieth key: %v", err)
	}
	if _, err := f.svc.CreateMyAPIKey(t.Context(), ana, personalKey("one more", "read")); service.CodeOf(err) != service.CodeConflict {
		t.Fatalf("a twenty-first live key: %v, want conflict", err)
	}
	if err := f.svc.RevokeMyAPIKey(t.Context(), ana, last.Prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateMyAPIKey(t.Context(), ana, personalKey("replacement", "read")); err != nil {
		t.Fatalf("after revoking one: %v", err)
	}
	// An expired key is not live either.
	f.exec(t, `UPDATE api_keys SET expires_at = 1 WHERE user_id = ? AND name = 'authtest' AND rowid = (
		SELECT min(rowid) FROM api_keys WHERE user_id = ? AND name = 'authtest')`, ana.UserID, ana.UserID)
	if _, err := f.svc.CreateMyAPIKey(t.Context(), ana, personalKey("after expiry", "read")); err != nil {
		t.Fatalf("after one expired: %v", err)
	}
	// Somebody else's keys are theirs to count.
	bob := f.person(t, "bob@example.com", auth.RoleMember)
	if _, err := f.svc.CreateMyAPIKey(t.Context(), bob, personalKey("bob's", "read")); err != nil {
		t.Fatalf("another person's first key: %v", err)
	}
}

func TestAKeyCannotMintKeys(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleOwner)
	created, err := f.svc.CreateMyAPIKey(t.Context(), ana, personalKey("assistant", "write"))
	if err != nil {
		t.Fatal(err)
	}
	asKey, err := f.svc.Authenticate(t.Context(), created.Key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateMyAPIKey(t.Context(), asKey, personalKey("minted", "read")); service.CodeOf(err) != service.CodeNotAuthorized {
		t.Errorf("a key created a key: %v", err)
	}
	if _, err := f.svc.ListMyAPIKeys(t.Context(), asKey); service.CodeOf(err) != service.CodeNotAuthorized {
		t.Errorf("a key listed keys: %v", err)
	}
	if err := f.svc.RevokeMyAPIKey(t.Context(), asKey, created.Prefix); service.CodeOf(err) != service.CodeNotAuthorized {
		t.Errorf("a key revoked a key: %v", err)
	}
	// Nor through the operator's route: that takes an instance admin key.
	if _, err := f.svc.CreateAPIKey(t.Context(), asKey, service.CreateAPIKeyRequest{Name: "x", Scope: "read"}); service.CodeOf(err) != service.CodeNotAuthorized {
		t.Errorf("a person's key used the instance key route: %v", err)
	}
}

func TestAPersonSeesAndRevokesOnlyTheirOwnKeys(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleOwner)
	bob := f.person(t, "bob@example.com", auth.RoleMember)
	anas, err := f.svc.CreateMyAPIKey(t.Context(), ana, personalKey("ana's", "read"))
	if err != nil {
		t.Fatal(err)
	}
	bobs, err := f.svc.CreateMyAPIKey(t.Context(), bob, personalKey("bob's", "read"))
	if err != nil {
		t.Fatal(err)
	}
	instance := authtest.NewKey(t, f.db, auth.ScopeAdmin, "")
	instancePrefix, _, _ := strings.Cut(instance, ".")

	listed, err := f.svc.ListMyAPIKeys(t.Context(), ana)
	if err != nil || len(listed) != 1 || listed[0].Prefix != anas.Prefix {
		t.Fatalf("ana lists %+v (%v), want only her key", listed, err)
	}
	for _, other := range []string{bobs.Prefix, instancePrefix, "ffffffff"} {
		if err := f.svc.RevokeMyAPIKey(t.Context(), ana, other); service.CodeOf(err) != service.CodeNotFound {
			t.Errorf("revoking %s: %v, want not_found", other, err)
		}
	}
	if _, err := f.svc.Authenticate(t.Context(), bobs.Key, nil); err != nil {
		t.Errorf("bob's key stopped working after ana tried to revoke it: %v", err)
	}
	if err := f.svc.RevokeMyAPIKey(t.Context(), ana, anas.Prefix); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RevokeMyAPIKey(t.Context(), ana, anas.Prefix); err != nil {
		t.Errorf("revoking twice: %v", err)
	}
	if _, err := f.svc.Authenticate(t.Context(), anas.Key, nil); service.CodeOf(err) != service.CodeUnauthorized {
		t.Errorf("a revoked key still authenticates: %v", err)
	}
	listed, _ = f.svc.ListMyAPIKeys(t.Context(), ana)
	if len(listed) != 1 || listed[0].RevokedAt == 0 {
		t.Errorf("a revoked key stays listed as revoked: %+v", listed)
	}
}

func TestAToolReachesAPersonsMailboxOnlyThroughAKeyThatPersonCreated(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleOwner)
	anas := f.mailbox(t, ana, "ana@mail.example")
	operator := authtest.NewKey(t, f.db, auth.ScopeAdmin, "")
	opP, err := f.svc.Authenticate(t.Context(), operator, nil)
	if err != nil {
		t.Fatal(err)
	}
	unowned := f.mailbox(t, opP, "ops@mail.example")

	// The operator's key reaches the operator workspace's mailboxes, over
	// REST as when a tool presents it, and never a person's.
	if got := ids(t, f, opP); len(got) != 1 || got[0] != unowned {
		t.Fatalf("over REST the instance key sees %v, want only %s", got, unowned)
	}
	tool, err := f.svc.AuthenticateTool(t.Context(), operator, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(t, f, tool); len(got) != 1 || got[0] != unowned {
		t.Errorf("a tool with the instance key sees %v, want only %s", got, unowned)
	}
	if _, err := f.svc.GetAccount(t.Context(), tool, anas); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("a tool with the instance key opened ana's mailbox: %v", err)
	}

	// A key made for ana by somebody else is not hers to hand a tool.
	minted := authtest.NewKey(t, f.db, auth.ScopeRead, ana.UserID)
	if _, err := f.svc.AuthenticateTool(t.Context(), minted, nil); service.CodeOf(err) != service.CodeNotAuthorized {
		t.Errorf("a key ana did not create authenticated a tool: %v", err)
	}
	// Her session is not a tool's credential either.
	session := authtest.SignIn(t, f.users, "ana@example.com")
	if _, err := f.svc.AuthenticateTool(t.Context(), session, nil); service.CodeOf(err) != service.CodeUnauthorized {
		t.Errorf("a console session authenticated a tool: %v", err)
	}
	created, err := f.svc.CreateMyAPIKey(t.Context(), ana, personalKey("assistant", "read"))
	if err != nil {
		t.Fatal(err)
	}
	hers, err := f.svc.AuthenticateTool(t.Context(), created.Key, nil)
	if err != nil {
		t.Fatalf("her own key: %v", err)
	}
	// Her key sees what she does, which is her own mailbox: being an owner
	// of the instance reaches no other.
	if got := ids(t, f, hers); len(got) != 1 || got[0] != anas {
		t.Errorf("her key's tool sees %v, want only %s", got, anas)
	}
}

func TestARevokedKeyWhoseMailboxWasRemovedIsNotListedAsReachingEveryMailbox(t *testing.T) {
	// A mailbox removed takes the key's restriction to it along, and no
	// restriction reads as every mailbox. The list a person reads is the
	// record of what each key could reach: a key made for chosen mailboxes
	// says so after they are gone, without keeping their ids.
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	work := f.mailbox(t, ana, "ana@work.example")
	home := f.mailbox(t, ana, "ana@home.example")
	only, err := f.svc.CreateMyAPIKey(t.Context(), ana, personalKey("work only", "read", work))
	if err != nil {
		t.Fatal(err)
	}
	both, err := f.svc.CreateMyAPIKey(t.Context(), ana, personalKey("both", "read", work, home))
	if err != nil {
		t.Fatal(err)
	}
	every, err := f.svc.CreateMyAPIKey(t.Context(), ana, personalKey("every", "read"))
	if err != nil {
		t.Fatal(err)
	}
	if !only.Restricted || !both.Restricted || every.Restricted {
		t.Errorf("created keys say restricted %t, %t, %t; want true, true, false",
			only.Restricted, both.Restricted, every.Restricted)
	}
	if err := f.svc.RemoveAccount(t.Context(), ana, work); err != nil {
		t.Fatal(err)
	}

	listed, err := f.svc.ListMyAPIKeys(t.Context(), ana)
	if err != nil {
		t.Fatal(err)
	}
	byPrefix := map[string]service.PersonalKey{}
	for _, k := range listed {
		byPrefix[k.Prefix] = k
	}
	if k := byPrefix[only.Prefix]; k.RevokedAt == 0 || !k.Restricted || len(k.AccountIDs) != 0 {
		t.Errorf("the key for the removed mailbox is listed as %+v; want revoked and restricted, naming nothing", k)
	}
	if k := byPrefix[both.Prefix]; k.RevokedAt != 0 || !k.Restricted || !slices.Equal(k.AccountIDs, []string{home}) {
		t.Errorf("the key for both mailboxes is listed as %+v; want live and restricted to %s", k, home)
	}
	if k := byPrefix[every.Prefix]; k.RevokedAt != 0 || k.Restricted || len(k.AccountIDs) != 0 {
		t.Errorf("the key for every mailbox is listed as %+v", k)
	}
}
