package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/thehappieco/mailie/internal/store"
)

// An invite is a one-time right to create an account for one address.
//
// The code is 32 random bytes and only its SHA-256 is stored, like a session
// token. It reaches the person inside a link whose fragment carries it —
// `#invite=…&email=…` — because a fragment is never sent to a server, so the
// code does not land in an access log, a proxy or a Referer header on the way.

// InviteTTL is how long an invite stays redeemable.
const InviteTTL = 7 * 24 * time.Hour

const inviteCodeBytes = 32

// ErrInviteInvalid is one answer for a code that does not exist, has expired,
// was already used, or is for another address. They are one error because
// the person holding the link can do the same thing about each: ask for a new
// one.
var ErrInviteInvalid = errors.New("auth: the invite is not valid, has been used, or has expired")

// Invite is an invite as its creator sees it. The code is not here: it exists
// once, in the link returned when the invite is made.
type Invite struct {
	Email     string
	Role      Role
	CreatedBy string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// NewInvite describes an invite to make.
type NewInvite struct {
	Email string
	Role  Role
	// CreatedBy is "usr_…", "key:<prefix>" or "cli", for the audit trail.
	CreatedBy string
}

// CreateInvite makes an invite and returns the only copy of its code.
//
// The invite keeps the role it was asked for. Whoever signs up first becomes
// an owner whatever their invite says — SignUp decides that inside the
// transaction that creates them, so an instance with only members, with nobody
// able to invite anyone else, cannot happen. Deciding it here instead would
// make every invite written before the first sign-up an owner's, including a
// colleague's the operator asked to be a member.
func (u *Users) CreateInvite(ctx context.Context, in NewInvite) (string, Invite, error) {
	email, err := NormalizeEmail(in.Email)
	if err != nil {
		return "", Invite{}, err
	}
	if _, err := ParseRole(string(in.Role)); err != nil {
		return "", Invite{}, err
	}
	raw := make([]byte, inviteCodeBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", Invite{}, fmt.Errorf("auth: read random: %w", err)
	}
	sum := sha256.Sum256(raw)
	now := u.now().UTC().Truncate(time.Second)
	out := Invite{Email: email, Role: in.Role, CreatedBy: in.CreatedBy, CreatedAt: now, ExpiresAt: now.Add(InviteTTL)}

	err = u.store.Write(ctx, func(tx *sql.Tx) error {
		var taken int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM users WHERE email = ?`, email).Scan(&taken); err != nil {
			return fmt.Errorf("auth: create invite: %w", err)
		}
		if taken > 0 {
			return ErrEmailTaken
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO invites(code_hash, email, role, created_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
			sum[:], out.Email, string(out.Role), out.CreatedBy, now.Unix(), out.ExpiresAt.Unix())
		if err != nil {
			return fmt.Errorf("auth: create invite: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", Invite{}, err
	}
	return base64.RawURLEncoding.EncodeToString(raw), out, nil
}

// InviteLink is the sign-up address for an invite: the console's origin, and
// the code and address in the fragment.
func InviteLink(publicURL, code, email string) string {
	return strings.TrimRight(publicURL, "/") + "/#invite=" + url.QueryEscape(code) + "&email=" + url.QueryEscape(email)
}

// SignUpRequest is what the sign-up form sends.
type SignUpRequest struct {
	Invite    string
	Email     string
	Name      string
	Password  string
	UserAgent string
}

// SignUp redeems an invite: it creates the account and starts its first
// session, in one transaction with marking the invite used, so a crash cannot
// leave an invite spent with nobody behind it or an account behind an invite
// that still works.
func (u *Users) SignUp(ctx context.Context, req SignUpRequest) (string, Session, User, error) {
	email, err := NormalizeEmail(req.Email)
	if err != nil {
		return "", Session{}, User{}, err
	}
	name, err := NormalizeName(req.Name)
	if err != nil {
		return "", Session{}, User{}, err
	}
	if err := CheckPassword(req.Password); err != nil {
		return "", Session{}, User{}, err
	}
	codeHash, ok := hashInviteCode(req.Invite)
	if !ok {
		return "", Session{}, User{}, ErrInviteInvalid
	}

	// Checked before the hash as well as inside the transaction: a bad code
	// or a taken address should not cost 64 MiB of Argon2id to refuse.
	if err := u.checkSignUp(ctx, codeHash, email); err != nil {
		return "", Session{}, User{}, err
	}
	hash, err := hashPassword(ctx, req.Password)
	if err != nil {
		return "", Session{}, User{}, err
	}
	userID, err := newID("usr_")
	if err != nil {
		return "", Session{}, User{}, err
	}

	now := u.now().UTC().Truncate(time.Second)
	user := User{ID: userID, Email: email, Name: name, PasswordChangedAt: now, CreatedAt: now, UpdatedAt: now}
	var token string
	var session Session
	err = u.store.Write(ctx, func(tx *sql.Tx) error {
		var invited string
		err := tx.QueryRowContext(ctx,
			`UPDATE invites SET used_at = ?, used_by = ?
			  WHERE code_hash = ? AND used_at = 0 AND expires_at > ?
			  RETURNING email, role`,
			now.Unix(), userID, codeHash, now.Unix()).Scan(&invited, &user.Role)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInviteInvalid
		}
		if err != nil {
			return fmt.Errorf("auth: redeem invite: %w", err)
		}
		// Returning here rolls the UPDATE back, so a mistyped address does
		// not spend the invite of the person it was really for.
		if !strings.EqualFold(invited, email) {
			return ErrInviteInvalid
		}
		var users int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&users); err != nil {
			return fmt.Errorf("auth: sign up: %w", err)
		}
		if users == 0 {
			user.Role = RoleOwner
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO users(id, email, name, password_hash, role, status, password_changed_at, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			user.ID, user.Email, user.Name, hash, string(user.Role), userActive, now.Unix(), now.Unix(), now.Unix())
		if store.IsUnique(err) {
			return ErrEmailTaken
		}
		if err != nil {
			return fmt.Errorf("auth: create user: %w", err)
		}
		token, session, err = startSessionTx(ctx, tx, user.ID, req.UserAgent, now)
		return err
	})
	if err != nil {
		return "", Session{}, User{}, err
	}
	return token, session, user, nil
}

// checkSignUp is the read-only half of SignUp's checks, run before the hash.
func (u *Users) checkSignUp(ctx context.Context, codeHash []byte, email string) error {
	var invited string
	err := u.store.Reader().QueryRowContext(ctx,
		`SELECT email FROM invites WHERE code_hash = ? AND used_at = 0 AND expires_at > ?`,
		codeHash, u.now().Unix()).Scan(&invited)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrInviteInvalid
	case err != nil:
		return fmt.Errorf("auth: check invite: %w", err)
	}
	if !strings.EqualFold(invited, email) {
		return ErrInviteInvalid
	}
	var taken int
	if err := u.store.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE email = ?`, email).Scan(&taken); err != nil {
		return fmt.Errorf("auth: check address: %w", err)
	}
	if taken > 0 {
		return ErrEmailTaken
	}
	return nil
}

func hashInviteCode(code string) ([]byte, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(code))
	if err != nil || len(raw) != inviteCodeBytes {
		return nil, false
	}
	sum := sha256.Sum256(raw)
	return sum[:], true
}
