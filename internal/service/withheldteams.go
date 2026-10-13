package service

import (
	"context"
	"database/sql"
	"errors"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Withholding teams (docs/workspaces.md, "Editions that withhold teams"). An
// edition of the daemon that does not offer team workspaces yet sets
// Deps.TeamsWithheld, which `mailserver serve` never does: it is a choice of
// the binary that embeds the daemon, not of the workspace source, and it
// holds whatever the database holds, a restored one included.
//
// Where teams are withheld nothing of a team is used, by anyone: no mailbox
// is linked into one, and its mailboxes reach nobody, its keys included —
// they are listed nowhere, and one asked for by a person or a key who would
// otherwise see it is refused by name; nobody creates a team, changes its
// members, invites or name, accepts an invite into one, creates, lists or
// revokes its keys, gives or takes what anyone holds on its mailboxes,
// gives or withdraws its consent to sync, or writes or supplies a mailbox
// key there. A person and a key are refused it as not_authorized, once they
// would otherwise reach it; the operator still reads a team's members, its
// invites and who holds what, and changes none of it. Teams are listed, to
// whoever would list them, marked withheld, so a client says why they are
// there and unusable. Personal workspaces and the operator's are as they
// were.
//
// What is withheld is the use, never the data: nothing of a team is deleted
// or changed by withholding it, and an edition that offers teams again finds
// them as they were.

// errTeamsWithheld is a team asked to be used where teams are withheld.
var errTeamsWithheld = E(CodeNotAuthorized, "this server does not offer team workspaces yet", nil)

// teamWithheld refuses a team where teams are withheld.
func (s *Service) teamWithheld(w workspace.Workspace) error {
	if s.teamsWithheld && w.Kind == workspace.KindTeam {
		return errTeamsWithheld
	}
	return nil
}

// workspaceWithheld refuses a workspace that is a team where teams are
// withheld. A workspace's kind never changes (migration 0016), so what it
// reads holds for the rest of the request.
func (s *Service) workspaceWithheld(ctx context.Context, workspaceID string) error {
	if !s.teamsWithheld || s.workspaces == nil {
		return nil
	}
	w, err := s.workspaces.Get(ctx, workspaceID)
	switch {
	case errors.Is(err, workspace.ErrNotFound):
		return errNoWorkspace
	case err != nil:
		return E(CodeInternal, "reading the workspace failed", err)
	}
	return s.teamWithheld(w)
}

// mailboxWithheld refuses a team's mailbox where teams are withheld. A
// personal mailbox names its person, and the operator's mailboxes are in the
// operator workspace: neither is a team's.
func (s *Service) mailboxWithheld(ctx context.Context, a account.Account) error {
	if !s.teamsWithheld || a.OwnerUserID != "" || a.WorkspaceID == workspace.OperatorID {
		return nil
	}
	return s.workspaceWithheld(ctx, a.WorkspaceID)
}

// withheldTx is workspaceWithheld inside a write's transaction.
func (s *Service) withheldTx(ctx context.Context, tx *sql.Tx, workspaceID string) error {
	if !s.teamsWithheld {
		return nil
	}
	w, err := workspace.GetTx(ctx, tx, workspaceID)
	switch {
	case errors.Is(err, workspace.ErrNotFound):
		return errNoWorkspace
	case err != nil:
		return err
	}
	return s.teamWithheld(w)
}
