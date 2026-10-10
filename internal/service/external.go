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
// keeps the public keys an identity is known by, the product key the
// provider delivers among them. A person who has no account key yet gets no
// session but a ticket, with which their page writes the account key and its
// wrap under the product key (EnrolExternal), which opens it
// (docs/key-scheme.md sections 6, 12.8 and 12.10). No transport calls any of
// them: the core has no provider of its own.
//
// A daemon whose people sign in only that way (Deps.ExternalSignInOnly)
// refuses every route that signs in with a password, signs up or accepts an
// invitation, changes a password, or creates an invitation: every route of
// the key scheme's password and recovery code (docs/key-scheme.md section
// 12). Its step-up is the provider's (MarkExternalStepUp, ExternalStepUp).

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
	// AuthTime is when the provider says the person last authenticated
	// (OpenID Connect's auth_time, from the userinfo of the access token
	// the sign-in presented): the session's step-up time, never the moment
	// of the sign-in, and at most now. A sign-in the provider answered
	// silently from a session of its own carries the person's earlier
	// authentication, and opens no step-up window unless that was within
	// the last ten minutes (docs/key-scheme.md section 11). Zero is none.
	AuthTime time.Time
	// WantsKey says the sign-in asked the provider for the product key,
	// which the provider delivered to the person's page alone
	// (docs/key-scheme.md section 6.2): the page opens the person's
	// platform wrap with it, or, at their first sign-in, makes their account
	// key and wraps it. ProductKeyID is that key's id as the provider names
	// it ("mailie:<epoch>"), which the extension pinned for the identity
	// (PinIdentityKey) and compared with the pin before calling; it is read
	// only with WantsKey, and must be pinned for the identity.
	WantsKey     bool
	ProductKeyID string
}

// ExternalSignedIn is what SignInExternal answers: a session, or, for a
// person who has no account key yet, the ticket that writes it.
type ExternalSignedIn struct {
	// Session is the person's new session, exactly as POST /v1/auth/login
	// answers one; nil for a person without an account key, who is
	// answered Enrolment instead.
	Session *Session `json:"session,omitempty"`
	// PlatformWrap is the person's platform wrap at the sign-in's
	// ProductKeyID, base64url of 61 bytes, when the sign-in asked for the
	// key: their page opens it with the product key, and checks that the
	// key inside is the one whose public half Session.User.PublicKey names.
	PlatformWrap string `json:"platform_wrap,omitempty"`
	// Enrolment is answered, in place of a session, to a sign-in that asked
	// for the key of a person who has no account key yet.
	Enrolment *ExternalEnrolmentTicket `json:"enrolment,omitempty"`
}

// ExternalEnrolmentTicket is what the page of a person without an account
// key needs to make one (docs/key-scheme.md section 12.10): a single-use
// ticket for EnrolExternal, and the seal id the platform wrap binds the
// person by.
type ExternalEnrolmentTicket struct {
	Ticket    string `json:"ticket"`
	SealID    string `json:"seal_id"`
	ExpiresAt int64  `json:"expires_at"`
}

// SignInExternal signs in the person an identity provider vouched for, in
// one transaction:
//
//   - an identity (issuer, subject) already linked signs in its person; a
//     disabled person is refused as their password sign-in is;
//   - an identity seen for the first time needs a verified address, and
//     only ever creates a person: an instance member with no password, as
//     signing up creates one, to whom it is linked. An address somebody here
//     has already, however they sign in, is a conflict, and nothing is
//     created or linked: accounts are never linked by matching addresses;
//   - a person who has an account key gets a session, which expires TTL
//     after it starts and is never extended, and, when the sign-in asked
//     for the product key, their platform wrap at ProductKeyID. None stored
//     there is a conflict whose cause is auth.ErrNoPlatformWrap, and starts
//     no session;
//   - a person who has none gets no session. A sign-in that asked for the
//     product key is answered an enrolment ticket (EnrolExternal), and one
//     that did not a conflict whose cause is auth.ErrAccountKeyNeeded: the
//     page signs in again asking for the key, and nothing was created or
//     linked. So nobody who copies a session ever chooses a person's account
//     key (docs/key-scheme.md section 12.10).
//
// The causes are what an extension tells refusals apart by (errors.Is),
// with auth.ErrEmailTaken for an address somebody here has. What an
// extension of internal/app uses; no transport calls it.
func (s *Service) SignInExternal(ctx context.Context, in ExternalSignIn) (ExternalSignedIn, error) {
	signed, err := s.users.SignInExternal(ctx, auth.ExternalSignIn{
		Issuer: in.Issuer, Subject: in.Subject, Email: in.Email, EmailVerified: in.EmailVerified,
		Name: in.Name, UserAgent: in.UserAgent, TTL: in.TTL, AuthTime: in.AuthTime,
		WantsKey: in.WantsKey, ProductKeyID: in.ProductKeyID,
	})
	if err != nil {
		return ExternalSignedIn{}, fromExternal(err, "signing in failed")
	}
	if signed.Token == "" {
		return ExternalSignedIn{Enrolment: &ExternalEnrolmentTicket{
			Ticket: signed.Ticket, SealID: signed.User.SealID, ExpiresAt: signed.TicketExpiresAt.Unix(),
		}}, nil
	}
	session := presentSession(signed.Token, signed.Session, signed.User)
	out := ExternalSignedIn{Session: &session}
	if len(signed.PlatformWrap) > 0 {
		out.PlatformWrap = b64(signed.PlatformWrap)
	}
	return out, nil
}

