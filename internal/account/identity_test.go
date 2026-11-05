package account_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/provider"
)

// What a link at the provider learns of the account chosen there: the
// address its ID token names (identity.go).

// idToken encodes claims as an ID token the way a provider writes one; its
// signature is never read.
func idToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "eyJhbGciOiJSUzI1NiIsImtpZCI6InRlc3QifQ." + base64.RawURLEncoding.EncodeToString(payload) + ".c2lnbmF0dXJl"
}

const (
	googleClient = "google-web.apps.example.test"
	msClient     = "6731de76-14a6-49ae-97bc-6eba6914391e"
	workTenant   = "72f988bf-86f1-41af-91ab-2d7cd011db47"
	otherTenant  = "f8cdef31-a31e-4b4a-93e4-5f571e91255a"
	consumers    = "9188040d-6c67-4c5b-b112-36a304b66dad"
)

var (
	google    = account.OAuthClient{ClientID: googleClient}
	microsoft = account.OAuthClient{ClientID: msClient, Tenant: "common"}
)

// googleClaims are a Google ID token's, for email, valid now.
func googleClaims(email string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": "https://accounts.google.com", "aud": googleClient, "azp": googleClient,
		"sub": "110169484474386276334", "hd": "example.test", "email": email, "email_verified": true,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
}

// microsoftClaims are a Microsoft v2.0 ID token's, for email in the tenant
// tid, valid now.
func microsoftClaims(email, tid string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": "https://login.microsoftonline.com/" + tid + "/v2.0", "aud": msClient, "tid": tid,
		"sub": "AAAAAAAAAAAAAAAAAAAAAIkzqFVrSaSaFHy782bbtaQ", "oid": "00000000-0000-0000-66f3-3332eca7ea81",
		"email": email, "preferred_username": email, "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
}

