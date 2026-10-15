package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Reading the journal of what was done to mailboxes as an extension's
// outbox: per workspace, in order of seq, from a cursor
// (mailbox_events_workspace). An extension reports each entry elsewhere and
// moves its cursor once it was taken; the entry stays for its retention
// (MailboxJournalRetention) whatever the extension does.

// MaxJournalPage is the most entries JournalOf returns at once.
const MaxJournalPage = 500

// JournalRow is one entry of the journal as an extension reads it: the
// event, the person behind its actor and who read the mailbox then.
type JournalRow struct {
	MailboxEvent
	// ActorUserID is the person the actor names: the person themself for
	// "usr_…", and, for "key:<prefix>", the person who created the key while
	// its row is there; empty for every other actor ("cli", "sweep",
	// "directory", one forgotten).
	ActorUserID string
	// Readers are who read the mailbox when it was done, by id.
	Readers []string
}

// JournalOf lists a workspace's entries after seq after, oldest first, at
// most limit (1 to MaxJournalPage; out of that range, MaxJournalPage), each
// with the readers it recorded, read at one moment.
func (s *Store) JournalOf(ctx context.Context, workspaceID string, after int64, limit int) ([]JournalRow, error) {
	if limit < 1 || limit > MaxJournalPage {
		limit = MaxJournalPage
	}
	tx, err := s.ReadTx(ctx)
	if err != nil {
		return nil, err
	}
	//nolint:errcheck // a read-only transaction: rolling back loses nothing
	defer func() { _ = tx.Rollback() }()
	out, err := journalRowsTx(ctx, tx, workspaceID, after, limit)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	if err := journalReadersTx(ctx, tx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// journalRowsTx reads JournalOf's entries, in the caller's read.
func journalRowsTx(ctx context.Context, tx *sql.Tx, workspaceID string, after int64, limit int) ([]JournalRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT e.seq, e.id, e.kind, e.workspace_id, e.account_id, e.label, e.hint,
		       e.actor, e.created_at, e.cause, e.lost_user, e.purge_after,
		       CASE WHEN e.actor GLOB 'usr_*' THEN e.actor
		            WHEN e.actor GLOB 'key:*' THEN coalesce((SELECT k.created_by FROM api_keys k
		                                                      WHERE k.prefix = substr(e.actor, 5)
		                                                        AND k.created_by GLOB 'usr_*'), '')
		            ELSE '' END
		  FROM mailbox_events e
		 WHERE e.workspace_id = ? AND e.seq > ?
		 ORDER BY e.seq LIMIT ?`, workspaceID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("store: read the journal: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	out := []JournalRow{}
	for rows.Next() {
		var (
			r    JournalRow
			kind string
		)
		if err := rows.Scan(&r.Seq, &r.ID, &kind, &r.WorkspaceID, &r.AccountID, &r.Label, &r.Hint, &r.Actor, &r.At,
			&r.Cause, &r.LostUser, &r.PurgeAfter, &r.ActorUserID); err != nil {
			return nil, fmt.Errorf("store: read the journal: %w", err)
		}
		r.Kind = MailboxEventKind(kind)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read the journal: %w", err)
	}
	return out, nil
}

// journalReadersTx fills in the readers of every entry of out, in the
// caller's read.
func journalReadersTx(ctx context.Context, tx *sql.Tx, out []JournalRow) error {
	seqs := make([]int64, len(out))
	at := make(map[int64]int, len(out))
	for i, r := range out {
		seqs[i] = r.Seq
		at[r.Seq] = i
	}
	list, err := json.Marshal(seqs)
	if err != nil {
		return fmt.Errorf("store: encode seqs: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT event_seq, user_id FROM mailbox_event_readers
		WHERE event_seq IN (SELECT value FROM json_each(?)) ORDER BY event_seq, user_id`, string(list))
	if err != nil {
		return fmt.Errorf("store: read the journal's readers: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			seq    int64
			userID string
		)
		if err := rows.Scan(&seq, &userID); err != nil {
			return fmt.Errorf("store: read the journal's readers: %w", err)
		}
		i := at[seq]
		out[i].Readers = append(out[i].Readers, userID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: read the journal's readers: %w", err)
	}
	return nil
}

// JournalHead is the seq of the newest entry of the journal, 0 when it holds
// none.
func (s *Store) JournalHead(ctx context.Context) (int64, error) {
	var seq int64
	if err := s.r.QueryRowContext(ctx, `SELECT coalesce(max(seq), 0) FROM mailbox_events`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("store: read the journal's head: %w", err)
	}
	return seq, nil
}

// FollowedWorkspace is a workspace from another source than the local one:
// what an extension reports on.
type FollowedWorkspace struct {
	ID   string
	Kind string
	// PersonID is a personal workspace's person; empty for a team.
	PersonID string
	Closed   bool
}

// FollowedWorkspaces lists every workspace whose source is not the local
// one, the oldest first.
func (s *Store) FollowedWorkspaces(ctx context.Context) ([]FollowedWorkspace, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT id, kind, coalesce(person_id, ''), closed_at <> 0 FROM workspaces
		WHERE source <> 'local' ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list the workspaces from another source: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	var out []FollowedWorkspace
	for rows.Next() {
		var w FollowedWorkspace
		if err := rows.Scan(&w.ID, &w.Kind, &w.PersonID, &w.Closed); err != nil {
			return nil, fmt.Errorf("store: list the workspaces from another source: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list the workspaces from another source: %w", err)
	}
	return out, nil
}
