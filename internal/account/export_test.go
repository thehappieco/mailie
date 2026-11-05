package account

import (
	"context"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/thehappieco/mailie/internal/provider"
)

// Test seams for the unexported machinery.
//
// In the package's own test file rather than exported for real: a token source
// built outside the registry would not be kept alive for the account's
// lifetime, which is the one thing about it that matters.

// NewTokenSourceForTest builds a token source directly.
func NewTokenSourceForTest(ctx context.Context, accountID string, config *oauth2.Config, token *oauth2.Token, store TokenStore) provider.TokenSource {
	return newTokenSource(ctx, accountID, config, token, store)
}

// ConfigForTest builds a provider's OAuth configuration for its installed
// client.
func ConfigForTest(kind provider.Kind, client OAuthClient, redirect string) (*oauth2.Config, error) {
	return configFor(kind, client, ClientInstalled, redirect, false)
}

// AuthURLForTest renders the URL a person would be sent to.
func AuthURLForTest(t *testing.T, kind provider.Kind, client OAuthClient, redirect, state string) string {
	t.Helper()
	return authURL(t, kind, client, ClientInstalled, redirect, state, "", false)
}

// ReauthURLForTest renders the URL a person authorizing loginHint again, or
// linking it by its address, is sent to with the web client.
func ReauthURLForTest(t *testing.T, kind provider.Kind, client OAuthClient, redirect, state, loginHint string) string {
	t.Helper()
	return authURL(t, kind, client, ClientWeb, redirect, state, loginHint, false)
}

// LinkAuthURLForTest renders the URL a link at the provider sends a person
// to, with the web client.
func LinkAuthURLForTest(t *testing.T, kind provider.Kind, client OAuthClient, redirect, state string) string {
	t.Helper()
	return authURL(t, kind, client, ClientWeb, redirect, state, "", true)
}

func authURL(t *testing.T, kind provider.Kind, client OAuthClient, which, redirect, state, loginHint string, identify bool) string {
	t.Helper()
	config, err := configFor(kind, client, which, redirect, identify)
	if err != nil {
		t.Fatalf("configFor: %v", err)
	}
	flow, err := startAuthCode(kind, config, state, loginHint, identify)
	if err != nil {
		t.Fatalf("startAuthCode: %v", err)
	}
	return flow.AuthURL
}

// IdentityForTest is the address an ID token names for a link at the
// provider of kind under client, at now.
func IdentityForTest(kind provider.Kind, client OAuthClient, idToken any, now time.Time) (string, error) {
	token := (&oauth2.Token{AccessToken: "access", RefreshToken: "refresh"}).WithExtra(map[string]any{"id_token": idToken})
	return identityOf(kind, client, token, now)
}

// RefusalForTest is the error a redirect carrying error=code stands for, and
// the state_reason a flow ended by it leaves on the account.
func RefusalForTest(code, description string) (reason string, err error) {
	err = Redirect{Error: code, ErrorDescription: description}.refusal()
	return failureReason(err), err
}

// RedirectForTest makes every IMAP connection to from dial to instead, so an
// in-process server can stand in for a provider whose servers cannot be
// overridden. Call it before the registry is used.
func (r *Registry) RedirectForTest(from, to string) {
	if r.redirect == nil {
		r.redirect = map[string]string{}
	}
	r.redirect[from] = to
}

// ScopeCheckForTest runs the check every code exchange makes on the scope a
// token response reports, for a provider's installed client, and returns the
// state_reason a consent it refuses leaves behind.
func ScopeCheckForTest(t *testing.T, kind provider.Kind, extra map[string]any) (reason string, err error) {
	t.Helper()
	return scopeCheck(t, kind, extra, false)
}

// LinkScopeCheckForTest is ScopeCheckForTest for a link at the provider,
// which asks for the account's address too.
func LinkScopeCheckForTest(t *testing.T, kind provider.Kind, extra map[string]any) (reason string, err error) {
	t.Helper()
	return scopeCheck(t, kind, extra, true)
}

func scopeCheck(t *testing.T, kind provider.Kind, extra map[string]any, identify bool) (reason string, err error) {
	t.Helper()
	config, cerr := configFor(kind, OAuthClient{ClientID: "client"}, ClientInstalled, "", identify)
	if cerr != nil {
		t.Fatalf("configFor: %v", cerr)
	}
	token := (&oauth2.Token{AccessToken: "access", RefreshToken: "refresh"}).WithExtra(extra)
	err = checkGrantedScopes(token, config.Scopes)
	if err == nil {
		return "", nil
	}
	return failureReason(err), err
}
