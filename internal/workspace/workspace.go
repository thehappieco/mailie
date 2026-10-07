// Package workspace keeps who belongs where: the workspaces mailboxes belong
// to, the people who are members of them, and the grants that let a member
// use a mailbox (docs/workspaces.md).
//
// Every mailbox belongs to its workspace. Using one takes active membership
// in its workspace and a grant on it: read, act and send each open one use.
// Owners and admins of a workspace manage every mailbox in it by their role —
// its card, re-authorizing it, who holds what on it — and read none of them
// by being one; manage is stored only for members, for whom it means the
// card and re-authorizing.
//
// This package holds the data and the rules the data must never break, the
// protections: a team keeps an active owner, a team mailbox someone can read
// keeps a reader, and nobody else ever joins a personal workspace or the
// operator's. Each is checked inside the transaction that would break it. Who
// may ask for a change is not decided here: that is authorization, and lives
// in internal/service, which hands every write a Check to run first in the
// same transaction, so the caller's authority is re-read where it is used.
package workspace

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/thehappieco/mailie/internal/store"
)

// OperatorID is the operator workspace's fixed id. The operator workspace has
// no members: instance keys and the command line act on its mailboxes, and no
// person ever gets operator powers through it.
const OperatorID = "wsp_operator"

// Kind is what a workspace is.
type Kind string

const (
	// KindPersonal is a person's own workspace: exactly that person, as its
	// owner, and nobody else, ever.
	KindPersonal Kind = "personal"
	// KindTeam is a workspace people share, with at least one active owner.
	KindTeam Kind = "team"
	// KindOperator is the one workspace with no members.
	KindOperator Kind = "operator"
)

// Role is a member's role in a workspace.
type Role string

const (
	// RoleOwner administers a team and everyone in it.
	RoleOwner Role = "owner"
	// RoleAdmin administers a team's members, but not its owners or admins.
	RoleAdmin Role = "admin"
	// RoleMember belongs to a team.
	RoleMember Role = "member"
)

// ParseRole converts a string, rejecting anything unknown.
func ParseRole(s string) (Role, error) {
	switch Role(s) {
	case RoleOwner, RoleAdmin, RoleMember:
		return Role(s), nil
	}
	return "", ErrInvalidRole
}

// Status is whether a membership counts.
type Status string

const (
	// StatusActive is a membership that counts, while its person is active
	// on the instance.
	StatusActive Status = "active"
	// StatusDisabled is a membership kept listed, with no access: disabling
	// it deleted its grants, and enabling it again restores none.
	StatusDisabled Status = "disabled"
)

// ParseStatus converts a string, rejecting anything unknown.
func ParseStatus(s string) (Status, error) {
	switch Status(s) {
	case StatusActive, StatusDisabled:
		return Status(s), nil
	}
	return "", ErrInvalidStatus
}

// MaxNameLength bounds a team's name, in characters.
const MaxNameLength = 80

// Workspace is one workspace.
type Workspace struct {
	ID string
	// Kind is personal, team or operator.
	Kind Kind
	// Source is SourceLocal or SourcePlatform: where the workspace and its
	// memberships come from. A platform workspace is never changed here.
	Source string
	// Name is a team's; empty for the others, which a client names itself.
	Name string
	// PersonID is the person of a personal workspace.
	PersonID  string
	CreatedAt time.Time
	UpdatedAt time.Time

	// Role and Status are the caller's own membership, in ForPerson.
	Role   Role
	Status Status
	// Members and Mailboxes are counts, in All.
	Members   int
	Mailboxes int
}

