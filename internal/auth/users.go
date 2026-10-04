package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/thehappieco/mailie/internal/store"
)

// Users are the people who sign in to the web console.
//
// A person owns the mailboxes they connect and sees nothing else, except that
// the owner role — the instance's administrator — also sees the accounts the
// CLI created, which belong to nobody. That rule is applied in
// internal/service; this file only keeps the people, their passwords and
// their sessions.
//
// Nobody signs up without an invite. There is no public registration, no email
// verification and no reset by email: an invite is printed by the CLI or made
// by an owner in the console, it names the one address it is for, and it
// works once.

// Role is what a person may do beyond their own mailboxes.
type Role string

const (
	// RoleOwner administers the instance: sees the accounts no user owns and
	// may invite people.
	RoleOwner Role = "owner"
	// RoleMember sees only the accounts they connected.
	RoleMember Role = "member"
)

// ParseRole converts a string, rejecting anything unknown.
func ParseRole(s string) (Role, error) {
	switch Role(s) {
	case RoleOwner, RoleMember:
		return Role(s), nil
	}
	return "", ErrInvalidRole
}

// Stored user statuses.
const (
	userActive   = "active"
	userDisabled = "disabled"
)

// MaxNameLength bounds a display name, in characters.
const MaxNameLength = 120

// maxEmailLength is RFC 5321's limit on a forward path.
const maxEmailLength = 254

// User is a person who can sign in. The password hash never leaves this
// package.
type User struct {
	ID                string
	Email             string
	Name              string
	Role              Role
	Disabled          bool
	PasswordChangedAt time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

var (
	// ErrBadCredentials is deliberately the same for a wrong password, an
	// address with no account and an account that has been disabled. Telling
	// them apart would be an oracle for which addresses have accounts.
	ErrBadCredentials = errors.New("auth: email or password is wrong")
	// ErrEmailTaken is an address that already has an account.
	ErrEmailTaken = errors.New("auth: that email already has an account")
	// ErrInvalidEmail is an address that does not parse as one bare address.
	ErrInvalidEmail = errors.New("auth: that is not a valid email address")
	// ErrInvalidName is a name that is too long or carries control characters.
	ErrInvalidName = errors.New("auth: a name is at most 120 characters and has no control characters")
	// ErrInvalidRole is a role other than owner or member.
	ErrInvalidRole = errors.New("auth: a role is owner or member")
)

// Users is the repository for people, their sessions and invites.
type Users struct {
	store *store.Store
	now   func() time.Time
}

// NewUsers builds the repository.
func NewUsers(s *store.Store) *Users { return &Users{store: s, now: s.Now} }

// NewUsersWithClock builds the repository with an injected clock, for tests.
func NewUsersWithClock(s *store.Store, now func() time.Time) *Users {
	return &Users{store: s, now: now}
}

// NormalizeEmail checks that s is one bare address and returns it lowercased.
//
// Bare means no display name and no angle brackets: "Ana <a@b.c>" parses, but
// it is not what anyone types into a sign-in form, and accepting it would give
// one account two spellings.
func NormalizeEmail(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxEmailLength {
		return "", ErrInvalidEmail
	}
	parsed, err := mail.ParseAddress(s)
	if err != nil || parsed.Name != "" || parsed.Address != s {
		return "", ErrInvalidEmail
	}
	return strings.ToLower(s), nil
}

// NormalizeName trims a display name and checks it.
func NormalizeName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > MaxNameLength || !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
		return "", ErrInvalidName
	}
	return s, nil
}

const userColumns = `id, email, name, role, status, password_changed_at, created_at, updated_at`

// Get reads one user.
func (u *Users) Get(ctx context.Context, id string) (User, error) {
	row := u.store.Reader().QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id)
	user, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	return user, err
}

// GetByEmail reads one user by address, compared without regard to case as
// sign-in compares it.
func (u *Users) GetByEmail(ctx context.Context, email string) (User, error) {
	row := u.store.Reader().QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE email = ?`, strings.TrimSpace(email))
	user, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	return user, err
}

// Count reports how many users exist. The first one is always an owner.
func (u *Users) Count(ctx context.Context) (int, error) {
	var n int
	if err := u.store.Reader().QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("auth: count users: %w", err)
	}
	return n, nil
}

// SetName changes a user's display name.
func (u *Users) SetName(ctx context.Context, id, name string) (User, error) {
	name, err := NormalizeName(name)
	if err != nil {
		return User{}, err
	}
	err = u.store.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE users SET name = ?, updated_at = ? WHERE id = ?`,
			name, u.now().Unix(), id)
		if err != nil {
			return fmt.Errorf("auth: rename user: %w", err)
		}
		return requireRow(res, ErrUserNotFound)
	})
	if err != nil {
		return User{}, err
	}
	return u.Get(ctx, id)
}

