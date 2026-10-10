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
// API key they created, in any workspace, so nothing they set up still gets
// in, and expires the invites they made; the identities they sign in with
// through a provider stay, and sign nobody in while they are off. Then they
// are deleted: the mailboxes of their personal workspace and of every team
// they were alone in, those mailboxes' credentials and keys, their sessions,
// their invite, and their identities with the keys pinned for them. A key
// they created in a team that stays is the team's record, revoked and
// without their name. The mailboxes belong to
// internal/account, so the deletion is one transaction that package opens and
// this one finishes (DeleteTx). A team mailbox of a team others are in is the
// team's, and stays; only a consent to sync it still bound to the person
// (migration 0011) goes with them, and its index with it.
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
// of a team other active members remain in, the last person who can read a
// team mailbox of a team that outlives them, or the person a team mailbox
// someone else reads still syncs under the consent of (migration 0011).
// Disabling or deleting them is refused unless the caller insists; Blocks
// says which teams and mailboxes.
type BlockedError struct {
	workspace.Blocks
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("auth: that person is the last owner of %d team(s), the last reader of %d team mailbox(es) "+
		"and the consent to sync of %d team mailbox(es)", len(e.LastOwnerOf), len(e.LastReaderOf), len(e.BoundTo))
}

// RequireNoBlocksTx refuses, inside the caller's transaction and unless
// force, a person whose teams depend on them (BlockedError). Deleting a
// person runs it before anything of theirs goes.
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
	// Invites counts the invites they made that were still waiting, now
	// expired.
	Invites int
	// Stopped are the team mailboxes whose consent to sync was still bound
	// to theirs (migration 0011), stopped with their index deleted.
	Stopped []string
}