// Errors.
var (
	// ErrNotFound is a workspace nobody knows.
	ErrNotFound = errors.New("workspace: no such workspace")
	// ErrNoMailbox is a mailbox nobody knows.
	ErrNoMailbox = errors.New("workspace: no such mailbox")
	// ErrNotMember is a person who is not an active member of the
	// workspace: no membership, a disabled one, or a person disabled on the
	// instance.
	ErrNotMember = errors.New("workspace: that person is not an active member of the workspace")
	// ErrAlreadyMember is a person who already has a membership there,
	// active or not.
	ErrAlreadyMember = errors.New("workspace: that person is already a member of the workspace")
	// ErrNoSuchPerson is a person who does not exist or is disabled.
	ErrNoSuchPerson = errors.New("workspace: no such active person")
	// ErrPersonal is a member, grant or invite operation on a personal
	// workspace: its person is its only member, and nothing of it changes.
	ErrPersonal = errors.New("workspace: a personal workspace has its person as its only member")
	// ErrOperator is a member, grant or invite operation on the operator
	// workspace, which has none.
	ErrOperator = errors.New("workspace: the operator workspace has no members, grants or invites")
	// ErrManagedElsewhere is a local change to workspaces whose source is
	// the platform: they mirror what the platform sends.
	ErrManagedElsewhere = errors.New("workspace: workspaces are managed in the account console")
	// ErrLastOwner is a change that would leave a team without an active
	// owner.
	ErrLastOwner = errors.New("workspace: that is the last active owner of the team")
	// ErrLastReader is a change that would leave a team mailbox someone can
	// read with nobody who can: revoking the read of its last reader,
	// disabling or removing their membership, or closing their account.
	// Keys never count as readers, and neither does a role.
	ErrLastReader = errors.New("workspace: that is the last person who can read a mailbox of the workspace")
	// ErrManageByRole is manage stored for an owner or an admin, who manage
	// every mailbox of their workspace by their role.
	ErrManageByRole = errors.New("workspace: owners and admins manage every mailbox of their workspace by their role")
	// ErrInvalidName is a team name that is empty, too long or carries
	// control characters.
	ErrInvalidName = errors.New("workspace: a team's name is 1 to 80 characters with no control characters")
	// ErrInvalidRole is a role other than owner, admin or member.
	ErrInvalidRole = errors.New("workspace: a role is owner, admin or member")
	// ErrInvalidStatus is a status other than active or disabled.
	ErrInvalidStatus = errors.New("workspace: a status is active or disabled")
	// ErrNoGrant is a person with no grant on the mailbox.
	ErrNoGrant = errors.New("workspace: no grant")
	// ErrNoFlags is a grant with every flag off; revoking is how a grant
	// goes.
	ErrNoFlags = errors.New("workspace: a grant needs at least one flag")
	// ErrActWithoutRead is act without read: an action names messages the
	// actor reads.
	ErrActWithoutRead = errors.New("workspace: act needs read")
	// ErrHoldsMailboxes is a workspace deleted while a mailbox is still in
	// it: the mailboxes go first, in the same transaction.
	ErrHoldsMailboxes = errors.New("workspace: the workspace still holds mailboxes")
)

// Check is run first inside a write's transaction: the service's re-check of
// the caller's authority, ordered against every other write by the database's
// one writer. A non-nil error rolls the write back and is returned as is.
type Check func(tx *sql.Tx) error

// Repository reads and writes workspaces, memberships and grants.
type Repository struct {
	store  *store.Store
	source Source
	now    func() time.Time
}

// NewRepository builds the repository. A nil source is the local one.
func NewRepository(s *store.Store, source Source) *Repository {
	if source == nil {
		source = Local()
	}
	return &Repository{store: s, source: source, now: s.Now}
}

// WithClock overrides the clock, for tests.
func (r *Repository) WithClock(now func() time.Time) *Repository {
	r.now = now
	return r
}

// Source is where this repository's workspaces come from.
func (r *Repository) Source() Source { return r.source }

// NormalizeName trims a team's name and checks it.
func NormalizeName(s string) (string, error) {
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	if n == 0 || n > MaxNameLength || !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
		return "", ErrInvalidName
	}
	return s, nil
}

// newID returns a local workspace id: "wsp_" and 16 hex characters.
func newID() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("workspace: read random: %w", err)
	}
	return "wsp_" + hex.EncodeToString(raw), nil
}

const workspaceColumns = `w.id, w.kind, w.source, w.name, coalesce(w.person_id, ''), w.created_at, w.updated_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanWorkspace(row rowScanner, extra ...any) (Workspace, error) {
	var (
		w                Workspace
		created, updated int64
	)
	dest := append([]any{&w.ID, &w.Kind, &w.Source, &w.Name, &w.PersonID, &created, &updated}, extra...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Workspace{}, ErrNotFound
		}
		return Workspace{}, fmt.Errorf("workspace: scan: %w", err)
	}
	w.CreatedAt, w.UpdatedAt = unix(created), unix(updated)
	return w, nil
}

func unix(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Get reads one workspace.
func (r *Repository) Get(ctx context.Context, id string) (Workspace, error) {
	return getOn(ctx, r.store.Reader(), id)
}

// GetTx reads one workspace inside the caller's transaction.
func GetTx(ctx context.Context, tx *sql.Tx, id string) (Workspace, error) { return getOn(ctx, tx, id) }

func getOn(ctx context.Context, q querier, id string) (Workspace, error) {
	return scanWorkspace(q.QueryRowContext(ctx, `SELECT `+workspaceColumns+` FROM workspaces w WHERE w.id = ?`, id))
}

// PersonalOf reads a person's personal workspace.
func (r *Repository) PersonalOf(ctx context.Context, userID string) (Workspace, error) {
	return personalOn(ctx, r.store.Reader(), userID)
}

// PersonalOfTx reads a person's personal workspace inside the caller's
// transaction.
func PersonalOfTx(ctx context.Context, tx *sql.Tx, userID string) (Workspace, error) {
	return personalOn(ctx, tx, userID)
}

func personalOn(ctx context.Context, q querier, userID string) (Workspace, error) {
	return scanWorkspace(q.QueryRowContext(ctx,
		`SELECT `+workspaceColumns+` FROM workspaces w WHERE w.person_id = ? AND w.kind = 'personal'`, userID))
}

// ForPerson lists the workspaces a person is an active member of, with their
// role there: their personal workspace first, then teams, oldest first (by
// name when they are as old). A
// person disabled on the instance is an active member of nothing.
func (r *Repository) ForPerson(ctx context.Context, userID string) ([]Workspace, error) {
	rows, err := r.store.Reader().QueryContext(ctx, `SELECT `+workspaceColumns+`, m.role, m.status
		  FROM workspaces w
		  JOIN workspace_members m ON m.workspace_id = w.id
		  JOIN users u ON u.id = m.user_id
		 WHERE m.user_id = ? AND m.status = 'active' AND u.status = 'active'
		 ORDER BY w.kind <> 'personal', w.created_at, w.name, w.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("workspace: list a person's: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	var out []Workspace
	for rows.Next() {
		var (
			role   Role
			status Status
		)
		w, err := scanWorkspace(rows, &role, &status)
		if err != nil {
			return nil, err
		}
		w.Role, w.Status = role, status
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: list a person's: %w", err)
	}
	return out, nil
}

