package service

import (
	"context"
	"encoding/base64"
	"errors"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
)

// User is a person, as the console shows them to themselves.
type User struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	CreatedAt int64  `json:"created_at"`
	// HasPassword is false for a person who signs in only through an
	// identity provider: there is no password to change, and a console
	// does not offer to.
	HasPassword bool `json:"has_password"`
	// SealID is the UUIDv4 every wrap and grant binds the person by
	// (docs/key-scheme.md section 3.1): the browser keeps the account key
	// under it, and opens a wrap only for it.
	SealID string `json:"seal_id,omitempty"`
	// PublicKey is the person's account public key, base64url, written
	// once at their enrolment; absent before. The browser compares the key
	// it opens with it.
	PublicKey string `json:"public_key,omitempty"`
}

// Session is a new sign-in. The token is in this reply and nowhere else: only
// its hash is stored.
type Session struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
	// AuthenticatedAt is the session's step-up time (docs/key-scheme.md
	// section 11), unix seconds; 0 is none. What the step-up guards is
	// refused once it is more than ten minutes old.
	AuthenticatedAt int64 `json:"authenticated_at"`
	User            User  `json:"user"`
}

// SessionInfo describes the session a request came in on.
type SessionInfo struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
	// AuthenticatedAt is the session's step-up time; 0 is none.
	AuthenticatedAt int64 `json:"authenticated_at"`
}

// Me is who the caller is, which is how a console that reloads finds out
// whether the token it kept still works.
type Me struct {
	User    User        `json:"user"`
	Session SessionInfo `json:"session"`
}

// Invite is an invite as its creator sees it: the link carries the code in its
// fragment, and this is the only time it is shown.
type Invite struct {
	Email     string `json:"email"`
	Role      string `json:"role"`
	URL       string `json:"url"`
	ExpiresAt int64  `json:"expires_at"`
}

// KDF is the Argon2id parameters a browser derives a password with, as the
// wire carries them: {"alg":"argon2id","m":65536,"t":3,"p":1}, m in KiB.
type KDF struct {
	Alg string `json:"alg"`
	M   int    `json:"m"`
	T   int    `json:"t"`
	P   int    `json:"p"`
}

// ChallengeRequest asks what a password is derived under for an address.
type ChallengeRequest struct {
	Email string `json:"email"`
}

// Challenge is the salt (base64url, 16 bytes) and the parameters to derive
// under. Upgrade says the address has a person whose password the server
// still checks itself, once: their sign-in is the upgrade's (POST
// /v1/auth/upgrade/login). It exists in the release that brings the key
// scheme only.
type Challenge struct {
	Salt    string `json:"salt"`
	KDF     KDF    `json:"kdf"`
	Upgrade bool   `json:"upgrade,omitempty"`
}

// Enrolment is what a browser sends to enrol a person: the auth key and the
// recovery proof (base64url of 32 bytes each), the parameters derived with,
// the account public key (base64url, 32 bytes) and the account key wrapped
// under the password and under the recovery code (base64url, 61 bytes each).
type Enrolment struct {
	AuthKey       string `json:"auth_key"`
	KDF           KDF    `json:"kdf"`
	PublicKey     string `json:"public_key"`
	PasswordWrap  string `json:"password_wrap"`
	RecoveryWrap  string `json:"recovery_wrap"`
	RecoveryProof string `json:"recovery_proof"`
}

// SignUpOpenRequest opens an invitation before its person chooses a
// password: the code and the address its link carries.
type SignUpOpenRequest struct {
	Invite string `json:"invite"`
	Email  string `json:"email"`
}

// SignUpOpen is what the browser binds and derives under to sign up with an
// invitation: the address's target (a salt, base64url of 16 bytes, and the
// parameters), and the seal id the person will have, which the server drew
// for the invitation.
type SignUpOpen struct {
	Salt   string `json:"salt"`
	KDF    KDF    `json:"kdf"`
	SealID string `json:"seal_id"`
}

// SignUpRequest is the form an invite link opens, with the seal id opening
// it answered, which the wraps are bound to.
type SignUpRequest struct {
	Invite string `json:"invite"`
	Email  string `json:"email"`
	Name   string `json:"name"`
	SealID string `json:"seal_id"`
	Enrolment
}

