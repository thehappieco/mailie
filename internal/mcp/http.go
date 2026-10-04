package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/ratelimit"
	"github.com/thehappieco/mailie/internal/service"
)

// The Streamable HTTP transport, at /mcp.
//
// Stateful: a session is created by the client's first POST, lives until
// the client deletes it or leaves it idle for SessionTimeout, and has a GET
// stream for the notifications a subscription produces. Stateless mode has
// neither, and would take subscriptions away from the clients that use them.
//
// Every request — POST, GET and DELETE — carries the key and is checked in
// full, as REST checks it: rate limited, then verified, then handed to the
// SDK through auth.RequireBearerToken, which puts the key in front of every
// tool call and binds the session to the key that opened it, so another key
// cannot use it. A console session token is refused: it is not a tool's
// credential.
//
// A request carries one JSON-RPC message, never a batch, so the rate limit
// counts calls; and a key holds a bounded number of sessions, so reconnecting
// in a loop cannot pile them up.

// Limits of the HTTP transport.
const (
	// DefaultSessionTimeout closes a session nobody has used for this long.
	DefaultSessionTimeout = 30 * time.Minute
	// DefaultMaxSessionsPerKey is how many sessions one key holds open.
	DefaultMaxSessionsPerKey = 16
	// maxRequestBody bounds a request, as the compose routes' body is.
	maxRequestBody = 16 << 20
	// maxTokenLen bounds the Authorization header, as REST does.
	maxTokenLen = 4096
)

// HTTPOptions configure the HTTP transport.
type HTTPOptions struct {
	// Limits is the bearer rate limiter REST uses: the same one, so a key
	// has one budget whichever way it comes in.
	Limits  *ratelimit.Auth
	Metrics *obs.Metrics
	// PublicURL is the console's origin. A browser page there, or on this
	// machine, may call /mcp; any other origin is refused.
	PublicURL string
	// EventStore holds streamed answers for resumption; nil builds one with
	// the default bounds.
	EventStore *EventStore
	// SessionTimeout is how long an idle session lives; zero is
	// DefaultSessionTimeout.
	SessionTimeout time.Duration
	// MaxSessionsPerKey bounds the sessions one key holds open: opening one
	// more closes the one the key used least recently. Zero is
	// DefaultMaxSessionsPerKey.
	MaxSessionsPerKey int
}

// HTTP serves MCP over Streamable HTTP.
type HTTP struct {
	server  *Server
	limits  *ratelimit.Auth
	metrics *obs.Metrics
	origin  string
	store   *EventStore
	next    http.Handler
	// sessions are the sessions open, by key.
	sessions *keySessions
	// probe is a server with no session, which the SDK asks which protocol
	// versions it speaks on every request that does not open a session.
	probe *sdk.Server
}

// HTTPHandler builds the transport.
func (s *Server) HTTPHandler(o HTTPOptions) *HTTP {
	store := o.EventStore
	if store == nil {
		store = NewEventStore(0, 0)
	}
	timeout := o.SessionTimeout
	if timeout <= 0 {
		timeout = DefaultSessionTimeout
	}
	maxSessions := o.MaxSessionsPerKey
	if maxSessions <= 0 {
		maxSessions = DefaultMaxSessionsPerKey
	}
	h := &HTTP{
		server: s, limits: o.Limits, metrics: o.Metrics, origin: originOf(o.PublicURL), store: store,
		sessions: newKeySessions(maxSessions),
		probe:    sdk.NewServer(&sdk.Implementation{Name: "mailie", Version: s.version}, nil),
	}
	streamable := sdk.NewStreamableHTTPHandler(h.serverFor, &sdk.StreamableHTTPOptions{
		SessionTimeout:      timeout,
		EventStore:          store,
		MaxRequestBodyBytes: maxRequestBody,
		// The SDK's own guard refuses a request that reaches a loopback
		// listener with a Host that is not loopback: every request behind
		// the reverse proxy in production. The Origin check below does
		// that job, for the requests a browser can be made to send.
		DisableLocalhostProtection: true,
	})
	h.next = sdkauth.RequireBearerToken(verified, &sdkauth.RequireBearerTokenOptions{
		// A key's expiry is checked when it is verified, from the
		// database; the token itself carries none.
		AllowMissingExpiration: true,
	})(streamable)
	return h
}

// EventStore is the transport's event store, for tests and for the daemon to
// report its bounds.
func (h *HTTP) EventStore() *EventStore { return h.store }

type callerKey struct{}

// sessionSlot remembers the server built for a request that opens a
// session: the SDK asks for it twice.
type sessionSlot struct{ server *sdk.Server }

type slotKey struct{}

