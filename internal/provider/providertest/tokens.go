package providertest

import (
	"context"
	"errors"
	"sync"

	"github.com/thehappieco/mailie/internal/provider"
)

// TokenSource is a provider.TokenSource a test drives by hand.
//
// It records refreshes, so a test can assert that the client actually took a
// new token after a refusal rather than that the retry happened to succeed.
type TokenSource struct {
	mu sync.Mutex
	// Tokens are handed out in order; the last one repeats.
	tokens   []string
	position int
	// stale marks the current token as needing a refresh before the next use.
	stale bool
	// refreshErr, when set, is what a refresh fails with — how a dead grant
	// arrives in production.
	refreshErr error
	refreshes  int
	issued     []string
}

var _ provider.TokenSource = (*TokenSource)(nil)

// NewTokenSource builds a source that yields the given tokens in order.
func NewTokenSource(tokens ...string) *TokenSource {
	if len(tokens) == 0 {
		tokens = []string{"token-1"}
	}
	return &TokenSource{tokens: tokens}
}

// Token returns the current token, advancing first if it was invalidated.
func (s *TokenSource) Token(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stale {
		if s.refreshErr != nil {
			return "", s.refreshErr
		}
		s.stale = false
		s.refreshes++
		if s.position < len(s.tokens)-1 {
			s.position++
		}
	}
	token := s.tokens[s.position]
	s.issued = append(s.issued, token)
	return token, nil
}

// Invalidate marks the cached token stale.
func (s *TokenSource) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stale = true
}

// Refreshes is how many times Invalidate was followed by a successful refresh.
func (s *TokenSource) Refreshes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshes
}

// Tokens is every token this source can ever yield, so a test server can be
// told to accept them all up front.
func (s *TokenSource) Tokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.tokens...)
}

// Issued is every token handed out, in order.
func (s *TokenSource) Issued() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.issued...)
}

// FailRefreshWith makes the next refresh fail, the way a revoked grant does.
func (s *TokenSource) FailRefreshWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshErr = err
}

// ErrGrantRevoked is what a token source reports when the refresh token itself
// is gone: the account stops until a person consents again.
var ErrGrantRevoked = errors.New("providertest: the grant was revoked")
