package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/service"
)

// Route options. Timeouts and body limits are per route rather than global,
// because three of this server's routes cannot live under a uniform limit: the
// event stream runs for as long as the client wants it, the long poll runs for
// up to a few minutes, and a compose body carries attachments. Applying one
// number everywhere would break real-time delivery and attachments on the same
// afternoon.
const (
	defaultTimeout = 30 * time.Second
	defaultMaxBody = 64 << 10
)

// routeOptions are per route because three of this server's endpoints cannot
// live under one number: the event stream runs as long as the client wants,
// the long poll runs for minutes, and a compose body carries attachments.
// A timeout of zero means the handler manages its own deadline.
type routeOptions struct {
	timeout time.Duration
	maxBody int64
}

func opts() routeOptions { return routeOptions{timeout: defaultTimeout, maxBody: defaultMaxBody} }

func (o routeOptions) withTimeout(d time.Duration) routeOptions { o.timeout = d; return o }

func (o routeOptions) withMaxBody(n int64) routeOptions { o.maxBody = n; return o }

// request is one authenticated call in flight.
type request struct {
	h    *Handler
	w    http.ResponseWriter
	r    *http.Request
	opts routeOptions

	principal auth.Principal
	route     string
	started   time.Time
	logger    *slog.Logger
}

// ctx is the request context, already bounded by the route's timeout.
func (q *request) ctx() context.Context { return q.r.Context() }

// log is the request's logger. It is built once, from the request context, so
// every line carries the route and — once authentication has run — the key
// prefix or session id that reached it. Both are selectors, not secrets.
func (q *request) log() *slog.Logger { return q.logger }

func (q *request) withCaller(p auth.Principal) {
	if p.IsSession() {
		q.logger = q.logger.With("session", p.SessionID)
		return
	}
	q.logger = q.logger.With("key", p.KeyPrefix)
}

// authenticated wraps a handler with the whole request preamble: headers,
// timeout, rate limit, bearer check and scope check. The bearer is an API key
// or a console session token; the service tells them apart.
//
// A per-route decorator rather than a middleware chain, so each route states
// its own scope, timeout and body limit at the point it is mounted and there
// is no ordering to get wrong somewhere else in the file.
func (h *Handler) authenticated(scope auth.Scope, o routeOptions, fn func(*request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		route := r.Pattern
		if route == "" {
			route = r.URL.Path
		}

		w.Header().Set("Content-Type", "application/json")
		// Mail metadata must not sit in a shared cache, and a browser that
		// sniffs a JSON body as HTML is an XSS waiting for a subject line.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		if o.timeout > 0 {
			ctx, cancel := context.WithTimeout(r.Context(), o.timeout)
			defer cancel()
			r = r.WithContext(ctx)
		}
		if o.maxBody > 0 && r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, o.maxBody)
		}

		q := &request{
			h: h, w: w, r: r, opts: o, route: route, started: started,
			logger: obs.LoggerFrom(r.Context()).With("route", route),
		}

		token, ok := bearerToken(r)
		if !ok {
			h.metricAuthFailure("malformed")
			q.fail(service.E(service.CodeUnauthorized, "a bearer api key or session token is required", nil))
			return
		}
		// Rate limit before verifying: the Argon2id derivation is the
		// expensive part, so a limiter that ran after it would be protecting
		// nothing. A key's prefix is the subject its failures are charged
		// to, reserved now and given back unless this turns out to be a
		// miss; a session token has no public part to charge, and a key
		// whose prefix matches nothing is charged to its address, by the
		// gate, only once the lookup has found nothing.
		subject := ""
		if prefix, _, ok := auth.SplitKey(token); ok {
			subject = "key:" + prefix
		}
		if allowed, retry := h.Limits.Admit(r, subject); !allowed {
			h.metricAuthFailure("rate_limited")
			q.fail(service.Retryable("too many requests", retry, nil))
			return
		}

		principal, err := h.Service.Authenticate(r.Context(), token, func() (bool, time.Duration) {
			return h.Limits.AdmitUnknownKey(r)
		})
		switch code := service.CodeOf(err); {
		case err == nil:
			// Proved: the reservation is given back at once, before the
			// handler runs, so a good credential holds it only for its check.
			h.Limits.Refund(subject)
		case code == service.CodeUnauthorized:
			// A miss, and it stays charged.
			h.metricAuthFailure("invalid")
		default:
			// The check never finished — a database hiccup, every hashing
			// slot busy, a key that matches nothing from an address that
			// has sent too many — and none of that is a guess at this key.
			h.Limits.Refund(subject)
			if code == service.CodeRateLimited {
				h.metricAuthFailure("rate_limited")
			}
		}
		if err != nil {
			// One message per kind of credential for every way of failing:
			// telling a caller which one hands them an oracle.
			q.fail(err)
			return
		}
		q.principal = principal
		q.withCaller(principal)

		if !principal.Scope.Covers(scope) {
			q.fail(service.Ef(service.CodeNotAuthorized, nil,
				"this key has %s scope; %s is required", principal.Scope, scope))
			return
		}
		fn(q)
	}
}