// LoginRequest is a sign-in: the address and the auth key derived under what
// its challenge answered.
type LoginRequest struct {
	Email   string `json:"email"`
	AuthKey string `json:"auth_key"`
}

// Rederive is a target a sign-in names when the account is not at it: the
// browser derives the same password under it and finishes with
// POST /v1/auth/password/finish and this ticket. No session ends.
type Rederive struct {
	Salt   string `json:"salt"`
	KDF    KDF    `json:"kdf"`
	Ticket string `json:"ticket"`
}

// Login is a sign-in's answer: the session, and the account key under the
// password, for the browser to open with the wrap key it derived.
type Login struct {
	Session
	PasswordWrap string    `json:"password_wrap"`
	Rederive     *Rederive `json:"rederive,omitempty"`
}

// PasswordBeginRequest proves the current password, as its auth key, to
// change it.
type PasswordBeginRequest struct {
	CurrentAuthKey string `json:"current_auth_key"`
}

// PasswordBegin is what changing the password needs: the current password
// wrap, the target to derive the new password under, and the ticket that
// finishes it.
type PasswordBegin struct {
	PasswordWrap string `json:"password_wrap"`
	Salt         string `json:"salt"`
	KDF          KDF    `json:"kdf"`
	Ticket       string `json:"ticket"`
}

// PasswordFinishRequest stores the new auth key and password wrap: of a
// password change (the ticket of PasswordBegin) or of a sign-in's
// re-derivation (the ticket of Login's Rederive).
type PasswordFinishRequest struct {
	Ticket       string `json:"ticket"`
	AuthKey      string `json:"auth_key"`
	KDF          KDF    `json:"kdf"`
	PasswordWrap string `json:"password_wrap"`
}

// RecoverOpenRequest proves a recovery code, as its proof, for an address.
type RecoverOpenRequest struct {
	Email         string `json:"email"`
	RecoveryProof string `json:"recovery_proof"`
}

// RecoverOpen is what a recovery needs: who the person is, the account key
// under the recovery code, the target to derive the new password under, and
// the ticket that finishes it.
type RecoverOpen struct {
	SealID       string `json:"seal_id"`
	PublicKey    string `json:"public_key"`
	RecoveryWrap string `json:"recovery_wrap"`
	Salt         string `json:"salt"`
	KDF          KDF    `json:"kdf"`
	Ticket       string `json:"ticket"`
}

// RecoverFinishRequest stores a new password and a new recovery code over
// the same account key.
type RecoverFinishRequest struct {
	Ticket        string `json:"ticket"`
	AuthKey       string `json:"auth_key"`
	KDF           KDF    `json:"kdf"`
	PasswordWrap  string `json:"password_wrap"`
	RecoveryWrap  string `json:"recovery_wrap"`
	RecoveryProof string `json:"recovery_proof"`
}

// RecoveryRequest replaces the recovery code: the current auth key, which
// proves the password in this request, the account key wrapped under a new
// code, and that code's proof.
type RecoveryRequest struct {
	CurrentAuthKey string `json:"current_auth_key"`
	RecoveryWrap   string `json:"recovery_wrap"`
	RecoveryProof  string `json:"recovery_proof"`
}

// StepUpRequest proves the session's own person again: the auth key derived
// under their stored salt and parameters.
type StepUpRequest struct {
	AuthKey string `json:"auth_key"`
}

// StepUp is the session's new step-up time, unix seconds.
type StepUp struct {
	AuthenticatedAt int64 `json:"authenticated_at"`
}

// UpgradeLoginRequest is the upgrade's one password in clear
// (docs/key-scheme.md section 12.7), for a person who signed up before the
// key scheme. It exists in the release that brings the scheme only.
type UpgradeLoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// UpgradeTicket is what the old password proves: a ticket to enrol with,
// not a session, the person's seal id to bind the wraps to, and the target
// to derive under, which the enrolment stores.
type UpgradeTicket struct {
	Ticket string `json:"ticket"`
	SealID string `json:"seal_id"`
	Salt   string `json:"salt"`
	KDF    KDF    `json:"kdf"`
}

