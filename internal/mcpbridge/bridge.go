package mcpbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Methods the bridge looks at. Everything else it relays without reading.
const (
	methodInitialize  = "initialize"
	methodInitialized = "notifications/initialized"
	methodSubscribe   = "resources/subscribe"
	methodUnsubscribe = "resources/unsubscribe"
)

// noStandaloneStream is the first protocol version without the standalone
// GET stream (SEP-2575), as the SDK's own client has it.
const noStandaloneStream = "2026-07-28"

// Bounds of the bridge's own work.
const (
	// ownCallTimeout bounds a request the bridge makes itself, to open a
	// session again.
	ownCallTimeout = 30 * time.Second
	// beforeSessionTimeout bounds a request made before the client's
	// initialize (protocol negotiation), which has a connection of its own.
	beforeSessionTimeout = 2 * time.Minute
)

// listenTimings are how the bridge asks for the standalone stream again.
type listenTimings struct {
	// first is the wait after the stream ends, or before asking again after a
	// refusal; each refusal in a row doubles it, up to max.
	first, max time.Duration
	// held is how long the bridge waits for the server to let go of a stream
	// it still holds (409: it has not noticed that the connection carrying it
	// is dead) before ending the session, which frees it, and opening
	// another. Past what the server keeps for a resumption (5 minutes), what
	// the dead stream carried is lost either way.
	held time.Duration
}

// defaultListenTimings are a bridge's; a test shortens them.
var defaultListenTimings = listenTimings{first: time.Second, max: 30 * time.Second, held: 3 * time.Minute}

// Why the bridge ends a session itself, for its standalone stream.
var (
	// errStreamLost is a stream the server can no longer resume: it no longer
	// keeps what it sent while the bridge was away.
	errStreamLost = errors.New("mcpbridge: the server can no longer resume the stream of notifications")
	// errStreamHeld is a stream the server still holds for a connection the
	// bridge lost, past listenTimings.held.
	errStreamHeld = errors.New("mcpbridge: the server still holds a stream of notifications the bridge lost")
)

// Run relays MCP between the client on local and the server o names, until
// the client closes local or ctx ends (nil), or the server gives an answer
// that ends it (a *Refusal: the key refused, MCP not served there).
func Run(ctx context.Context, local sdk.Transport, o Options) error {
	client, keys, err := o.client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn, err := local.Connect(ctx)
	if err != nil {
		return fmt.Errorf("mcpbridge: opening the client's side: %w", err)
	}
	b := &bridge{
		ctx: ctx, endpoint: mustEndpoint(o.Endpoint), client: client, keys: keys, local: conn,
		log: o.logger(), stop: make(chan error, 1), subs: map[string]bool{}, timings: defaultListenTimings,
	}
	b.log.Info("relaying MCP", "endpoint", b.endpoint)
	//nolint:contextcheck // ctx is the bridge's, held in it: every request and goroutine it starts uses it
	err = b.run()
	b.shutdown()
	if cerr := conn.Close(); cerr != nil {
		b.log.Debug("closing the client's side", "err", cerr)
	}
	return err
}

// bridge is one client's relay.
type bridge struct {
	ctx      context.Context
	endpoint string
	client   *http.Client
	keys     *keyTransport
	local    sdk.Connection
	log      *slog.Logger
	timings  listenTimings
	// stop carries what ends the bridge: nil for a client that went away.
	stop chan error
	// ids numbers the requests the bridge makes itself.
	ids atomic.Int64

	// opening serialises opening a session: one at a time, and a request
	// that finds none waits for the one being opened.
	opening sync.Mutex

	mu      sync.Mutex
	current *remote
	// init is the client's initialize, replayed to open another session.
	init *jsonrpc.Request
	// ready is the client's notifications/initialized, sent: a new session
	// gets one too, and its standalone stream.
	ready bool
	// subs are the resources the client subscribed to, subscribed to again
	// in a new session.
	subs map[string]bool
}

