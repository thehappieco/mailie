package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/thehappieco/mailie/internal/store"
)

// Member is a membership, with the person's address and name for listing.
type Member struct {
	WorkspaceID string
	UserID      string
	Email       string
	Name        string
	Role        Role
	Status      Status
	// PersonDisabled is the person switched off on the instance: a
	// membership of theirs counts for nothing until they are back.
	PersonDisabled bool
	// LastOwner is the only active owner of a team, who cannot be demoted,
	// disabled or removed (ErrLastOwner).
	LastOwner bool
	// LastReaderOf are the team mailboxes this member is the only reader of:
	// their read cannot be revoked, nor their membership disabled or removed,
	// until someone else reads them (ErrLastReader).
	LastReaderOf []string
	// SealID and PublicKey are what a grant to the person is sealed to and
	// bound by (docs/key-scheme.md sections 3.1 and 9.1): their seal id,
	// and their account public key, nil until they enrol.
	SealID    string
	PublicKey []byte
	JoinedAt  time.Time
	UpdatedAt time.Time
}

// Active reports whether the membership counts: active, of a person active
// on the instance.
func (m Member) Active() bool { return m.Status == StatusActive && !m.PersonDisabled }

const memberColumns = `m.workspace_id, m.user_id, u.email, u.name, m.role, m.status, u.status <> 'active',
	u.seal_id, u.public_key, m.created_at, m.updated_at`

func scanMember(row rowScanner) (Member, error) {
	var (
		m                Member
		created, updated int64
	)
	err := row.Scan(&m.WorkspaceID, &m.UserID, &m.Email, &m.Name, &m.Role, &m.Status, &m.PersonDisabled,
		&m.SealID, &m.PublicKey, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Member{}, ErrNotMember
		}
		return Member{}, fmt.Errorf("workspace: scan member: %w", err)
	}
	m.JoinedAt, m.UpdatedAt = unix(created), unix(updated)
	return m, nil
}

// Member reads one membership, active or not; ErrNotMember when there is
// none.
func (r *Repository) Member(ctx context.Context, workspaceID, userID string) (Member, error) {
	return memberOn(ctx, r.store.Reader(), workspaceID, userID)
}

// MemberTx reads one membership inside the caller's transaction: how a Check
// re-reads the caller's role where it is used.
func MemberTx(ctx context.Context, tx *sql.Tx, workspaceID, userID string) (Member, error) {
	return memberOn(ctx, tx, workspaceID, userID)
}

func memberOn(ctx context.Context, q querier, workspaceID, userID string) (Member, error) {
	m, err := scanMember(q.QueryRowContext(ctx, `SELECT `+memberColumns+`
		  FROM workspace_members m JOIN users u ON u.id = m.user_id
		 WHERE m.workspace_id = ? AND m.user_id = ?`, workspaceID, userID))
	if err != nil {
		return Member{}, err
	}
	if m.Role == RoleOwner && m.Active() {
		others, err := otherActiveOwners(ctx, q, workspaceID, userID)
		if err != nil {
			return Member{}, err
		}
		m.LastOwner = others == 0
	}
	if m.LastReaderOf, err = lastReaderOf(ctx, q, userID, lastReaders{workspaceID: workspaceID}); err != nil {
		return Member{}, err
	}
	return m, nil
}

// Members lists a workspace's memberships, active or not, owners first, then
// admins, then members, each by when they joined and then by address.
func (r *Repository) Members(ctx context.Context, workspaceID string) ([]Member, error) {
	if _, err := r.Get(ctx, workspaceID); err != nil {
		return nil, err
	}
	rows, err := r.store.Reader().QueryContext(ctx, `SELECT `+memberColumns+`
		  FROM workspace_members m JOIN users u ON u.id = m.user_id
		 WHERE m.workspace_id = ?
		 ORDER BY CASE m.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, m.created_at, u.email, m.user_id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("workspace: list members: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	var (
		out    []Member
		owners int
	)
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		if m.Role == RoleOwner && m.Active() {
			owners++
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: list members: %w", err)
	}
	last, err := lastReadersIn(ctx, r.store.Reader(), workspaceID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].LastOwner = owners == 1 && out[i].Role == RoleOwner && out[i].Active()
		out[i].LastReaderOf = last[out[i].UserID]
	}
	return out, nil
}

func otherActiveOwners(ctx context.Context, q querier, workspaceID, userID string) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM workspace_members m JOIN users u ON u.id = m.user_id
		WHERE m.workspace_id = ? AND m.user_id <> ? AND m.role = 'owner' AND m.status = 'active' AND u.status = 'active'`,
		workspaceID, userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("workspace: count owners: %w", err)
	}
	return n, nil
}