// ExternalEnrolment is what the page of a person without an account key
// sends with the ticket its sign-in answered (docs/key-scheme.md section
// 12.10), as the extension passes it on.
type ExternalEnrolment struct {
	// Ticket is ExternalEnrolmentTicket.Ticket.
	Ticket string `json:"ticket"`
	// PublicKey is the account public key the page made, base64url of 32
	// bytes.
	PublicKey string `json:"public_key"`
	// PlatformWrap is the account key wrapped under the product key, bound
	// to the person's seal id, the identity's subject, ProductKeyID and
	// PublicKey: base64url of 61 bytes starting with 0x03, which the server
	// checks the shape of and never opens.
	PlatformWrap string `json:"platform_wrap"`
	// ProductKeyID is the product key id the page wrapped under, the one
	// the sign-in that issued the ticket was delivered.
	ProductKeyID string `json:"product_key_id"`
	// UserAgent is the browser's, as the session list shows it.
	UserAgent string `json:"-"`
}

// EnrolExternal writes the account key of a person who signs in through an
// identity provider and has none yet, with their sign-in's enrolment ticket,
// and starts their session (docs/key-scheme.md section 12.10), in one
// transaction: the ticket is used once (unexpired, issued for ProductKeyID,
// its person active and its identity still theirs); the public key is
// written once, and a person who has one already is a conflict; the platform
// wrap is stored, insert only, under the ticket's product key id; and the
// session starts with the length and the step-up time (the provider's
// authentication time) of the sign-in that issued the ticket. Its answer is
// what POST /v1/auth/login answers.
//
// What an extension of internal/app uses; no transport calls it.
func (s *Service) EnrolExternal(ctx context.Context, in ExternalEnrolment) (Session, error) {
	pub, ok := strictBytes(in.PublicKey, publicKeyLen)
	if !ok {
		return Session{}, errBadPlatformKeyMaterial
	}
	wrap, ok := strictBytes(in.PlatformWrap, platformWrapLen)
	if !ok {
		return Session{}, errBadPlatformKeyMaterial
	}
	token, session, user, err := s.users.EnrolExternal(ctx, auth.ExternalEnrolment{
		Ticket: in.Ticket, PublicKey: pub, PlatformWrap: wrap, ProductKeyID: in.ProductKeyID, UserAgent: in.UserAgent,
	})
	if err != nil {
		return Session{}, fromExternal(err, "enrolling failed")
	}
	return presentSession(token, session, user), nil
}

// platformWrapLen is the decoded size of a platform wrap.
const platformWrapLen = 61

// errBadPlatformKeyMaterial is a public key or a platform wrap outside its
// shape.
var errBadPlatformKeyMaterial = E(CodeBadRequest,
	"the public key is base64url of 32 bytes the server accepts, and a platform wrap base64url of 61 bytes starting with 0x03", nil)

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

// MarkExternalStepUp starts a step-up through the identity provider on the
// caller's session (docs/key-scheme.md section 11, hosted): it records a mark,
// the time now, bound to this session, used once and valid for ten minutes,
// and returns it. The page then signs in at the provider again, asking it to
// authenticate the person anew, and the extension finishes with
// ExternalStepUp.
//
// What an extension of internal/app uses; no transport calls it.
func (s *Service) MarkExternalStepUp(ctx context.Context, p Principal) (time.Time, error) {
	if err := requireSession(p); err != nil {
		return time.Time{}, err
	}
	at, err := s.users.MarkExternalStepUp(ctx, p.UserID, p.SessionID)
	if err != nil {
		return time.Time{}, fromUsers(err, "starting the step-up failed")
	}
	return at, nil
}

