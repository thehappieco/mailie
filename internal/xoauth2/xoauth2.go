// Package xoauth2 implements the XOAUTH2 SASL mechanism.
//
// go-sasl ships PLAIN, LOGIN, ANONYMOUS, EXTERNAL and OAUTHBEARER, but not
// this one, and XOAUTH2 is what both providers this server targets actually
// accept: Exchange Online advertises AUTH=XOAUTH2 and nothing else usable,
// and Gmail advertises it alongside OAUTHBEARER. Standardising on XOAUTH2
// means one code path for both.
//
// The mechanism is Google's, and simple: one base64 blob holding
//
//	user=<address>\x01auth=Bearer <access token>\x01\x01
//
// The interesting part is failure. Gmail does not answer a bad token with a
// tagged NO; it sends a SASL continuation whose payload is base64 JSON like
// {"status":"401","schemes":"bearer","scope":"https://mail.google.com/"}, and
// only after the client answers does the tagged failure arrive. Exchange
// Online skips the continuation and fails outright. Both have to work.
package xoauth2

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/emersion/go-sasl"
)

// Name is the mechanism name as advertised in CAPABILITY and EHLO.
const Name = "XOAUTH2"

// Challenge is the error a server reported through a SASL continuation.
//
// It is recorded on the client rather than returned from Next, because
// returning an error there would make go-imap abandon the exchange without
// sending the empty response the server is waiting for, leaving the connection
// wedged mid-AUTHENTICATE.
type Challenge struct {
	Status  string `json:"status"`
	Schemes string `json:"schemes"`
	Scope   string `json:"scope"`
	// Raw is the decoded payload, kept when it is not the JSON Google
	// documents.
	Raw string `json:"-"`
}

// Error says what the server reported: the status and, when Gmail names it,
// the scope the token would have needed. Both are what tell an expired token
// from one that never had access to the mailbox, and neither identifies
// anyone, so a log may carry them whole.
func (c *Challenge) Error() string {
	if c.Status != "" {
		detail := "status " + c.Status
		if c.Scope != "" {
			detail += ", scope " + c.Scope
		}
		return "xoauth2: server rejected the access token (" + detail + ")"
	}
	return "xoauth2: server rejected the access token: " + c.Raw
}

// Expired reports whether the challenge says the token is no longer valid, so
// the caller should refresh once and retry rather than ask the user to consent
// again. Google uses 401 for an expired or revoked token and 400 for a
// malformed request.
func (c *Challenge) Expired() bool {
	return c.status() == 401
}

// Throttled reports whether the server refused because of how often it is
// being asked, not because of the token.
func (c *Challenge) Throttled() bool {
	return c.status() == 429
}

// ServerFault reports whether the status is the server failing — a 5xx —
// rather than refusing the token. Google documents only 400 and 401 here; a
// fault it may send one day must not read as a grant that is gone.
func (c *Challenge) ServerFault() bool {
	code := c.status()
	return code >= 500 && code <= 599
}

// status is the HTTP-like status, or 0 when there is none to read.
func (c *Challenge) status() int {
	code, err := strconv.Atoi(c.Status)
	if err != nil {
		return 0
	}
	return code
}

// ErrTokenRejected is what a Challenge unwraps to, so callers can classify
// without reaching for the concrete type.
var ErrTokenRejected = errors.New("xoauth2: access token rejected")

func (c *Challenge) Unwrap() error { return ErrTokenRejected }

// Client authenticates as user with an OAuth 2.0 access token.
type Client struct {
	user  string
	token string

	// challenge records what the server said when it refused, for the caller
	// to inspect after the command fails.
	challenge *Challenge
}

// NewClient builds an XOAUTH2 client. The token is the access token, without
// the "Bearer " prefix.
func NewClient(user, token string) *Client { return &Client{user: user, token: token} }

var _ sasl.Client = (*Client)(nil)

