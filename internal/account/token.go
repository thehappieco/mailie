package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"

	"github.com/thehappieco/mailie/internal/provider"
)

// refreshWindow is how early a token is replaced.
//
// Five minutes rather than the library's ten seconds, because Gmail ends an
// IMAP session at roughly the moment its token expires: authenticating with a
// token that has four minutes left buys a connection that dies in four
// minutes, and reconnecting costs one of the fifteen the account is allowed.
const refreshWindow = 5 * time.Minute

// TokenStore persists an account's OAuth token.
type TokenStore interface {
	SaveToken(ctx context.Context, accountID string, token *oauth2.Token) error
	// MarkNeedsReauth records that the grant is gone, so the account stops
	// and the API can say why.
	MarkNeedsReauth(ctx context.Context, accountID, reason string) error
}

// tokenSource keeps one account's access token fresh and persists what it is
// given.
//
// Two things make this more than a wrapper around oauth2.ReuseTokenSource:
//
//   - Microsoft returns a new refresh token on every refresh, and does not
//     revoke the old one immediately. A daemon that does not write the new one
//     down therefore works fine until it restarts, and then cannot
//     authenticate at all — weeks later, with no obvious cause. So the token
//     is persisted synchronously, before it is handed out, rather than in the
//     background where a crash could lose it.
//
//   - A server can refuse a token the library still considers valid: clock
//     skew, an early server-side expiry, a grant revoked and restored.
//     Invalidate forces the next call to refresh, which is what lets a
//     rejected login recover instead of parking the account.
type tokenSource struct {
	accountID string
	config    *oauth2.Config
	store     TokenStore
	// ctx outlives any request: oauth2 uses the context it was given at
	// construction for every later refresh, so a request context here would
	// make every refresh fail with "context canceled" once that request
	// ended.
	ctx context.Context

	mu      sync.Mutex
	current *oauth2.Token
	inner   oauth2.TokenSource

	// retired is set once the account stops using this source's grant — a
	// new consent replaced it, the mail server refused it, the account was
	// removed. A connection still opening with it may use what it refreshes,
	// but nothing it learns is written over what the account holds now: not
	// a refreshed token, and not a dead grant.
	retired atomic.Bool
}

var _ provider.TokenSource = (*tokenSource)(nil)

// newTokenSource builds a source for one account.
func newTokenSource(ctx context.Context, accountID string, config *oauth2.Config, token *oauth2.Token, store TokenStore) *tokenSource {
	s := &tokenSource{accountID: accountID, config: config, store: store, ctx: ctx, current: token}
	s.inner = oauth2.ReuseTokenSourceWithExpiry(token, config.TokenSource(ctx, token), refreshWindow)
	return s
}

// persistTimeout bounds writing a refreshed token down.
const persistTimeout = 10 * time.Second

// Token returns a valid access token, refreshing and persisting as needed.
func (s *tokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	token, err := s.inner.Token()
	if err != nil {
		return "", s.classifyRefresh(ctx, err)
	}

	// A new pointer means the library refreshed, and the refresh token may
	// have rotated with it.
	if s.current == nil || token.AccessToken != s.current.AccessToken || token.RefreshToken != s.current.RefreshToken {
		if s.retired.Load() {
			s.current = token
			return token.AccessToken, nil
		}
		// Detached from the caller's context on purpose. By the time this
		// runs the provider has already rotated the token: the old refresh
		// token is spent, and the new one exists only in memory. A cancelled
		// request must not be able to discard it — that loses the account,
		// silently, until the next restart.
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
		defer cancel()
		if err := s.store.SaveToken(writeCtx, s.accountID, token); err != nil {
			return "", fmt.Errorf("account: persist refreshed token: %w", err)
		}
		s.current = token
	}
	return token.AccessToken, nil
}

// retire stops the source writing to the account; see retired.
func (s *tokenSource) retire() { s.retired.Store(true) }

// Invalidate discards the cached access token.
//
// It rebuilds the inner source around a copy whose access token is empty and
// whose expiry is in the past, because oauth2 has no way to say "refresh now":
// asking a ReuseTokenSource for a token it believes is still valid returns the
// same one, and the retry after a rejected login would present exactly the
// credential that was just refused.
func (s *tokenSource) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return
	}
	stale := &oauth2.Token{
		RefreshToken: s.current.RefreshToken,
		TokenType:    s.current.TokenType,
		Expiry:       time.Now().Add(-time.Hour),
	}
	s.inner = oauth2.ReuseTokenSourceWithExpiry(stale, s.config.TokenSource(s.ctx, stale), refreshWindow)
}

