package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/thehappieco/mailie/internal/service"
)

// Actions on messages, and the consent they need. Who may change which
// mailbox, whether its owner allowed it, and everything said to the mail
// server are decided in internal/service; this file parses bodies and speaks
// HTTP.

// flagsBody is PATCH /v1/messages/{id}: the flags to change on one message.
type flagsBody struct {
	Seen    *bool `json:"seen,omitempty"`
	Flagged *bool `json:"flagged,omitempty"`
}

// patchMessage serves PATCH /v1/messages/{id}.
func (h *Handler) patchMessage(q *request) {
	var body flagsBody
	if err := q.decode(&body); err != nil {
		q.fail(err)
		return
	}
	res, err := h.Service.SetFlags(q.ctx(), q.principal, service.SetFlagsRequest{
		IDs: []int64{messageID(q)}, Seen: body.Seen, Flagged: body.Flagged,
	})
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, res)
}

// setFlags serves POST /v1/messages/flags: the same change on several
// messages of one account.
func (h *Handler) setFlags(q *request) {
	var req service.SetFlagsRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	res, err := h.Service.SetFlags(q.ctx(), q.principal, req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, res)
}

// moveBody is POST /v1/messages/move. To is "archive", "inbox" or a folder
// id; the id is taken as a JSON string, as documented, or as a number.
type moveBody struct {
	IDs []int64         `json:"ids"`
	To  json.RawMessage `json:"to"`
}

// moveMessages serves POST /v1/messages/move.
func (h *Handler) moveMessages(q *request) {
	var body moveBody
	if err := q.decode(&body); err != nil {
		q.fail(err)
		return
	}
	to, ok := destination(body.To)
	if !ok {
		q.fail(service.E(service.CodeBadRequest, "to must be archive, inbox or a folder id", nil))
		return
	}
	res, err := h.Service.MoveMessages(q.ctx(), q.principal, service.MoveRequest{IDs: body.IDs, To: to})
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, res)
}

// destination reads "to": a string, or a whole number standing for a
// folder id.
func destination(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return strconv.FormatInt(n, 10), true
	}
	return "", false
}

// trashMessages serves POST /v1/messages/trash.
func (h *Handler) trashMessages(q *request) {
	var req service.TrashRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	res, err := h.Service.TrashMessages(q.ctx(), q.principal, req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, res)
}

func (h *Handler) actionsConsent(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	consent, err := h.Service.ActionsConsent(q.ctx(), q.principal)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, consent)
}

func (h *Handler) grantActionsConsent(q *request) {
	var req service.ActionsConsentRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	consent, err := h.Service.GrantActionsConsent(q.ctx(), q.principal, req.Version)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, consent)
}

func (h *Handler) withdrawActionsConsent(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	consent, err := h.Service.WithdrawActionsConsent(q.ctx(), q.principal)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, consent)
}
