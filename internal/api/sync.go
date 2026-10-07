package api

import (
	"net/http"

	"github.com/thehappieco/mailie/internal/service"
)

// Consent to sync, and each account's sync: its status, a pass on request,
// and the operator's switch for a mailbox nobody owns. Who may do which is
// decided in internal/service.

func (h *Handler) syncConsent(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	consent, err := h.Service.SyncConsent(q.ctx(), q.principal)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, consent)
}

func (h *Handler) grantSyncConsent(q *request) {
	var req service.SyncConsentRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	consent, err := h.Service.GrantSyncConsent(q.ctx(), q.principal, req.Version)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, consent)
}

func (h *Handler) withdrawSyncConsent(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	consent, err := h.Service.WithdrawSyncConsent(q.ctx(), q.principal)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, consent)
}

func (h *Handler) syncStatus(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	status, err := h.Service.SyncStatus(q.ctx(), q.principal, q.r.PathValue("id"))
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, status)
}

// triggerSync answers 202: the pass is asked for, not waited on. The body is
// the status as it stands, so a console can show the request was taken.
func (h *Handler) triggerSync(q *request) {
	if err := q.decodeOptional(&struct{}{}); err != nil {
		q.fail(err)
		return
	}
	id := q.r.PathValue("id")
	if err := h.Service.TriggerSync(q.ctx(), q.principal, id); err != nil {
		q.fail(err)
		return
	}
	status, err := h.Service.SyncStatus(q.ctx(), q.principal, id)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusAccepted, status)
}

func (h *Handler) setMailboxSync(q *request) {
	var req service.MailboxSyncRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	status, err := h.Service.SetMailboxSync(q.ctx(), q.principal, q.r.PathValue("id"), req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, status)
}