// end stops the bridge with err, or cleanly with nil; the first call wins.
func (b *bridge) end(err error) {
	select {
	case b.stop <- err:
	default:
	}
}

func (b *bridge) run() error {
	in := make(chan jsonrpc.Message)
	readErr := make(chan error, 1)
	go func() {
		for {
			msg, err := b.local.Read(b.ctx)
			if err != nil {
				readErr <- err
				return
			}
			select {
			case in <- msg:
			case <-b.ctx.Done():
				return
			}
		}
	}()
	for {
		select {
		case msg := <-in:
			b.fromClient(msg)
		case err := <-readErr:
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || b.ctx.Err() != nil {
				b.log.Info("the client closed its side; ending")
				return nil //nolint:nilerr // the client closing its side is the bridge's clean end
			}
			return fmt.Errorf("mcpbridge: reading from the client: %w", err)
		case err := <-b.stop:
			return err
		case <-b.ctx.Done():
			return nil
		}
	}
}

// shutdown ends the session on the server, and every request still open.
func (b *bridge) shutdown() {
	b.mu.Lock()
	r := b.current
	b.current = nil
	b.mu.Unlock()
	if r != nil {
		r.close(b.log)
	}
}

// fromClient relays one message of the client's.
func (b *bridge) fromClient(msg jsonrpc.Message) {
	req, isRequest := msg.(*jsonrpc.Request)
	switch {
	case isRequest && req.IsCall() && req.Method == methodInitialize:
		b.initialize(req)
	case isRequest && req.IsCall():
		b.mu.Lock()
		inSession := b.init != nil
		b.mu.Unlock()
		if !inSession {
			// Before initialize: a negotiation of its own (server/discover,
			// ping), on a connection of its own.
			b.beforeSession(req)
			return
		}
		// A call waits for its answer, which may take minutes
		// (wait_for_new_mail): the next message does not wait for it.
		go b.call(req)
	default:
		// Notifications and the client's answers to the server's requests,
		// in the order the client wrote them.
		b.notify(msg)
	}
}

// initialize opens a session for the client's initialize, on a connection of
// its own: a session that answered an earlier initialize, or refused one,
// is not reused. Any failure ends the bridge: the client has no session to
// use and will say so.
func (b *bridge) initialize(req *jsonrpc.Request) {
	r, err := b.dial()
	if err != nil {
		b.replyError(req.ID, err)
		b.end(err)
		return
	}
	b.opening.Lock()
	b.mu.Lock()
	previous := b.current
	b.current, b.init, b.ready = r, cloneRequest(req), false
	b.subs = map[string]bool{}
	b.mu.Unlock()
	b.opening.Unlock()
	if previous != nil {
		go previous.close(b.log)
	}
	r.track(req)
	out, err := b.write(r, req)
	if err == nil {
		if r.markSent(req.ID) {
			b.replyError(req.ID, errLost)
		}
		return
	}
	if r.take(req.ID) == nil {
		return
	}
	err = classify(out, err, false)
	b.replyError(req.ID, err)
	b.end(err)
}

// beforeSession sends a call made before initialize on a connection that is
// closed once it is answered.
func (b *bridge) beforeSession(req *jsonrpc.Request) {
	r, err := b.dial()
	if err != nil {
		b.replyError(req.ID, err)
		return
	}
	defer func() { go r.close(b.log) }()
	c := r.track(req)
	out, err := b.write(r, req)
	if err != nil {
		if r.take(req.ID) != nil {
			err = classify(out, err, false)
			b.replyError(req.ID, err)
			if fatal(err) {
				b.end(err)
			}
		}
		return
	}
	if r.markSent(req.ID) {
		b.replyError(req.ID, errLost)
		return
	}
	timer := time.NewTimer(beforeSessionTimeout)
	defer timer.Stop()
	select {
	case <-c.done:
	case <-r.broken:
	case <-b.ctx.Done():
	case <-timer.C:
		if r.take(req.ID) != nil {
			b.replyError(req.ID, fmt.Errorf("%w: no answer within %s", ErrUnreachable, beforeSessionTimeout))
		}
	}
}

