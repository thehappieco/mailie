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

	"github.com/thehappieco/mailie/internal/keyscheme"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Users are the people who sign in to the web console.
//
// Which mailboxes a person sees is a matter of workspaces and grants
// (internal/workspace), decided in internal/service; this file keeps the
// people, their passwords, their sessions and their invites. Creating a
// person creates their personal workspace in the same transaction, through
// the workspace source.
//
// Nobody signs up without an invite. There is no public registration, no email
// verification and no reset by email: an invite is printed by the CLI or made
// in the console, it names the one address it is for, and it works once. The
// password never reaches the server: the person's browser derives an auth key
// from it, which the server keeps as a hash, and wraps the person's account
// key with the rest (accountkeys.go, docs/key-scheme.md). A forgotten
// password is the recovery code's to replace, and with both lost, the
// operator's reset invitation (CreateReset) gives the person a new password
// and a new account key.
//
// The one other way in is an identity provider an extension of the daemon
// trusts (identities.go): a person who arrives that way has no password until
// the operator gives them a reset invitation.

// Role is a person's role on the instance: the self-hosted server's own
// administration, apart from any workspace.
type Role string

const (
	// RoleOwner administers the instance: invites people to it, and
	// disables and deletes them. The first one comes from an invite the
	// operator makes with the owner role (`user invite --bootstrap`).
	RoleOwner Role = "owner"
	// RoleMember is everybody else.
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

// User is a person who can sign in. No hash or verifier leaves this package.
type User struct {
	ID       string
	Email    string
	Name     string
	Role     Role
	Disabled bool
	// HasPassword is false for a person who signs in only through an
	// identity provider (SignInExternal) and has never been given a
	// password: no password signs them in, and none can be changed. It is
	// true for a person enrolled in the key scheme (Enrolled) and for one
	// whose old password the server still checks, once, for the upgrade.
	HasPassword bool
	// Enrolled is a person enrolled in the key scheme (docs/key-scheme.md
	// section 12): their password never reaches the server.
	Enrolled bool
	// SealID is the UUIDv4 every wrap and grant binds the person by: drawn
	// once, never changed.
	SealID string
	// PublicKey is the person's account public key, 32 bytes, written once
	// at their own enrolment; nil before.
	PublicKey []byte
	// PasswordChangedAt is zero for a person without a password.
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
	// ErrInvalidWorkspaceRole is a team invite's role other than owner,
	// admin or member.
	ErrInvalidWorkspaceRole = errors.New("auth: a role in a team is owner, admin or member")
)

// Users is the repository for people, their sessions and invites.
type Users struct {
	store      *store.Store
	now        func() time.Time
	source     workspace.Source
	workspaces *workspace.Repository
	// saltKey is K_salt: the key of the salt every address is answered
	// (docs/key-scheme.md section 5.3).
	saltKey []byte
}

// NewUsers builds the repository, with the local workspace source.
func NewUsers(s *store.Store) *Users { return NewUsersWithClock(s, s.Now) }

// NewUsersWithClock builds the repository with an injected clock, for tests.
func NewUsersWithClock(s *store.Store, now func() time.Time) *Users {
	key := make([]byte, keyscheme.KeyLen)
	//nolint:errcheck // crypto/rand.Read never returns an error
	_, _ = rand.Read(key)
	u := &Users{store: s, now: now, saltKey: key}
	return u.WithWorkspaceSource(nil)
}

// WithSaltKey sets the key of the salts addresses are answered: the daemon's
// is the database's salt key (store.KDFSaltKey), so every address is answered
// the same salt across restarts. Without it, a repository has a random one of
// its own, which is what a test or a command that never answers a salt needs.
// The key is copied.
func (u *Users) WithSaltKey(key []byte) *Users {
	u.saltKey = append([]byte(nil), key...)
	return u
}

// WithWorkspaceSource sets where workspaces come from: what creating a person
// creates for them, and whether a team invite may be made or redeemed here.
// nil is the local source.
func (u *Users) WithWorkspaceSource(source workspace.Source) *Users {
	if source == nil {
		source = workspace.Local()
	}
	u.source = source
	u.workspaces = workspace.NewRepository(u.store, source).WithClock(u.now)
	return u
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

// userColumns are a person as User holds them. Whether they have a password
// is read from the hash and the enrolment, never the hash itself.
const userColumns = `id, email, name, role, status, (password_hash <> '' OR zk_enrolled_at <> 0), password_changed_at,
	created_at, updated_at, zk_enrolled_at <> 0, seal_id, public_key`

// userFields are where userColumns scan into; status is the stored one.
func userFields(user *User, status *string) []any {
	return []any{&user.ID, &user.Email, &user.Name, &user.Role, status, &user.HasPassword,
		unixScanner{&user.PasswordChangedAt}, unixScanner{&user.CreatedAt}, unixScanner{&user.UpdatedAt},
		&user.Enrolled, &user.SealID, &user.PublicKey}
}

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

// Count reports how many users exist.
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

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var user User
	var status string
	err := row.Scan(userFields(&user, &status)...)
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
