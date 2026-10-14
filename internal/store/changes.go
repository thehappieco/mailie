package store

import (
	"context"
	"fmt"
)

// Which workspaces from another source changed what an extension reports of
// them (migration 0023): the schema's triggers move a workspace's mark to a
// new seq in the transaction of every change to its mailboxes, who holds
// what on them, its members and their state, its name and closure, and its
// journal. A local workspace is never marked.

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