// UpgradeEnrolRequest enrols the person the ticket names.
type UpgradeEnrolRequest struct {
	Ticket string `json:"ticket"`
	Enrolment
}

// ResetOpenRequest checks a reset invitation before its person chooses a
// password: the code and the address its link carries.
type ResetOpenRequest struct {
	Reset string `json:"reset"`
	Email string `json:"email"`
}

// ResetOpen is the salt (base64url, 16 bytes) and the parameters a reset's
// new password is derived under: the account's target, which the reset
// stores, and not what a challenge answers for an account off its target;
// and the person's seal id, which never changes, to bind the new wraps to.
type ResetOpen struct {
	Salt   string `json:"salt"`
	KDF    KDF    `json:"kdf"`
	SealID string `json:"seal_id"`
}

// ResetRequest redeems a reset invitation (docs/key-scheme.md section 12.6):
// the code and the address its link carries, and a new enrolment derived
// under what OpenReset answered.
type ResetRequest struct {
	Reset string `json:"reset"`
	Email string `json:"email"`
	Enrolment
}

// SignOutRequest ends the caller's session, or every session they have.
type SignOutRequest struct {
	Everywhere bool `json:"everywhere,omitempty"`
}

// ProfileRequest changes what the console calls the caller.
type ProfileRequest struct {
	Name string `json:"name"`
}

// InviteRequest invites a person by address.
type InviteRequest struct {
	Email string `json:"email"`
	Role  string `json:"role,omitempty"`
}

// hashWaitRetry is what a caller that could not get a hashing slot is told to
// wait: the queue was full, or its deadline came first. Either drains in
// seconds.
const hashWaitRetry = 5 * time.Second

// errBadKeyMaterial is a public key or a wrap that is not base64url of its
// length, or not of its shape.
var errBadKeyMaterial = E(CodeBadRequest,
	"the public key is base64url of 32 bytes the server accepts, and a wrap base64url of 61 bytes starting with 0x02", nil)

// Challenge answers the salt and the parameters a browser derives a password
// under for an address (docs/key-scheme.md section 5.3): an enrolled person's
// own, and for any other address the address's target, so the answer does
// not say whether it has an account; but for the upgrade's, which does, in
// this release only.
func (s *Service) Challenge(ctx context.Context, req ChallengeRequest) (Challenge, error) {
	if err := s.passwordsInUse(); err != nil {
		return Challenge{}, err
	}
	c, err := s.users.Challenge(ctx, req.Email)
	if err != nil {
		return Challenge{}, fromUsers(err, "answering the challenge failed")
	}
	return Challenge{Salt: b64(c.Salt), KDF: presentKDF(c.KDF), Upgrade: c.Upgrade}, nil
}

// OpenSignUp checks an invitation as signing up will, before its person
// chooses a password, and answers what their browser binds and derives
// under. Refused where people sign in only through an extension.
func (s *Service) OpenSignUp(ctx context.Context, req SignUpOpenRequest) (SignUpOpen, error) {
	if err := s.passwordsInUse(); err != nil {
		return SignUpOpen{}, err
	}
	opened, err := s.users.OpenSignUp(ctx, req.Invite, req.Email)
	if err != nil {
		return SignUpOpen{}, fromUsers(err, "opening the invitation failed")
	}
	return SignUpOpen{Salt: b64(opened.Salt), KDF: presentKDF(opened.KDF), SealID: opened.SealID}, nil
}

// SignUp redeems an invite: it creates the account, enrolled in the key
// scheme, and signs it in. Refused where people sign in only through an
// extension.
func (s *Service) SignUp(ctx context.Context, req SignUpRequest, userAgent string) (Session, error) {
	if err := s.passwordsInUse(); err != nil {
		return Session{}, err
	}
	in, err := enrolment(req.Enrolment)
	if err != nil {
		return Session{}, err
	}
	token, session, user, err := s.users.SignUp(ctx, auth.SignUpRequest{
		Invite: req.Invite, Email: req.Email, Name: req.Name, SealID: req.SealID, Enrolment: in, UserAgent: userAgent,
	})
	if err != nil {
		return Session{}, fromUsers(err, "creating the account failed")
	}
	return presentSession(token, session, user), nil
}

