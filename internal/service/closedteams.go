package service

import (
	"context"
	"errors"
	"strings"

	"github.com/thehappieco/mailie/internal/workspace"
)

// Closing a team (docs/workspaces.md, "Closed teams"). While a team is
// closed nothing in it is used: its mailboxes reach nobody, its owners, its
// admins and its keys included, and sync nothing, keeping their index, their
// consent to sync, their grants and the keys that hold them; nothing in it
// is linked, granted, keyed, invited or otherwise changed, by anyone, the
// operator included. Its members still list it, with when it closed, and the
// operator still reads its members and who holds what. Reopened, it is as it
// was: what was kept works again, and its mailboxes catch up.
//
// A local team closes and reopens only through the operator, with an
// unrestricted instance admin key (CloseWorkspace, ReopenWorkspace): no
// person closes one, an owner of it included. A team whose source is not
// local is closed and reopened where it is managed, which an extension of
// the daemon follows (CloseTeam, ReopenTeam), as it follows a suspension:
// the operator is refused it here.

// CloseWorkspace closes a local team, for the operator: nothing in it is
// used until ReopenWorkspace. Closing a closed team is not an error. It
// answers the team as the operator lists it, without its counts.
func (s *Service) CloseWorkspace(ctx context.Context, p Principal, id string) (Workspace, error) {
	return s.closeLocal(ctx, p, id, true)
}

// ReopenWorkspace reopens a local team the operator closed, as it was.
// Reopening an open team is not an error.
func (s *Service) ReopenWorkspace(ctx context.Context, p Principal, id string) (Workspace, error) {
	return s.closeLocal(ctx, p, id, false)
}

func (s *Service) closeLocal(ctx context.Context, p Principal, id string, closed bool) (Workspace, error) {
	if !isOperator(p) {
		return Workspace{}, errCloseOperator
	}
	if s.workspaces == nil {
		return Workspace{}, errNoWorkspace
	}
	// A workspace's kind and source never change (migration 0016): read
	// before the write, they are what the write finds.
	w, err := s.workspaces.Get(ctx, id)
	switch {
	case errors.Is(err, workspace.ErrNotFound):
		return Workspace{}, errNoWorkspace
	case err != nil:
		return Workspace{}, E(CodeInternal, "reading the workspace failed", err)
	case w.Kind != workspace.KindTeam:
		return Workspace{}, errOnlyTeamCloses
	case w.Source != workspace.SourceLocal:
		return Workspace{}, fromWorkspace(workspace.ErrManagedElsewhere, "")
	}
	if err := s.workspaces.Source().Changeable(); err != nil {
		return Workspace{}, fromWorkspace(err, "closing the team failed")
	}
	if _, err := s.setClosed(ctx, id, closed, p.Actor()); err != nil {
		return Workspace{}, err
	}
	w, err = s.workspaces.Get(ctx, id)
	if err != nil {
		return Workspace{}, E(CodeInternal, "reading the workspace failed", err)
	}
	return presentWorkspace(w, false), nil
}

// CloseTeam closes a team, for an extension of the daemon that follows a
// decision taken elsewhere, as SuspendPerson does: no route or command of
// the core reaches it, it takes no principal, and it closes any team, local
// or not. It reports whether the team was open.
func (s *Service) CloseTeam(ctx context.Context, id string) (bool, error) {
	return s.setTeamClosed(ctx, id, true)
}

// ReopenTeam reopens a team, for the extension that closed it, as it was.
// It reports whether the team was closed.
func (s *Service) ReopenTeam(ctx context.Context, id string) (bool, error) {
	return s.setTeamClosed(ctx, id, false)
}

func (s *Service) setTeamClosed(ctx context.Context, id string, closed bool) (bool, error) {
	if strings.TrimSpace(id) == "" {
		return false, E(CodeBadRequest, "closing or reopening a team needs the team", nil)
	}
	if s.workspaces == nil {
		return false, errNoWorkspace
	}
	return s.setClosed(ctx, id, closed, "extension")
}

// setClosed closes or reopens a team and, once that has committed, tells
// whatever answers for its mailboxes: access may have changed for everyone
// who held something there, and the sync engine stops or starts them.
func (s *Service) setClosed(ctx context.Context, id string, closed bool, by string) (bool, error) {
	changed, accounts, err := s.workspaces.SetClosed(ctx, id, closed)
	switch {
	case errors.Is(err, workspace.ErrNotFound):
		return false, errNoWorkspace
	case errors.Is(err, workspace.ErrPersonal), errors.Is(err, workspace.ErrOperator):
		return false, errOnlyTeamCloses
	case err != nil:
		return false, E(CodeInternal, "closing or reopening the team failed", err)
	}
	if !changed {
		return false, nil
	}
	s.accessChanged()
	s.reconcile(accounts...)
	if closed {
		s.log.Info("team closed: its mailboxes reach nobody and sync nothing until it reopens",
			"workspace", id, "by", by, "mailboxes", len(accounts))
	} else {
		s.log.Info("team reopened", "workspace", id, "by", by, "mailboxes", len(accounts))
	}
	return true, nil
}
