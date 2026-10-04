package xoauth2_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/xoauth2"
)

func TestTheInitialResponseMatchesTheDocumentedWireFormat(t *testing.T) {
	// user=<address>^Aauth=Bearer <token>^A^A, base64-encoded. Both Google and
	// Microsoft document exactly this; a stray space or a missing separator is
	// an authentication failure with no useful message.
	got := xoauth2.Encode("me@example.com", "tok123")
	raw, err := base64.StdEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("not valid base64: %v", err)
	}
	if want := "user=me@example.com\x01auth=Bearer tok123\x01\x01"; string(raw) != want {
		t.Fatalf("initial response = %q, want %q", raw, want)
	}
}

func TestStartAdvertisesTheMechanismWithAnInitialResponse(t *testing.T) {
	c := xoauth2.NewClient("me@example.com", "tok123")
	mech, ir, err := c.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if mech != "XOAUTH2" {
		t.Fatalf("mechanism = %q", mech)
	}
	if len(ir) == 0 {
		t.Fatal("XOAUTH2 must send an initial response; SASL-IR is what both providers expect")
	}
	// The raw mechanism, not its base64: the transport encodes it. Encoding
	// it here as well is what Gmail refused as a malformed request.
	if want := "user=me@example.com\x01auth=Bearer tok123\x01\x01"; string(ir) != want {
		t.Fatalf("initial response = %q, want the raw %q", ir, want)
	}
}

func TestNextNeverReturnsAnErrorSoTheExchangeCanFinish(t *testing.T) {
	// go-imap's Authenticate abandons the command the moment Next errors,
	// without sending a response and without draining the tagged reply. The
	// connection is then wedged mid-AUTHENTICATE, which looks like a hung
	// account rather than a rejected token.
	c := xoauth2.NewClient("me@example.com", "expired")
	payload := base64.StdEncoding.EncodeToString([]byte(`{"status":"401","schemes":"bearer","scope":"https://mail.google.com/"}`))

	resp, err := c.Next([]byte(payload))
	if err != nil {
		t.Fatalf("Next returned an error: %v", err)
	}
	if resp == nil {
		t.Fatal("Next must return an empty response, not nil: the server is waiting for one")
	}
	if len(resp) != 0 {
		t.Fatalf("Next returned %q, want an empty response", resp)
	}
}

func TestAGmailRejectionIsReadableAfterTheCommandFails(t *testing.T) {
	c := xoauth2.NewClient("me@example.com", "expired")
	payload := base64.StdEncoding.EncodeToString([]byte(`{"status":"401","schemes":"bearer","scope":"https://mail.google.com/"}`))
	if _, err := c.Next([]byte(payload)); err != nil {
		t.Fatalf("Next: %v", err)
	}

	ch := c.Challenge()
	if ch == nil {
		t.Fatal("the challenge was not recorded")
	}
	if ch.Status != "401" {
		t.Errorf("status = %q, want 401", ch.Status)
	}
	// 401 means refresh the token and try once more; asking the user to
	// consent again for an expired access token would be wrong.
	if !ch.Expired() {
		t.Error("a 401 challenge should be classified as an expired token")
	}
	if !errors.Is(ch, xoauth2.ErrTokenRejected) {
		t.Error("a challenge should unwrap to ErrTokenRejected")
	}
}

func TestARejectionSaysTheStatusAndTheScopeTheServerReported(t *testing.T) {
	// The status tells an expired token from a refused one, and Gmail's scope
	// field says what the token lacked. A log that has only "rejected" leaves
	// an operator guessing which.
	c := xoauth2.NewClient("me@example.com", "tok")
	payload := base64.StdEncoding.EncodeToString([]byte(`{"status":"400","schemes":"Bearer","scope":"https://mail.google.com/"}`))
	if _, err := c.Next([]byte(payload)); err != nil {
		t.Fatalf("Next: %v", err)
	}
	want := "xoauth2: server rejected the access token (status 400, scope https://mail.google.com/)"
	if got := c.Challenge().Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}

	bare := xoauth2.NewClient("me@example.com", "tok")
	if _, err := bare.Next([]byte(`{"status":"401"}`)); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got := bare.Challenge().Error(); got != "xoauth2: server rejected the access token (status 401)" {
		t.Fatalf("without a scope, Error() = %q", got)
	}
}

