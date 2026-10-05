package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/thehappieco/mailie/internal/service"
)

// The event stream (SSE) and the long poll. Both are the service's
// subscription to the journal, resumed from a cursor and holding only the
// events of accounts the caller may see; this file only speaks the wire
// format.

const (
	// defaultEventPing is how often an idle stream says it is alive. Short
	// enough for the proxies in front of a console, which close a response
	// that has been silent for a minute, and for noticing a client that went
	// away without closing its connection.
	defaultEventPing = 15 * time.Second
	// eventWriteTimeout bounds one write to a stream. A client that stopped
	// reading holds a goroutine and a subscription only this long.
	eventWriteTimeout = 30 * time.Second
)

func (h *Handler) eventPing() time.Duration {
	if h.EventPing > 0 {
		return h.EventPing
	}
	return defaultEventPing
}

// streamEvents serves GET /v1/events.
//
// Resumes after Last-Event-ID, the header a reconnecting client sends, or
// after ?since=, and otherwise starts from now. ?account= and ?types= take
// comma-separated lists; ?workspace= narrows to one workspace. The route has
// no timeout: it lives as long as the client, the credential and the daemon
// do. The credential is checked again before every batch of events is written
// and at every ping, so a session that ends or a key that is revoked receives
// nothing more: an idle stream learns it within one interval, and never
// through another event. So is access: a mailbox the caller gains or loses
// read on is announced as `event: access`, and once every mailbox the stream
// follows is gone it ends with `event: error`, not_found.
func (h *Handler) streamEvents(q *request) {
	params, err := q.query("since", "account", "types", "workspace")
	if err != nil {
		q.fail(err)
		return
	}
	since, err := resumePoint(q.r.Header.Get("Last-Event-ID"), params["since"])
	if err != nil {
		q.fail(err)
		return
	}
	stream, err := h.Service.Subscribe(q.ctx(), q.principal, since, service.EventFilter{
		AccountIDs: list(params["account"]), Types: list(params["types"]), Workspace: params["workspace"],
	})
	if err != nil {
		q.fail(err)
		return
	}
	defer stream.Close()
	if err := h.Service.Recheck(q.ctx(), q.principal); err != nil {
		q.fail(err)
		return
	}

	sse := &eventWriter{w: q.w, rc: http.NewResponseController(q.w)}
	header := q.w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-store")
	// nginx buffers responses by default, which would hold events back until
	// its buffer filled.
	header.Set("X-Accel-Buffering", "no")
	q.w.WriteHeader(http.StatusOK)
	defer func() { h.observe(q.route, q.r.Method, http.StatusOK, time.Since(q.started)) }()

	sse.comment("connected")
	if stream.Gap() {
		// Events after the client's cursor were pruned. What survived still
		// follows; the client has to re-read what it keeps, because no
		// cursor brings the rest back.
		sse.event("", "lagged", map[string]any{"since": since})
	}
	if !sse.flush() {
		return
	}

	ping := time.NewTicker(h.eventPing())
	defer ping.Stop()
	events := stream.Events()
	// credentialWorks checks the caller again, and what they may still
	// read, and ends the stream when the credential no longer works or
	// nothing it follows is left.
	credentialWorks := func() bool {
		err := h.Service.Recheck(q.ctx(), q.principal)
		if err == nil {
			var changes []service.AccessChange
			changes, err = stream.CheckAccess(q.ctx())
			// Like lagged, no id: an access change is not a journal entry,
			// and a reconnection resumes after the last event that was.
			for _, change := range changes {
				sse.event("", "access", change)
			}
			if err == nil {
				return true
			}
		}
		// Too late for a status code. The client is told why the stream
		// ended, in the error body every route uses, so it does not
		// reconnect with a credential that no longer works, or for
		// mailboxes it can no longer read.
		sse.event("", "error", wireError{Code: service.CodeOf(err), Message: service.MessageOf(err)})
		sse.flush()
		q.log().Debug("event stream ended: the credential no longer works or nothing it follows is readable",
			"err", err)
		return false
	}
	// forward writes one event, and reports whether the stream goes on.
	forward := func(ev service.Event, ok bool) bool {
		if !ok {
			if err := stream.Err(); err != nil {
				// The journal could not be read. What the client received is
				// complete up to its last id: it reconnects with that as
				// Last-Event-ID and loses nothing.
				q.log().Warn("event stream ended: reading the journal failed", "err", err)
			}
			return false
		}
		if !stream.Allowed(ev) {
			// Read before the caller lost the mailbox, written after:
			// held back.
			return true
		}
		return sse.event(strconv.FormatInt(ev.Seq, 10), ev.Type, ev)
	}
	for {
		select {
		case ev, ok := <-events:
			// A batch begins: the credential is checked before any of it
			// is written, then everything already queued goes out in one
			// flush.
			if ok && !credentialWorks() {
				return
			}
			if !forward(ev, ok) {
				return
			}
			for queued := true; queued; {
				select {
				case ev, ok := <-events:
					if !forward(ev, ok) {
						return
					}
				default:
					queued = false
				}
			}
			if !sse.flush() {
				return
			}
		case <-ping.C:
			if !credentialWorks() {
				return
			}
			if !sse.comment("ping") || !sse.flush() {
				return
			}
		case <-q.ctx().Done():
			return
		}
	}
}