// call sends one of the client's calls in the session, opening a new session
// first when there is none; a call that finds its session gone is sent again,
// once, in a new one.
func (b *bridge) call(req *jsonrpc.Request) {
	for attempt := 0; ; attempt++ {
		r, err := b.session()
		if err != nil {
			b.replyError(req.ID, err)
			if fatal(err) {
				b.end(err)
			}
			return
		}
		if r.track(req) == nil {
			// The session broke between being found and being used.
			if attempt < 2 {
				continue
			}
			b.replyError(req.ID, errLost)
			return
		}
		out, err := b.write(r, req)
		if err == nil {
			if r.markSent(req.ID) {
				b.replyError(req.ID, errLost)
			}
			return
		}
		if r.take(req.ID) == nil {
			return
		}
		err = classify(out, err, true)
		if errors.Is(err, errSessionGone) {
			b.lost(r, err)
			if attempt == 0 {
				continue
			}
		}
		b.replyError(req.ID, err)
		if fatal(err) {
			b.end(err)
		}
		return
	}
}

// notify sends a notification or an answer in the session there is. Without
// one, it is dropped: a cancellation or an answer for a session that ended
// means nothing to the next one, which is opened by the next call.
func (b *bridge) notify(msg jsonrpc.Message) {
	req, isRequest := msg.(*jsonrpc.Request)
	initialized := isRequest && req.Method == methodInitialized
	b.mu.Lock()
	r := b.current
	if initialized {
		// Whatever becomes of this one, a new session gets it too.
		b.ready = true
	}
	b.mu.Unlock()
	if r == nil || r.isBroken() {
		if isRequest {
			b.log.Debug("a notification with no session to go to is dropped", "method", req.Method)
		}
		return
	}
	out, err := b.write(r, msg)
	if err != nil {
		err = classify(out, err, true)
		if errors.Is(err, errSessionGone) {
			b.lost(r, err)
			return
		}
		b.log.Warn("the server refused a notification", "err", err)
		if fatal(err) {
			b.end(err)
		}
		return
	}
	if initialized {
		go b.listen(r)
	}
}

// session is the session calls go to: the current one, or a new one opened
// with the client's own initialize when the server ended the last.
func (b *bridge) session() (*remote, error) {
	b.opening.Lock()
	defer b.opening.Unlock()
	b.mu.Lock()
	r, init, ready := b.current, b.init, b.ready
	subs := slices.Sorted(maps.Keys(b.subs))
	b.mu.Unlock()
	if r != nil && !r.isBroken() {
		return r, nil
	}
	if init == nil {
		return nil, fmt.Errorf("%w: the client has not initialized", ErrAnswered)
	}
	// The client's initialize waits on opening too: init is still its.
	r, err := b.reopen(init, ready, subs)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.current = r
	b.mu.Unlock()
	return r, nil
}

// reopen opens a new session as the client opened its first: its initialize,
// its notifications/initialized and the standalone stream, and its
// subscriptions.
func (b *bridge) reopen(init *jsonrpc.Request, ready bool, subs []string) (*remote, error) {
	r, err := b.dial()
	if err != nil {
		return nil, err
	}
	resp, err := b.ownCall(r, methodInitialize, init.Params)
	if err != nil {
		go r.close(b.log)
		return nil, err
	}
	if resp.Error != nil {
		go r.close(b.log)
		return nil, fmt.Errorf("%w: %w", ErrSessionRefused, resp.Error)
	}
	if v := protocolOf(resp.Result); v != "" {
		if previous := b.keys.setVersion(v); previous != "" && previous != v {
			b.log.Warn("the new session speaks another protocol version than the client negotiated",
				"negotiated", previous, "now", v)
		}
	}
	if ready {
		note := &jsonrpc.Request{Method: methodInitialized, Params: json.RawMessage(`{}`)}
		if out, err := b.write(r, note); err != nil {
			go r.close(b.log)
			return nil, classify(out, err, true)
		}
		go b.listen(r)
	}
	for _, uri := range subs {
		params, err := json.Marshal(map[string]string{"uri": uri})
		if err != nil {
			continue
		}
		resp, err := b.ownCall(r, methodSubscribe, params)
		if err == nil && resp.Error == nil {
			continue
		}
		b.log.Warn("a subscription could not be made again in the new session; it is dropped")
		b.mu.Lock()
		delete(b.subs, uri)
		b.mu.Unlock()
		if fatal(err) {
			go r.close(b.log)
			return nil, err
		}
	}
	b.log.Info("the session had ended; a new one is open")
	return r, nil
}