// public wraps a route that needs no credential: the health probe, and signing
// in or up, which is where credentials come from.
func (h *Handler) public(o routeOptions, fn func(*request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A sign-in reply carries a session token; no cache may keep it.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if o.timeout > 0 {
			ctx, cancel := context.WithTimeout(r.Context(), o.timeout)
			defer cancel()
			r = r.WithContext(ctx)
		}
		if o.maxBody > 0 && r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, o.maxBody)
		}
		route := r.Pattern
		if route == "" {
			route = r.URL.Path
		}
		fn(&request{
			h: h, w: w, r: r, opts: o, route: route, started: time.Now(),
			logger: obs.LoggerFrom(r.Context()).With("route", route),
		})
	}
}

// bearerToken extracts the credential.
//
// Exactly one Authorization header, the Bearer scheme, and a bounded length.
// More than one header is ambiguous and a long one is an invitation to make
// the server hash something enormous.
const maxTokenLen = 4096

func bearerToken(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > maxTokenLen {
		return "", false
	}
	return token, true
}

// finish serialises a successful response.
//
// It re-checks the credential first. A query that began while a key was live
// or a session open must not publish its answer after the key was revoked or
// the person signed out, and long reads give that window real width. The
// re-check is a row read, not a second Argon2id derivation: the secret was
// already proved, and paying for another hash would halve throughput to learn
// nothing new.
func (q *request) finish(status int, value any) {
	if err := q.h.Service.Recheck(q.ctx(), q.principal); err != nil {
		q.fail(err)
		return
	}
	q.write(status, value)
}

func (q *request) write(status int, value any) {
	if status == http.StatusNoContent {
		q.w.WriteHeader(status)
		q.h.observe(q.route, q.r.Method, status, time.Since(q.started))
		return
	}
	body, err := json.Marshal(value)
	if err != nil {
		q.log().Error("encoding the response failed", "err", err)
		q.write(http.StatusInternalServerError, wireError{Code: service.CodeInternal, Message: "internal error"})
		return
	}
	q.w.WriteHeader(status)
	if _, err := q.w.Write(body); err != nil {
		q.log().Debug("the client went away before the response was written", "err", err)
	}
	q.h.observe(q.route, q.r.Method, status, time.Since(q.started))
}

// wireError is the error shape every route returns: flat, two fields, a closed
// vocabulary. A caller branches on code; a human reads message.
type wireError struct {
	Code    service.Code `json:"code"`
	Message string       `json:"message"`
}

