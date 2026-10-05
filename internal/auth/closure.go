package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Closing a person's account is two steps, as the privacy policy describes
// them. First they are disabled, which ends every session and revokes every
// key they hold, so nothing of theirs still gets in. Then they are deleted:
// their mailboxes, those mailboxes' credentials, their sessions, their keys and
// their invite. The mailboxes belong to internal/account, so the deletion is
// one transaction that package opens and this one finishes (DeleteTx).
//
// Invites have a retention of their own: one that is never used is deleted
// InviteRetention after it expires, by the daemon's hourly sweep.

// InviteRetention is how long an unused invite is kept once it has expired:
// long enough to answer "my invite did not work", and no longer than the
// privacy policy promises.
const InviteRetention = 30 * 24 * time.Hour

var (
	// ErrLastOwner is switching off or deleting the only active owner, which
	// would leave nobody able to invite people or see the instance's own
	// accounts from the console. It is refused unless the caller insists.
	ErrLastOwner = errors.New("auth: that is the last active owner")
	// ErrOwnsAccounts is deleting a person while an account still names them
	// as its owner. The accounts go first, in the same transaction.
	ErrOwnsAccounts = errors.New("auth: that person still owns accounts")
	// ErrUserDisabled is a person who has been switched off.
	ErrUserDisabled = errors.New("auth: that person is disabled")
)

// BlockedError is a person whose teams depend on them: the last active owner
// of a team other active members remain in, or the person a team mailbox
// another member reads syncs under. Disabling or deleting them is refused
// unless the caller insists; Blocks says which teams and mailboxes.
type BlockedError struct {
	workspace.Blocks
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("auth: that person is the last owner of %d team(s) and linked %d team mailbox(es) others read",
		len(e.LastOwnerOf), len(e.Linked))
}

// RequireNoBlocksTx refuses, inside the caller's transaction and unless
// force, a person whose teams depend on them (BlockedError). Deleting a
// person runs it before their mailboxes go, which is when the mailboxes they
// linked can still be asked about.
func (u *Users) RequireNoBlocksTx(ctx context.Context, tx *sql.Tx, id string, force bool) error {
	if force {
		return nil
	}
	blocks, err := workspace.BlocksTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if blocks.Any() {
		return &BlockedError{Blocks: blocks}
	}
	return nil
}

// RequireActiveTx reports, inside the caller's transaction, whether a person
// may still have anything stored on their behalf: ErrUserNotFound once they
// are deleted, ErrUserDisabled once they are switched off. Inside the
// transaction that would do the storing, so it is ordered against Disable
// rather than racing it.
func (u *Users) RequireActiveTx(ctx context.Context, tx *sql.Tx, id string) error {
	var status string
	err := tx.QueryRowContext(ctx, `SELECT status FROM users WHERE id = ?`, id).Scan(&status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrUserNotFound
	case err != nil:
		return fmt.Errorf("auth: read user status: %w", err)
	case status != userActive:
		return ErrUserDisabled
	}
	return nil
}

// Ended is what disabling a person ended.
type Ended struct {
	Sessions int
	Keys     int
}

