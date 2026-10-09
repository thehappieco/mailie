package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// The last reader (docs/workspaces.md, "Protections").
//
// A team mailbox someone can read is never left with nobody who can. A reader
// is an active member of the mailbox's workspace, active on the instance,
// holding read on it; keys never count, and neither does a role: an owner who
// reads nothing is no way back into a mailbox. Read passes only from someone
// who holds it, so a mailbox left with no reader could never be read again;
// the only way out is removing it and linking it again.
//
// Guarded: revoking the last reader's read, disabling or removing their
// membership (leaving included), and disabling or deleting them on the
// instance without force, unless they are the team's only member and it goes
// with them. Not guarded: removing the mailbox itself, and role
// changes, which change no read. Each check runs inside the transaction that
// would break the rule; the database has one writer, so two changes that
// would each leave one reader are ordered, and the second is refused.

// readerOf is the condition, over mailbox_access aliased with the given name,
// that its holder reads the mailbox: an active member, active on the
// instance, holding read.
func readerOf(alias string) string {
	return alias + `.read = 1 AND EXISTS (SELECT 1 FROM workspace_members rm JOIN users ru ON ru.id = rm.user_id
		WHERE rm.workspace_id = ` + alias + `.workspace_id AND rm.user_id = ` + alias + `.user_id
		  AND rm.status = 'active' AND ru.status = 'active')`
}

// lastReaderOf lists the team mailboxes userID is the only reader of, oldest
// first: among accountIDs when it is not nil, within workspaceID when that is
// not empty, and otherwise every one. With outlived, only those of teams that
// outlive the person's account: teams with another member, whatever that
// member's status, which deleting the person leaves standing
// (SoleMemberTeamsTx is the other half of the same test). A team whose
// remaining members are all disabled still outlives them, and its mailbox
// could never be read again, since read passes only from a reader; closing the
// account of someone alone in a team takes the team with them instead.
func lastReaderOf(ctx context.Context, q querier, userID, workspaceID string, accountIDs []string, outlived bool) ([]string, error) {
	query := `SELECT a.id FROM accounts a
		  JOIN workspaces w ON w.id = a.workspace_id AND w.kind = 'team'
		  JOIN mailbox_access g ON g.account_id = a.id AND g.user_id = ?1
		 WHERE ` + readerOf("g") + `
		   AND NOT EXISTS (SELECT 1 FROM mailbox_access o
		                    WHERE o.account_id = a.id AND o.user_id <> ?1 AND ` + readerOf("o") + `)
		   AND (?2 = '' OR a.workspace_id = ?2)`
	if outlived {
		query += ` AND EXISTS (SELECT 1 FROM workspace_members om
		                        WHERE om.workspace_id = a.workspace_id AND om.user_id <> ?1)`
	}
	args := []any{userID, workspaceID}
	if accountIDs != nil {
		list, err := json.Marshal(accountIDs)
		if err != nil {
			return nil, fmt.Errorf("workspace: encode ids: %w", err)
		}
		query += ` AND a.id IN (SELECT value FROM json_each(?3))`
		args = append(args, string(list))
	}
	return listIDs(ctx, q, query+` ORDER BY a.created_at, a.rowid`, args...)
}

// LastReaderOfTx lists, inside the caller's transaction, the team mailboxes
// userID is the only reader of, in every team: what a change that takes
// "read" from a person who stays, in teams that stay with them, must not
// leave behind (the reset of their account key, docs/key-scheme.md section
// 12.6). Unlike BlocksTx's LastReaderOf, a team whose only member is the
// person counts, since nothing takes it away with them.
func LastReaderOfTx(ctx context.Context, tx *sql.Tx, userID string) ([]string, error) {
	return lastReaderOf(ctx, tx, userID, "", nil, false)
}

// HasReaderTx reports, inside the caller's transaction, whether a mailbox
// has a reader: whether anyone could ever read what syncing it would store.
func HasReaderTx(ctx context.Context, tx *sql.Tx, accountID string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_access g
		 WHERE g.account_id = ? AND `+readerOf("g"), accountID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("workspace: read the mailbox's readers: %w", err)
	}
	return n > 0, nil
}

// requireAnotherReaderTx refuses, inside the caller's transaction, a change
// that takes read on accountIDs from userID when they are the last reader of
// one of them (ErrLastReader).
func requireAnotherReaderTx(ctx context.Context, tx *sql.Tx, userID string, accountIDs []string) error {
	if len(accountIDs) == 0 {
		return nil
	}
	last, err := lastReaderOf(ctx, tx, userID, "", accountIDs, false)
	if err != nil {
		return err
	}
	if len(last) > 0 {
		return fmt.Errorf("%w: %v", ErrLastReader, last)
	}
	return nil
}

// requireNotLastReaderInTx refuses, inside the caller's transaction, a change
// that takes userID's place in a workspace — disabling or removing their
// membership — while they are the last reader of one of its team mailboxes
// (ErrLastReader).
func requireNotLastReaderInTx(ctx context.Context, tx *sql.Tx, workspaceID, userID string) error {
	last, err := lastReaderOf(ctx, tx, userID, workspaceID, nil, false)
	if err != nil {
		return err
	}
	if len(last) > 0 {
		return fmt.Errorf("%w: %v", ErrLastReader, last)
	}
	return nil
}

// LastReaders maps each person to the team mailboxes of a workspace they are
// the only reader of: what a listing marks in advance, so a console can
// explain before anyone tries.
func (r *Repository) LastReaders(ctx context.Context, workspaceID string) (map[string][]string, error) {
	return lastReadersIn(ctx, r.store.Reader(), workspaceID)
}

func lastReadersIn(ctx context.Context, q querier, workspaceID string) (map[string][]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT g.user_id, a.id FROM accounts a
		  JOIN workspaces w ON w.id = a.workspace_id AND w.kind = 'team'
		  JOIN mailbox_access g ON g.account_id = a.id
		 WHERE a.workspace_id = ?1 AND `+readerOf("g")+`
		   AND NOT EXISTS (SELECT 1 FROM mailbox_access o
		                    WHERE o.account_id = a.id AND o.user_id <> g.user_id AND `+readerOf("o")+`)
		 ORDER BY a.created_at, a.rowid`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("workspace: read the last readers: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var userID, accountID string
		if err := rows.Scan(&userID, &accountID); err != nil {
			return nil, fmt.Errorf("workspace: read the last readers: %w", err)
		}
		out[userID] = append(out[userID], accountID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: read the last readers: %w", err)
	}
	return out, nil
}

// ReadBy lists the mailboxes a person holds read on, in any workspace,
// whether or not it counts right now: what may stop syncing when they go,
// for a team mailbox they were the last to read.
func (r *Repository) ReadBy(ctx context.Context, userID string) ([]string, error) {
	return listIDs(ctx, r.store.Reader(), `SELECT account_id FROM mailbox_access WHERE user_id = ? AND read = 1
		ORDER BY account_id`, userID)
}