func (q *request) fail(err error) {
	code := service.CodeOf(err)
	status := statusOf(code)
	if retry := service.RetryAfterOf(err); retry > 0 {
		q.w.Header().Set("Retry-After", strconv.Itoa(int(retry.Round(time.Second).Seconds())))
	}
	if status == http.StatusUnauthorized {
		// No resource_metadata: MCP clients that see one start an OAuth
		// discovery dance and abandon the static bearer header they were
		// configured with.
		q.w.Header().Set("WWW-Authenticate", `Bearer realm="mailserver"`)
	}
	switch {
	case errors.Is(q.ctx().Err(), context.Canceled):
		// The client left before the answer: the console cancels a read
		// whenever a person opens the next message. Nothing failed here,
		// and nobody reads what is written below.
		q.log().Debug("the client went away", "code", code, "err", err)
	case status >= http.StatusInternalServerError:
		q.log().Error("request failed", "code", code, "err", err)
	default:
		q.log().Debug("request refused", "code", code, "err", err)
	}
	q.write(status, wireError{Code: code, Message: service.MessageOf(err)})
}

func statusOf(code service.Code) int {
	switch code {
	case service.CodeUnauthorized:
		return http.StatusUnauthorized
	case service.CodeNotAuthorized:
		return http.StatusForbidden
	case service.CodeBadRequest:
		return http.StatusBadRequest
	case service.CodeNotFound:
		return http.StatusNotFound
	case service.CodeConflict:
		return http.StatusConflict
	case service.CodeRateLimited:
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

// decode reads a JSON body into v.
//
// Unknown fields are rejected rather than ignored: a caller that misspells
// "confirm" should be told, not silently treated as having omitted it.
func (q *request) decode(v any) error { return q.decodeBody(v, false) }

// decodeOptional is decode for a route whose body may be absent altogether,
// leaving v at its zero value.
func (q *request) decodeOptional(v any) error { return q.decodeBody(v, true) }

func (q *request) decodeBody(v any, optional bool) error {
	if q.r.Body == nil || q.r.Body == http.NoBody {
		if optional {
			return nil
		}
		return service.E(service.CodeBadRequest, "a request body is required", nil)
	}
	dec := json.NewDecoder(q.r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if optional && errors.Is(err, io.EOF) {
			return nil
		}
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return service.Ef(service.CodeBadRequest, err, "request body exceeds %d bytes", maxErr.Limit)
		}
		return service.Ef(service.CodeBadRequest, err, "malformed request body: %s", jsonProblem(err))
	}
	// A second JSON value in the body means the caller sent something other
	// than the single object this route documents.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return service.E(service.CodeBadRequest, "the request body must hold exactly one JSON object", nil)
	}
	return nil
}

// jsonProblem renders a decoding failure without quoting the body back.
func jsonProblem(err error) string {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return fmt.Sprintf("field %q should be %s", typeErr.Field, typeErr.Type)
	}
	if msg := err.Error(); strings.Contains(msg, "unknown field") {
		return msg[strings.Index(msg, "unknown field"):]
	}
	return "not valid JSON"
}

// query reads the query string, rejecting anything not on the route's list.
//
// Unknown and repeated parameters are errors. A typo in a filter that silently
// widens a search is how a caller ends up acting on the wrong mail, and a
// repeated parameter means two different intentions arrived and one was about
// to be discarded.
func (q *request) query(allowed ...string) (map[string]string, error) {
	values := q.r.URL.Query()
	if len(q.r.URL.RawQuery) > maxQueryLen {
		return nil, service.E(service.CodeBadRequest, "the query string is too long", nil)
	}
	out := make(map[string]string, len(values))
	for name, vs := range values {
		if !contains(allowed, name) {
			return nil, service.Ef(service.CodeBadRequest, nil,
				"unknown query parameter %q (allowed: %s)", name, strings.Join(allowed, ", "))
		}
		if len(vs) > 1 {
			return nil, service.Ef(service.CodeBadRequest, nil, "query parameter %q was given more than once", name)
		}
		out[name] = vs[0]
	}
	return out, nil
}

const maxQueryLen = 8192

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
