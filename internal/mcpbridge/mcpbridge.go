// Package mcpbridge relays MCP between a client that launches a local
// process (standard input and output) and a Mailie server's Streamable HTTP
// endpoint, with an API key the client never sees on the wire.
//
// It is a message bridge, not a second server: every JSON-RPC message the
// client writes is posted to the server as it came, and every message the
// server sends — answers, progress, notifications, its own requests — is
// written back to the client as it came. Tools, resources, subscriptions and
// whatever the server adds later pass through without the bridge knowing
// them. Both ends are the go-sdk's own transports: StdioTransport towards the
// client and StreamableClientTransport towards the server, each used through
// its raw Connection (Read and Write of JSON-RPC messages).
//
// The SDK's HTTP client connection does two things only when its own
// ClientSession tells it the handshake is over, through a method nothing
// outside the SDK can call: it sends the negotiated MCP-Protocol-Version on
// every later request, and it opens the session's standalone GET stream, where
// a server sends what it says outside a request (a subscription's
// notifications/resources/updated, for one). A bridge has no ClientSession —
// the session is the client's — so it does both itself: it reads the version
// from the server's answer to initialize and adds the header (keyTransport),
// and it holds the GET stream (listen).
//
// A Mailie server ends a session left idle for 30 minutes, when a key opens
// too many, and when it restarts. The protocol tells a client to open another
// session then, and the client behind a bridge cannot know it should: so the
// bridge does it, replaying the client's own initialize and its
// subscriptions, and retries the request that found the session gone. Calls
// that were in flight on the session that ended are answered with an error,
// so the client never waits for an answer that cannot come.
//
// What ends the bridge: the client closing standard input (a clean end), and
// an answer that no retry can change — the key refused (401) or not allowed to
// use MCP (403), no MCP endpoint at the address (404 before any session) or a
// redirect, which is never followed because the key would go along.
package mcpbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thehappieco/mailie/internal/config"
)

// Errors a bridge or a check ends with. A *Refusal wraps the ones that come
// from an answer of the server, with its status and words.
var (
	// ErrBadURL is an address that is not a server's.
	ErrBadURL = errors.New("mcpbridge: not a server address")
	// ErrInsecureURL is plain http to another machine: the key would cross
	// the network in clear.
	ErrInsecureURL = errors.New("mcpbridge: plain http is accepted only for this machine " +
		"(localhost, 127.0.0.1, ::1); use the server's https:// address")
	// ErrNoKey is a bridge started without a key.
	ErrNoKey = errors.New("mcpbridge: no API key")
	// ErrKeyRefused is the server's 401: the key is wrong, expired or revoked.
	ErrKeyRefused = errors.New("mcpbridge: the server refused the key; it is wrong, expired or revoked")
	// ErrKeyNotAllowed is the server's 403: the key exists but may not use
	// MCP, such as a key nobody agreed to the key terms through.
	ErrKeyNotAllowed = errors.New("mcpbridge: the server does not let this key use MCP")
	// ErrNoMCP is a 404 before any session: no MCP endpoint at the address.
	ErrNoMCP = errors.New("mcpbridge: no MCP endpoint at this address: the address is not a Mailie server's, " +
		"or MCP over HTTP is switched off there (MAIL_MCP_HTTP=false)")
	// ErrRedirected is a redirect, which is never followed: the key would
	// go along to wherever it points.
	ErrRedirected = errors.New("mcpbridge: the server answered with a redirect, which is not followed; " +
		"give the address it redirects to")
	// ErrUnreachable is a request that never got an answer.
	ErrUnreachable = errors.New("mcpbridge: the server could not be reached")
	// ErrSessionRefused is a server that refuses to open a session again
	// with the client's own initialize, which it accepted before.
	ErrSessionRefused = errors.New("mcpbridge: the server refused to open a new session")
	// ErrAnswered is any other refusal: a rate limit, a server error. It
	// fails the request and leaves the bridge running.
	ErrAnswered = errors.New("mcpbridge: the server refused the request")
)

// Refusal is an answer of the server that refused a request: the sentinel it
// stands for, the HTTP status, and the {code, message} a Mailie server gives
// with it.
type Refusal struct {
	Err    error
	Status int
	// Detail is "code: message" from a Mailie server's body, or the status
	// text. Never the key: the server does not repeat it.
	Detail string
	// RetryAfter is the server's Retry-After, in seconds, when it gave one.
	RetryAfter string
}

func (r *Refusal) Error() string {
	s := fmt.Sprintf("%v (HTTP %d: %s)", r.Err, r.Status, r.Detail)
	if r.RetryAfter != "" {
		s += "; retry in " + r.RetryAfter + " s"
	}
	return s
}

func (r *Refusal) Unwrap() error { return r.Err }

// fatal reports whether err ends the bridge: nothing it could retry would
// change the answer.
func fatal(err error) bool {
	return errors.Is(err, ErrKeyRefused) || errors.Is(err, ErrKeyNotAllowed) || errors.Is(err, ErrNoMCP) ||
		errors.Is(err, ErrRedirected) || errors.Is(err, ErrSessionRefused)
}

