package auth_test

import (
	"errors"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/argon2"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/service/issuertest"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

// Keys an issuer makes (issued.go): checked by one indexed read and the
// issuer's own constant-time comparison, never by Argon2id.

func TestAnIssuedCredentialNeverReachesTheArgon2idPath(t *testing.T) {
	var derivations atomic.Int64
	auth.SetDeriveKeyForTest(t, func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
		derivations.Add(1)
		return argon2.IDKey(password, salt, time, memory, threads, keyLen)
	})
	db := storetest.New(t)
	users := auth.NewUsers(db)
	keys := auth.NewKeys(db)
	issuer := issuertest.New()
	svc := service.New(service.Deps{Keys: keys, Users: users, Store: db, KeyIssuer: issuer})
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	good, _ := issuertest.Store(t, db, issuertest.Key{WorkspaceID: authtest.Personal(t, db, ana.ID), CreatorID: ana.ID})
	unknown, selector, _ := issuertest.Credential(t)
	own := authtest.NewKey(t, db, auth.ScopeRead)
	derivations.Store(0)

	for what, raw := range map[string]string{
		"a good one":                 good,
		"an unknown one":             unknown,
		"a wrong secret":             good[:len(good)-43] + unknown[len(unknown)-43:],
		"a malformed one":            issuertest.Prefix + selector + ".x",
		"one with a space more":      " " + good,
		"one of a family's spelling": "TST_" + good[4:],
	} {
		_, _ = svc.Authenticate(t.Context(), raw, nil)
		_, _ = svc.AuthenticateTool(t.Context(), raw, nil)
		if n := derivations.Load(); n != 0 {
			t.Fatalf("%s cost %d Argon2id derivations", what, n)
		}
	}
	if issuer.Verifications() == 0 {
		t.Fatal("the issuer compared nothing")
	}
	// This server's own key still costs one: the seam is the issuer's
	// claim, not a shortcut for every key.
	if _, err := svc.Authenticate(t.Context(), own, nil); err != nil {
		t.Fatal(err)
	}
	if derivations.Load() == 0 {
		t.Fatal("an own key was checked without Argon2id")
	}
}

func TestEveryIssuedKeyRefusalIsTheOneInvalidKeyAndSaysWhichKeyOnlyOnceItsSecretMatched(t *testing.T) {
	db := storetest.New(t)
	keys := auth.NewKeys(db)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	ws := authtest.Personal(t, db, ana.ID)
	_, live := issuertest.Store(t, db, issuertest.Key{WorkspaceID: ws, CreatorID: ana.ID, ExternalID: "tok-live"})
	_, revoked := issuertest.Store(t, db, issuertest.Key{WorkspaceID: ws, CreatorID: ana.ID, ExternalID: "tok-revoked",
		Revoked: true})
	matches := func(string) bool { return true }
	wrong := func(string) bool { return false }

	p, check, err := keys.AuthenticateIssued(t.Context(), issuertest.Issuer, live, matches, nil, false)
	if err != nil || p.Issuer != issuertest.Issuer || p.KeyPrefix != live || check != (auth.IssuedCheck{ExternalID: "tok-live"}) {
		t.Fatalf("a live key = %+v, %+v, %v", p, check, err)
	}
	for what, c := range map[string]struct {
		issuer, selector string
		verify           func(string) bool
		refuseAll        bool
		want             auth.IssuedCheck
	}{
		"a wrong secret":         {issuertest.Issuer, live, wrong, false, auth.IssuedCheck{}},
		"another issuer's":       {"https://other.test", live, matches, false, auth.IssuedCheck{}},
		"no selector":            {issuertest.Issuer, "", matches, false, auth.IssuedCheck{}},
		"a revoked key":          {issuertest.Issuer, revoked, matches, false, auth.IssuedCheck{ExternalID: "tok-revoked", Outcome: auth.IssuedRevoked}},
		"every issued key held":  {issuertest.Issuer, live, matches, true, auth.IssuedCheck{ExternalID: "tok-live", Outcome: auth.IssuedHeld}},
		"a revoked one, refused": {issuertest.Issuer, revoked, matches, true, auth.IssuedCheck{ExternalID: "tok-revoked", Outcome: auth.IssuedRevoked}},
	} {
		_, check, err := keys.AuthenticateIssued(t.Context(), c.issuer, c.selector, c.verify, nil, c.refuseAll)
		if !errors.Is(err, auth.ErrInvalidKey) || check != c.want {
			t.Errorf("%s = %+v, %v; want %+v and the one invalid key", what, check, err, c.want)
		}
	}
	// An own key's lookup never finds an issued key, and the operator's
	// revocation does not reach one.
	if err := keys.Revoke(t.Context(), live); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("revoking an issued key as an own one = %v", err)
	}
}
