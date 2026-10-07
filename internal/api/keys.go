package api

import (
	"net/http"

	"github.com/thehappieco/mailie/internal/service"
)

// A workspace's API keys, which its owners and admins create, list and revoke
// signed in to the console, and the keys a person created, which they list
// and revoke. The service refuses a key on every one of these, so no key can
// mint, see or change another, and every rule about what a key may reach
// lives there.

func (h *Handler) listWorkspaceKeys(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	keys, err := h.Service.ListWorkspaceKeys(q.ctx(), q.principal, q.r.PathValue("id"))
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, keys)
}

func (h *Handler) createWorkspaceKey(q *request) {
	var req service.WorkspaceKeyRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	created, err := h.Service.CreateWorkspaceKey(q.ctx(), q.principal, q.r.PathValue("id"), req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusCreated, created)
}

func (h *Handler) revokeWorkspaceKey(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	if err := h.Service.RevokeWorkspaceKey(q.ctx(), q.principal, q.r.PathValue("id"), q.r.PathValue("prefix")); err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusNoContent, nil)
}

func (h *Handler) setKeyAccess(q *request) {
	var req service.KeyAccessRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	held, err := h.Service.SetKeyAccess(q.ctx(), q.principal, q.r.PathValue("id"), q.r.PathValue("prefix"),
		q.r.PathValue("account"), req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, held)
}

func (h *Handler) revokeKeyAccess(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	if err := h.Service.RevokeKeyAccess(q.ctx(), q.principal, q.r.PathValue("id"), q.r.PathValue("prefix"),
		q.r.PathValue("account")); err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusNoContent, nil)
}

func (h *Handler) listKeySends(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	sends, err := h.Service.ListKeySends(q.ctx(), q.principal, q.r.PathValue("id"), q.r.PathValue("prefix"))
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, sends)
}

func (h *Handler) listMyAPIKeys(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	keys, err := h.Service.ListMyAPIKeys(q.ctx(), q.principal)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, keys)
}

// createMyAPIKey answers a console of before: keys are created in a
// workspace now. The body is not read.
func (h *Handler) createMyAPIKey(q *request) {
	q.fail(h.Service.CreateMyAPIKey(q.ctx(), q.principal))
}

func (h *Handler) revokeMyAPIKey(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	if err := h.Service.RevokeMyAPIKey(q.ctx(), q.principal, q.r.PathValue("prefix")); err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusNoContent, nil)
}

func (h *Handler) mcpAccess(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	access, err := h.Service.MCPAccess(q.ctx(), q.principal)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, access)
}
