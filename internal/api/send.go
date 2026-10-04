package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/thehappieco/mailie/internal/service"
)

// Sending, and the consent it needs. Who may send from which mailbox,
// whether its owner allowed it, the idempotency key and everything said to
// the submission server are decided in internal/service; this file reads the
// multipart body and speaks HTTP.

// maxComposeBytes bounds the compose part: the text and room for the rest.
const maxComposeBytes = service.MaxTextBytes + 256<<10

// errNotMultipart is a send whose body is not what the route documents.
var errNotMultipart = service.E(service.CodeBadRequest,
	"the body must be multipart/form-data: a compose part (JSON) first, then zero or more attachment parts", nil)

// sendMessage serves POST /v1/messages/send.
//
// The compose part is decoded whole before anything else is read, and the
// attachments are handed to the service one at a time as they arrive, so
// nothing is buffered here: the service spools each to disk and removes it
// when the send ends.
func (h *Handler) sendMessage(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	keys := q.r.Header.Values("Idempotency-Key")
	if len(keys) > 1 {
		q.fail(service.E(service.CodeBadRequest, "send one Idempotency-Key header", nil))
		return
	}
	mr, err := q.r.MultipartReader()
	if err != nil {
		q.fail(errNotMultipart)
		return
	}
	part, err := mr.NextPart()
	if err != nil {
		q.fail(bodyError(err, errNotMultipart))
		return
	}
	if part.FormName() != "compose" {
		q.fail(service.E(service.CodeBadRequest, "the first part must be compose", nil))
		return
	}
	var compose service.Compose
	if err := decodePart(part, &compose); err != nil {
		q.fail(err)
		return
	}
	req := service.SendRequest{Compose: compose, Attachments: attachmentsOf(mr)}
	if len(keys) == 1 {
		req.IdempotencyKey = keys[0]
	}
	res, err := h.Service.SendMessage(q.ctx(), q.principal, req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, res)
}

// attachmentsOf hands the service the parts after compose, each one while it
// is being read from the request.
func attachmentsOf(mr *multipart.Reader) service.UploadSource {
	return func(add func(service.Upload) error) error {
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return bodyError(err, errNotMultipart)
			}
			if part.FormName() != "attachment" {
				return service.E(service.CodeBadRequest, "after compose, every part must be an attachment", nil)
			}
			err = add(service.Upload{
				Filename:    part.FileName(),
				ContentType: part.Header.Get("Content-Type"),
				Body:        bodyReader{part},
			})
			if err != nil {
				return err
			}
		}
	}
}

// bodyReader turns a request body that ran past its limit into the caller's
// error, as the JSON routes report it.
type bodyReader struct{ r io.Reader }

func (b bodyReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, bodyError(err, err)
	}
	return n, err
}

// bodyError is the error for a body that could not be read: its limit, when
// that is what stopped it, and otherwise fallback.
func bodyError(err, fallback error) error {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return service.Ef(service.CodeBadRequest, err, "request body exceeds %d bytes", maxErr.Limit)
	}
	return fallback
}

// decodePart reads the compose part as the JSON routes read a body: one
// object, no unknown field.
func decodePart(part io.Reader, v any) error {
	raw, err := io.ReadAll(io.LimitReader(part, maxComposeBytes+1))
	if err != nil {
		return bodyError(err, errNotMultipart)
	}
	if len(raw) > maxComposeBytes {
		return service.Ef(service.CodeBadRequest, nil, "the compose part exceeds %d bytes", maxComposeBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return service.Ef(service.CodeBadRequest, err, "malformed compose part: %s", jsonProblem(err))
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return service.E(service.CodeBadRequest, "the compose part must hold exactly one JSON object", nil)
	}
	return nil
}

// sendStatus serves GET /v1/sends/{key}?account=.
func (h *Handler) sendStatus(q *request) {
	params, err := q.query("account")
	if err != nil {
		q.fail(err)
		return
	}
	account := strings.TrimSpace(params["account"])
	if account == "" {
		q.fail(service.E(service.CodeBadRequest, "account is required", nil))
		return
	}
	status, err := h.Service.SendStatus(q.ctx(), q.principal, account, q.r.PathValue("key"))
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, status)
}

// sendConsent serves GET /v1/me/send-consent.
func (h *Handler) sendConsent(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	consent, err := h.Service.SendConsent(q.ctx(), q.principal)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, consent)
}

// grantSendConsent serves POST /v1/me/send-consent.
func (h *Handler) grantSendConsent(q *request) {
	var req service.SendConsentRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	consent, err := h.Service.GrantSendConsent(q.ctx(), q.principal, req.Version)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, consent)
}

// withdrawSendConsent serves DELETE /v1/me/send-consent.
func (h *Handler) withdrawSendConsent(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	consent, err := h.Service.WithdrawSendConsent(q.ctx(), q.principal)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, consent)
}