// waitForEvents serves GET /v1/events/wait: a long poll for new mail.
// ?timeout= is in seconds, at most 55, and 0 or absent is the default;
// ?since= is the next_cursor of the previous call.
func (h *Handler) waitForEvents(q *request) {
	params, err := q.query("since", "timeout", "account", "workspace")
	if err != nil {
		q.fail(err)
		return
	}
	since, err := resumePoint("", params["since"])
	if err != nil {
		q.fail(err)
		return
	}
	var timeout time.Duration
	if raw := params["timeout"]; raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil {
			q.fail(service.E(service.CodeBadRequest, "timeout is a whole number of seconds", err))
			return
		}
		timeout = time.Duration(seconds) * time.Second
	}
	result, err := h.Service.WaitForNewMail(q.ctx(), q.principal, since, timeout, service.EventFilter{
		AccountIDs: list(params["account"]), Workspace: params["workspace"],
	})
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, result)
}

// resumePoint reads the cursor a subscription resumes after: the SSE
// reconnection header when a client sent one, else the query parameter.
func resumePoint(header, param string) (int64, error) {
	raw, name := strings.TrimSpace(header), "Last-Event-ID"
	if raw == "" {
		raw, name = param, "since"
	}
	if raw == "" {
		return 0, nil
	}
	seq, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seq < 0 {
		return 0, service.Ef(service.CodeBadRequest, err, "%s must be an event id: a whole number, 0 or more", name)
	}
	return seq, nil
}

// list splits a comma-separated parameter, dropping empty items.
func list(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// eventWriter writes the text/event-stream format. Each write has its own
// deadline, since the server has none: the stream is meant to outlive any
// fixed limit, but one write to a client that stopped reading must not.
type eventWriter struct {
	w      http.ResponseWriter
	rc     *http.ResponseController
	failed bool
}

// event writes one event. The id is omitted for events that are not journal
// entries, so a client's Last-Event-ID only ever names one.
func (e *eventWriter) event(id, name string, data any) bool {
	body, err := json.Marshal(data)
	if err != nil {
		e.failed = true
		return false
	}
	var b strings.Builder
	if id != "" {
		b.WriteString("id: " + id + "\n")
	}
	b.WriteString("event: " + name + "\n")
	// JSON never holds a raw newline, so the data is always one line.
	b.WriteString("data: ")
	b.Write(body)
	b.WriteString("\n\n")
	return e.write(b.String())
}

// comment writes a line every SSE parser ignores: the pings.
func (e *eventWriter) comment(text string) bool { return e.write(": " + text + "\n\n") }

func (e *eventWriter) write(s string) bool {
	if e.failed {
		return false
	}
	//nolint:errcheck // not every writer can set a deadline; the write below still fails on a dead client
	_ = e.rc.SetWriteDeadline(time.Now().Add(eventWriteTimeout))
	if _, err := fmt.Fprint(e.w, s); err != nil {
		e.failed = true
		return false
	}
	return true
}

func (e *eventWriter) flush() bool {
	if e.failed {
		return false
	}
	//nolint:errcheck // as in write
	_ = e.rc.SetWriteDeadline(time.Now().Add(eventWriteTimeout))
	if err := e.rc.Flush(); err != nil {
		e.failed = true
		return false
	}
	return true
}