// Login signs a person in with an auth key. A wrong key, an address with no
// account, a disabled person, a person who has not enrolled and a person with
// no password all get the same answer, after the same amount of work. Refused
// outright where people sign in only through an extension.
func (s *Service) Login(ctx context.Context, req LoginRequest, userAgent string) (Login, error) {
	if err := s.passwordsInUse(); err != nil {
		return Login{}, err
	}
	login, err := s.users.Login(ctx, req.Email, req.AuthKey, userAgent)
	if err != nil {
		return Login{}, fromSecret(err, CodeUnauthorized, "email or password is wrong", "signing in failed")
	}
	out := Login{Session: presentSession(login.Token, login.Session, login.User), PasswordWrap: b64(login.PasswordWrap)}
	if r := login.Rederive; r != nil {
		out.Rederive = &Rederive{Salt: b64(r.Salt), KDF: presentKDF(r.KDF), Ticket: r.Ticket}
	}
	return out, nil
}

// BeginPasswordChange checks the caller's current auth key and answers what
// changing the password needs. A session alone gets nothing: the wrap is
// answered only to the auth key verified in this request.
func (s *Service) BeginPasswordChange(ctx context.Context, p Principal, req PasswordBeginRequest) (PasswordBegin, error) {
	if err := s.personalSecrets(p); err != nil {
		return PasswordBegin{}, err
	}
	begun, err := s.users.BeginPasswordChange(ctx, p.UserID, p.SessionID, req.CurrentAuthKey)
	if err != nil {
		// not_authorized rather than unauthorized: the session is fine, the
		// proof offered for this one operation is not, and a console that
		// signed people out for a typo would be hostile.
		return PasswordBegin{}, fromSecret(err, CodeNotAuthorized, "the current password is wrong",
			"changing the password failed")
	}
	return PasswordBegin{
		PasswordWrap: b64(begun.PasswordWrap), Salt: b64(begun.Salt), KDF: presentKDF(begun.KDF), Ticket: begun.Ticket,
	}, nil
}

// FinishPasswordChange stores the new auth key and password wrap the ticket
// is for. A password change ends every session the caller has, this one
// included, and answers the session this browser goes on with; a sign-in's
// re-derivation ends nothing, and answers none (rotated false).
func (s *Service) FinishPasswordChange(ctx context.Context, p Principal, req PasswordFinishRequest, userAgent string) (Session, bool, error) {
	if err := s.personalSecrets(p); err != nil {
		return Session{}, false, err
	}
	wrap, err := keyBytes(req.PasswordWrap, accountWrapLen)
	if err != nil {
		return Session{}, false, err
	}
	changed, err := s.users.FinishPasswordChange(ctx, p.UserID, p.SessionID, auth.NewPassword{
		Ticket: req.Ticket, AuthKey: req.AuthKey, KDF: kdfOf(req.KDF), PasswordWrap: wrap,
	}, userAgent)
	if err != nil {
		return Session{}, false, fromUsers(err, "changing the password failed")
	}
	if !changed.Rotated {
		return Session{}, false, nil
	}
	user, err := s.users.Get(ctx, p.UserID)
	if err != nil {
		return Session{}, false, fromUsers(err, "reading the account failed")
	}
	return presentSession(changed.Token, changed.Session, user), true, nil
}

// OpenRecovery checks a recovery code's proof for an address and answers what
// a recovery needs. Every way of failing is the same answer after the same
// work.
func (s *Service) OpenRecovery(ctx context.Context, req RecoverOpenRequest) (RecoverOpen, error) {
	if err := s.passwordsInUse(); err != nil {
		return RecoverOpen{}, err
	}
	r, err := s.users.OpenRecovery(ctx, req.Email, req.RecoveryProof)
	if err != nil {
		return RecoverOpen{}, fromSecret(err, CodeUnauthorized, "email or recovery code is wrong",
			"opening the recovery failed")
	}
	return RecoverOpen{
		SealID: r.SealID, PublicKey: b64(r.PublicKey), RecoveryWrap: b64(r.RecoveryWrap), Salt: b64(r.Salt),
		KDF: presentKDF(r.KDF), Ticket: r.Ticket,
	}, nil
}

