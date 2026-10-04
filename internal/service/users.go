package service

import (
	"context"
	"errors"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
)

// User is a person, as the console shows them.
type User struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	CreatedAt int64  `json:"created_at"`
}

// Session is a new sign-in. The token is in this reply and nowhere else: only
// its hash is stored.
type Session struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
	User      User   `json:"user"`
}

// SessionInfo describes the session a request came in on.
type SessionInfo struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
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

// SignInRequest is the sign-in form.
type SignInRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// SignUpRequest is the form an invite link opens.
type SignUpRequest struct {
	Invite   string `json:"invite"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

// SignOutRequest ends the caller's session, or every session they have.
type SignOutRequest struct {
	Everywhere bool `json:"everywhere,omitempty"`
}

// PasswordRequest changes a password the caller still knows.
type PasswordRequest struct {
	Current string `json:"current"`
	Next    string `json:"next"`
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

// SignIn checks an address and a password and starts a session.
//
// A wrong password, an address with no account and a disabled account all get
// the same answer, after the same amount of work.
func (s *Service) SignIn(ctx context.Context, req SignInRequest, userAgent string) (Session, error) {
	token, session, user, err := s.users.SignIn(ctx, req.Email, req.Password, userAgent)
	switch {
	case errors.Is(err, auth.ErrBadCredentials):
		return Session{}, E(CodeUnauthorized, "email or password is wrong", err)
	case err != nil:
		return Session{}, fromUsers(err, "signing in failed")
	}
	return presentSession(token, session, user), nil
}

// SignUp redeems an invite: it creates the account and signs it in.
func (s *Service) SignUp(ctx context.Context, req SignUpRequest, userAgent string) (Session, error) {
	token, session, user, err := s.users.SignUp(ctx, auth.SignUpRequest{
		Invite: req.Invite, Email: req.Email, Name: req.Name, Password: req.Password, UserAgent: userAgent,
	})
	if err != nil {
		return Session{}, fromUsers(err, "creating the account failed")
	}
	return presentSession(token, session, user), nil
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

// ChangePassword replaces the caller's password, ends every session they
// have — this one included — and signs them in again with a new token.
func (s *Service) ChangePassword(ctx context.Context, p Principal, req PasswordRequest, userAgent string) (Session, error) {
	if err := requireSession(p); err != nil {
		return Session{}, err
	}
	token, session, err := s.users.ChangePassword(ctx, p.UserID, req.Current, req.Next, userAgent)
	switch {
	case errors.Is(err, auth.ErrBadCredentials):
		// not_authorized rather than unauthorized: the session is fine, the
		// proof offered for this one operation is not, and a console that
		// signed people out for a typo would be hostile.
		return Session{}, E(CodeNotAuthorized, "the current password is wrong", err)
	case err != nil:
		return Session{}, fromUsers(err, "changing the password failed")
	}
	user, err := s.users.Get(ctx, p.UserID)
	if err != nil {
		return Session{}, fromUsers(err, "reading the account failed")
	}
	return presentSession(token, session, user), nil
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
// instance.
func (s *Service) CreateInvite(ctx context.Context, p Principal, req InviteRequest) (Invite, error) {
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

// fromUsers maps a failure from the user repository onto the transport
// vocabulary.
func fromUsers(err error, what string) error {
	switch {
	case errors.Is(err, auth.ErrInviteInvalid):
		return E(CodeNotAuthorized,
			"that invite is not valid: it may have expired, been used, or be meant for another address", err)
	case errors.Is(err, auth.ErrEmailTaken):
		return E(CodeConflict, "that address already has an account; sign in instead", err)
	case errors.Is(err, auth.ErrInvalidEmail):
		return E(CodeBadRequest, "that is not a valid email address", err)
	case errors.Is(err, auth.ErrInvalidName):
		return Ef(CodeBadRequest, err, "a name is at most %d characters and has no control characters", auth.MaxNameLength)
	case errors.Is(err, auth.ErrPasswordTooShort):
		return Ef(CodeBadRequest, err, "a password needs at least %d characters", auth.MinPasswordLength)
	case errors.Is(err, auth.ErrPasswordTooLong):
		return Ef(CodeBadRequest, err, "a password may be at most %d bytes", auth.MaxPasswordBytes)
	case errors.Is(err, auth.ErrUserNotFound), errors.Is(err, auth.ErrInvalidSession):
		// The session authenticated a moment ago; if its user or its row is
		// gone now, the honest answer is the one a revoked session gets.
		return E(CodeUnauthorized, "the session has ended; sign in again", err)
	case errors.Is(err, auth.ErrHashBusy), errors.Is(err, context.DeadlineExceeded):
		// The queue for the two hashing slots was full, or waiting in it
		// outlasted the request.
		return Retryable("the server is busy checking other passwords; try again shortly", hashWaitRetry, err)
	default:
		return E(CodeInternal, what, err)
	}
}

func presentUser(u auth.User) User {
	return User{ID: u.ID, Email: u.Email, Name: u.Name, Role: string(u.Role), CreatedAt: u.CreatedAt.Unix()}
}

func presentSession(token string, s auth.Session, u auth.User) Session {
	return Session{Token: token, ExpiresAt: s.ExpiresAt.Unix(), User: presentUser(u)}
}