func insertMemberTx(ctx context.Context, tx *sql.Tx, workspaceID, userID string, role Role, now time.Time) error {
	at := now.UTC().Unix()
	_, err := tx.ExecContext(ctx,
		`INSERT INTO workspace_members(workspace_id, user_id, role, status, created_at, updated_at)
		 VALUES (?, ?, ?, 'active', ?, ?)`, workspaceID, userID, string(role), at, at)
	switch {
	case store.IsUnique(err):
		return ErrAlreadyMember
	case store.IsForeignKey(err):
		return ErrNoSuchPerson
	case err != nil:
		return fmt.Errorf("workspace: add member: %w", err)
	}
	return nil
}

// AddMemberTx makes a person a member of a team, inside the caller's
// transaction: how accepting a team invite joins it. The person must be
// active and not already a member, active or not.
func (r *Repository) AddMemberTx(ctx context.Context, tx *sql.Tx, workspaceID, userID string, role Role, now time.Time) error {
	if _, err := ParseRole(string(role)); err != nil {
		return err
	}
	w, err := GetTx(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	if err := r.changeable(w); err != nil {
		return err
	}
	if err := requireActivePersonTx(ctx, tx, userID); err != nil {
		return err
	}
	return insertMemberTx(ctx, tx, workspaceID, userID, role, now)
}

// MemberChange is what SetMember changes; a nil field stays as it is.
type MemberChange struct {
	Role   *Role
	Status *Status
}

// SetMember changes a member's role or status in a team.
//
// Any real change expires the invites the person created in the team that
// are still waiting: they were made under a role or a standing the person no
// longer has. Making them an owner or an admin takes a stored manage away,
// which the role gives (the schema's trigger); taking that role away ends the
// consent attempts they started on the team's mailboxes, which they manage no
// longer. Disabling a membership deletes the person's grants in the workspace
// in the same transaction, ends the consent attempts they started on its
// mailboxes and deletes the team's invites still waiting for their address
// (leaveTx); enabling it again restores the membership only. The keys they
// created stay, as a demotion leaves them: they are the workspace's. Refused:
// leaving the team without an active owner (ErrLastOwner), and disabling the
// last reader of one of its mailboxes (ErrLastReader).
func (r *Repository) SetMember(ctx context.Context, workspaceID, userID string, change MemberChange, check Check) (Member, error) {
	if change.Role != nil {
		if _, err := ParseRole(string(*change.Role)); err != nil {
			return Member{}, err
		}
	}
	if change.Status != nil {
		if _, err := ParseStatus(string(*change.Status)); err != nil {
			return Member{}, err
		}
	}
	now := r.now().Unix()
	var out Member
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		w, err := GetTx(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		if err := r.changeable(w); err != nil {
			return err
		}
		if err := runCheck(tx, check); err != nil {
			return err
		}
		m, err := MemberTx(ctx, tx, workspaceID, userID)
		if err != nil {
			return err
		}
		role, status := m.Role, m.Status
		if change.Role != nil {
			role = *change.Role
		}
		if change.Status != nil {
			status = *change.Status
		}
		if role == m.Role && status == m.Status {
			out = m
			return nil
		}
		if m.LastOwner && (role != RoleOwner || status != StatusActive) {
			return ErrLastOwner
		}
		if status == StatusDisabled && m.Status == StatusActive {
			if err := requireNotLastReaderInTx(ctx, tx, workspaceID, userID); err != nil {
				return err
			}
			if err := leaveTx(ctx, tx, workspaceID, userID); err != nil {
				return err
			}
		}
		if role == RoleMember && m.Role != RoleMember {
			// They managed the team's mailboxes by their role, and manage
			// none of them now: they hold no stored manage, which the role
			// took away when they got it.
			if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_pending
				WHERE owner_user_id = ? AND account_id IN (SELECT id FROM accounts WHERE workspace_id = ?)`,
				userID, workspaceID); err != nil {
				return fmt.Errorf("workspace: end the consent attempts of mailboxes no longer managed: %w", err)
			}
		}
		if err := expireInvitesByTx(ctx, tx, workspaceID, userID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE workspace_members SET role = ?, status = ?, updated_at = ? WHERE workspace_id = ? AND user_id = ?`,
			string(role), string(status), now, workspaceID, userID); err != nil {
			return fmt.Errorf("workspace: change member: %w", err)
		}
		out, err = MemberTx(ctx, tx, workspaceID, userID)
		return err
	})
	if err != nil {
		return Member{}, err
	}
	return out, nil
}