func (h *HTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	sw := &statusWriter{ResponseWriter: w}
	w = sw
	defer func() { h.observe(r.Method, sw.status, time.Since(started)) }()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	log := obs.LoggerFrom(r.Context()).With("route", "/mcp")

	// A page in a browser can be made to call a local daemon — by DNS
	// rebinding, for one — and it always says where it came from.
	if origin := r.Header.Get("Origin"); origin != "" && !h.allowedOrigin(origin) {
		h.fail(w, log, service.E(service.CodeNotAuthorized, "requests from other web origins are refused", nil))
		return
	}
	token, ok := bearerToken(r)
	if !ok {
		h.metricAuthFailure("malformed")
		h.fail(w, log, service.E(service.CodeUnauthorized, "a bearer api key is required", nil))
		return
	}
	// As REST: rate limited before the Argon2id, with a key's failures
	// charged to its prefix and given back when the key proves.
	subject := ""
	if prefix, _, ok := auth.SplitKey(token); ok {
		subject = "key:" + prefix
	}
	if allowed, retry := h.limits.Admit(r, subject); !allowed {
		h.metricAuthFailure("rate_limited")
		h.fail(w, log, service.Retryable("too many requests", retry, nil))
		return
	}
	p, err := h.server.svc.AuthenticateTool(r.Context(), token, func() (bool, time.Duration) {
		return h.limits.AdmitUnknownKey(r)
	})
	switch code := service.CodeOf(err); {
	case err == nil:
		h.limits.Refund(subject)
	case code == service.CodeUnauthorized:
		h.metricAuthFailure("invalid")
	default:
		h.limits.Refund(subject)
		if code == service.CodeRateLimited {
			h.metricAuthFailure("rate_limited")
		}
	}
	if err != nil {
		h.fail(w, log, err)
		return
	}

	if r.Method == http.MethodPost {
		body, err := refuseBatch(w, r.Body)
		if err != nil {
			h.fail(w, log, err)
			return
		}
		r.Body = body
	}

	ctx := context.WithValue(r.Context(), callerKey{}, p)
	var slot *sessionSlot
	switch id := r.Header.Get("Mcp-Session-Id"); {
	case id != "":
		h.sessions.used(p.KeyPrefix, id)
	case r.Method == http.MethodPost:
		slot = &sessionSlot{}
		ctx = context.WithValue(ctx, slotKey{}, slot)
	}
	h.next.ServeHTTP(w, r.WithContext(ctx))
	if slot != nil && slot.server != nil {
		// The session the SDK opened on the server built for this request,
		// if it opened one and the session outlived the request.
		for sess := range slot.server.Sessions() {
			h.sessions.opened(p.KeyPrefix, sess)
		}
	}
}

// errBatch refuses a JSON-RPC batch.
var errBatch = service.E(service.CodeBadRequest, "JSON-RPC batches are not accepted: send one message per request", nil)

// refuseBatch reads a POST body up to its first JSON token, and refuses a
// batch: an array carries any number of calls through one request, one
// rate-limit token and one check of the key, and the SDK runs them all at
// once. The SDK accepts one from any request that names no protocol version,
// whatever version its session negotiated. The body is handed on unread
// otherwise.
func refuseBatch(w http.ResponseWriter, body io.ReadCloser) (io.ReadCloser, error) {
	br := bufio.NewReader(http.MaxBytesReader(w, body, maxRequestBody))
	for {
		b, err := br.ReadByte()
		var tooLarge *http.MaxBytesError
		switch {
		case errors.Is(err, io.EOF):
			// Nothing but blanks: the SDK answers that.
			return readCloser{br, body}, nil
		case errors.As(err, &tooLarge):
			return nil, service.Ef(service.CodeBadRequest, err, "request body exceeds %d bytes", tooLarge.Limit)
		case err != nil:
			return nil, service.E(service.CodeBadRequest, "the request body could not be read", err)
		}
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '[':
			return nil, errBatch
		}
		if err := br.UnreadByte(); err != nil {
			return nil, service.E(service.CodeInternal, "reading the request failed", err)
		}
		return readCloser{br, body}, nil
	}
}

// readCloser reads what is left of a body through the reader that looked at
// its start.
type readCloser struct {
	io.Reader
	io.Closer
}

// keySessions bounds the sessions each key holds open. A session is a server
// and its goroutines, and lives until its client closes it or leaves it idle
// for the session timeout: a client that reconnects without closing, or a key
// opening sessions in a loop, would otherwise hold them by the thousand. When
// a key opens one too many, the one it used least recently is closed; a client
// still holding that one is told the session is gone (404) and opens another,
// as the protocol has it.
type keySessions struct {
	max int

	mu    sync.Mutex
	byKey map[string][]*liveSession
	// clock orders uses: the least recently used is the lowest.
	clock uint64
}

type liveSession struct {
	sess *sdk.ServerSession
	used uint64
}

func newKeySessions(maxSessions int) *keySessions {
	return &keySessions{max: maxSessions, byKey: map[string][]*liveSession{}}
}

