package service

import (
	"context"
	"errors"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
)

// Signing in through an identity provider, for an extension of the daemon
// (internal/app's Options.Extensions, which reach the service through
// app.Deps). The extension does the provider's protocol on routes of its own,
// and calls SignInExternal with who the provider says the person is; its page
// answers with the Session that comes back, exactly as POST /v1/auth/login
// answers the console, and the console adopts it the same way. PinIdentityKey
// keeps the public keys an identity is known by. No transport calls either:
// the core has no provider of its own.
//
// A daemon whose people sign in only that way (Deps.ExternalSignInOnly)
// refuses every route that signs in with a password, signs up or accepts an
// invitation, changes a password, or creates an invitation.

// ExternalSignIn is a person an identity provider vouched for, as an
// extension presents them once it has done the provider's protocol.
type ExternalSignIn struct {
	// Issuer is the provider's origin, exactly as a browser writes one:
	// https://host[:port], or http:// on loopback or a name under
	// .localhost, with no path.
	Issuer string
	// Subject is the provider's stable id for the person (OpenID Connect's
	// sub): never an address, which may change.
	Subject string
	// Email is the person's address at the provider, and EmailVerified
	// whether the provider vouches for it. An identity seen for the first
	// time creates a person only with a verified address, never with one
	// that lower case would make another (the Kelvin sign is not the letter
	// K), and never with one somebody here has already: it is never linked
	// to a person who exists. A linked identity does not look at either.
	Email         string
	EmailVerified bool
	// Name is a new person's display name, made into one the server takes
	// rather than refused; the person a linked identity signs in keeps
	// theirs, and it is not looked at.
	Name string
	// UserAgent is the browser's, as the session list shows it.
	UserAgent string
	// TTL is how long the session lasts: more than nothing, at most
	// auth.SessionTTL. Nothing extends it. It bounds the session, not what
	// the person does with it: a key they create in a workspace while signed
	// in (CreateWorkspaceKey) lasts what they chose for it, and until the
	// provider can tell this server it closed someone, disabling the person
	// here is what revokes the keys they created.
	TTL time.Duration
}

// SignInExternal signs in the person an identity provider vouched for and
// starts their session, in one transaction:
//
//   - an identity (issuer, subject) already linked signs in its person; a
//     disabled person is refused as their password sign-in is;
//   - an identity seen for the first time needs a verified address, and
//     only ever creates a person: an instance member with no password, as
//     signing up creates one, to whom it is linked. An address somebody here
//     has already, however they sign in, is a conflict, and nothing is
//     created or linked: accounts are never linked by matching addresses;
//   - the session expires TTL after it starts, and is never extended.
//
// What an extension of internal/app uses; no transport calls it.
func (s *Service) SignInExternal(ctx context.Context, in ExternalSignIn) (Session, error) {
	token, session, user, err := s.users.SignInExternal(ctx, auth.ExternalSignIn{
		Issuer: in.Issuer, Subject: in.Subject, Email: in.Email, EmailVerified: in.EmailVerified,
		Name: in.Name, UserAgent: in.UserAgent, TTL: in.TTL,
	})
	if err != nil {
		return Session{}, fromExternal(err, "signing in failed")
	}
	return presentSession(token, session, user), nil
}

// PinIdentityKey pins key under keyID for the identity (issuer, subject),
// unless a key is pinned there already, and returns the key pinned there now
// and whether this call pinned it: atomically, so of two sign-ins racing each
// other both read the one that won. A pin is never replaced, and goes only
// with the person the identity signs in. An extension that gets back another
// key than the one it offered refuses the sign-in. The key returned is the
// caller's own copy.
//
// What an extension of internal/app uses; no transport calls it.
func (s *Service) PinIdentityKey(ctx context.Context, issuer, subject, keyID string, key []byte) ([]byte, bool, error) {
	pinned, inserted, err := s.users.PinKey(ctx, issuer, subject, keyID, key)
	if err != nil {
		return nil, false, fromExternal(err, "pinning the key failed")
	}
	return pinned, inserted, nil
}

// errExternalSignInOnly is every password and invitation route of a daemon
// whose people sign in only through an extension.
var errExternalSignInOnly = E(CodeNotAuthorized,
	"people sign in to this server another way: passwords and invitations are not used here", nil)

// passwordsInUse refuses, on a daemon whose people sign in only through an
// extension, what signs in with a password, signs up or accepts an
// invitation, changes a password or creates an invitation.
func (s *Service) passwordsInUse() error {
	if s.externalSignInOnly {
		return errExternalSignInOnly
	}
	return nil
}

// fromExternal maps a failure of an external sign-in or a pin onto the
// transport vocabulary.
func fromExternal(err error, what string) error {
	switch {
	case errors.Is(err, auth.ErrInvalidIssuer):
		return E(CodeBadRequest, "the issuer must be an origin: https://host[:port], or http:// on loopback", err)
	case errors.Is(err, auth.ErrInvalidSubject):
		return Ef(CodeBadRequest, err, "the subject must be 1 to %d bytes of text", auth.MaxSubjectLength)
	case errors.Is(err, auth.ErrInvalidSessionTTL):
		return E(CodeBadRequest, "a session lasts more than nothing and at most 14 days", err)
	case errors.Is(err, auth.ErrInvalidPin):
		return Ef(CodeBadRequest, err, "a pinned key has an id of 1 to %d bytes and 1 to %d bytes of key",
			auth.MaxKeyIDLength, auth.MaxPinnedKeyBytes)
	case errors.Is(err, auth.ErrEmailNotVerified):
		return E(CodeNotAuthorized, "the identity provider has not verified this address", err)
	case errors.Is(err, auth.ErrEmailTaken):
		// Before fromUsers, whose answer tells a person to sign in instead.
		return E(CodeConflict, "this address already has an account here, which signing in through an identity provider never takes over", err)
	case errors.Is(err, auth.ErrUserDisabled):
		// As a disabled person's password sign-in is refused.
		return E(CodeUnauthorized, "this account cannot sign in", err)
	default:
		return fromUsers(err, what)
	}
}