// FinishRecovery stores a new password and a new recovery code over the same
// account key. Every session of the person ends; they sign in with the new
// password.
func (s *Service) FinishRecovery(ctx context.Context, req RecoverFinishRequest) error {
	if err := s.passwordsInUse(); err != nil {
		return err
	}
	wraps, err := keysBytes(accountWrapLen, req.PasswordWrap, req.RecoveryWrap)
	if err != nil {
		return err
	}
	if err := s.users.FinishRecovery(ctx, auth.RecoveryFinish{
		Ticket: req.Ticket, AuthKey: req.AuthKey, KDF: kdfOf(req.KDF), PasswordWrap: wraps[0], RecoveryWrap: wraps[1],
		RecoveryProof: req.RecoveryProof,
	}); err != nil {
		return fromUsers(err, "finishing the recovery failed")
	}
	return nil
}

// ReplaceRecovery replaces the caller's recovery code, with their current
// auth key verified in the same request: a session alone, even right after
// its sign-in, sets no secret of its person's.
func (s *Service) ReplaceRecovery(ctx context.Context, p Principal, req RecoveryRequest) error {
	if err := s.personalSecrets(p); err != nil {
		return err
	}
	wrap, err := keyBytes(req.RecoveryWrap, accountWrapLen)
	if err != nil {
		return err
	}
	if err := s.users.ReplaceRecovery(ctx, p.UserID, p.SessionID, req.CurrentAuthKey, wrap, req.RecoveryProof); err != nil {
		// not_authorized, as a password change's: the session is fine, the
		// proof offered for this one operation is not.
		return fromSecret(err, CodeNotAuthorized, "the current password is wrong", "replacing the recovery code failed")
	}
	return nil
}

// StepUp proves the caller's session's own person again, with their auth
// key, and refreshes that session's step-up time, and no other's.
func (s *Service) StepUp(ctx context.Context, p Principal, req StepUpRequest) (StepUp, error) {
	if err := s.personalSecrets(p); err != nil {
		return StepUp{}, err
	}
	at, err := s.users.StepUp(ctx, p.UserID, p.SessionID, req.AuthKey)
	if err != nil {
		return StepUp{}, fromSecret(err, CodeNotAuthorized, "the password is wrong", "stepping up failed")
	}
	return StepUp{AuthenticatedAt: at.Unix()}, nil
}

// SessionAddress is the address the session's person signs in with, as it is
// stored (folded as auth.NormalizeEmail folds it), read now: what the
// transport's sign-in limits key an account's attempts by, so that a session's
// ceremonies that check a secret (password/begin, recovery, stepup) spend the
// same budget as a sign-in that types the address. Empty for a key, which has
// no person and reaches none of them.
func (s *Service) SessionAddress(ctx context.Context, p Principal) (string, error) {
	if !p.IsSession() {
		return "", nil
	}
	user, err := s.users.Get(ctx, p.UserID)
	if err != nil {
		return "", fromUsers(err, "reading the account failed")
	}
	return user.Email, nil
}

// UpgradeLogin is the upgrade's one check of a password in clear, for a
// person who signed up before the key scheme: it answers a ticket to enrol
// with, never a session. An enrolled person's password, like every other
// way of failing, is answered as a wrong one. It exists in the release that
// brings the key scheme only.
func (s *Service) UpgradeLogin(ctx context.Context, req UpgradeLoginRequest) (UpgradeTicket, error) {
	if err := s.passwordsInUse(); err != nil {
		return UpgradeTicket{}, err
	}
	ticket, err := s.users.LegacySignIn(ctx, req.Email, req.Password)
	if err != nil {
		return UpgradeTicket{}, fromSecret(err, CodeUnauthorized, "email or password is wrong", "signing in failed")
	}
	return UpgradeTicket{
		Ticket: ticket.Ticket, SealID: ticket.SealID, Salt: b64(ticket.Salt), KDF: presentKDF(ticket.KDF),
	}, nil
}

// UpgradeEnrol enrols the person the upgrade's ticket names: from then on the
// server refuses their password in clear. Their other sessions end, and this
// browser is signed in.
func (s *Service) UpgradeEnrol(ctx context.Context, req UpgradeEnrolRequest, userAgent string) (Session, error) {
	if err := s.passwordsInUse(); err != nil {
		return Session{}, err
	}
	in, err := enrolment(req.Enrolment)
	if err != nil {
		return Session{}, err
	}
	token, session, user, err := s.users.Enrol(ctx, req.Ticket, in, userAgent)
	if err != nil {
		return Session{}, fromUsers(err, "enrolling failed")
	}
	return presentSession(token, session, user), nil
}

