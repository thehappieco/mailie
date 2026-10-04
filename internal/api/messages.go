package api

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/thehappieco/mailie/internal/service"
)

// Searching the index and reading messages. Who may read what, and
// everything about what is fetched from the mail server, is decided in
// internal/service; this file parses the query and speaks HTTP.

// Route timeouts. Searching reads the database; reading a message fetches
// its body from the mail server; an attachment or an original is fetched
// whole before the first byte is sent, and may be large.
const (
	messageTimeout  = 60 * time.Second
	downloadTimeout = 10 * time.Minute
	// defaultDownloadStall is Handler.DownloadStall's default.
	defaultDownloadStall = time.Minute
)

// searchMessages serves GET /v1/messages.
func (h *Handler) searchMessages(q *request) {
	params, err := q.query("account", "folder", "q", "from", "unseen", "flagged", "has_attachments",
		"since", "until", "cursor", "limit")
	if err != nil {
		q.fail(err)
		return
	}
	req := service.SearchRequest{
		AccountID: params["account"], Query: params["q"], From: params["from"],
		Since: params["since"], Until: params["until"], Cursor: params["cursor"],
	}
	if raw := params["folder"]; raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 1 {
			q.fail(service.E(service.CodeBadRequest, "folder must be a folder id from the folder listing", err))
			return
		}
		req.FolderID = id
	}
	if raw := params["limit"]; raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			q.fail(service.Ef(service.CodeBadRequest, err, "limit must be between 1 and %d", service.MaxPageSize))
			return
		}
		req.Limit = n
	}
	for _, filter := range []struct {
		name string
		dest **bool
	}{{"unseen", &req.Unseen}, {"flagged", &req.Flagged}, {"has_attachments", &req.HasAttachments}} {
		v, err := boolFilter(filter.name, params[filter.name])
		if err != nil {
			q.fail(err)
			return
		}
		*filter.dest = v
	}
	page, err := h.Service.SearchMessages(q.ctx(), q.principal, req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, page)
}

// getMessage serves GET /v1/messages/{id}.
func (h *Handler) getMessage(q *request) {
	params, err := q.query("format", "max_bytes")
	if err != nil {
		q.fail(err)
		return
	}
	req := service.GetMessageRequest{ID: messageID(q), Format: params["format"]}
	if raw := params["max_bytes"]; raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 {
			q.fail(service.Ef(service.CodeBadRequest, err, "max_bytes must be between 1 and %d", service.MaxBodyBytes))
			return
		}
		req.MaxBytes = n
	}
	msg, err := h.Service.GetMessage(q.ctx(), q.principal, req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, msg)
}

// getRaw serves GET /v1/messages/{id}/raw: the message as the mail server
// holds it.
func (h *Handler) getRaw(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	dl, err := h.Service.GetRaw(q.ctx(), q.principal, messageID(q))
	if err != nil {
		q.fail(err)
		return
	}
	q.download(dl)
}

// getAttachment serves GET /v1/messages/{id}/attachments/{path}, where path
// is the part's IMAP section from the message's parts.
func (h *Handler) getAttachment(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	dl, err := h.Service.GetAttachment(q.ctx(), q.principal, messageID(q), q.r.PathValue("path"))
	if err != nil {
		q.fail(err)
		return
	}
	q.download(dl)
}

// messageID reads the id in the path. Anything that is not a positive whole
// number names no message, and the service answers it as one that does not
// exist.
func messageID(q *request) int64 {
	id, err := strconv.ParseInt(q.r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0
	}
	return id
}

// boolFilter reads a boolean filter: absent is no filter.
func boolFilter(name, raw string) (*bool, error) {
	var v bool
	switch strings.ToLower(raw) {
	case "":
		return nil, nil
	case "true", "1":
		v = true
	case "false", "0":
		v = false
	default:
		return nil, service.Ef(service.CodeBadRequest, nil, "%s must be true or false", name)
	}
	return &v, nil
}

// download sends a file fetched from the mail server: always as an
// attachment, never rendered. The sender chose these bytes and their type,
// so a browser must neither run them with this origin nor guess at them:
// the type is one the service made safe, sniffing is off, and the response
// is sandboxed in case anything renders it anyway. Nothing is cached.
func (q *request) download(dl service.Download) {
	//nolint:errcheck // closing removes the spooled copy; nothing to report
	defer func() { _ = dl.Body.Close() }()
	if err := q.h.Service.Recheck(q.ctx(), q.principal); err != nil {
		q.fail(err)
		return
	}
	header := q.w.Header()
	header.Set("Content-Type", dl.ContentType)
	header.Set("Content-Disposition", contentDisposition(dl.Filename))
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	header.Set("Cache-Control", "no-store")
	if dl.Size >= 0 {
		header.Set("Content-Length", strconv.FormatInt(dl.Size, 10))
	}
	// The server has no write timeout. This one moves with the client: each
	// write must finish within the stall allowance of the last, and never
	// past the route's deadline. A client that stops reading holds the
	// spooled file, and a place among the downloads in flight, for a minute
	// rather than for the whole route.
	stall := q.h.DownloadStall
	if stall <= 0 {
		stall = defaultDownloadStall
	}
	rc := http.NewResponseController(q.w)
	routeDeadline, bounded := q.ctx().Deadline()
	extend := func() {
		next := time.Now().Add(stall)
		if bounded && routeDeadline.Before(next) {
			next = routeDeadline
		}
		//nolint:errcheck // not every writer can set a deadline; the route's context still ends
		_ = rc.SetWriteDeadline(next)
	}
	extend()
	q.w.WriteHeader(http.StatusOK)
	if n, err := copyExtending(q.w, dl.Body, extend); err != nil {
		q.log().Debug("the download ended before the whole file was sent", "sent", n, "err", err)
	}
	q.h.observe(q.route, q.r.Method, http.StatusOK, time.Since(q.started))
}

// copyExtending is io.Copy calling progress after every write that went
// through.
func copyExtending(w io.Writer, r io.Reader, progress func()) (int64, error) {
	buf := make([]byte, 64<<10)
	var sent int64
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			written, err := w.Write(buf[:n])
			sent += int64(written)
			if err != nil {
				return sent, err
			}
			if written < n {
				return sent, io.ErrShortWrite
			}
			progress()
		}
		switch {
		case errors.Is(readErr, io.EOF):
			return sent, nil
		case readErr != nil:
			return sent, readErr
		}
	}
}

// contentDisposition names a download: the UTF-8 name in RFC 5987 form,
// which every current browser reads, and a plain ASCII stand-in before it
// for anything older. The name is already safe as a file name (the service
// made it so); this only makes it safe as a header.
func contentDisposition(name string) string {
	return `attachment; filename="` + asciiName(name) + `"; filename*=UTF-8''` + encodeRFC5987(name)
}

func asciiName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r >= 0x7f, r == '"', r == '\\', r == '%':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// encodeRFC5987 percent-encodes every byte that is not an attr-char.
func encodeRFC5987(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if attrChar(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

func attrChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("!#$&+-.^_`|~", c) >= 0
}