// SetDisabled switches a user off or back on. Switching off is Disable, forced:
// every session ends and every key the user holds is revoked, in the same
// transaction. Switching back on revives none of them.
func (u *Users) SetDisabled(ctx context.Context, id string, disabled bool) error {
	if disabled {
		_, err := u.Disable(ctx, id, true)
		return err
	}
	return u.store.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE users SET status = ?, updated_at = ? WHERE id = ?`,
			userActive, u.now().Unix(), id)
		if err != nil {
			return fmt.Errorf("auth: set user status: %w", err)
		}
		return requireRow(res, ErrUserNotFound)
	})
}

// SignIn checks an address and a password and starts a session.
//
// The shape is the security property, as in Keys.Authenticate: exactly one
// Argon2id derivation whether or not the address exists, and one error for
// every way of failing.
func (u *Users) SignIn(ctx context.Context, email, password, userAgent string) (string, Session, User, error) {
	var (
		user   User
		hash   string
		status string
	)
	err := u.store.Reader().QueryRowContext(ctx,
		`SELECT `+userColumns+`, password_hash FROM users WHERE email = ?`, strings.TrimSpace(email),
	).Scan(&user.ID, &user.Email, &user.Name, &user.Role, &status,
		unixScanner{&user.PasswordChangedAt}, unixScanner{&user.CreatedAt}, unixScanner{&user.UpdatedAt}, &hash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := verifyPassword(ctx, password, dummyPasswordHash); err != nil {
			return "", Session{}, User{}, err
		}
		return "", Session{}, User{}, ErrBadCredentials
	case err != nil:
		return "", Session{}, User{}, fmt.Errorf("auth: sign in: %w", err)
	}

	if status != userActive {
		// Compared against the dummy rather than the real hash, so a
		// disabled account cannot even be used to confirm its old password.
		if _, err := verifyPassword(ctx, password, dummyPasswordHash); err != nil {
			return "", Session{}, User{}, err
		}
		return "", Session{}, User{}, ErrBadCredentials
	}
	ok, err := verifyPassword(ctx, password, hash)
	if err != nil {
		return "", Session{}, User{}, err
	}
	if !ok {
		return "", Session{}, User{}, ErrBadCredentials
	}

	var token string
	var session Session
	err = u.store.Write(ctx, func(tx *sql.Tx) error {
		// Re-read under the write lock: the account may have been disabled
		// while the hash ran, and a session must not outlive that.
		var current string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM users WHERE id = ?`, user.ID).Scan(&current); err != nil {
			return fmt.Errorf("auth: sign in: %w", err)
		}
		if current != userActive {
			return ErrBadCredentials
		}
		var err error
		token, session, err = startSessionTx(ctx, tx, user.ID, userAgent, u.now())
		return err
	})
	if err != nil {
		return "", Session{}, User{}, err
	}
	return token, session, user, nil
}

// ChangePassword replaces a password that the caller proves they still know,
// ends every session the user has — the one asking included — and starts a
// new one.
//
// Everything goes, because the usual reason for changing a password is no
// longer trusting where the old one was typed; a session opened under it that
// survived the change would be that distrust ignored.
func (u *Users) ChangePassword(ctx context.Context, userID, current, next, userAgent string) (string, Session, error) {
	if err := CheckPassword(next); err != nil {
		return "", Session{}, err
	}
	var hash, status string
	err := u.store.Reader().QueryRowContext(ctx,
		`SELECT password_hash, status FROM users WHERE id = ?`, userID).Scan(&hash, &status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", Session{}, ErrUserNotFound
	case err != nil:
		return "", Session{}, fmt.Errorf("auth: change password: %w", err)
	}
	ok, err := verifyPassword(ctx, current, hash)
	if err != nil {
		return "", Session{}, err
	}
	if !ok || status != userActive {
		return "", Session{}, ErrBadCredentials
	}
	fresh, err := hashPassword(ctx, next)
	if err != nil {
		return "", Session{}, err
	}

	var token string
	var session Session
	err = u.store.Write(ctx, func(tx *sql.Tx) error {
		now := u.now()
		// The old hash is part of the condition: two changes racing each
		// other must not both succeed against the same proof.
		res, err := tx.ExecContext(ctx,
			`UPDATE users SET password_hash = ?, password_changed_at = ?, updated_at = ?
			  WHERE id = ? AND status = ? AND password_hash = ?`,
			fresh, now.Unix(), now.Unix(), userID, userActive, hash)
		if err != nil {
			return fmt.Errorf("auth: change password: %w", err)
		}
		if err := requireRow(res, ErrBadCredentials); err != nil {
			return err
		}
		if _, err := revokeSessionsTx(ctx, tx, userID, now.Unix()); err != nil {
			return err
		}
		token, session, err = startSessionTx(ctx, tx, userID, userAgent, now)
		return err
	})
	if err != nil {
		return "", Session{}, err
	}
	return token, session, nil
}

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var user User
	var status string
	err := row.Scan(&user.ID, &user.Email, &user.Name, &user.Role, &status,
		unixScanner{&user.PasswordChangedAt}, unixScanner{&user.CreatedAt}, unixScanner{&user.UpdatedAt})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, err
		}
		return User{}, fmt.Errorf("auth: scan user: %w", err)
	}
	user.Disabled = status != userActive
	return user, nil
}

// unixScanner reads a unix-seconds column into a time.Time.
type unixScanner struct{ t *time.Time }

func (s unixScanner) Scan(src any) error {
	v, ok := src.(int64)
	if !ok {
		return fmt.Errorf("auth: want a unix timestamp, got %T", src)
	}
	*s.t = unixOrZero(v)
	return nil
}

// execCount runs a statement and reports how many rows it touched.
func execCount(ctx context.Context, tx *sql.Tx, query string, args ...any) (int, error) {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("auth: rows affected: %w", err)
	}
	return int(n), nil
}

// requireRow turns "no row matched" into the caller's error.
func requireRow(res sql.Result, missing error) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("auth: rows affected: %w", err)
	}
	if n == 0 {
		return missing
	}
	return nil
}

// newID returns prefix + 16 hex characters: the shape account ids have, so a
// log line names every kind of row the same way.
func newID(prefix string) (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: read random: %w", err)
	}
	return prefix + hex.EncodeToString(raw), nil
}