func TestAMalformedRequestIsNotTreatedAsAnExpiredToken(t *testing.T) {
	// Google answers 400 when the request itself is wrong. Refreshing the
	// token would not help and would hide the real problem.
	c := xoauth2.NewClient("me@example.com", "tok")
	payload := base64.StdEncoding.EncodeToString([]byte(`{"status":"400","schemes":"bearer"}`))
	if _, err := c.Next([]byte(payload)); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if c.Challenge().Expired() {
		t.Error("a 400 challenge must not trigger a token refresh")
	}
}

func TestOnlyA400Or401ChallengeIsARefusalOfTheToken(t *testing.T) {
	// A refusal now costs a consent or parks an account, so a status that
	// says the server is failing or busy must not be read as one.
	for status, want := range map[string]struct{ expired, throttled, fault bool }{
		"400": {}, "403": {}, "": {},
		"401": {expired: true},
		"429": {throttled: true},
		"500": {fault: true}, "503": {fault: true},
	} {
		c := xoauth2.NewClient("me@example.com", "tok")
		payload := base64.StdEncoding.EncodeToString([]byte(`{"status":"` + status + `","schemes":"Bearer"}`))
		if _, err := c.Next([]byte(payload)); err != nil {
			t.Fatalf("Next: %v", err)
		}
		ch := c.Challenge()
		if ch.Expired() != want.expired || ch.Throttled() != want.throttled || ch.ServerFault() != want.fault {
			t.Errorf("status %q: expired=%v throttled=%v fault=%v, want %+v",
				status, ch.Expired(), ch.Throttled(), ch.ServerFault(), want)
		}
	}
}

func TestANonJSONChallengeIsKeptVerbatim(t *testing.T) {
	c := xoauth2.NewClient("me@example.com", "tok")
	if _, err := c.Next([]byte("something unexpected")); err != nil {
		t.Fatalf("Next: %v", err)
	}
	ch := c.Challenge()
	if !strings.Contains(ch.Error(), "something unexpected") {
		t.Fatalf("the raw challenge was lost: %q", ch.Error())
	}
	if ch.Expired() {
		t.Error("an unparsable challenge must not be read as an expired token")
	}
}

func TestTheServerHalfAcceptsWhatTheClientHalfSends(t *testing.T) {
	// The two halves together are what lets the whole OAuth path be tested
	// offline against the in-process IMAP and SMTP fakes.
	var gotUser, gotToken string
	srv := xoauth2.NewServer(func(user, token string) error {
		gotUser, gotToken = user, token
		return nil
	})

	client := xoauth2.NewClient("me@example.com", "tok123")
	_, ir, err := client.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	challenge, done, err := srv.Next(ir)
	if err != nil {
		t.Fatalf("server Next: %v", err)
	}
	if !done {
		t.Fatalf("authentication did not complete; challenge %q", challenge)
	}
	if gotUser != "me@example.com" || gotToken != "tok123" {
		t.Fatalf("server saw user=%q token=%q", gotUser, gotToken)
	}
}

func TestTheServerHalfRejectsABadToken(t *testing.T) {
	want := errors.New("no")
	srv := xoauth2.NewServer(func(user, token string) error { return want })

	_, done, err := srv.Next(xoauth2.Raw("me@example.com", "wrong"))
	if done {
		t.Fatal("a rejected token must not complete authentication")
	}
	if !errors.Is(err, want) {
		t.Fatalf("want the authenticator's error, got %v", err)
	}
}

func TestDecodeRefusesAnInitialResponseEncodedTwice(t *testing.T) {
	// A transport hands the server the initial response already decoded once.
	// If the client had encoded it too, what arrives is still base64 — and a
	// server that accepted that would hide the bug Gmail refuses.
	if _, _, err := xoauth2.Decode([]byte(xoauth2.Encode("me@example.com", "tok123"))); err == nil {
		t.Fatal("a doubly encoded initial response was accepted")
	}
	user, token, err := xoauth2.Decode(xoauth2.Raw("me@example.com", "tok123"))
	if err != nil || user != "me@example.com" || token != "tok123" {
		t.Fatalf("raw response: user=%q token=%q err=%v", user, token, err)
	}
}

func TestDecodeRejectsAResponseMissingItsParts(t *testing.T) {
	for name, response := range map[string]string{
		"no token": "user=me@example.com\x01\x01",
		"no user":  "auth=Bearer tok\x01\x01",
		"empty":    "",
	} {
		if _, _, err := xoauth2.Decode([]byte(response)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
