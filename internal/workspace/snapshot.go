package workspace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/thehappieco/mailie/internal/store"
)

// A snapshot of who holds what on a workspace's mailboxes, for an extension
// that reports it elsewhere (docs/workspaces.md, "Following an external
// directory"): every mailbox, live and removed until it is deleted for good,
// with why it does not sync, its label and masked hint (never its address),
// and every person who holds a flag on it or a sealed grant, with what the
// one reading rule says of them. It decides nothing: the rules are
// internal/store's fragments, read in one transaction.

// The bounds of a snapshot: more mailboxes in the workspace, or more people
// on one mailbox, is ErrSnapshotTooLarge.
const (
	SnapshotMaxMailboxes = 1000
	SnapshotMaxPeople    = 5000
)

// ErrSnapshotTooLarge is a workspace with more mailboxes, or a mailbox with
// more people, than a snapshot holds.
var ErrSnapshotTooLarge = errors.New("workspace: the workspace is larger than a snapshot holds")

// SnapshotOptions say how a snapshot reads.
type SnapshotOptions struct {
	// RemovedAsLive reports a removed mailbox's people as for a live one:
	// who would read it, wait for its key and hold its key once recovered
	// (store.KeptReaderSQL, KeptWaitingSQL, a grant at its current epoch),
	// and its kept last readers as protected. Off, a removed mailbox has
	// nobody reading, waiting, holding its key or protected, as the rules
	// here decide (store.LiveSQL). Either way nothing here changes: a
	// removed mailbox protects nobody in what this server refuses.
	RemovedAsLive bool
	// Withheld is the edition withholding teams (store.PauseSQL).
	Withheld bool
}

// Snapshot is a workspace's mailboxes and who holds what on them, read at
// one moment.
type Snapshot struct {
	WorkspaceID string
	Kind        Kind
	Closed      bool
	Mailboxes   []SnapshotMailbox
}

// SnapshotMailbox is one mailbox of a snapshot.
type SnapshotMailbox struct {
	ID string
	// Label is its name for the journal (store.MailboxLabel), Hint its
	// address masked (store.MailboxHint); never the address.
	Label, Hint string
	// Removed is a mailbox removed and kept, until PurgeAfter (unix
	// seconds).
	Removed    bool
	PurgeAfter int64
	// Pause is why a live mailbox does not sync (store.Pause*), empty when
	// it does and on a removed one.
	Pause string
	// People hold a flag on it or a sealed grant, by id.
	People []SnapshotPerson
	// Protected are the people a team mailbox cannot lose: each the only
	// one who reads it. None in a personal workspace.
	Protected []string
}

// SnapshotPerson is what a person holds on a mailbox, and what the reading
// rule says of them.
type SnapshotPerson struct {
	UserID string
	// Flags are the stored flags; none for someone who holds only a grant.
	Flags Flags
	// Reads is reading it now (store.ReaderSQL); Waiting, holding read
	// without its key yet (store.WaitingSQL); Enrolled, having an account
	// key; HoldsKey, holding a sealed grant at its current epoch, whether or
	// not they read it, a suspended person included.
	Reads, Waiting, Enrolled, HoldsKey bool
}

// SnapshotTx reads a workspace's snapshot inside the caller's transaction,
// which a read-only one keeps to one moment (store.ReadTx). ErrNotFound for
// a workspace nobody knows; ErrSnapshotTooLarge past its bounds.
func SnapshotTx(ctx context.Context, tx *sql.Tx, workspaceID string, o SnapshotOptions) (Snapshot, error) {
	out := Snapshot{WorkspaceID: workspaceID}
	var kind string
	err := tx.QueryRowContext(ctx, `SELECT kind, closed_at <> 0 FROM workspaces WHERE id = ?`, workspaceID).
		Scan(&kind, &out.Closed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Snapshot{}, ErrNotFound
	case err != nil:
		return Snapshot{}, fmt.Errorf("workspace: read the workspace: %w", err)
	}
	out.Kind = Kind(kind)
	if out.Mailboxes, err = snapshotMailboxes(ctx, tx, workspaceID, o); err != nil {
		return Snapshot{}, err
	}
	index := make(map[string]int, len(out.Mailboxes))
	for i, mb := range out.Mailboxes {
		index[mb.ID] = i
	}
	if err := snapshotPeople(ctx, tx, workspaceID, o, out.Mailboxes, index); err != nil {
		return Snapshot{}, err
	}
	if out.Kind == KindTeam {
		if err := snapshotProtected(ctx, tx, workspaceID, o, out.Mailboxes, index); err != nil {
			return Snapshot{}, err
		}
	}
	return out, nil
}

