package account

import (
	"context"
	"testing"

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
	return configFor(kind, client, ClientInstalled, redirect)
}

// AuthURLForTest renders the URL a person would be sent to.
func AuthURLForTest(t *testing.T, kind provider.Kind, client OAuthClient, redirect, state string) string {
	t.Helper()
	config, err := configFor(kind, client, ClientInstalled, redirect)
	if err != nil {
		t.Fatalf("configFor: %v", err)
	}
	flow, err := startAuthCode(kind, config, state, "")
	if err != nil {
		t.Fatalf("startAuthCode: %v", err)
	}
	return flow.AuthURL
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
	config, cerr := configFor(kind, OAuthClient{ClientID: "client"}, ClientInstalled, "")
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