// Disable switches a person off and ends every way they had in: their status
// becomes disabled, every live session is revoked, every ticket of a
// ceremony under way and every reset invitation for them is deleted, every
// live key they created is revoked, in whichever workspace, and every invite
// they made that is still waiting expires, in one transaction. What they
// enrolled with (their account key, verifiers and wraps) stays: switched back
// on, they sign in with their password again. Their grants on mailboxes keep
// their flags, which count for nothing while they are off, and lose every
// sealed grant (docs/key-scheme.md section 12.13): switched back on, they
// wait for the key on every mailbox that has one. A disabled person still
// signed in somewhere, or holding a key or an invite that would work again if
// they were switched back on, would not be disabled. The mailboxes of their
// personal workspace stop syncing, since they are no longer active; a team
// mailbox whose consent was still bound to theirs stops in the same
// transaction, and its index is deleted (store.StopBoundTx). Every other team
// mailbox carries on: it is the team's.
//
// Disabling someone already disabled changes nothing and is not an error: a
// team mailbox migration 0011 found bound to them, stopped with its index
// kept, keeps it. The only active owner is not switched off unless force says
// so, nor somebody their teams depend on (BlockedError).
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
		// Deleted rather than left to fail: a reset invitation would work
		// again if the person were switched back on, as a key would.
		if err := dropTicketsTx(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM reset_invites WHERE user_id = ?`, id); err != nil {
			return fmt.Errorf("auth: delete the user's reset invitations: %w", err)
		}
		// Being disabled takes the key with "read" (docs/key-scheme.md
		// section 12.13): deleted, so that switching them back on does not
		// bring it back either.
		if err := workspace.DropSealedGrantsOfTx(ctx, tx, id); err != nil {
			return err
		}
		// Revoked rather than left to fail: switching the person back on
		// must not bring a key back, any more than it brings a session back.
		// A key stops when the person who created it is closed, whatever it
		// holds: the key terms they agreed to say so.
		out.Keys, err = execCount(ctx, tx,
			`UPDATE api_keys SET revoked_at = ? WHERE created_by = ? AND revoked_at = 0`, now, id)
		if err != nil {
			return fmt.Errorf("auth: revoke the user's keys: %w", err)
		}
		if status != userActive {
			// Already off: what disabling them stopped stopped then, and a
			// mailbox migration 0011 found bound to them, stopped with its
			// index kept, keeps it until the team decides or they are
			// deleted.
			return nil
		}
		if out.Invites, err = expireInvitesByTx(ctx, tx, id, now); err != nil {
			return err
		}
		out.Stopped, err = store.StopBoundTx(ctx, tx, id, now)
		return err
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
	// Stopped are the team mailboxes whose consent to sync was still bound
	// to theirs (migration 0011), stopped with their index deleted.
	Stopped []string
	// Teams counts the teams deleted with the person, who was their only
	// member.
	Teams int
	// Identities counts the identities they signed in with through a
	// provider, deleted with the keys pinned for each.
	Identities int
}

// DeleteTx deletes a person inside the caller's transaction: their sessions,
// with the row the person goes everything they enrolled with in the key
// scheme (their seal id, public key, verifiers and wraps) and their tickets
// and reset invitations, the keys they created, revoked and without their name — those of their
// personal workspace and of the teams they were alone in go with those
// workspaces —, every invite for their address — the one they signed up with and any other,
// used or not — the identities they signed in with through a provider and the
// keys pinned for those identities, the team mailboxes whose consent to sync
// was still bound to theirs (stopped, their index deleted), their personal
// workspace and every team whose only member they are, their memberships and
// grants elsewhere, and then the person.
//
// The mailboxes that go with them — their personal workspace's, and those of
// the teams they were alone in — must already be gone from the same
// transaction: the row cannot be deleted while an account names it, which is
// ErrOwnsAccounts, and it is deliberately not a cascade (see migration 0002);
// nor can a workspace that still holds one. Invites they sent to other
// people, grants they gave, mailboxes they linked and team consents they gave
// stay, since each is the record of how someone arrived, got access or what
// a team agreed to; who did it does not. An invite of theirs still waiting
// expires first.
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
	// A key they created stays where it belongs, revoked, until its
	// workspace goes: their personal workspace's and the teams' they were
	// alone in below, with what they held (both cascade on the workspace);
	// another team's stays as the team's record, without their name.
	if out.Keys, err = execCount(ctx, tx, `UPDATE api_keys
		SET revoked_at = CASE revoked_at WHEN 0 THEN ? ELSE revoked_at END, created_by = ''
		WHERE created_by = ?`, u.now().Unix(), id); err != nil {
		return Removed{}, fmt.Errorf("auth: revoke the user's keys: %w", err)
	}
	if out.Invites, err = execCount(ctx, tx,
		`DELETE FROM invites WHERE email = ? OR used_by = ?`, email, id); err != nil {
		return Removed{}, fmt.Errorf("auth: delete the user's invites: %w", err)
	}
	now := u.now().Unix()
	if _, err := expireInvitesByTx(ctx, tx, id, now); err != nil {
		return Removed{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE invites SET created_by = '' WHERE created_by = ?`, id); err != nil {
		return Removed{}, fmt.Errorf("auth: forget who sent invites: %w", err)
	}
	if out.Stopped, err = store.StopBoundTx(ctx, tx, id, now); err != nil {
		return Removed{}, err
	}
	// Explicitly, rather than by the cascade from users: a pin goes only
	// with the person its identity signs in, and the cascade would leave it.
	if out.Identities, err = deleteIdentitiesTx(ctx, tx, id); err != nil {
		return Removed{}, err
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

// expireInvitesByTx expires every invite a person made that is still
// waiting, an instance invite or a team's: once they are disabled or deleted
// nobody stands behind it. The record stays until the sweep.
func expireInvitesByTx(ctx context.Context, tx *sql.Tx, id string, now int64) (int, error) {
	n, err := execCount(ctx, tx, `UPDATE invites SET expires_at = ?1
		WHERE created_by = ?2 AND used_at = 0 AND expires_at > ?1`, now, id)
	if err != nil {
		return 0, fmt.Errorf("auth: expire the user's invites: %w", err)
	}
	return n, nil
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
