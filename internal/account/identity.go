package account

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/thehappieco/mailie/internal/provider"
)

// Learning the address of a link at the provider (links.go): the account the
// person chose on the provider's page, as the provider's ID token names it.

// ErrNoAddress is a link at the provider whose answer named no address to
// connect: no ID token, one this server cannot take as its own, or one
// whose address is not one. Each error says only which check failed
// (noAddress), never a value the token carried.
var ErrNoAddress = errors.New("account: the provider named no usable address for the account")

// noAddress is ErrNoAddress for one check of the ID token, named by check
// alone: whatever the token said is the provider's and the person's, and an
// error may be logged.
func noAddress(check string) error {
	return fmt.Errorf("%w: the ID token fails its %s check", ErrNoAddress, check)
}

const (
	// maxIDToken bounds the ID token read: Google's and Microsoft's are a
	// few kilobytes.
	maxIDToken = 32 << 10
	// maxAddress is the longest address taken, in bytes.
	maxAddress = 320
	// idTokenSkew is how far the token's times may be off this server's
	// clock.
	idTokenSkew = 5 * time.Minute
	// consumersTenant is the tenant every personal Microsoft account signs
	// in under.
	consumersTenant = "9188040d-6c67-4c5b-b112-36a304b66dad"
	// microsoftIssuer is the issuer of Microsoft's v2.0 ID tokens, the
	// tenant's id in place of {tenantid}, as its discovery document writes
	// it for the tenants common, organizations and consumers.
	microsoftIssuer = "https://login.microsoftonline.com/{tenantid}/v2.0"
)

// googleIssuers are the two spellings Google documents for its ID tokens'
// issuer.
var googleIssuers = []string{"https://accounts.google.com", "accounts.google.com"}

// tenantID is a tenant's id: a GUID.
var tenantID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// idClaims are the claims of an ID token this server reads: tid only for the
// checks, never kept. sub, hd and the rest are never read: the address is all
// a link keeps, and the account's identifier is matched to nothing here.
type idClaims struct {
	Issuer        string   `json:"iss"`
	Audience      audience `json:"aud"`
	AuthorizedFor string   `json:"azp"`
	Expires       *float64 `json:"exp"`
	IssuedAt      *float64 `json:"iat"`
	Tenant        string   `json:"tid"`
	Email         string   `json:"email"`
	EmailVerified any      `json:"email_verified"`
}

// audience is the aud claim: one string, or a list of them.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("account: aud is neither a string nor a list of them")
	}
	*a = many
	return nil
}

