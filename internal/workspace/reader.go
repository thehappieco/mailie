package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/thehappieco/mailie/internal/store"
)

// The last reader (docs/workspaces.md, "Protections").
//
// A team mailbox someone can read is never left with nobody who can. A reader
// is who reads it by the one rule (store.ReaderSQL, docs/key-scheme.md section
// 12.13): an active member of the mailbox's workspace, active on the
// instance, holding read on it, and, on a mailbox that has a key, holding a
// grant at its current epoch. Keys never count, and neither does a role: an
// owner who reads nothing is no way back into a mailbox. Read passes only
// from someone who reads, and the key only from someone who holds it, so a
// mailbox left with no reader could never be read again; the only way out is
// removing it and linking it again. A member who holds the flag and waits for
// the key is no reader, and taking their flag leaves the readers as they were.
//
// Guarded: revoking the last reader's read, disabling or removing their
// membership (leaving included), disabling or deleting them on the instance
// without force, unless they are the team's only member and it goes with
// them, and resetting their account key without force (the reset deletes
// their grants, so it takes read only on a mailbox that has a key). Not
// guarded: removing the mailbox itself, and role changes, which change no
// read. Each check runs inside the transaction that would break the rule; the
// database has one writer, so two changes that would each leave one reader
// are ordered, and the second is refused.

// lastReaders narrows lastReaderOf.
type lastReaders struct {
	// workspaceID, when not empty, keeps the mailboxes of that workspace.
	workspaceID string
	// accountIDs, when not nil, keeps those mailboxes.
	accountIDs []string
	// outlived keeps only the mailboxes of teams that outlive the person's
	// account: teams with another member, whatever that member's status,
	// which deleting the person leaves standing (SoleMemberTeamsTx is the
	// other half of the same test). A team whose remaining members are all
	// disabled still outlives them, and its mailbox could never be read
	// again, since read passes only from a reader; closing the account of
	// someone alone in a team takes the team with them instead.
	outlived bool
	// keyed keeps only the mailboxes that have a key: what deleting the
	// person's grants, and nothing else, takes read from.
	keyed bool
}

// lastReaderOf lists the team mailboxes userID is the only reader of, oldest
// first, among those in narrows to.
func lastReaderOf(ctx context.Context, q querier, userID string, in lastReaders) ([]string, error) {
	query := `SELECT a.id FROM accounts a
		  JOIN workspaces w ON w.id = a.workspace_id AND w.kind = 'team'
		  JOIN mailbox_access g ON g.account_id = a.id AND g.user_id = ?1
		 WHERE ` + store.ReaderSQL("g") + `
		   AND NOT EXISTS (SELECT 1 FROM mailbox_access o
		                    WHERE o.account_id = a.id AND o.user_id <> ?1 AND ` + store.ReaderSQL("o") + `)
		   AND (?2 = '' OR a.workspace_id = ?2)`
	if in.outlived {
		query += ` AND EXISTS (SELECT 1 FROM workspace_members om
		                        WHERE om.workspace_id = a.workspace_id AND om.user_id <> ?1)`
	}
	if in.keyed {
		query += ` AND ` + store.HasKeySQL("a.id")
	}
	args := []any{userID, in.workspaceID}
	if in.accountIDs != nil {
		list, err := json.Marshal(in.accountIDs)
		if err != nil {
			return nil, fmt.Errorf("workspace: encode ids: %w", err)
		}
		query += ` AND a.id IN (SELECT value FROM json_each(?3))`
		args = append(args, string(list))
	}
	return listIDs(ctx, q, query+` ORDER BY a.created_at, a.rowid`, args...)
}

// LastReaderOfKeyedTx lists, inside the caller's transaction, the team
// mailboxes that have a key and that userID is the only reader of, in every
// team: what deleting the person's grants while they stay, in teams that stay
// with them, must not leave behind (the reset of their account key,
// docs/key-scheme.md section 12.6). A mailbox without a key is left out: they
// keep reading it by the flag, which the reset does not take. Unlike
// BlocksTx's LastReaderOf, a team whose only member is the person counts,
// since nothing takes it away with them.
func LastReaderOfKeyedTx(ctx context.Context, tx *sql.Tx, userID string) ([]string, error) {
	return lastReaderOf(ctx, tx, userID, lastReaders{keyed: true})
}

// HasReaderTx reports, inside the caller's transaction, whether a mailbox
// has a reader: whether anyone could ever read what syncing it would store.
func HasReaderTx(ctx context.Context, tx *sql.Tx, accountID string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_access g
		 WHERE g.account_id = ? AND `+store.ReaderSQL("g"), accountID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("workspace: read the mailbox's readers: %w", err)
	}
	return n > 0, nil
}

// ReadsNowTx reports, inside the caller's transaction, whether a person reads
// a mailbox now, by the one rule: what giving read to someone else, or to a
// workspace key, and handing the mailbox's key on, are held to (the giver
// reads it themself, docs/key-scheme.md section 9.3).
func ReadsNowTx(ctx context.Context, tx *sql.Tx, accountID, userID string) (bool, error) {
	return readsNow(ctx, tx, accountID, userID)
}

func readsNow(ctx context.Context, q querier, accountID, userID string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_access g
		 WHERE g.account_id = ? AND g.user_id = ? AND `+store.ReaderSQL("g"), accountID, userID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("workspace: read whether the person reads the mailbox: %w", err)
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
	last, err := lastReaderOf(ctx, tx, userID, lastReaders{accountIDs: accountIDs})
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
	last, err := lastReaderOf(ctx, tx, userID, lastReaders{workspaceID: workspaceID})
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
		 WHERE a.workspace_id = ?1 AND `+store.ReaderSQL("g")+`
		   AND NOT EXISTS (SELECT 1 FROM mailbox_access o
		                    WHERE o.account_id = a.id AND o.user_id <> g.user_id AND `+store.ReaderSQL("o")+`)
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

// ReadBy lists the mailboxes a person holds the read flag on, in any
// workspace, whether or not it counts right now, and whether or not they hold
// the key: what may stop syncing when they go, for a team mailbox they were
// the last to read.
func (r *Repository) ReadBy(ctx context.Context, userID string) ([]string, error) {
	return listIDs(ctx, r.store.Reader(), `SELECT account_id FROM mailbox_access WHERE user_id = ? AND read = 1
		ORDER BY account_id`, userID)
}
