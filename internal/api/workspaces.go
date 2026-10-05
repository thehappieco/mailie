package api

import (
	"net/http"

	"github.com/thehappieco/mailie/internal/service"
)

// Workspaces, their members and invites, and who holds what on each mailbox
// (docs/workspaces.md). Every rule about who may do what is in
// internal/service; these parse the request and render the answer.

func (h *Handler) listWorkspaces(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	out, err := h.Service.ListWorkspaces(q.ctx(), q.principal)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, out)
}

func (h *Handler) createWorkspace(q *request) {
	var req service.CreateWorkspaceRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	out, err := h.Service.CreateWorkspace(q.ctx(), q.principal, req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusCreated, out)
}

func (h *Handler) renameWorkspace(q *request) {
	var req service.RenameWorkspaceRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	out, err := h.Service.RenameWorkspace(q.ctx(), q.principal, q.r.PathValue("id"), req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, out)
}

func (h *Handler) listMembers(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	out, err := h.Service.ListMembers(q.ctx(), q.principal, q.r.PathValue("id"))
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, out)
}

func (h *Handler) setMember(q *request) {
	var req service.MemberRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	out, err := h.Service.SetMember(q.ctx(), q.principal, q.r.PathValue("id"), q.r.PathValue("user"), req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, out)
}

func (h *Handler) removeMember(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	if err := h.Service.RemoveMember(q.ctx(), q.principal, q.r.PathValue("id"), q.r.PathValue("user")); err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusNoContent, nil)
}

func (h *Handler) listTeamInvites(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	out, err := h.Service.ListTeamInvites(q.ctx(), q.principal, q.r.PathValue("id"))
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, out)
}

func (h *Handler) createTeamInvite(q *request) {
	var req service.TeamInviteRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	out, err := h.Service.CreateTeamInvite(q.ctx(), q.principal, q.r.PathValue("id"), req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusCreated, out)
}

func (h *Handler) revokeTeamInvite(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	if err := h.Service.RevokeTeamInvite(q.ctx(), q.principal, q.r.PathValue("id"), q.r.PathValue("invite")); err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusNoContent, nil)
}

func (h *Handler) acceptInvite(q *request) {
	var req service.AcceptInviteRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	out, err := h.Service.AcceptInvite(q.ctx(), q.principal, req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, out)
}

func (h *Handler) accessDirectory(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	out, err := h.Service.AccessDirectory(q.ctx(), q.principal, q.r.PathValue("id"))
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, out)
}

func (h *Handler) setAccess(q *request) {
	var req service.GrantRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	out, err := h.Service.SetAccess(q.ctx(), q.principal, q.r.PathValue("id"), q.r.PathValue("user"), req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, out)
}

// revokeAccess serves DELETE /v1/accounts/{id}/access/{user}; ?flags= names
// the flags to take away, comma-separated, and every one when absent.
func (h *Handler) revokeAccess(q *request) {
	params, err := q.query("flags")
	if err != nil {
		q.fail(err)
		return
	}
	drop, err := service.ParseFlags(params["flags"])
	if err != nil {
		q.fail(err)
		return
	}
	if err := h.Service.RevokeAccess(q.ctx(), q.principal, q.r.PathValue("id"), q.r.PathValue("user"), drop); err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusNoContent, nil)
}

func (h *Handler) takeOver(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	out, err := h.Service.TakeOver(q.ctx(), q.principal, q.r.PathValue("id"))
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, out)
}