// Start returns the mechanism and its initial response.
//
// The raw bytes, never their base64: go-imap and go-smtp encode the initial
// response themselves. Returning the encoded form here sent Gmail a blob it
// decoded to more base64, which it refused as a malformed request (status
// 400) — every Gmail account failed on its first use, while the offline fakes,
// which then accepted either form, passed. Decode is strict for that reason.
func (c *Client) Start() (string, []byte, error) {
	return Name, Raw(c.user, c.token), nil
}

// Next answers a continuation.
//
// It never returns an error, deliberately. A server that refuses the token
// sends its explanation as a challenge and then waits for an empty response
// before reporting failure; go-imap's Authenticate abandons the exchange the
// moment this returns an error, without sending that response and without
// draining the tagged reply, which leaves the connection unusable. Returning an
// empty response — which the IMAP and SMTP layers encode as "=" — lets the
// exchange finish so the real error arrives through the command itself.
func (c *Client) Next(challenge []byte) ([]byte, error) {
	c.challenge = parseChallenge(challenge)
	return []byte{}, nil
}

// Challenge returns what the server said when it refused, or nil.
func (c *Client) Challenge() *Challenge { return c.challenge }

// Encode builds the initial response as it travels on the wire, base64 of Raw.
// Transports produce it from what Start returns; it is here for callers that
// write the wire format by hand.
func Encode(user, token string) string {
	return base64.StdEncoding.EncodeToString(Raw(user, token))
}

// Raw builds the unencoded initial response.
func Raw(user, token string) []byte {
	return fmt.Appendf(nil, "user=%s\x01auth=Bearer %s\x01\x01", user, token)
}

func parseChallenge(payload []byte) *Challenge {
	raw := string(payload)
	// Some servers send the JSON already decoded, some base64 it a second
	// time inside the SASL payload; try both rather than lose the reason.
	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil && json.Valid(decoded) {
		raw = string(decoded)
	}
	out := &Challenge{Raw: raw}
	// A challenge that is not the JSON Google documents is still a refusal
	// worth reporting; Raw carries it verbatim either way.
	//nolint:errcheck // unparsable challenges fall back to Raw
	_ = json.Unmarshal([]byte(raw), out)
	return out
}

// Authenticator verifies a user and token pair on the server side.
type Authenticator func(user, token string) error

// NewServer builds the server half of the mechanism.
//
// It exists for tests: with it, the in-process IMAP and SMTP fakes speak the
// same authentication the real providers do, so the whole OAuth path — mint a
// token, present it, have it refused, refresh, present it again — runs offline.
func NewServer(auth Authenticator) sasl.Server { return &server{auth: auth} }

type server struct {
	auth Authenticator
	done bool
}

func (s *server) Next(response []byte) (challenge []byte, done bool, err error) {
	if s.done {
		return nil, false, sasl.ErrUnexpectedClientResponse
	}
	if response == nil {
		// No initial response: ask for one.
		return []byte{}, false, nil
	}
	s.done = true

	user, token, err := Decode(response)
	if err != nil {
		return nil, false, err
	}
	if err := s.auth(user, token); err != nil {
		return nil, false, err
	}
	return nil, true, nil
}

// Decode parses an XOAUTH2 initial response as the transport hands it over:
// already base64-decoded.
//
// Strict on purpose. It used to accept a second layer of base64 as well, and
// that tolerance is what let a client that encoded twice pass every offline
// test while Gmail, which decodes once, refused it.
func Decode(response []byte) (user, token string, err error) {
	parts := strings.Split(strings.TrimRight(string(response), "\x01"), "\x01")
	for _, part := range parts {
		switch {
		case strings.HasPrefix(part, "user="):
			user = strings.TrimPrefix(part, "user=")
		case strings.HasPrefix(part, "auth=Bearer "):
			token = strings.TrimPrefix(part, "auth=Bearer ")
		}
	}
	if user == "" || token == "" {
		return "", "", errors.New("xoauth2: malformed initial response")
	}
	return user, token, nil
}