// with is claims with changes: a nil value deletes the claim.
func with(claims map[string]any, changes map[string]any) map[string]any {
	out := maps.Clone(claims)
	for k, v := range changes {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}

// identity is the address a token with claims names, under client.
func identity(t *testing.T, kind provider.Kind, client account.OAuthClient, claims map[string]any) (string, error) {
	t.Helper()
	return account.IdentityForTest(kind, client, idToken(t, claims), time.Now())
}

// refusedFor checks a refusal is ErrNoAddress for the check named.
func refusedFor(t *testing.T, what, check string, got string, err error) {
	t.Helper()
	if !errors.Is(err, account.ErrNoAddress) || !strings.Contains(err.Error(), "its "+check+" check") {
		t.Errorf("%s: %q, %v, want ErrNoAddress for its %s check", what, got, err, check)
	}
}

func TestAnIDTokenNamesAnAddressOnlyFromItsIssuerForThisClient(t *testing.T) {
	for _, c := range []struct {
		what   string
		kind   provider.Kind
		client account.OAuthClient
		claims map[string]any
		want   string
	}{
		{"Google's", provider.KindGmail, google, googleClaims("ana@gmail.com"), "ana@gmail.com"},
		{"Google's, its issuer without the scheme", provider.KindGmail, google,
			with(googleClaims("ana@gmail.com"), map[string]any{"iss": "accounts.google.com"}), "ana@gmail.com"},
		{"Google's, as reported and trimmed", provider.KindGmail, google,
			googleClaims("  Ana.Doe@Example.test "), "Ana.Doe@Example.test"},
		{"Microsoft's", provider.KindMicrosoft, microsoft, microsoftClaims("ana@outlook.com", consumers), "ana@outlook.com"},
		{"a test's issuer", provider.KindMicrosoft, account.OAuthClient{ClientID: msClient, Issuer: "https://idp.example.test/{tenantid}/v2.0"},
			with(microsoftClaims("ana@example.test", workTenant), map[string]any{"iss": "https://idp.example.test/" + workTenant + "/v2.0"}),
			"ana@example.test"},
	} {
		if got, err := identity(t, c.kind, c.client, c.claims); err != nil || got != c.want {
			t.Errorf("%s: %q, %v, want %q", c.what, got, err, c.want)
		}
	}

	for _, c := range []struct {
		what, check string
		kind        provider.Kind
		claims      map[string]any
	}{
		{"another issuer", "issuer", provider.KindGmail,
			with(googleClaims("ana@gmail.com"), map[string]any{"iss": "https://accounts.google.com.example.test"})},
		{"Microsoft's issuer for Google", "issuer", provider.KindGmail,
			with(googleClaims("ana@gmail.com"), map[string]any{"iss": "https://login.microsoftonline.com/" + consumers + "/v2.0"})},
		{"no issuer", "issuer", provider.KindGmail, with(googleClaims("ana@gmail.com"), map[string]any{"iss": nil})},
		{"another client's", "audience", provider.KindGmail,
			with(googleClaims("ana@gmail.com"), map[string]any{"aud": "other.apps.example.test", "azp": nil})},
		{"no audience", "audience", provider.KindGmail, with(googleClaims("ana@gmail.com"), map[string]any{"aud": nil})},
		{"Microsoft's for another tenant than its issuer's", "issuer", provider.KindMicrosoft,
			with(microsoftClaims("ana@example.test", workTenant), map[string]any{"tid": otherTenant})},
		{"Microsoft's v1.0 issuer", "issuer", provider.KindMicrosoft,
			with(microsoftClaims("ana@example.test", workTenant), map[string]any{"iss": "https://sts.windows.net/" + workTenant + "/"})},
		{"Google's issuer for Microsoft", "issuer", provider.KindMicrosoft,
			with(microsoftClaims("ana@example.test", workTenant), map[string]any{"iss": "https://accounts.google.com"})},
		{"Microsoft's without a tenant", "tenant", provider.KindMicrosoft,
			with(microsoftClaims("ana@example.test", workTenant), map[string]any{"tid": nil})},
		{"Microsoft's with a tenant that is not one", "tenant", provider.KindMicrosoft,
			with(microsoftClaims("ana@example.test", "common"), map[string]any{})},
		{"Microsoft's for another client", "audience", provider.KindMicrosoft,
			with(microsoftClaims("ana@example.test", workTenant), map[string]any{"aud": otherTenant})},
	} {
		client := google
		if c.kind == provider.KindMicrosoft {
			client = microsoft
		}
		got, err := identity(t, c.kind, client, c.claims)
		refusedFor(t, c.what, c.check, got, err)
	}

	// A token that is not one names nothing either.
	for _, c := range []struct {
		what, check string
		token       any
	}{
		{"no ID token", "absent", nil},
		{"an empty one", "absent", ""},
		{"one that is not text", "absent", 42},
		{"two parts", "format", "eyJhbGciOiJub25lIn0.e30"},
		{"a payload that is not base64url", "format", "a.@@@.c"},
		{"a payload that is not JSON", "format", "a." + base64.RawURLEncoding.EncodeToString([]byte("not json")) + ".c"},
		{"claims of the wrong types", "format", "a." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":1,"aud":"x"}`)) + ".c"},
	} {
		got, err := account.IdentityForTest(provider.KindGmail, google, c.token, time.Now())
		refusedFor(t, c.what, c.check, got, err)
	}
	if _, err := account.IdentityForTest(provider.KindIMAP, google, idToken(t, googleClaims("ana@gmail.com")), time.Now()); errors.Is(err, account.ErrNoAddress) || err == nil {
		t.Errorf("a generic IMAP mailbox's: %v, want no link at all", err)
	}
}

func TestAnIDTokenWithSeveralAudiencesNeedsThisClientAsItsParty(t *testing.T) {
	base := googleClaims("ana@gmail.com")
	for _, c := range []struct {
		what   string
		claims map[string]any
		ok     bool
	}{
		{"one audience, no party", with(base, map[string]any{"azp": nil}), true},
		{"several, this client its party", with(base, map[string]any{"aud": []string{googleClient, "other"}}), true},
		{"several, no party", with(base, map[string]any{"aud": []string{googleClient, "other"}, "azp": nil}), false},
		{"several, another party", with(base, map[string]any{"aud": []string{googleClient, "other"}, "azp": "other"}), false},
		{"one, another party", with(base, map[string]any{"azp": "other"}), false},
		{"several, not this client", with(base, map[string]any{"aud": []string{"other", "more"}}), false},
		{"an audience of the wrong type", with(base, map[string]any{"aud": 7}), false},
	} {
		got, err := identity(t, provider.KindGmail, google, c.claims)
		if c.ok {
			if err != nil || got != "ana@gmail.com" {
				t.Errorf("%s: %q, %v", c.what, got, err)
			}
			continue
		}
		if !errors.Is(err, account.ErrNoAddress) {
			t.Errorf("%s: %q, %v, want ErrNoAddress", c.what, got, err)
		}
	}
}

func TestAGoogleAddressThatIsNotVerifiedNamesNothing(t *testing.T) {
	base := googleClaims("ana@example.test")
	// A nil value is the claim left out.
	for _, v := range []any{false, "false", "", 1, nil} {
		got, err := identity(t, provider.KindGmail, google, with(base, map[string]any{"email_verified": v}))
		refusedFor(t, fmt.Sprintf("email_verified %#v", v), "verified", got, err)
	}
	if got, err := identity(t, provider.KindGmail, google, with(base, map[string]any{"email_verified": "true"})); err != nil || got != "ana@example.test" {
		t.Errorf(`email_verified "true", as Google once wrote it: %q, %v`, got, err)
	}
	got, err := identity(t, provider.KindGmail, google, with(base, map[string]any{"email": nil}))
	refusedFor(t, "no email", "absent", got, err)
	// Microsoft says nothing of it: the login that proves the grant is what
	// tells the address and the token are one mailbox's.
	if got, err := identity(t, provider.KindMicrosoft, microsoft, microsoftClaims("ana@example.test", workTenant)); err != nil || got != "ana@example.test" {
		t.Errorf("Microsoft's, which never says: %q, %v", got, err)
	}
}

func TestAMicrosoftIDTokenFromAnotherTenantThanTheRegistrationsNamesNothing(t *testing.T) {
	for _, c := range []struct {
		tenant, tid string
		ok          bool
	}{
		{workTenant, workTenant, true},
		{strings.ToUpper(workTenant), workTenant, true},
		{workTenant, otherTenant, false},
		{workTenant, consumers, false},
		{"consumers", consumers, true},
		{"consumers", workTenant, false},
		{"common", workTenant, true},
		{"common", consumers, true},
		{"", otherTenant, true},
		{"contoso.example.test", otherTenant, true},
	} {
		client := account.OAuthClient{ClientID: msClient, Tenant: c.tenant}
		got, err := identity(t, provider.KindMicrosoft, client, microsoftClaims("ana@example.test", c.tid))
		if c.ok {
			if err != nil || got != "ana@example.test" {
				t.Errorf("tenant %q, tid %s: %q, %v", c.tenant, c.tid, got, err)
			}
			continue
		}
		refusedFor(t, "tenant "+c.tenant+", tid "+c.tid, "tenant", got, err)
	}
}

func TestAnOrganizationsRegistrationRefusesAPersonalMicrosoftAccount(t *testing.T) {
	client := account.OAuthClient{ClientID: msClient, Tenant: "organizations"}
	got, err := identity(t, provider.KindMicrosoft, client, microsoftClaims("ana@outlook.com", consumers))
	refusedFor(t, "a personal account", "tenant", got, err)
	if got, err := identity(t, provider.KindMicrosoft, client, microsoftClaims("ana@example.test", workTenant)); err != nil || got != "ana@example.test" {
		t.Errorf("a work account: %q, %v", got, err)
	}
}

func TestAnExpiredOversizedOrNonASCIIIDTokenNamesNothing(t *testing.T) {
	now := time.Now()
	base := googleClaims("ana@gmail.com")
	for _, c := range []struct {
		what, check string
		claims      map[string]any
	}{
		{"expired ten minutes ago", "expiry", with(base, map[string]any{"exp": now.Add(-10 * time.Minute).Unix()})},
		{"with no expiry", "expiry", with(base, map[string]any{"exp": nil})},
		{"issued ten minutes from now", "expiry", with(base, map[string]any{"iat": now.Add(10 * time.Minute).Unix()})},
		{"with no moment of issue", "expiry", with(base, map[string]any{"iat": nil})},
		{"an expiry past what a time holds", "expiry", with(base, map[string]any{"exp": -1e300})},
		{"an address with a letter outside ASCII", "not ASCII", googleClaims("anã@gmail.com")},
		{"an address longer than 320 bytes", "too long", googleClaims(strings.Repeat("a", 64) + "@" + strings.Repeat("b", 252) + ".test")},
		{"a name and an address", "address", googleClaims("Ana <ana@gmail.com>")},
		{"two addresses", "address", googleClaims("ana@gmail.com, bea@gmail.com")},
		{"no address at all", "address", googleClaims("ana")},
		{"spaces alone", "absent", googleClaims("   ")},
		{"a token past 32 KiB", "too long", with(base, map[string]any{"padding": strings.Repeat("x", 32<<10)})},
	} {
		got, err := identity(t, provider.KindGmail, google, c.claims)
		refusedFor(t, c.what, c.check, got, err)
	}
	// The clocks may be a few minutes apart.
	for _, c := range []struct {
		what   string
		claims map[string]any
	}{
		{"expired four minutes ago", with(base, map[string]any{"exp": now.Add(-4 * time.Minute).Unix()})},
		{"issued four minutes from now", with(base, map[string]any{"iat": now.Add(4 * time.Minute).Unix()})},
		{"its times fractional", with(base, map[string]any{"iat": float64(now.Unix()) + 0.5, "exp": float64(now.Add(time.Hour).Unix()) + 0.25})},
		{"an address of 320 bytes", googleClaims(strings.Repeat("a", 64) + "@" + strings.Repeat("b", 250) + ".test")},
	} {
		if _, err := identity(t, provider.KindGmail, google, c.claims); err != nil {
			t.Errorf("%s: %v", c.what, err)
		}
	}
}

func TestAFailedIDTokenCheckNamesNoClaimValue(t *testing.T) {
	// Every claim carries the needle: whatever check fails, its error,
	// which may be logged, says which check and nothing the token said.
	const needle = "zqxneedle"
	now := time.Now()
	g := map[string]any{
		"iss": "https://accounts.google.com", "aud": googleClient, "azp": googleClient,
		"sub": "sub-" + needle, "hd": needle + ".example.test", "email": "ana." + needle + "@gmail.com",
		"email_verified": true, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	ms := with(microsoftClaims("ana."+needle+"@example.test", workTenant), map[string]any{"sub": needle, "name": needle})
	cases := []struct {
		kind   provider.Kind
		client account.OAuthClient
		claims map[string]any
	}{
		{provider.KindGmail, google, with(g, map[string]any{"iss": "https://" + needle + ".example.test"})},
		{provider.KindGmail, google, with(g, map[string]any{"aud": needle, "azp": nil})},
		{provider.KindGmail, google, with(g, map[string]any{"aud": []string{googleClient, needle}, "azp": needle})},
		{provider.KindGmail, google, with(g, map[string]any{"email_verified": needle})},
		{provider.KindGmail, google, with(g, map[string]any{"exp": now.Add(-time.Hour).Unix()})},
		{provider.KindGmail, google, with(g, map[string]any{"email": needle + "ã@gmail.com"})},
		{provider.KindGmail, google, with(g, map[string]any{"email": needle + " <ana@gmail.com>"})},
		{provider.KindGmail, google, with(g, map[string]any{"email": strings.Repeat(needle, 40) + "@gmail.com"})},
		{provider.KindGmail, google, with(g, map[string]any{"iss": 7, "email": needle})},
		{provider.KindMicrosoft, microsoft, with(ms, map[string]any{"tid": needle})},
		{provider.KindMicrosoft, microsoft, with(ms, map[string]any{"iss": "https://login.microsoftonline.com/" + needle + "/v2.0"})},
		{provider.KindMicrosoft, account.OAuthClient{ClientID: msClient, Tenant: otherTenant}, ms},
	}
	for i, c := range cases {
		got, err := identity(t, c.kind, c.client, c.claims)
		if !errors.Is(err, account.ErrNoAddress) {
			t.Errorf("case %d: %q, %v, want a refusal", i, got, err)
			continue
		}
		message := err.Error()
		if strings.Contains(message, needle) {
			t.Errorf("case %d: the error %q names a claim's value", i, message)
		}
		for k, v := range c.claims {
			if s, ok := v.(string); ok && len(s) > 8 && strings.Contains(message, s) {
				t.Errorf("case %d: the error %q names the %s claim", i, message, k)
			}
		}
	}
}