// Disable switches a person off and ends every way they had in: their status
// becomes disabled, every live session is revoked and every live key issued
// for them is revoked, in one transaction. A disabled person still signed in
// somewhere, or holding a key that would work again if they were switched back
// on, would not be disabled.
//
// Disabling someone already disabled changes nothing and is not an error. The
// only active owner is not switched off unless force says so, nor somebody
// their teams depend on (BlockedError).
func (u *Users) Disable(ctx context.Context, id string, force bool) (Ended, error) {
	var out Ended
	now := u.now().Unix()
	err := u.store.Write(ctx, func(tx *sql.Tx) error {
		var role, status string
		err := tx.QueryRowContext(ctx, `SELECT role, status FROM users WHERE id = ?`, id).Scan(&role, &status)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrUserNotFound
		case err != nil:
			return fmt.Errorf("auth: disable user: %w", err)
		}
		if status == userActive && Role(role) == RoleOwner && !force {
			if err := requireAnotherOwnerTx(ctx, tx, id); err != nil {
				return err
			}
		}
		if status == userActive {
			if err := u.RequireNoBlocksTx(ctx, tx, id, force); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET status = ?, updated_at = ? WHERE id = ? AND status <> ?`,
			userDisabled, now, id, userDisabled); err != nil {
			return fmt.Errorf("auth: disable user: %w", err)
		}
		if out.Sessions, err = revokeSessionsTx(ctx, tx, id, now); err != nil {
			return err
		}
		// Revoked rather than left to fail: switching the person back on
		// must not bring a key back, any more than it brings a session back.
		out.Keys, err = execCount(ctx, tx,
			`UPDATE api_keys SET revoked_at = ? WHERE user_id = ? AND revoked_at = 0`, now, id)
		if err != nil {
			return fmt.Errorf("auth: revoke the user's keys: %w", err)
		}
		return nil
	})
	if err != nil {
		return Ended{}, err
	}
	return out, nil
}

// Removed is what deleting a person removed.
type Removed struct {
	Sessions int
	Keys     int
	Invites  int
	// Teams counts the teams deleted with the person, who was their only
	// member.
	Teams int
}

// DeleteTx deletes a person inside the caller's transaction: their sessions,
// the keys issued for them together with those keys' account restrictions,
// every invite for their address — the one they signed up with and any other,
// used or not — their personal workspace and every team whose only member
// they are, their memberships and grants elsewhere, and then the person.
//
// The accounts they linked must already be gone from the same transaction:
// the row cannot be deleted while an account names it, which is
// ErrOwnsAccounts, and it is deliberately not a cascade (see migration 0002);
// nor can a workspace that still holds one. Invites they sent to other people,
// and grants they gave, stay, since each is the record of how that person
// arrived or got access; who sent or gave it does not.
//
// The only active owner is not deleted unless force says so, disabled or not.
func (u *Users) DeleteTx(ctx context.Context, tx *sql.Tx, id string, force bool) (Removed, error) {
	var email, role string
	err := tx.QueryRowContext(ctx, `SELECT email, role FROM users WHERE id = ?`, id).Scan(&email, &role)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Removed{}, ErrUserNotFound
	case err != nil:
		return Removed{}, fmt.Errorf("auth: delete user: %w", err)
	}
	if Role(role) == RoleOwner && !force {
		if err := requireAnotherOwnerTx(ctx, tx, id); err != nil {
			return Removed{}, err
		}
	}

	var out Removed
	if out.Sessions, err = execCount(ctx, tx, `DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return Removed{}, fmt.Errorf("auth: delete the user's sessions: %w", err)
	}
	// Each key's restriction rows follow it: api_key_accounts cascades on
	// the key.
	if out.Keys, err = execCount(ctx, tx, `DELETE FROM api_keys WHERE user_id = ?`, id); err != nil {
		return Removed{}, fmt.Errorf("auth: delete the user's keys: %w", err)
	}
	if out.Invites, err = execCount(ctx, tx,
		`DELETE FROM invites WHERE email = ? OR used_by = ?`, email, id); err != nil {
		return Removed{}, fmt.Errorf("auth: delete the user's invites: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE invites SET created_by = '' WHERE created_by = ?`, id); err != nil {
		return Removed{}, fmt.Errorf("auth: forget who sent invites: %w", err)
	}
	var linked int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM accounts WHERE owner_user_id = ?`, id).Scan(&linked); err != nil {
		return Removed{}, fmt.Errorf("auth: delete user: %w", err)
	}
	if linked > 0 {
		return Removed{}, ErrOwnsAccounts
	}
	teams, err := workspace.DeletePersonTx(ctx, tx, id)
	if err != nil {
		return Removed{}, err
	}
	out.Teams = len(teams)
	_, err = tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	switch {
	case store.IsForeignKey(err):
		return Removed{}, ErrOwnsAccounts
	case err != nil:
		return Removed{}, fmt.Errorf("auth: delete user: %w", err)
	}
	return out, nil
}

// DeleteInvites deletes every invite for an address that has no account: the
// person was invited and never signed up, and asks for what was kept about
// them. It reports how many it deleted. An address with an account is left
// alone — its invites go with the account, through DeleteTx.
func (u *Users) DeleteInvites(ctx context.Context, email string) (int, error) {
	email = strings.TrimSpace(email)
	var n int
	err := u.store.Write(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = execCount(ctx, tx,
			`DELETE FROM invites WHERE email = ? AND NOT EXISTS (SELECT 1 FROM users WHERE email = ?)`, email, email)
		if err != nil {
			return fmt.Errorf("auth: delete invites: %w", err)
		}
		return nil
	})
	return n, err
}

// SweepInvites deletes the unused invites that have been expired for longer
// than InviteRetention, and reports how many it deleted.
//
// ahead is how long until the caller sweeps again. An invite that would pass
// its retention before then is deleted now, so that sweeping once an hour
// never keeps one up to an hour longer than InviteRetention. Used invites are
// never swept: each is the record of how somebody's account came to be, and
// it goes when that account does.
func (u *Users) SweepInvites(ctx context.Context, ahead time.Duration) (int, error) {
	cutoff := u.now().Add(ahead - InviteRetention).Unix()
	var n int
	err := u.store.Write(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = execCount(ctx, tx, `DELETE FROM invites WHERE used_at = 0 AND expires_at < ?`, cutoff)
		if err != nil {
			return fmt.Errorf("auth: sweep invites: %w", err)
		}
		return nil
	})
	return n, err
}

// requireAnotherOwnerTx refuses unless some other owner is active.
func requireAnotherOwnerTx(ctx context.Context, tx *sql.Tx, id string) error {
	var others int
	err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE role = ? AND status = ? AND id <> ?`,
		string(RoleOwner), userActive, id).Scan(&others)
	if err != nil {
		return fmt.Errorf("auth: count owners: %w", err)
	}
	if others == 0 {
		return ErrLastOwner
	}
	return nil
}
