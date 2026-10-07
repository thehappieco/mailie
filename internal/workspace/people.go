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

// Blocks are what a person cannot leave behind without force.
type Blocks struct {
	// LastOwnerOf are the teams where they are the last active owner and
	// other active members remain, who would be left without one.
	LastOwnerOf []string
	// LastReaderOf are the team mailboxes they are the only reader of, in
	// teams that outlive them (another member remains, whatever their
	// status), which nobody could ever read again.
	LastReaderOf []string
	// BoundTo are the team mailboxes whose consent to sync migration 0011
	// copied from theirs, and nobody has confirmed for the team since, that
	// someone else reads: closing them stops each and deletes its index for
	// that reader (store.StopBoundTx), until an owner or an admin of the
	// team confirms its sync.
	BoundTo []string
}

// Any reports whether anything blocks.
func (b Blocks) Any() bool {
	return len(b.LastOwnerOf) > 0 || len(b.LastReaderOf) > 0 || len(b.BoundTo) > 0
}

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
	b.LastReaderOf, err = lastReaderOf(ctx, tx, userID, "", nil, true)
	if err != nil {
		return Blocks{}, err
	}
	b.BoundTo, err = listIDs(ctx, tx, `SELECT a.id FROM accounts a
		  JOIN workspaces w ON w.id = a.workspace_id AND w.kind = 'team'
		 WHERE a.sync_enabled_via = 'migration' AND a.sync_enabled_by = ?1
		   AND EXISTS (SELECT 1 FROM mailbox_access o WHERE o.account_id = a.id AND o.user_id <> ?1 AND `+readerOf("o")+`)
		 ORDER BY a.created_at, a.rowid`, userID)
	if err != nil {
		return Blocks{}, err
	}
	return b, nil
}

// SoleMemberTeamsTx lists the teams whose only member, active or not, is
// userID: the teams that go with them when they are deleted, mailboxes and
// all.
func SoleMemberTeamsTx(ctx context.Context, tx *sql.Tx, userID string) ([]string, error) {
	return listIDs(ctx, tx, `SELECT w.id FROM workspaces w
		 WHERE w.kind = 'team'
		   AND EXISTS (SELECT 1 FROM workspace_members m WHERE m.workspace_id = w.id AND m.user_id = ?1)
		   AND NOT EXISTS (SELECT 1 FROM workspace_members m WHERE m.workspace_id = w.id AND m.user_id <> ?1)
		 ORDER BY w.id`, userID)
}

// DeletePersonTx removes what workspaces keep of a person, inside the
// transaction that deletes them, once the mailboxes that go with them are
// gone from it — their personal workspace's, and those of every team whose
// only member they are (account.Registry.RemoveOwner): those teams and their
// personal workspace, with their memberships and the invites still waiting
// to join them; and their name wherever it is kept as attribution: on the
// grants they gave others, on the mailboxes they linked and on the team
// consents to sync they gave, which stay, with their date and revision, the
// workspace's. Their memberships of other teams, and their grants there, go
// with the person (ON DELETE CASCADE). It returns the teams it deleted.
//
// A used invite to a deleted team is not theirs to take: it is the record of
// how another person arrived, and stays, without its team (ON DELETE SET
// NULL), until that person goes.
//
// A workspace that still holds a mailbox is not deleted: the mailbox names
// it, and the foreign key refuses (ErrHoldsMailboxes).
func DeletePersonTx(ctx context.Context, tx *sql.Tx, userID string) ([]string, error) {
	for _, step := range []struct{ what, query string }{
		{"forget who granted", `UPDATE mailbox_access SET granted_by = '' WHERE granted_by = ?`},
		{"forget who linked", `UPDATE accounts SET linked_by = '' WHERE linked_by = ?`},
		{"forget who agreed to sync", `UPDATE accounts SET sync_enabled_by = '', sync_enabled_via = ''
		   WHERE sync_enabled_by = ?`},
	} {
		if _, err := tx.ExecContext(ctx, step.query, userID); err != nil {
			return nil, fmt.Errorf("workspace: %s: %w", step.what, err)
		}
	}
	teams, err := SoleMemberTeamsTx(ctx, tx, userID)
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
