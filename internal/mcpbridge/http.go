package mcpbridge

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Headers of the Streamable HTTP transport.
const (
	sessionHeader  = "Mcp-Session-Id"
	protocolHeader = "Mcp-Protocol-Version"
	lastEventID    = "Last-Event-ID"
)

// errOtherOrigin is a request to anywhere but the endpoint's origin, which
// the key never goes to.
var errOtherOrigin = errors.New("mcpbridge: a request to another origin than the endpoint's is not sent")

// keyTransport adds the key to every request to the endpoint, and the
// protocol version the session negotiated to every request of the session
// that does not carry one already (see the package comment).
type keyTransport struct {
	next   http.RoundTripper
	key    string
	origin string
	agent  string

	mu       sync.Mutex
	protocol string
}

func (t *keyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.EqualFold(req.URL.Scheme+"://"+req.URL.Host, t.origin) {
		if req.Body != nil {
			_ = req.Body.Close() //nolint:errcheck // the request is refused whatever closing says
		}
		return nil, errOtherOrigin
	}
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.key)
	if t.agent != "" {
		req.Header.Set("User-Agent", t.agent)
	}
	if req.Header.Get(protocolHeader) == "" && req.Header.Get(sessionHeader) != "" {
		if v := t.version(); v != "" {
			req.Header.Set(protocolHeader, v)
		}
	}
	resp, err := t.next.RoundTrip(req)
	if out, ok := req.Context().Value(outcomeKey{}).(*outcome); ok {
		out.record(resp, err)
	}
	return resp, err
}

func (t *keyTransport) version() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.protocol
}

func (t *keyTransport) setVersion(v string) (previous string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	previous, t.protocol = t.protocol, v
	return previous
}

// outcome is what the server answered a request the SDK made on the
// bridge's behalf: the SDK's errors say "Unauthorized" in words, and the
// bridge needs the status, and a Mailie server's {code, message}, to say what
// to do about it.
type outcome struct {
	mu         sync.Mutex
	seen       bool
	status     int
	body       []byte
	retryAfter string
}

type outcomeKey struct{}

// withOutcome is ctx carrying out: the requests made with it record their
// first answer there.
func withOutcome(ctx context.Context, out *outcome) context.Context {
	return context.WithValue(ctx, outcomeKey{}, out)
}

// maxRefusalBody is how much of a refusal's body is kept to read its code.
const maxRefusalBody = 4 << 10

// record keeps the first answer: a refusal's body is read up to
// maxRefusalBody and handed on unread to whoever reads the response next.
func (o *outcome) record(resp *http.Response, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.seen || err != nil || resp == nil {
		return
	}
	o.seen, o.status = true, resp.StatusCode
	if resp.StatusCode < 300 {
		return
	}
	o.retryAfter = resp.Header.Get("Retry-After")
	peek, rerr := io.ReadAll(io.LimitReader(resp.Body, maxRefusalBody))
	if rerr != nil {
		peek = nil
	}
	o.body = peek
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(peek), resp.Body), resp.Body}
}

func (o *outcome) result() (status int, body []byte, retryAfter string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.status, o.body, o.retryAfter
}

func (o *outcome) detail() string {
	status, body, _ := o.result()
	if d := detailOf(body); d != "" {
		return d
	}
	return strings.ToLower(http.StatusText(status))
}

// errSessionGone is a request that found its session gone: a new one is
// opened and the request sent again, once.
var errSessionGone = errors.New("mcpbridge: the server ended the session")

// classify turns what a request to the server ended with into the error the
// bridge acts on. inSession says whether the request belonged to a session:
// a 404 then means the session is gone; before one, that there is no MCP at
// the address.
func classify(out *outcome, err error, inSession bool) error {
	if errors.Is(err, sdk.ErrSessionMissing) {
		return errSessionGone
	}
	status, _, retryAfter := out.result()
	refusal := func(sentinel error) error {
		return &Refusal{Err: sentinel, Status: status, Detail: out.detail(), RetryAfter: retryAfter}
	}
	switch {
	case status == 0:
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	case status == http.StatusUnauthorized:
		return refusal(ErrKeyRefused)
	case status == http.StatusForbidden:
		return refusal(ErrKeyNotAllowed)
	case status == http.StatusNotFound && inSession:
		return errSessionGone
	case status == http.StatusNotFound:
		return refusal(ErrNoMCP)
	case status >= 300 && status < 400:
		return refusal(ErrRedirected)
	case status >= 300:
		return refusal(ErrAnswered)
	}
	// The request was answered, and the answer could not be used: a body the
	// SDK could not read, for one.
	return fmt.Errorf("%w: %w", ErrAnswered, err)
}

// maxEvent bounds one server-sent event on the standalone stream, as the
// server bounds a request.
const maxEvent = 16 << 20

// event is one server-sent event.
type event struct {
	name, id, data string
	retry          string
}

// readEvents reads server-sent events from r and hands each to fn, until r
// ends or fails. Comments (the server's keep-alives) are skipped.
func readEvents(r io.Reader, fn func(event)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxEvent)
	var ev event
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if data.Len() > 0 || ev.id != "" {
				ev.data = data.String()
				fn(ev)
			}
			ev, data = event{}, strings.Builder{}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			ev.name = value
		case "data":
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		case "id":
			ev.id = value
		case "retry":
			ev.retry = value
		}
	}
	return sc.Err()
}
