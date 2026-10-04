package api

import (
	"net/http"

	"github.com/thehappieco/mailie/internal/service"
)

// A person's own API keys: the ones they create in the console for a tool to
// use. Only the person, signed in, may list, create or revoke them — the
// service refuses a key, so no key can mint another — and every rule about
// what a key may reach lives there.

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

func (h *Handler) createMyAPIKey(q *request) {
	var req service.PersonalKeyRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	created, err := h.Service.CreateMyAPIKey(q.ctx(), q.principal, req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusCreated, created)
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