// OpenReset checks a reset invitation and answers what its new password is
// derived under. A link that is not valid, and one that would take the last
// reader of a team mailbox, are refused as the reset itself would refuse
// them, before anyone types a password.
func (s *Service) OpenReset(ctx context.Context, req ResetOpenRequest) (ResetOpen, error) {
	if err := s.passwordsInUse(); err != nil {
		return ResetOpen{}, err
	}
	opened, err := s.users.OpenReset(ctx, req.Reset, req.Email)
	if err != nil {
		return ResetOpen{}, fromUsers(err, "opening the reset link failed")
	}
	return ResetOpen{Salt: b64(opened.Salt), KDF: presentKDF(opened.KDF), SealID: opened.SealID}, nil
}

// CompleteReset redeems a reset invitation: the person gets a new password,
// recovery code and account key, every grant sealed to their old key goes,
// every session of theirs ends, and this browser is signed in.
func (s *Service) CompleteReset(ctx context.Context, req ResetRequest, userAgent string) (Session, error) {
	if err := s.passwordsInUse(); err != nil {
		return Session{}, err
	}
	in, err := enrolment(req.Enrolment)
	if err != nil {
		return Session{}, err
	}
	token, session, user, err := s.users.CompleteReset(ctx, req.Reset, req.Email, in, userAgent)
	if err != nil {
		return Session{}, fromUsers(err, "resetting the password failed")
	}
	return presentSession(token, session, user), nil
}

// personalSecrets guards a route about the caller's own secrets: a person
// signed in, on a daemon where people have passwords here.
func (s *Service) personalSecrets(p Principal) error {
	if err := s.passwordsInUse(); err != nil {
		return err
	}
	return requireSession(p)
}

// Me describes the signed-in caller and the session they are using.
func (s *Service) Me(ctx context.Context, p Principal) (Me, error) {
	if err := requireSession(p); err != nil {
		return Me{}, err
	}
	user, err := s.users.Get(ctx, p.UserID)
	if err != nil {
		return Me{}, fromUsers(err, "reading the account failed")
	}
	session, err := s.users.Session(ctx, p.SessionID)
	if err != nil {
		return Me{}, fromUsers(err, "reading the session failed")
	}
	return Me{
		User: presentUser(user),
		Session: SessionInfo{
			ID: session.ID, CreatedAt: session.CreatedAt.Unix(), ExpiresAt: session.ExpiresAt.Unix(),
			AuthenticatedAt: unixOrZero(session.AuthenticatedAt),
		},
	}, nil
}

// SignOut ends the caller's session, or with everywhere every session the
// caller has on any device.
func (s *Service) SignOut(ctx context.Context, p Principal, req SignOutRequest) error {
	if err := requireSession(p); err != nil {
		return err
	}
	var err error
	if req.Everywhere {
		err = s.users.EndAllSessions(ctx, p.UserID)
	} else {
		err = s.users.EndSession(ctx, p.SessionID)
	}
	if err != nil {
		return E(CodeInternal, "signing out failed", err)
	}
	return nil
}

// UpdateProfile changes what the console calls the caller.
func (s *Service) UpdateProfile(ctx context.Context, p Principal, req ProfileRequest) (User, error) {
	if err := requireSession(p); err != nil {
		return User{}, err
	}
	user, err := s.users.SetName(ctx, p.UserID, req.Name)
	if err != nil {
		return User{}, fromUsers(err, "saving the profile failed")
	}
	return presentUser(user), nil
}

