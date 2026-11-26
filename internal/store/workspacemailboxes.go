package store

import (
	"context"
	"fmt"
)

// WorkspaceMailboxes counts the mailboxes outside the operator workspace: a
// person's personal workspace's and a team's, each live or removed and kept
// until deleted for good (migration 0020). An instance key reaches the
// operator workspace's mailboxes alone, and a mailbox never moves to another
// workspace (0008's accounts_workspace_fixed), so these are what no instance
// key will ever reach.
type WorkspaceMailboxes struct {
	PersonalLive, PersonalRemoved int
	TeamLive, TeamRemoved         int
}

// Total is every mailbox counted, live and removed.
func (n WorkspaceMailboxes) Total() int {
	return n.PersonalLive + n.PersonalRemoved + n.TeamLive + n.TeamRemoved
}

// CountWorkspaceMailboxes counts the mailboxes outside the operator
// workspace, whatever their state, sync, pause or hold, a closed or withheld
// team's included: what the daemon says at start of a server whose
// workspaces people made.
func (s *Store) CountWorkspaceMailboxes(ctx context.Context) (WorkspaceMailboxes, error) {
	var n WorkspaceMailboxes
	// The schema allows three kinds of workspace (0008), so whatever is
	// neither the operator's nor personal is a team's.
	err := s.r.QueryRowContext(ctx, `SELECT
		coalesce(sum(w.kind = 'personal' AND a.removed_at = 0), 0),
		coalesce(sum(w.kind = 'personal' AND a.removed_at <> 0), 0),
		coalesce(sum(w.kind <> 'personal' AND a.removed_at = 0), 0),
		coalesce(sum(w.kind <> 'personal' AND a.removed_at <> 0), 0)
		FROM accounts a JOIN workspaces w ON w.id = a.workspace_id
		WHERE w.kind <> 'operator'`).Scan(&n.PersonalLive, &n.PersonalRemoved, &n.TeamLive, &n.TeamRemoved)
	if err != nil {
		return WorkspaceMailboxes{}, fmt.Errorf("store: count the mailboxes outside the operator workspace: %w", err)
	}
	return n, nil
}
