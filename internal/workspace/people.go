package workspace

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/thehappieco/mailie/internal/store"
)

// What a person's workspaces have to say when the person is disabled or
// deleted on the instance.

// Blocks are the teams a person cannot leave behind without force.
type Blocks struct {
	// LastOwnerOf are the teams where they are the last active owner and
	// other active members remain, who would be left without one.
	LastOwnerOf []string
	// Linked are team mailboxes they linked that another active member can
	// read: the mailbox syncs under their consent, and goes with them.
	Linked []string
}

// Any reports whether anything blocks.
func (b Blocks) Any() bool { return len(b.LastOwnerOf) > 0 || len(b.Linked) > 0 }

// BlocksTx reads, inside the caller's transaction, what disabling or deleting
// a person would break in their teams. Personal workspaces block nothing.
func BlocksTx(ctx context.Context, tx *sql.Tx, userID string) (Blocks, error) {
	return blocksOn(ctx, tx, userID)
}

// Blocks is BlocksTx outside a transaction, for a listing that explains in
// advance; the deletion checks again inside its own.
func (r *Repository) Blocks(ctx context.Context, userID string) (Blocks, error) {
	return blocksOn(ctx, r.store.Reader(), userID)
}

func blocksOn(ctx context.Context, tx querier, userID string) (Blocks, error) {
	var (
		b   Blocks
		err error
	)
	b.LastOwnerOf, err = listIDs(ctx, tx, `SELECT w.id FROM workspaces w
		  JOIN workspace_members me ON me.workspace_id = w.id AND me.user_id = ?1
		 WHERE w.kind = 'team' AND me.role = 'owner' AND me.status = 'active'
		   AND NOT EXISTS (SELECT 1 FROM workspace_members o JOIN users u ON u.id = o.user_id
		                    WHERE o.workspace_id = w.id AND o.user_id <> ?1 AND o.role = 'owner'
		                      AND o.status = 'active' AND u.status = 'active')
		   AND EXISTS (SELECT 1 FROM workspace_members o JOIN users u ON u.id = o.user_id
		                WHERE o.workspace_id = w.id AND o.user_id <> ?1 AND o.status = 'active' AND u.status = 'active')
		 ORDER BY w.created_at, w.id`, userID)
	if err != nil {
		return Blocks{}, err
	}
	b.Linked, err = listIDs(ctx, tx, `SELECT a.id FROM accounts a JOIN workspaces w ON w.id = a.workspace_id
		 WHERE w.kind = 'team' AND a.owner_user_id = ?1
		   AND EXISTS (SELECT 1 FROM mailbox_access g WHERE g.account_id = a.id AND g.user_id <> ?1 AND g.read = 1
		                 AND `+activeGrant+`)
		 ORDER BY a.created_at, a.id`, userID)
	if err != nil {
		return Blocks{}, err
	}
	return b, nil
}

// DeletePersonTx removes what a person's workspaces keep of them, inside the
// transaction that deletes the person, once the mailboxes they linked are
// gone from it: every team whose only member they are, their personal
// workspace (both with their memberships and the invites still waiting to
// join them), and their name on the grants they gave others. Their
// memberships of other teams, and their grants there, go with the person (ON
// DELETE CASCADE). It returns the teams it deleted.
//
// A used invite to a deleted team is not theirs to take: it is the record of
// how another person arrived, and stays, without its team (ON DELETE SET
// NULL), until that person goes.
//
// A workspace that still holds a mailbox is not deleted: the mailbox names
// it, and the foreign key refuses. The caller removes the person's mailboxes
// first, in the same transaction.
func DeletePersonTx(ctx context.Context, tx *sql.Tx, userID string) ([]string, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE mailbox_access SET granted_by = '' WHERE granted_by = ?`, userID); err != nil {
		return nil, fmt.Errorf("workspace: forget who granted: %w", err)
	}
	teams, err := listIDs(ctx, tx, `SELECT w.id FROM workspaces w
		 WHERE w.kind = 'team'
		   AND EXISTS (SELECT 1 FROM workspace_members m WHERE m.workspace_id = w.id AND m.user_id = ?1)
		   AND NOT EXISTS (SELECT 1 FROM workspace_members m WHERE m.workspace_id = w.id AND m.user_id <> ?1)
		 ORDER BY w.id`, userID)
	if err != nil {
		return nil, err
	}
	for _, id := range teams {
		// The schema does it too (workspaces_unused_invites): an unused
		// invite left without its team would read as an instance invite.
		if _, err := tx.ExecContext(ctx, `DELETE FROM invites WHERE workspace_id = ? AND used_at = 0`, id); err != nil {
			return nil, fmt.Errorf("workspace: delete the team %s's invites: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE id = ?`, id); err != nil {
			if store.IsForeignKey(err) {
				return nil, fmt.Errorf("%w: the team %s", ErrHoldsMailboxes, id)
			}
			return nil, fmt.Errorf("workspace: delete the team %s: %w", id, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE person_id = ? AND kind = 'personal'`, userID); err != nil {
		if store.IsForeignKey(err) {
			return nil, fmt.Errorf("%w: the personal workspace", ErrHoldsMailboxes)
		}
		return nil, fmt.Errorf("workspace: delete the personal workspace: %w", err)
	}
	return teams, nil
}

// RepairPersonal gives every person without a personal workspace the one the
// local source makes when it creates a person, as old as the person, and
// returns how many it made. A person gets theirs in the transaction that
// creates them; one without it was created by a binary from before migration
// 0008 run on a migrated database, which only a binary rolled back alone does
// (store.ErrSchemaTooNew stops that from 0008 on, but not a binary older than
// the guard). The daemon calls it once at start. Under any source but the
// local one it makes nothing: the platform's workspaces are its to send.
func (r *Repository) RepairPersonal(ctx context.Context) (int, error) {
	if r.source.Name() != SourceLocal {
		return 0, nil
	}
	made := 0
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		missing, err := withoutPersonalTx(ctx, tx)
		if err != nil {
			return err
		}
		for _, p := range missing {
			if err := r.source.PersonCreatedTx(ctx, tx, p.id, time.Unix(p.created, 0)); err != nil {
				return err
			}
		}
		made = len(missing)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return made, nil
}

type unhoused struct {
	id      string
	created int64
}

// withoutPersonalTx lists the people who have no personal workspace, oldest
// first, and closes its cursor before returning.
func withoutPersonalTx(ctx context.Context, tx *sql.Tx) ([]unhoused, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, created_at FROM users u
		WHERE NOT EXISTS (SELECT 1 FROM workspaces w WHERE w.person_id = u.id) ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("workspace: look for people without a personal workspace: %w", err)
	}
	//nolint:errcheck // read to the end below; a close failure changes nothing
	defer func() { _ = rows.Close() }()
	var out []unhoused
	for rows.Next() {
		var p unhoused
		if err := rows.Scan(&p.id, &p.created); err != nil {
			return nil, fmt.Errorf("workspace: look for people without a personal workspace: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: look for people without a personal workspace: %w", err)
	}
	return out, nil
}