// ownCall makes a request of the bridge's own in r and waits for its answer,
// which the client never sees.
func (b *bridge) ownCall(r *remote, method string, params json.RawMessage) (*jsonrpc.Response, error) {
	id, err := jsonrpc.MakeID("mailie-bridge-" + strconv.FormatInt(b.ids.Add(1), 10))
	if err != nil {
		return nil, err
	}
	answer := r.expect(id)
	if answer == nil {
		return nil, errLost
	}
	out, err := b.write(r, &jsonrpc.Request{ID: id, Method: method, Params: params})
	if err != nil {
		r.unexpect(id)
		return nil, classify(out, err, method != methodInitialize)
	}
	timer := time.NewTimer(ownCallTimeout)
	defer timer.Stop()
	select {
	case resp, ok := <-answer:
		if !ok {
			return nil, errLost
		}
		return resp, nil
	case <-timer.C:
		r.unexpect(id)
		return nil, fmt.Errorf("%w: no answer to %s within %s", ErrUnreachable, method, ownCallTimeout)
	case <-b.ctx.Done():
		return nil, b.ctx.Err()
	}
}

// dial opens a connection to the server, and reads it until it ends. Nothing
// goes over the network until something is written to it.
func (b *bridge) dial() (*remote, error) {
	t := &sdk.StreamableClientTransport{Endpoint: b.endpoint, HTTPClient: b.client}
	conn, err := t.Connect(b.ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	ctx, cancel := context.WithCancel(b.ctx)
	r := &remote{
		conn: conn, ctx: ctx, cancel: cancel,
		calls: map[jsonrpc.ID]*call{}, own: map[jsonrpc.ID]chan *jsonrpc.Response{},
		broken: make(chan struct{}),
	}
	go b.read(r)
	return r, nil
}

// write sends msg on r, with the bridge's life as the request's (an answer
// streamed over minutes is read with it) and an outcome that says what the
// server answered.
func (b *bridge) write(r *remote, msg jsonrpc.Message) (*outcome, error) {
	out := &outcome{}
	return out, r.conn.Write(withOutcome(b.ctx, out), msg)
}

// read relays what the server sends on r until r ends.
func (b *bridge) read(r *remote) {
	for {
		msg, err := r.conn.Read(b.ctx)
		if err != nil {
			b.lost(r, err)
			return
		}
		b.fromServer(r, msg)
	}
}

// fromServer relays one message of the server's.
func (b *bridge) fromServer(r *remote, msg jsonrpc.Message) {
	resp, ok := msg.(*jsonrpc.Response)
	if !ok {
		b.toClient(msg)
		return
	}
	if r.deliverOwn(resp) {
		return
	}
	c := r.take(resp.ID)
	if c == nil {
		// Its caller was told already that the session broke.
		b.log.Debug("an answer to a call that was already answered is dropped")
		return
	}
	if resp.Error == nil {
		b.answered(c, resp)
	}
	b.toClient(resp)
}

// answered keeps what a successful answer changes about the session: the
// protocol version, and the subscriptions to make again in a new session.
func (b *bridge) answered(c *call, resp *jsonrpc.Response) {
	switch c.method {
	case methodInitialize:
		if v := protocolOf(resp.Result); v != "" {
			b.keys.setVersion(v)
		}
	case methodSubscribe, methodUnsubscribe:
		var p struct {
			URI string `json:"uri"`
		}
		if json.Unmarshal(c.params, &p) != nil || p.URI == "" {
			return
		}
		b.mu.Lock()
		if c.method == methodSubscribe {
			b.subs[p.URI] = true
		} else {
			delete(b.subs, p.URI)
		}
		b.mu.Unlock()
	}
}

// errLost answers a call whose session ended before the server answered it.
var errLost = errors.New("mcpbridge: the session with the server ended before it answered; try again")

// lost ends r after its connection broke or the server ended its session:
// every call still waiting on it is answered with an error. A client with
// subscriptions gets a new session at once, so that its notifications go on;
// otherwise the next call opens one.
func (b *bridge) lost(r *remote, cause error) {
	calls, first := r.breakOff()
	if !first {
		return
	}
	for _, id := range calls {
		b.replyError(id, errLost)
	}
	b.mu.Lock()
	current := b.current == r
	if current {
		b.current = nil
	}
	resubscribe := current && len(b.subs) > 0
	b.mu.Unlock()
	go r.close(b.log)
	if !current || b.ctx.Err() != nil {
		return
	}
	switch {
	case errors.Is(cause, sdk.ErrSessionMissing) || errors.Is(cause, errSessionGone):
		b.log.Info("the server ended the session; the next request opens another")
	case errors.Is(cause, errStreamLost) || errors.Is(cause, errStreamHeld):
		b.log.Warn("the bridge ended the session for its stream of notifications; the next request opens another",
			"why", cause)
	default:
		b.log.Warn("the connection to the server broke; the next request opens another", "err", cause)
	}
	if resubscribe {
		go func() {
			if _, err := b.session(); err != nil {
				b.log.Warn("a new session for the client's subscriptions could not be opened", "err", err)
				if fatal(err) {
					b.end(err)
				}
			}
		}()
	}
}

// listen holds r's standalone stream, where the server sends what it says
// outside a request, and relays what comes on it. It asks again when the
// stream ends, with Last-Event-ID so that nothing is missed, until r ends.
//
// A refusal is asked again too, waiting longer each time: the server may be
// restarting (5xx), limiting the key (429), or still holding the stream for
// a connection it has not noticed is dead (409), which it lets go of when it
// does. Only a server without a standalone stream (405) is left alone, and
// one that ended the session (404) or refuses the key is acted on as
// anywhere else. Two answers end the session, which the bridge then opens
// again with the client's subscriptions (lost): a stream the server can no
// longer resume (400 once the stream was had: it no longer keeps what it sent
// meanwhile, and asking without Last-Event-ID resumes from the start, which
// is gone too), and a stream the server still holds after timings.held.
func (b *bridge) listen(r *remote) {
	if !r.startListening() {
		return
	}
	if v := b.keys.version(); v == "" || v >= noStandaloneStream {
		return
	}
	session := r.conn.SessionID()
	if session == "" {
		return
	}
	t := b.timings
	var (
		// last is the id of the last event read, for Last-Event-ID.
		last  string
		delay = t.first
		// had says the stream was had at least once in this session.
		had bool
		// heldSince is when the server first answered, in a row, that it
		// still holds the stream.
		heldSince time.Time
		// refused is the status of the last refusal logged, so that one
		// refused again and again is logged once.
		refused int
	)
	for r.ctx.Err() == nil {
		req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, b.endpoint, nil)
		if err != nil {
			return
		}
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set(sessionHeader, session)
		if last != "" {
			req.Header.Set(lastEventID, last)
		}
		out := &outcome{}
		resp, err := b.client.Do(req.WithContext(withOutcome(r.ctx, out)))
		if err != nil {
			if r.ctx.Err() != nil {
				return
			}
			b.log.Debug("the stream of notifications could not be opened", "err", err)
			if !r.sleep(delay) {
				return
			}
			delay = min(2*delay, t.max)
			continue
		}
		status := resp.StatusCode
		switch {
		case status == http.StatusOK && mediaType(resp) == "text/event-stream":
			had, heldSince, refused, delay = true, time.Time{}, 0, t.first
			if err := readEvents(resp.Body, func(ev event) {
				if ev.id != "" {
					last = ev.id
				}
				if ms, err := strconv.Atoi(ev.retry); err == nil && ms > 0 {
					delay = time.Duration(ms) * time.Millisecond
				}
				if ev.data == "" || (ev.name != "" && ev.name != "message") {
					return
				}
				msg, err := jsonrpc.DecodeMessage([]byte(ev.data))
				if err != nil {
					b.log.Warn("the server sent a notification that is not JSON-RPC; it is dropped")
					return
				}
				b.fromServer(r, msg)
			}); err != nil && r.ctx.Err() == nil {
				b.log.Debug("the stream of notifications broke", "err", err)
			}
			closeBody(resp)
			if !r.sleep(delay) {
				return
			}
			continue
		case status == http.StatusMethodNotAllowed:
			closeBody(resp)
			b.log.Info("the server has no stream of notifications; what it says outside a request does not reach the client")
			return
		case status == http.StatusNotFound:
			closeBody(resp)
			b.lost(r, errSessionGone)
			return
		case status == http.StatusUnauthorized, status == http.StatusForbidden, status >= 300 && status < 400:
			closeBody(resp)
			err := classify(out, errors.New(resp.Status), true)
			b.lost(r, err)
			b.end(err)
			return
		case status == http.StatusBadRequest && had:
			closeBody(resp)
			b.log.Warn("the server no longer keeps what it sent outside a request while the stream of notifications " +
				"was down (a subscription's updates, for one): that is lost, and the session is opened again")
			b.lost(r, errStreamLost)
			return
		case status == http.StatusConflict:
			closeBody(resp)
			now := time.Now()
			if heldSince.IsZero() {
				heldSince = now
			}
			if now.Sub(heldSince) >= t.held {
				b.log.Warn("the server still holds the stream of notifications for a connection the bridge lost; "+
					"the session is opened again, and what the server sent on that connection is lost",
					"waited", t.held)
				b.lost(r, errStreamHeld)
				return
			}
			b.log.Debug("the server still holds the stream of notifications for a connection the bridge lost; asking again",
				"in", delay)
		default:
			closeBody(resp)
			if status != refused {
				refused = status
				level := slog.LevelWarn
				if status == http.StatusTooManyRequests || status >= 500 {
					level = slog.LevelDebug
				}
				b.log.Log(r.ctx, level, "the server refused the stream of notifications; asking again", "status", status)
			}
		}
		if !r.sleep(delay) {
			return
		}
		delay = min(2*delay, t.max)
	}
}