// classifyRefresh decides whether a failed refresh is fatal for the account.
//
// invalid_grant and the AADSTS codes mean the refresh token itself is gone:
// no amount of retrying brings it back, and the only thing that helps is a
// person consenting again. Everything else — a network blip, a 500 from the
// identity provider, this server's own client being refused — is not the
// account's fault, and treating it as fatal would stop a working account over
// a hiccup or a setting.
func (s *tokenSource) classifyRefresh(ctx context.Context, err error) error {
	var retrieve *oauth2.RetrieveError
	if !errors.As(err, &retrieve) {
		return fmt.Errorf("%w: refreshing the access token failed: %w", provider.ErrTemporary, err)
	}
	if clientRejected(retrieve) {
		// This server's registration was refused, not this account's grant:
		// a rotated or expired secret, a deleted app. Every account on the
		// client fails the same way and every one recovers the moment the
		// configuration is fixed, so nothing is written to the account.
		// Temporary, so whatever retries keeps retrying until then.
		return fmt.Errorf("%w: %w (%s%s): check the OAuth client id and secret in the environment",
			provider.ErrTemporary, ErrClientRejected, retrieve.ErrorCode, microsoftDetail(retrieve))
	}

	reason, dead := deadGrantReason(retrieve)
	if !dead {
		return fmt.Errorf("%w: the identity provider refused the refresh: %s",
			provider.ErrTemporary, retrieve.ErrorCode)
	}
	if s.retired.Load() {
		// A grant the account no longer uses died; its current one did not.
		return fmt.Errorf("%w: %s", provider.ErrNeedsReauth, reason)
	}
	// Also detached: the account's state has changed whether or not the
	// request that noticed is still around to hear about it.
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	if markErr := s.store.MarkNeedsReauth(markCtx, s.accountID, reason); markErr != nil {
		return fmt.Errorf("%w (and recording it failed: %w)", provider.ErrNeedsReauth, markErr)
	}
	return fmt.Errorf("%w: %s", provider.ErrNeedsReauth, reason)
}

// transientErrorCodes are token-endpoint failures that pass on their own.
// Stopping an account over one would mean a person re-authorising a mailbox
// because the identity provider had a bad minute.
var transientErrorCodes = map[string]bool{
	"temporarily_unavailable": true,
	"server_error":            true,
	"slow_down":               true,
	"authorization_pending":   true,
}

// deadGrantReason reports whether a token-endpoint error means the grant is
// gone for good, and says so in words an operator can act on.
func deadGrantReason(err *oauth2.RetrieveError) (string, bool) {
	if transientErrorCodes[err.ErrorCode] {
		return "", false
	}
	switch err.ErrorCode {
	case "invalid_grant":
		// Both providers use this for a revoked, expired or password-changed
		// grant. Microsoft adds a code saying which.
		return "the refresh token is no longer valid" + microsoftDetail(err), true
	case "interaction_required", "consent_required", "login_required":
		return "the identity provider is asking the user to sign in again", true
	case "unauthorized_client", "invalid_client":
		// The client, not the grant: see clientRejected. Returned here as
		// well so the AADSTS fallback below cannot reclassify it.
		return "", false
	}
	// Microsoft sometimes reports a dead grant without one of the standard
	// error codes, so a recognised AADSTS code in the body still counts —
	// but only when the error code itself was not one of the transient ones
	// handled above.
	if detail := microsoftDetail(err); detail != "" {
		return "the refresh token is no longer valid" + detail, true
	}
	return "", false
}

// microsoftDetail extracts the AADSTS code, which is the only part of an Entra
// error worth showing someone.
func microsoftDetail(err *oauth2.RetrieveError) string {
	var body struct {
		ErrorCodes  []int  `json:"error_codes"`
		Description string `json:"error_description"`
		Suberror    string `json:"suberror"`
	}
	if jsonErr := json.Unmarshal(err.Body, &body); jsonErr != nil {
		return ""
	}
	for _, code := range body.ErrorCodes {
		switch code {
		case 70008, 700082:
			return " (AADSTS" + itoa(code) + ": it expired through disuse)"
		case 50173:
			return " (AADSTS50173: the password was changed or the tokens were revoked)"
		case 70000, 700084:
			return " (AADSTS" + itoa(code) + ")"
		}
	}
	if body.Suberror != "" {
		return " (" + body.Suberror + ")"
	}
	// The description carries the code in its text on some responses.
	if idx := strings.Index(body.Description, "AADSTS"); idx >= 0 {
		end := idx + 6
		for end < len(body.Description) && body.Description[end] >= '0' && body.Description[end] <= '9' {
			end++
		}
		return " (" + body.Description[idx:end] + ")"
	}
	return ""
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