// opened records a session key just opened, closing the key's least recently
// used when that makes one too many.
func (k *keySessions) opened(key string, sess *sdk.ServerSession) {
	k.mu.Lock()
	k.clock++
	list := append(k.byKey[key], &liveSession{sess: sess, used: k.clock})
	var evicted *sdk.ServerSession
	if len(list) > k.max {
		oldest := 0
		for i, s := range list {
			if s.used < list[oldest].used {
				oldest = i
			}
		}
		evicted = list[oldest].sess
		list = slices.Delete(list, oldest, oldest+1)
	}
	k.byKey[key] = list
	k.mu.Unlock()
	if evicted != nil {
		// Close waits for the session's calls to end, which is not this
		// request's to wait for.
		//nolint:errcheck // a session that fails to close cleanly is closed all the same
		go func() { _ = evicted.Close() }()
	}
	go func() {
		// Wait returns whatever ended the session; either way it is gone.
		//nolint:errcheck // the session's end is the only news here
		_ = sess.Wait()
		k.closed(key, sess)
	}()
}

// used records a request of a key's session.
func (k *keySessions) used(key, id string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, s := range k.byKey[key] {
		if s.sess.ID() == id {
			k.clock++
			s.used = k.clock
			return
		}
	}
}

func (k *keySessions) closed(key string, sess *sdk.ServerSession) {
	k.mu.Lock()
	defer k.mu.Unlock()
	list := slices.DeleteFunc(k.byKey[key], func(s *liveSession) bool { return s.sess == sess })
	if len(list) == 0 {
		delete(k.byKey, key)
		return
	}
	k.byKey[key] = list
}

// open reports how many sessions key holds open.
func (k *keySessions) open(key string) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.byKey[key])
}

// serverFor is the SDK's getServer: the server a request that opens a
// session will talk to, built for the key that opens it, and otherwise one
// that only answers which protocol versions there are.
func (h *HTTP) serverFor(r *http.Request) *sdk.Server {
	slot, ok := r.Context().Value(slotKey{}).(*sessionSlot)
	p, known := r.Context().Value(callerKey{}).(service.Principal)
	if !ok || !known {
		return h.probe
	}
	if slot.server == nil {
		slot.server = h.server.NewSession(p)
	}
	return slot.server
}

// verified hands the SDK the key ServeHTTP verified: RequireBearerToken is
// what puts it where every tool call finds it, and binds the session to it.
func verified(ctx context.Context, _ string, _ *http.Request) (*sdkauth.TokenInfo, error) {
	p, ok := ctx.Value(callerKey{}).(service.Principal)
	if !ok {
		return nil, sdkauth.ErrInvalidToken
	}
	return &sdkauth.TokenInfo{
		Scopes: []string{string(p.Scope)},
		// The session belongs to the key: another key presenting its id is
		// refused by the SDK.
		UserID: "key:" + p.KeyPrefix,
		Extra:  map[string]any{principalExtra: p},
	}, nil
}

// allowedOrigin admits a page on this machine and the console's own origin.
func (h *HTTP) allowedOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if config.IsLoopbackHost(u.Hostname()) {
		return true
	}
	return h.origin != "" && strings.EqualFold(originOf(origin), h.origin)
}

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// bearerToken extracts the credential: exactly one Authorization header,
// the Bearer scheme and a bounded length, as REST takes it.
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
	if token == "" || len(token) > maxTokenLen || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}

// fail answers as REST does: {code, message}, the status the code means, a
// Retry-After when there is a wait, and on 401 a challenge without
// resource_metadata — which would send an MCP client off to an OAuth
// discovery it cannot finish, instead of using the key it was given.
func (h *HTTP) fail(w http.ResponseWriter, log *slog.Logger, err error) {
	code := service.CodeOf(err)
	status := statusOf(code)
	if retry := service.RetryAfterOf(err); retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Round(time.Second).Seconds())))
	}
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="mailserver"`)
	}
	if status >= http.StatusInternalServerError {
		log.Error("mcp request failed", "code", code, "err", err)
	} else {
		log.Debug("mcp request refused", "code", code)
	}
	body, merr := json.Marshal(struct {
		Code    service.Code `json:"code"`
		Message string       `json:"message"`
	}{code, service.MessageOf(err)})
	if merr != nil {
		body = []byte(`{"code":"internal","message":"internal error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		log.Debug("the client went away before the response was written", "err", err)
	}
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

func (h *HTTP) observe(method string, status int, took time.Duration) {
	if h.metrics == nil {
		return
	}
	if status == 0 {
		status = http.StatusOK
	}
	h.metrics.HTTPRequests.WithLabelValues("/mcp", method, strconv.Itoa(status)).Inc()
	h.metrics.HTTPDuration.WithLabelValues("/mcp").Observe(took.Seconds())
}

func (h *HTTP) metricAuthFailure(reason string) {
	if h.metrics == nil {
		return
	}
	h.metrics.AuthFailures.WithLabelValues(reason).Inc()
}

// statusWriter remembers the status, for the metrics. Unwrap lets the SDK's
// ResponseController flush the event stream through it.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