// toClient writes msg to the client. A client that cannot be written to has
// gone: the bridge ends.
func (b *bridge) toClient(msg jsonrpc.Message) {
	if err := b.local.Write(b.ctx, msg); err != nil {
		if b.ctx.Err() == nil {
			b.log.Debug("the client cannot be written to; ending", "err", err)
		}
		b.end(nil)
	}
}

// replyError answers the client's call id with err, which never carries the
// key: neither the server nor the bridge puts it in an error.
func (b *bridge) replyError(id jsonrpc.ID, err error) {
	b.toClient(&jsonrpc.Response{ID: id, Error: &jsonrpc.Error{
		Code: jsonrpc.CodeInternalError, Message: "mailie: " + err.Error(),
	}})
}

// remote is one connection to the server: a session, or a negotiation
// before one.
type remote struct {
	conn   sdk.Connection
	ctx    context.Context // ends with the connection: its standalone stream
	cancel context.CancelFunc

	mu sync.Mutex
	// calls are the client's calls sent on this connection and not answered.
	calls map[jsonrpc.ID]*call
	// own are the bridge's own requests waiting for their answer.
	own       map[jsonrpc.ID]chan *jsonrpc.Response
	listening bool
	// broken is closed when the connection ends: the server ended the
	// session, the connection failed, or the bridge closed it.
	broken     chan struct{}
	breakOnce  sync.Once
	closeOnce  sync.Once
	brokenFlag bool
}