// RemoveMember removes a person from a team. Their grants there go with the
// membership, the consent attempts they started on its mailboxes end, the
// team's invites still waiting for their address are deleted, so a leftover
// invite cannot bring them back, the invites they created there expire, and
// the keys they created there are revoked (a key carried over from theirs
// loses the team's mailboxes), in the same transaction. Refused for the last
// active owner (ErrLastOwner) and the last reader of one of the team's
// mailboxes (ErrLastReader).
func (r *Repository) RemoveMember(ctx context.Context, workspaceID, userID string, check Check) error {
	now := r.now().Unix()
	return r.store.Write(ctx, func(tx *sql.Tx) error {
		w, err := GetTx(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		if err := r.changeable(w); err != nil {
			return err
		}
		if err := runCheck(tx, check); err != nil {
			return err
		}
		m, err := MemberTx(ctx, tx, workspaceID, userID)
		if err != nil {
			return err
		}
		if m.LastOwner {
			return ErrLastOwner
		}
		if err := requireNotLastReaderInTx(ctx, tx, workspaceID, userID); err != nil {
			return err
		}
		if err := leaveTx(ctx, tx, workspaceID, userID); err != nil {
			return err
		}
		if err := expireInvitesByTx(ctx, tx, workspaceID, userID, now); err != nil {
			return err
		}
		if _, err := revokeCreatedInTx(ctx, tx, workspaceID, userID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?`, workspaceID, userID); err != nil {
			return fmt.Errorf("workspace: remove member: %w", err)
		}
		return nil
	})
}

// expireInvitesByTx expires the invites a person created in a team that are
// still waiting, when their role or their place there changes: each was made
// under a standing they may no longer have. The record stays, and the hourly
// sweep deletes it once it has been expired long enough.
func expireInvitesByTx(ctx context.Context, tx *sql.Tx, workspaceID, userID string, now int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE invites SET expires_at = ?1
		WHERE workspace_id = ?2 AND created_by = ?3 AND used_at = 0 AND expires_at > ?1`,
		now, workspaceID, userID); err != nil {
		return fmt.Errorf("workspace: expire the invites the person created: %w", err)
	}
	return nil
}

// leaveTx is what a member losing their place in a workspace takes with it:
// their grants there, flags and sealed grants of every epoch, the consent
// attempts they started on its mailboxes, and the team's invites still
// waiting for their address. The last-reader rule is the caller's to have
// checked.
func leaveTx(ctx context.Context, tx *sql.Tx, workspaceID, userID string) error {
	// Whatever consent attempt they started on a mailbox here ends: they
	// manage none of them any more (dropAttemptsTx).
	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_pending
		WHERE owner_user_id = ? AND account_id IN (SELECT id FROM accounts WHERE workspace_id = ?)`,
		userID, workspaceID); err != nil {
		return fmt.Errorf("workspace: end the consent attempts of mailboxes no longer managed: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mailbox_access WHERE workspace_id = ? AND user_id = ?`,
		workspaceID, userID); err != nil {
		return fmt.Errorf("workspace: delete grants: %w", err)
	}
	// A disabled membership stays, and the foreign key that takes the sealed
	// grants with a removed one would not: they go with the flag, here
	// (docs/key-scheme.md section 12.13).
	if _, err := tx.ExecContext(ctx, `DELETE FROM mailbox_grants WHERE workspace_id = ? AND user_id = ?`,
		workspaceID, userID); err != nil {
		return fmt.Errorf("workspace: delete the sealed grants: %w", err)
	}
	// An invite to the team still waiting for the person's address would
	// bring them straight back, with whatever role it names: it goes with
	// their place.
	if _, err := tx.ExecContext(ctx, `DELETE FROM invites
		WHERE workspace_id = ? AND used_at = 0 AND email = (SELECT email FROM users WHERE id = ?)`,
		workspaceID, userID); err != nil {
		return fmt.Errorf("workspace: delete the person's pending invites: %w", err)
	}
	return nil
}

// dropAttemptsTx ends the consent attempts a person started on mailboxes they
// no longer manage: the pending rows, so a redirect that comes back later is
// refused and a listener or device poll the daemon runs for one finds nothing
// to store. Storing a grant checks again, in its own transaction, that its
// starter still manages the mailbox (ManagesTx), which also covers an attempt
// already past this point.
func dropAttemptsTx(ctx context.Context, tx *sql.Tx, userID string, accountIDs ...string) error {
	if len(accountIDs) == 0 {
		return nil
	}
	list, err := json.Marshal(accountIDs)
	if err != nil {
		return fmt.Errorf("workspace: encode ids: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_pending
		WHERE owner_user_id = ? AND account_id IN (SELECT value FROM json_each(?))`, userID, string(list)); err != nil {
		return fmt.Errorf("workspace: end the consent attempts of a mailbox no longer managed: %w", err)
	}
	return nil
}

// listIDs runs a query of one text column and returns its values.
func listIDs(ctx context.Context, q querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("workspace: list ids: %w", err)
	}
	//nolint:errcheck // read to the end below; a close failure changes nothing
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("workspace: list ids: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: list ids: %w", err)
	}
	return ids, nil
}
