package store

import (
	"context"
	"fmt"
)

// Which workspaces from another source changed what an extension reports of
// them (migrations 0023 and 0027): the schema's triggers move a workspace's
// mark to a new seq in the transaction of every change to its mailboxes and
// what is reported of each (their state and its reason, name, removal,
// attribution, sync consent, agreement, hold and pause, sent copy, host,
// first connection and server), who holds what on them, its members and
// their state, its name and closure, and its journal. How much of a mailbox
// is indexed and when it last synced are measured, never marked. A local
// workspace is never marked.

// WorkspaceChange is a workspace's mark: Seq is that of its last change, and
// only rises. The mark of a deleted workspace stays.
type WorkspaceChange struct {
	WorkspaceID string
	Seq         int64
}

// MaxWorkspaceChanges is the most marks WorkspaceChanges returns at once.
const MaxWorkspaceChanges = 1000

// WorkspaceChanges lists the workspaces marked after seq after, in the order
// of their marks, at most limit (1 to MaxWorkspaceChanges; out of that range,
// MaxWorkspaceChanges), and the highest mark there is, 0 when none, both
// read at one moment. Fewer than limit marks listed every one up to that
// head; otherwise the next read goes on from the last one listed. A
// workspace that changes again meanwhile is listed again, with its new mark.
func (s *Store) WorkspaceChanges(ctx context.Context, after int64, limit int) ([]WorkspaceChange, int64, error) {
	if limit < 1 || limit > MaxWorkspaceChanges {
		limit = MaxWorkspaceChanges
	}
	tx, err := s.r.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("store: read the workspace changes: %w", err)
	}
	//nolint:errcheck // a read-only transaction: rolling back loses nothing
	defer func() { _ = tx.Rollback() }()
	var head int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(seq), 0) FROM workspace_changes`).Scan(&head); err != nil {
		return nil, 0, fmt.Errorf("store: read the workspace changes: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT workspace_id, seq FROM workspace_changes WHERE seq > ? ORDER BY seq LIMIT ?`,
		after, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("store: read the workspace changes: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	var out []WorkspaceChange
	for rows.Next() {
		var c WorkspaceChange
		if err := rows.Scan(&c.WorkspaceID, &c.Seq); err != nil {
			return nil, 0, fmt.Errorf("store: read the workspace changes: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: read the workspace changes: %w", err)
	}
	return out, head, nil
}

// Which people an identity signs in changed their own settings as an
// extension reports them (migration 0027): their consents, their account
// key, their last sign-out everywhere, and their notices — given, gone, or
// changed by what they show of their mailbox, its journal entry and its
// actor's membership. Each person has one mark, at the seq of their last
// change, however many notices one write touched. A person no identity
// signs in is never marked, and a deleted person's mark goes with them.

// PersonChange is a person's mark: Seq is that of their last change, and
// only rises.
type PersonChange struct {
	UserID string
	Seq    int64
}

// MaxPersonChanges is the most marks PersonChanges returns at once.
const MaxPersonChanges = 1000

// PersonChanges lists the people marked after seq after, in the order of
// their marks, at most limit (1 to MaxPersonChanges; out of that range,
// MaxPersonChanges), and the highest mark there is, 0 when none, both read
// at one moment: the twin of WorkspaceChanges.
func (s *Store) PersonChanges(ctx context.Context, after int64, limit int) ([]PersonChange, int64, error) {
	if limit < 1 || limit > MaxPersonChanges {
		limit = MaxPersonChanges
	}
	tx, err := s.r.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("store: read the person changes: %w", err)
	}
	//nolint:errcheck // a read-only transaction: rolling back loses nothing
	defer func() { _ = tx.Rollback() }()
	var head int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(seq), 0) FROM person_changes`).Scan(&head); err != nil {
		return nil, 0, fmt.Errorf("store: read the person changes: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT user_id, seq FROM person_changes WHERE seq > ? ORDER BY seq LIMIT ?`,
		after, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("store: read the person changes: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	var out []PersonChange
	for rows.Next() {
		var c PersonChange
		if err := rows.Scan(&c.UserID, &c.Seq); err != nil {
			return nil, 0, fmt.Errorf("store: read the person changes: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: read the person changes: %w", err)
	}
	return out, head, nil
}