// call is one of the client's calls in flight.
type call struct {
	method string
	params json.RawMessage
	done   chan struct{}
	// sent is the call's request accepted by the server. Until then the call
	// is its sender's to answer, or to send again in a new session: a call
	// that never reached a session that ended is not lost with it.
	sent bool
}

// track records the client's call req as in flight on r; nil when r has
// ended.
func (r *remote) track(req *jsonrpc.Request) *call {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.brokenFlag {
		return nil
	}
	c := &call{method: req.Method, params: req.Params, done: make(chan struct{})}
	r.calls[req.ID] = c
	return c
}

// take removes the call id and returns it, to whoever answers it: nil when
// it was answered already.
func (r *remote) take(id jsonrpc.ID) *call {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.calls[id]
	if !ok {
		return nil
	}
	delete(r.calls, id)
	close(c.done)
	return c
}

// markSent records that the server accepted the call id. It reports true
// when r ended while the request was being sent: no answer will come, and
// the caller answers the call itself.
func (r *remote) markSent(id jsonrpc.ID) (lost bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.calls[id]
	if !ok {
		return false // answered already
	}
	if r.brokenFlag {
		delete(r.calls, id)
		close(c.done)
		return true
	}
	c.sent = true
	return false
}

// expect registers one of the bridge's own requests; nil when r has ended.
func (r *remote) expect(id jsonrpc.ID) chan *jsonrpc.Response {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.brokenFlag {
		return nil
	}
	ch := make(chan *jsonrpc.Response, 1)
	r.own[id] = ch
	return ch
}