// CreateInvite invites a person by address and returns the link to send them.
//
// Owners and unrestricted instance admin keys may invite, and may invite
// either role. Members may not invite at all: a member who could mint accounts
// would be an administrator with a different name. Nor may a key restricted to
// some accounts, which was handed to one integration, not to whoever runs the
// instance. A team invite, which any person may make into a team of their
// own, signs a new person up only when an instance owner or the operator made
// it (auth.SignUp); anyone else's adds an existing account to the team.
// Where people sign in only through an extension, nobody invites anyone.
func (s *Service) CreateInvite(ctx context.Context, p Principal, req InviteRequest) (Invite, error) {
	if err := s.passwordsInUse(); err != nil {
		return Invite{}, err
	}
	switch {
	case p.IsSession() && p.UserRole == auth.RoleOwner:
	case p.IsInstance() && len(p.AccountIDs) == 0:
		if err := s.authorize(p, auth.ScopeAdmin); err != nil {
			return Invite{}, err
		}
	default:
		return Invite{}, E(CodeNotAuthorized, "inviting people needs an owner or an instance admin key", nil)
	}
	if s.publicURL == "" {
		// An invite is a link, and a link needs an origin that is not
		// guessed from whichever Host header this request arrived with.
		return Invite{}, E(CodeConflict, "set MAIL_PUBLIC_URL first: invite links are built from it", nil)
	}
	role := auth.RoleMember
	if req.Role != "" {
		parsed, err := auth.ParseRole(req.Role)
		if err != nil {
			return Invite{}, E(CodeBadRequest, "role must be owner or member", err)
		}
		role = parsed
	}
	code, invite, err := s.users.CreateInvite(ctx, auth.NewInvite{Email: req.Email, Role: role, CreatedBy: p.Actor()})
	if err != nil {
		return Invite{}, fromUsers(err, "creating the invite failed")
	}
	return Invite{
		Email: invite.Email, Role: string(invite.Role),
		URL:       auth.InviteLink(s.publicURL, code, invite.Email),
		ExpiresAt: invite.ExpiresAt.Unix(),
	}, nil
}

// fromSecret maps a failure of a ceremony that proves a secret: a secret
// that is not right is code with message, never saying which part was wrong;
// anything else is fromUsers'.
func fromSecret(err error, code Code, message, what string) error {
	if errors.Is(err, auth.ErrBadCredentials) {
		return E(code, message, err)
	}
	return fromUsers(err, what)
}

// fromUsers maps a failure from the user repository onto the transport
// vocabulary.
func fromUsers(err error, what string) error {
	var blocked *auth.BlockedError
	switch {
	case errors.Is(err, auth.ErrInviteInvalid):
		return E(CodeNotAuthorized,
			"that invite is not valid: it may have expired, been used, or be meant for another address", err)
	case errors.Is(err, auth.ErrInviteJoinsOnly):
		return E(CodeNotAuthorized, "that invite adds someone who already has an account here to a team: "+
			"sign in with that address and accept it. A new account needs an invitation from the server's owner", err)
	case errors.Is(err, auth.ErrResetInvalid):
		return E(CodeNotAuthorized,
			"that reset link is not valid: it may have expired, been used, or be meant for another address", err)
	case errors.As(err, &blocked):
		return E(CodeConflict, "this reset would leave a team mailbox nobody can read: "+
			"have someone else given read on it first, or ask the operator for a reset with force", err)
	case errors.Is(err, auth.ErrSealIDNotOpened):
		return E(CodeConflict, "open the invitation again and bind the account key to the seal id it answers", err)
	case errors.Is(err, auth.ErrTicketInvalid):
		return E(CodeNotAuthorized, "that step is not valid any more: it was used, has expired, or belongs to "+
			"another sign-in; start again", err)
	case errors.Is(err, auth.ErrStepUpNeeded):
		return E(CodeNotAuthorized, "this needs your password again: step up first", err)
	case errors.Is(err, auth.ErrStepUpRefused):
		return E(CodeNotAuthorized, "the step-up does not prove this session's person", err)
	case errors.Is(err, auth.ErrBadCredentials):
		return E(CodeNotAuthorized, "the account cannot do that", err)
	case errors.Is(err, auth.ErrKDFNotCurrent):
		return E(CodeConflict, "derive again under the salt and parameters the server answers now", err)
	case errors.Is(err, auth.ErrMalformedSecret):
		return E(CodeBadRequest, "an auth key and a recovery proof are base64url of 32 bytes", err)
	case errors.Is(err, auth.ErrInvalidPublicKey), errors.Is(err, auth.ErrInvalidWrap):
		return errBadKeyMaterial
	case errors.Is(err, auth.ErrEmailTaken):
		return E(CodeConflict, "that address already has an account; sign in instead", err)
	case errors.Is(err, auth.ErrInvalidEmail):
		return E(CodeBadRequest, "that is not a valid email address", err)
	case errors.Is(err, auth.ErrInvalidName):
		return Ef(CodeBadRequest, err, "a name is at most %d characters and has no control characters", auth.MaxNameLength)
	case errors.Is(err, auth.ErrUserNotFound), errors.Is(err, auth.ErrInvalidSession):
		// The session authenticated a moment ago; if its user or its row is
		// gone now, the honest answer is the one a revoked session gets.
		return E(CodeUnauthorized, "the session has ended; sign in again", err)
	case errors.Is(err, auth.ErrHashBusy), errors.Is(err, context.DeadlineExceeded):
		// The queue for the two hashing slots was full, or waiting in it
		// outlasted the request.
		return Retryable("the server is busy checking other passwords; try again shortly", hashWaitRetry, err)
	default:
		// A sign-up with a team invite joins the team, which may refuse:
		// a workspace managed elsewhere, one that is gone.
		return fromWorkspace(err, what)
	}
}