// Endpoint resolves a server's address to its MCP endpoint: the base address
// with /mcp after it, or the endpoint itself. https only, except for this
// machine; no user, password, query or fragment.
func Endpoint(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Opaque != "" || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		// Not quoted back: whatever was typed here may hold a credential.
		return "", fmt.Errorf("%w: give its base address, such as https://mail.example.com", ErrBadURL)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: the address carries a user name or password; "+
			"the key never goes in an address", ErrBadURL)
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("%w: the address carries a query or a fragment", ErrBadURL)
	}
	if u.Scheme == "http" && !config.IsLoopbackHost(u.Hostname()) {
		return "", ErrInsecureURL
	}
	path := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(path, "/mcp") {
		path += "/mcp"
	}
	u.Path, u.RawPath = path, ""
	return u.String(), nil
}

// Base is the server's address for an endpoint Endpoint returned.
func Base(endpoint string) string { return strings.TrimSuffix(endpoint, "/mcp") }

// Options configure a bridge or a check.
type Options struct {
	// Endpoint is the server's MCP endpoint; Endpoint checks it again.
	Endpoint string
	// Key is the API key, sent as a bearer token on every request to the
	// endpoint, and nowhere else.
	Key string
	// Log receives the bridge's own lines (never a message's content). It
	// must not be the client's standard output; nil discards.
	Log *slog.Logger
	// UserAgent names this program to the server.
	UserAgent string
	// Transport carries the requests; nil is a clone of
	// http.DefaultTransport.
	Transport http.RoundTripper
}

// client builds the HTTP client every request to the server goes through.
func (o Options) client() (*http.Client, *keyTransport, error) {
	endpoint, err := Endpoint(o.Endpoint)
	if err != nil {
		return nil, nil, err
	}
	if o.Key == "" {
		return nil, nil, ErrNoKey
	}
	next := o.Transport
	if next == nil {
		next = http.DefaultTransport
		if t, ok := next.(*http.Transport); ok {
			next = t.Clone()
		}
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrBadURL, err)
	}
	keys := &keyTransport{next: next, key: o.Key, origin: u.Scheme + "://" + u.Host, agent: o.UserAgent}
	return &http.Client{
		Transport: keys,
		// A redirect is answered, never followed: the key would follow it.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, keys, nil
}

func (o Options) logger() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.New(slog.DiscardHandler)
}

// checkedProtocol is the version a check asks for: the newest the stateful
// HTTP transport serves, so the check is the handshake a client will make.
const checkedProtocol = "2025-11-25"

// Check opens a session with the key and closes it again: what a client will
// do, so a key that cannot is refused before anything is configured with it.
func Check(ctx context.Context, o Options) error {
	client, _, err := o.client()
	if err != nil {
		return err
	}
	out := &outcome{}
	transport := &sdk.StreamableClientTransport{
		Endpoint: mustEndpoint(o.Endpoint), HTTPClient: client, MaxRetries: -1, DisableStandaloneSSE: true,
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "mailie-check", Version: "1"}, nil).
		Connect(withOutcome(ctx, out), transport, &sdk.ClientSessionOptions{ProtocolVersion: checkedProtocol})
	if err != nil {
		return classify(out, err, false)
	}
	if err := cs.Close(); err != nil {
		o.logger().Debug("closing the session the check opened", "err", err)
	}
	return nil
}

// Reachable asks the endpoint for a session without a key: a Mailie server
// answers 401, which says the address is right without spending a key on it.
// It fails only on what no key would change: no endpoint, a redirect, no
// answer.
func Reachable(ctx context.Context, endpoint string, transport http.RoundTripper) error {
	endpoint, err := Endpoint(endpoint)
	if err != nil {
		return err
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + checkedProtocol +
		`","capabilities":{},"clientInfo":{"name":"mailie-check","version":"1"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBadURL, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // a body read to its end or abandoned
	out := &outcome{}
	out.record(resp, nil)
	switch status, _, _ := out.result(); {
	case status == http.StatusNotFound:
		return &Refusal{Err: ErrNoMCP, Status: status, Detail: out.detail()}
	case status >= 300 && status < 400:
		return &Refusal{Err: ErrRedirected, Status: status, Detail: out.detail()}
	}
	return nil
}

// mustEndpoint is Endpoint for an address Options.client has already
// checked.
func mustEndpoint(raw string) string {
	endpoint, err := Endpoint(raw)
	if err != nil {
		return raw
	}
	return endpoint
}

// mailieError is the body a Mailie server answers a refusal with.
type mailieError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// detailOf is "code: message" when body is a Mailie server's refusal, made
// printable and short; "" otherwise. Another server's body, an HTML page for
// one, is not repeated.
func detailOf(body []byte) string {
	var e mailieError
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil || e.Code == "" {
		return ""
	}
	s := e.Code
	if e.Message != "" {
		s += ": " + e.Message
	}
	s = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}