// identityOf is the address an ID token in token names, for a link at the
// provider of kind, under client, the registration that asked for it (its
// ClientID, its Tenant for Microsoft, and the Issuer a test sets). now is the
// wall clock, which the token's times are on.
//
// The signature is not checked. OpenID Connect allows that for an ID token
// received directly from the token endpoint over TLS, in the answer to this
// client's own code and PKCE verifier (Core 1.0, section 3.1.3.7, item 6):
// nobody else could have put it there, so no nonce is needed either. The
// claims are checked as that section says: the issuer, this client as its
// audience (and its authorized party when there are several), and its
// times; for Google, that the address is verified; for Microsoft, that the
// tenant is one the registration's tenant admits.
//
// Microsoft's email claim is not verified by Microsoft, and need not be: the
// login that proves a new grant (checkGrant) logs in as this address, which
// Exchange opens only for a token whose holder may open it. The address and
// token stored are exactly as trustworthy as a typed address is.
//
// Every refusal is ErrNoAddress, naming only the check that failed. Nothing
// of the token is kept: not the token, which an encoded grant leaves out,
// nor the account's identifier, its tenant or its domain.
func identityOf(kind provider.Kind, client OAuthClient, token *oauth2.Token, now time.Time) (string, error) {
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return "", noAddress("absent")
	}
	if len(raw) > maxIDToken {
		return "", noAddress("too long")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", noAddress("format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", noAddress("format")
	}
	var claims idClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", noAddress("format")
	}

	if client.ClientID == "" || !slices.Contains(claims.Audience, client.ClientID) {
		return "", noAddress("audience")
	}
	if (len(claims.Audience) > 1 && claims.AuthorizedFor == "") ||
		(claims.AuthorizedFor != "" && claims.AuthorizedFor != client.ClientID) {
		return "", noAddress("audience")
	}
	if claims.Expires == nil || claims.IssuedAt == nil ||
		!unixFloat(*claims.Expires).After(now.Add(-idTokenSkew)) ||
		!unixFloat(*claims.IssuedAt).Before(now.Add(idTokenSkew)) {
		return "", noAddress("expiry")
	}

	var email string
	switch kind {
	case provider.KindGmail:
		issuers := googleIssuers
		if client.Issuer != "" {
			issuers = []string{client.Issuer}
		}
		if !slices.Contains(issuers, claims.Issuer) {
			return "", noAddress("issuer")
		}
		if claims.Email == "" {
			return "", noAddress("absent")
		}
		if !verified(claims.EmailVerified) {
			return "", noAddress("verified")
		}
		email = claims.Email
	case provider.KindMicrosoft:
		if !tenantID.MatchString(claims.Tenant) {
			return "", noAddress("tenant")
		}
		issuer := orDefault(client.Issuer, microsoftIssuer)
		if claims.Issuer != strings.ReplaceAll(issuer, "{tenantid}", claims.Tenant) {
			return "", noAddress("issuer")
		}
		if !tenantAdmits(client.Tenant, claims.Tenant) {
			return "", noAddress("tenant")
		}
		email = claims.Email
	default:
		return "", fmt.Errorf("%w: %s accounts are not linked at the provider", ErrNotOAuth, kind)
	}
	return linkAddress(email)
}

// tenantAdmits reports whether a Microsoft registration configured for
// tenant, the one the authorization URL named, admits an account of the
// tenant whose id is tid: its own GUID only that tenant, consumers only
// personal accounts, organizations every tenant but theirs, and common or a
// domain name any.
func tenantAdmits(tenant, tid string) bool {
	switch t := strings.ToLower(strings.TrimSpace(tenant)); {
	case tenantID.MatchString(t):
		return strings.EqualFold(t, tid)
	case t == "consumers":
		return strings.EqualFold(tid, consumersTenant)
	case t == "organizations":
		return !strings.EqualFold(tid, consumersTenant)
	default:
		return true
	}
}

// linkAddress is an ID token's address as a link stores it: trimmed, as
// reported, and refused unless it is a bare address of ASCII alone, of at
// most maxAddress bytes.
func linkAddress(email string) (string, error) {
	email = strings.TrimSpace(email)
	switch {
	case email == "":
		return "", noAddress("absent")
	case len(email) > maxAddress:
		return "", noAddress("too long")
	}
	for i := 0; i < len(email); i++ {
		if email[i] >= 0x80 {
			return "", noAddress("not ASCII")
		}
	}
	if parsed, err := mail.ParseAddress(email); err != nil || parsed.Name != "" || parsed.Address != email {
		return "", noAddress("address")
	}
	return email, nil
}

// verified reports whether an email_verified claim says yes: true, as
// Google writes it, or "true", as it once did.
func verified(claim any) bool {
	switch v := claim.(type) {
	case bool:
		return v
	case string:
		return v == "true"
	default:
		return false
	}
}

// unixFloat is a NumericDate claim, seconds since the epoch, as a time, to
// the second; one further than some thirty million years either way is held
// there, which is as far as anything here needs.
func unixFloat(seconds float64) time.Time {
	const far = 1e15
	switch {
	case seconds > far:
		seconds = far
	case seconds < -far:
		seconds = -far
	}
	return time.Unix(int64(seconds), 0)
}
