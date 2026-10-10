package api

import (
	"net/http"

	"github.com/thehappieco/mailie/internal/service"
)

// A mailbox's key (docs/key-scheme.md sections 8, 9 and 12.11 to 12.15), for
// a person signed in: what their console reads to seal and open grants, the
// first key of a mailbox that has none, a personal mailbox's next one, and the
// key handed to a member who holds read without it. The service decides who
// may, and refuses an API key.

// mailboxKey serves GET /v1/accounts/{id}/mailbox-key.
func (h *Handler) mailboxKey(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	out, err := h.Service.MailboxKey(q.ctx(), q.principal, q.r.PathValue("id"))
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, out)
}

// writeFirstKey serves POST /v1/accounts/{id}/mailbox-key.
func (h *Handler) writeFirstKey(q *request) {
	var req service.FirstKeyRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	out, err := h.Service.WriteFirstKey(q.ctx(), q.principal, q.r.PathValue("id"), req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusCreated, out)
}

// writeNewKey serves PUT /v1/accounts/{id}/mailbox-key.
func (h *Handler) writeNewKey(q *request) {
	var req service.NewKeyRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	out, err := h.Service.WriteNewKey(q.ctx(), q.principal, q.r.PathValue("id"), req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, out)
}

// supplyKey serves PUT /v1/accounts/{id}/grants/{user}.
func (h *Handler) supplyKey(q *request) {
	var req service.SupplyKeyRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	out, err := h.Service.SupplyKey(q.ctx(), q.principal, q.r.PathValue("id"), q.r.PathValue("user"), req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, out)
}