// snapshotMailboxes reads a workspace's mailboxes, oldest first.
func snapshotMailboxes(ctx context.Context, tx *sql.Tx, workspaceID string, o SnapshotOptions) ([]SnapshotMailbox, error) {
	//nolint:gosec // G202: PauseSQL is built from constants alone; every value is bound
	rows, err := tx.QueryContext(ctx, `SELECT a.id, a.display_name, a.email, a.removed_at <> 0, a.purge_after,
		  `+store.PauseSQL(o.Withheld)+`
		  FROM accounts a WHERE a.workspace_id = ? ORDER BY a.created_at, a.rowid LIMIT ?`,
		workspaceID, SnapshotMaxMailboxes+1)
	if err != nil {
		return nil, fmt.Errorf("workspace: read the snapshot's mailboxes: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	out := []SnapshotMailbox{}
	for rows.Next() {
		var (
			mb          SnapshotMailbox
			name, email string
		)
		if err := rows.Scan(&mb.ID, &name, &email, &mb.Removed, &mb.PurgeAfter, &mb.Pause); err != nil {
			return nil, fmt.Errorf("workspace: read the snapshot's mailboxes: %w", err)
		}
		mb.Label, mb.Hint = store.MailboxLabel(name, email), store.MailboxHint(email)
		if mb.Removed {
			// A removed mailbox is what it is: kept until PurgeAfter.
			mb.Pause = ""
		}
		mb.People, mb.Protected = []SnapshotPerson{}, []string{}
		out = append(out, mb)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: read the snapshot's mailboxes: %w", err)
	}
	if len(out) > SnapshotMaxMailboxes {
		return nil, fmt.Errorf("%w: more than %d mailboxes", ErrSnapshotTooLarge, SnapshotMaxMailboxes)
	}
	return out, nil
}

// snapshotPeople reads who holds a flag or a sealed grant on each mailbox of
// the workspace, into mailboxes.
func snapshotPeople(ctx context.Context, tx *sql.Tx, workspaceID string, o SnapshotOptions, mailboxes []SnapshotMailbox,
	index map[string]int,
) error {
	holds := store.CurrentGrantSQL("p.account_id", "p.user_id")
	//nolint:gosec // G202: built from the rule's fragments alone; every value is bound
	rows, err := tx.QueryContext(ctx, `WITH p(account_id, user_id) AS (
		    SELECT account_id, user_id FROM mailbox_access WHERE workspace_id = ?1
		    UNION SELECT account_id, user_id FROM mailbox_grants WHERE workspace_id = ?1)
		SELECT p.account_id, p.user_id,
		       coalesce(g.read, 0), coalesce(g.act, 0), coalesce(g.send, 0), coalesce(g.manage, 0),
		       CASE WHEN g.account_id IS NULL THEN 0
		            WHEN a.removed_at = 0 THEN `+store.ReaderSQL("g")+`
		            WHEN ?2 THEN `+store.KeptReaderSQL("g")+` ELSE 0 END,
		       CASE WHEN g.account_id IS NULL THEN 0
		            WHEN a.removed_at = 0 THEN `+store.WaitingSQL("g")+`
		            WHEN ?2 THEN `+store.KeptWaitingSQL("g")+` ELSE 0 END,
		       u.public_key IS NOT NULL,
		       CASE WHEN a.removed_at = 0 OR ?2 THEN `+holds+` ELSE 0 END
		  FROM p
		  JOIN accounts a ON a.id = p.account_id
		  JOIN users u ON u.id = p.user_id
		  LEFT JOIN mailbox_access g ON g.account_id = p.account_id AND g.user_id = p.user_id
		 ORDER BY a.created_at, a.rowid, p.user_id`, workspaceID, o.RemovedAsLive)
	if err != nil {
		return fmt.Errorf("workspace: read the snapshot's people: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			accountID string
			p         SnapshotPerson
		)
		if err := rows.Scan(&accountID, &p.UserID, &p.Flags.Read, &p.Flags.Act, &p.Flags.Send, &p.Flags.Manage,
			&p.Reads, &p.Waiting, &p.Enrolled, &p.HoldsKey); err != nil {
			return fmt.Errorf("workspace: read the snapshot's people: %w", err)
		}
		i, ok := index[accountID]
		if !ok {
			// A grant on a mailbox of another workspace than its row says:
			// the schema keeps them equal.
			continue
		}
		if len(mailboxes[i].People) == SnapshotMaxPeople {
			return fmt.Errorf("%w: more than %d people on a mailbox", ErrSnapshotTooLarge, SnapshotMaxPeople)
		}
		mailboxes[i].People = append(mailboxes[i].People, p)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("workspace: read the snapshot's people: %w", err)
	}
	return nil
}

// snapshotProtected reads, for each mailbox of a team, the people who are
// the only one reading it, into mailboxes: by the one rule on a live
// mailbox, and, on a removed one reported as live, by the readers it would
// have once recovered.
func snapshotProtected(ctx context.Context, tx *sql.Tx, workspaceID string, o SnapshotOptions, mailboxes []SnapshotMailbox,
	index map[string]int,
) error {
	//nolint:gosec // G202: built from the rule's fragments alone; every value is bound
	rows, err := tx.QueryContext(ctx, `SELECT a.id, g.user_id FROM accounts a
		  JOIN mailbox_access g ON g.account_id = a.id
		 WHERE a.workspace_id = ?1 AND CASE
		   WHEN a.removed_at = 0 THEN `+store.ReaderSQL("g")+`
		     AND NOT EXISTS (SELECT 1 FROM mailbox_access o
		                      WHERE o.account_id = a.id AND o.user_id <> g.user_id AND `+store.ReaderSQL("o")+`)
		   WHEN ?2 THEN `+store.KeptReaderSQL("g")+`
		     AND NOT EXISTS (SELECT 1 FROM mailbox_access o
		                      WHERE o.account_id = a.id AND o.user_id <> g.user_id AND `+store.KeptReaderSQL("o")+`)
		   ELSE 0 END
		 ORDER BY a.created_at, a.rowid, g.user_id`, workspaceID, o.RemovedAsLive)
	if err != nil {
		return fmt.Errorf("workspace: read the snapshot's last readers: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var accountID, userID string
		if err := rows.Scan(&accountID, &userID); err != nil {
			return fmt.Errorf("workspace: read the snapshot's last readers: %w", err)
		}
		if i, ok := index[accountID]; ok {
			mailboxes[i].Protected = append(mailboxes[i].Protected, userID)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("workspace: read the snapshot's last readers: %w", err)
	}
	return nil
}
