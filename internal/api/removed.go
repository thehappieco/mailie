package api

import (
	"net/http"

	"github.com/thehappieco/mailie/internal/service"
)

// Removed mailboxes and a person's notices (docs/workspaces.md, "Removed
// mailboxes"). A removed mailbox is kept 30 days: its workspace's owners list
// it, recover it or delete it for good, and the operator lists and deletes any
// workspace's. Everyone who read it, but whoever removed it, is told with a
// notice. Who may do which is decided in internal/service.

func (h *Handler) listRemovedMailboxes(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	removed, err := h.Service.ListRemovedMailboxes(q.ctx(), q.principal, q.r.PathValue("id"))
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, removed)
}

// recoverAccount answers the mailbox recovered, live again, its sync held
// until someone who may turns it on.
func (h *Handler) recoverAccount(q *request) {
	if err := q.decodeOptional(&struct{}{}); err != nil {
		q.fail(err)
		return
	}
	a, err := h.Service.RecoverAccount(q.ctx(), q.principal, q.r.PathValue("id"))
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, a)
}

// purgeAccount deletes a removed mailbox for good: the body repeats its id
// in confirm, or nothing is deleted.
func (h *Handler) purgeAccount(q *request) {
	var req service.PurgeAccountRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	if err := h.Service.PurgeAccount(q.ctx(), q.principal, q.r.PathValue("id"), req); err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusNoContent, nil)
}

func (h *Handler) listNotices(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	notices, err := h.Service.Notices(q.ctx(), q.principal)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, notices)
}

func (h *Handler) dismissNotice(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	if err := h.Service.DismissNotice(q.ctx(), q.principal, q.r.PathValue("id")); err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusNoContent, nil)
}