// All lists every workspace, with how many members and mailboxes each has:
// the operator's view. The operator workspace first, then the others oldest
// first.
func (r *Repository) All(ctx context.Context) ([]Workspace, error) {
	rows, err := r.store.Reader().QueryContext(ctx, `SELECT `+workspaceColumns+`,
		  (SELECT count(*) FROM workspace_members m WHERE m.workspace_id = w.id),
		  (SELECT count(*) FROM accounts a WHERE a.workspace_id = w.id)
		  FROM workspaces w ORDER BY w.kind <> 'operator', w.created_at, w.name, w.id`)
	if err != nil {
		return nil, fmt.Errorf("workspace: list: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	var out []Workspace
	for rows.Next() {
		var members, mailboxes int
		w, err := scanWorkspace(rows, &members, &mailboxes)
		if err != nil {
			return nil, err
		}
		w.Members, w.Mailboxes = members, mailboxes
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: list: %w", err)
	}
	return out, nil
}

// changeable refuses a change to the memberships or the name of w: a personal
// or the operator workspace, or one the platform sends, or any while the
// configured source is the platform.
func (r *Repository) changeable(w Workspace) error {
	switch w.Kind {
	case KindPersonal:
		return ErrPersonal
	case KindOperator:
		return ErrOperator
	}
	if w.Source != SourceLocal {
		return ErrManagedElsewhere
	}
	return r.source.Changeable()
}

// CreateTeam creates a team named name with owner as its first member, an
// active owner. The owner must be an active person.
func (r *Repository) CreateTeam(ctx context.Context, name, owner string, check Check) (Workspace, error) {
	if err := r.source.Changeable(); err != nil {
		return Workspace{}, err
	}
	name, err := NormalizeName(name)
	if err != nil {
		return Workspace{}, err
	}
	id, err := newID()
	if err != nil {
		return Workspace{}, err
	}
	now := r.now().UTC().Truncate(time.Second)
	w := Workspace{ID: id, Kind: KindTeam, Source: SourceLocal, Name: name, CreatedAt: now, UpdatedAt: now,
		Role: RoleOwner, Status: StatusActive}
	err = r.store.Write(ctx, func(tx *sql.Tx) error {
		if err := runCheck(tx, check); err != nil {
			return err
		}
		if err := requireActivePersonTx(ctx, tx, owner); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO workspaces(id, kind, source, name, created_at, updated_at) VALUES (?, 'team', 'local', ?, ?, ?)`,
			w.ID, w.Name, now.Unix(), now.Unix()); err != nil {
			return fmt.Errorf("workspace: create team: %w", err)
		}
		return insertMemberTx(ctx, tx, w.ID, owner, RoleOwner, now)
	})
	if err != nil {
		return Workspace{}, err
	}
	return w, nil
}

// Rename renames a team.
func (r *Repository) Rename(ctx context.Context, id, name string, check Check) (Workspace, error) {
	name, err := NormalizeName(name)
	if err != nil {
		return Workspace{}, err
	}
	now := r.now().Unix()
	var out Workspace
	err = r.store.Write(ctx, func(tx *sql.Tx) error {
		w, err := GetTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := r.changeable(w); err != nil {
			return err
		}
		if err := runCheck(tx, check); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET name = ?, updated_at = ? WHERE id = ?`, name, now, id); err != nil {
			return fmt.Errorf("workspace: rename: %w", err)
		}
		out, err = GetTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return Workspace{}, err
	}
	return out, nil
}

func runCheck(tx *sql.Tx, check Check) error {
	if check == nil {
		return nil
	}
	return check(tx)
}

func requireActivePersonTx(ctx context.Context, tx *sql.Tx, userID string) error {
	var status string
	err := tx.QueryRowContext(ctx, `SELECT status FROM users WHERE id = ?`, userID).Scan(&status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNoSuchPerson
	case err != nil:
		return fmt.Errorf("workspace: read the person: %w", err)
	case status != "active":
		return ErrNoSuchPerson
	}
	return nil
}