func (r *remote) unexpect(id jsonrpc.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.own, id)
}

// deliverOwn hands an answer to the bridge's own request that waits for it.
func (r *remote) deliverOwn(resp *jsonrpc.Response) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch, ok := r.own[resp.ID]
	if ok {
		delete(r.own, resp.ID)
		ch <- resp
	}
	return ok
}

// breakOff ends r and returns the calls the server accepted and has not
// answered; first says whether this call ended it.
func (r *remote) breakOff() (calls []jsonrpc.ID, first bool) {
	r.breakOnce.Do(func() {
		first = true
		r.mu.Lock()
		defer r.mu.Unlock()
		r.brokenFlag = true
		for id, c := range r.calls {
			if !c.sent {
				continue // its sender is still sending it, and answers it
			}
			calls = append(calls, id)
			close(c.done)
			delete(r.calls, id)
		}
		for id, ch := range r.own {
			close(ch)
			delete(r.own, id)
		}
		close(r.broken)
	})
	return calls, first
}

func (r *remote) isBroken() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.brokenFlag
}

// startListening reports whether the standalone stream is this caller's to
// hold: once per connection.
func (r *remote) startListening() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listening {
		return false
	}
	r.listening = true
	return true
}

// close ends r and its session on the server (the SDK's DELETE, bounded).
func (r *remote) close(log *slog.Logger) {
	r.closeOnce.Do(func() {
		r.cancel()
		if err := r.conn.Close(); err != nil {
			log.Debug("closing a connection to the server", "err", err)
		}
	})
}

// sleep waits d, or less if r ends; it reports whether r is still alive.
func (r *remote) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.ctx.Done():
		return false
	}
}

// protocolOf is the protocolVersion of an initialize result.
func protocolOf(result json.RawMessage) string {
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(result, &res) != nil {
		return ""
	}
	return res.ProtocolVersion
}

func cloneRequest(req *jsonrpc.Request) *jsonrpc.Request {
	return &jsonrpc.Request{ID: req.ID, Method: req.Method, Params: bytes.Clone(req.Params)}
}

func mediaType(resp *http.Response) string {
	v, _, _ := bytes.Cut([]byte(resp.Header.Get("Content-Type")), []byte(";"))
	return string(bytes.ToLower(bytes.TrimSpace(v)))
}

func closeBody(resp *http.Response) {
	_ = resp.Body.Close() //nolint:errcheck // a stream that ended or was refused; nothing more to read
}