// ExternalStepUpProof is what the identity provider says of a step-up, as
// the extension read it from the provider (OpenID Connect's userinfo of the
// access token the step-up presented).
type ExternalStepUpProof struct {
	// Issuer and Subject are the identity that authenticated.
	Issuer  string
	Subject string
	// AuthTime is when it authenticated (auth_time).
	AuthTime time.Time
	// ProductKeyID and ProductKey are the product key the provider names
	// for the identity now, compared with the one pinned under that id and
	// never pinned. Empty only for a provider that delivers no product key,
	// and then refused for an identity that has keys pinned.
	ProductKeyID string
	ProductKey   []byte
}

// ExternalStepUp finishes a step-up through the identity provider: the
// provider says the identity authenticated at proof.AuthTime. The caller's
// session's step-up time becomes that time, which it returns, only if the
// session has a mark younger than ten minutes, the time is after the mark
// (in whole seconds, strictly) and not after now, and the identity is the
// one linked to the session's own person; anything else is refused,
// not_authorized, and changes nothing. The product key the provider names
// is then compared with the pin, read only: another key than the one pinned
// under its id, or an id nothing is pinned under, is a conflict whose cause
// is auth.ErrProductKeyChanged, logged as an error (only the provider, or
// whoever writes its database, can present one), and changes nothing; a
// step-up never pins a key. The extension checks, before calling this, that
// the provider issued the access token to this product. It never creates a
// session, nor changes another one or whose this one is.
//
// What an extension of internal/app uses; no transport calls it.
func (s *Service) ExternalStepUp(ctx context.Context, p Principal, proof ExternalStepUpProof) (time.Time, error) {
	if err := requireSession(p); err != nil {
		return time.Time{}, err
	}
	at, err := s.users.ExternalStepUp(ctx, p.UserID, p.SessionID, auth.ExternalProof{
		Issuer: proof.Issuer, Subject: proof.Subject, AuthTime: proof.AuthTime,
		ProductKeyID: proof.ProductKeyID, ProductKey: proof.ProductKey,
	})
	if errors.Is(err, auth.ErrProductKeyChanged) {
		// The alert operators look for, as for a sign-in the extension
		// refuses for a key other than the pinned one.
		s.log.Error("a step-up through the identity provider was refused: the identity's product key differs "+
			"from the one pinned for it", "user", p.UserID, "product_key_id", proof.ProductKeyID)
	}
	if err != nil {
		return time.Time{}, fromExternal(err, "stepping up failed")
	}
	return at, nil
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

// fromExternal maps a failure of an external sign-in, an enrolment, a pin or
// an external step-up onto the transport vocabulary. Each keeps the auth
// sentinel as its cause, which is what an extension tells refusals apart by.
func fromExternal(err error, what string) error {
	switch {
	case errors.Is(err, auth.ErrAccountKeyNeeded):
		return E(CodeConflict, "this person has no account key yet: sign in again asking for the product key, "+
			"which makes one", err)
	case errors.Is(err, auth.ErrNoPlatformWrap):
		return E(CodeConflict, "no platform wrap is stored for this person under that product key id", err)
	case errors.Is(err, auth.ErrAccountKeyExists):
		return E(CodeConflict, "this person has an account key already: sign in again, which answers its wrap", err)
	case errors.Is(err, auth.ErrProductKeyChanged):
		return E(CodeConflict, "the identity's product key differs from the one pinned for it", err)
	case errors.Is(err, auth.ErrProductKeyNotPinned):
		return E(CodeNotAuthorized, "no product key is pinned for this identity under that id", err)
	case errors.Is(err, auth.ErrInvalidProductKeyID):
		return E(CodeBadRequest, "a product key id is mailie:<epoch>, the epoch 1 to 2147483647 without a leading zero", err)
	case errors.Is(err, auth.ErrInvalidPublicKey), errors.Is(err, auth.ErrInvalidPlatformWrap):
		return E(CodeBadRequest, errBadPlatformKeyMaterial.Message, err)
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