// accountWrapLen and publicKeyLen are the decoded sizes of an account wrap
// and an account public key.
const (
	accountWrapLen = 61
	publicKeyLen   = 32
)

// strictB64 is base64url without padding that refuses non-zero trailing bits;
// keyBytes re-encodes as well, so each value has exactly one spelling.
var strictB64 = base64.RawURLEncoding.Strict()

// keyBytes decodes a public key or a wrap of n bytes.
func keyBytes(s string, n int) ([]byte, error) {
	b, err := strictB64.DecodeString(s)
	if err != nil || len(b) != n || strictB64.EncodeToString(b) != s {
		return nil, errBadKeyMaterial
	}
	return b, nil
}

// keysBytes decodes several values of n bytes each.
func keysBytes(n int, values ...string) ([][]byte, error) {
	out := make([][]byte, len(values))
	for i, v := range values {
		b, err := keyBytes(v, n)
		if err != nil {
			return nil, err
		}
		out[i] = b
	}
	return out, nil
}

// enrolment decodes what a browser sent to enrol.
func enrolment(e Enrolment) (auth.Enrolment, error) {
	pub, err := keyBytes(e.PublicKey, publicKeyLen)
	if err != nil {
		return auth.Enrolment{}, err
	}
	wraps, err := keysBytes(accountWrapLen, e.PasswordWrap, e.RecoveryWrap)
	if err != nil {
		return auth.Enrolment{}, err
	}
	return auth.Enrolment{
		AuthKey: e.AuthKey, KDF: kdfOf(e.KDF), PublicKey: pub, PasswordWrap: wraps[0], RecoveryWrap: wraps[1],
		RecoveryProof: e.RecoveryProof,
	}, nil
}

// kdfOf is the parameters a browser names, or none for another algorithm:
// which is never the server's default, and is refused as parameters that
// are not.
func kdfOf(k KDF) auth.KDF {
	if k.Alg != auth.KDFAlg {
		return auth.KDF{}
	}
	return auth.KDF{M: k.M, T: k.T, P: k.P}
}

func presentKDF(k auth.KDF) KDF { return KDF{Alg: auth.KDFAlg, M: k.M, T: k.T, P: k.P} }

// b64 is a value as the ceremonies carry it: base64url without padding.
func b64(b []byte) string { return strictB64.EncodeToString(b) }

func presentUser(u auth.User) User {
	out := User{
		ID: u.ID, Email: u.Email, Name: u.Name, Role: string(u.Role), CreatedAt: u.CreatedAt.Unix(),
		HasPassword: u.HasPassword, SealID: u.SealID,
	}
	if len(u.PublicKey) > 0 {
		out.PublicKey = b64(u.PublicKey)
	}
	return out
}

func presentSession(token string, s auth.Session, u auth.User) Session {
	return Session{
		Token: token, ExpiresAt: s.ExpiresAt.Unix(), AuthenticatedAt: unixOrZero(s.AuthenticatedAt), User: presentUser(u),
	}
}
