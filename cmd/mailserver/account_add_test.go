package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/config"
)

// cliDaemon is a daemon the account commands talk to: each route, "METHOD
// /path", answers the JSON the test gives it, and every request is kept.
type cliDaemon struct {
	cfg      config.Config
	mu       sync.Mutex
	requests []cliRequest
}

// cliRequest is one request a cliDaemon received.
type cliRequest struct {
	route string
	body  map[string]any
}

func newCLIDaemon(t *testing.T, answers map[string]string) *cliDaemon {
	t.Helper()
	d := &cliDaemon{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer deadbeef.secret" {
			t.Errorf("a request without the admin key: %q", r.Header.Get("Authorization"))
		}
		route := r.Method + " " + r.URL.Path
		var body map[string]any
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("%s: decoding the request: %v", route, err)
			}
		}
		d.mu.Lock()
		d.requests = append(d.requests, cliRequest{route: route, body: body})
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		answer, ok := answers[route]
		if !ok {
			t.Errorf("unexpected %s", route)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"not_found","message":"no such route"}`))
			return
		}
		if route == "POST /v1/accounts" {
			w.WriteHeader(http.StatusCreated)
		}
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	d.cfg = config.Config{HTTPAddr: strings.TrimPrefix(srv.URL, "http://"), AdminKey: "deadbeef.secret"}
	return d
}

// sent is every request the daemon received, in order.
func (d *cliDaemon) sent() []cliRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]cliRequest(nil), d.requests...)
}

// passwordMailboxAdded is POST /v1/accounts's answer for an operator
// mailbox that signs in with a password, as the daemon gives it: connected,
// its sync off.
const passwordMailboxAdded = `{"account":{"id":"acc_7","email":"ops@mail.example","provider":"imap",` +
	`"auth_kind":"password","state":"active","sync":{"enabled":false,"running":false,"state":"off"}}}`

// addPasswordMailbox is account add for a generic IMAP mailbox, its
// password read as --password says.
func addPasswordMailbox(t *testing.T, d *cliDaemon, password string) (stderr string, err error) {
	t.Helper()
	_, stderr, err = captureOutput(t, func() error {
		return accountAdd(t.Context(), d.cfg, []string{
			"--email", "ops@mail.example", "--provider", "imap", "--imap-host", "imap.mail.example",
			"--password", password,
		})
	})
	return stderr, err
}

func TestAPasswordOnTheCommandLineIsRefusedBeforeAnythingIsSent(t *testing.T) {
	// sudo, systemd-run and shell history record the command line: a
	// mailbox's password given there is in the host's journal for good.
	d := newCLIDaemon(t, map[string]string{"POST /v1/accounts": passwordMailboxAdded})
	const secret = "hunter2 is my password"
	for _, args := range [][]string{
		{"--email", "ops@mail.example", "--provider", "imap", "--password", secret},
		{"--email", "ops@mail.example", "--password=" + secret},
		{"--password", secret, "--email", "ops@mail.example"},
		{"--email", "ops@mail.example", "--password", "--"},
	} {
		// What standard input holds is never read for a refused argument.
		in := pipeStdin(t, "another password\n")
		err := accountAdd(t.Context(), d.cfg, args)
		if !errors.Is(err, errPasswordArgument) {
			t.Errorf("account add %q: %v, want the argument refused", args, err)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the refusal quotes the password back: %v", err)
		}
		if !strings.Contains(err.Error(), "--password -") {
			t.Errorf("the refusal does not say how to give the password: %v", err)
		}
		if in.Len() != len("another password\n") {
			t.Errorf("standard input was read for a refused argument")
		}
	}
	if sent := d.sent(); len(sent) != 0 {
		t.Errorf("the daemon was asked %d times: %v", len(sent), sent)
	}
}

func TestPasswordDashReadsOneLineOfStandardInputWithOnlyItsLineEndingTakenOff(t *testing.T) {
	d := newCLIDaemon(t, map[string]string{"POST /v1/accounts": passwordMailboxAdded})
	in := pipeStdin(t, "  correct horse battery staple \r\nwhat follows is not read\n")
	if _, err := addPasswordMailbox(t, d, "-"); err != nil {
		t.Fatalf("account add --password -: %v", err)
	}
	sent := d.sent()
	if len(sent) != 1 || sent[0].route != "POST /v1/accounts" {
		t.Fatalf("the daemon was asked %v", sent)
	}
	// Spaces at its ends are the password's: the daemon logs in with it once
	// before storing it, so a wrong one is refused, never stored.
	if got := sent[0].body["password"]; got != "  correct horse battery staple " {
		t.Errorf("sent the password %q", got)
	}
	if in.Len() != len("what follows is not read\n") {
		t.Errorf("read %d bytes past the password's line", len("what follows is not read\n")-in.Len())
	}

	// A last line with no line feed is read whole.
	pipeStdin(t, "no line feed")
	if _, err := addPasswordMailbox(t, d, "-"); err != nil {
		t.Fatalf("account add --password - with no line feed: %v", err)
	}
	if sent := d.sent(); sent[len(sent)-1].body["password"] != "no line feed" {
		t.Errorf("sent the password %q", sent[len(sent)-1].body["password"])
	}
}

func TestPasswordDashAtATerminalIsTypedAtAPromptThatDoesNotShowIt(t *testing.T) {
	d := newCLIDaemon(t, map[string]string{"POST /v1/accounts": passwordMailboxAdded})
	terminalStdin(t, "", " typed at the prompt")
	stderr, err := addPasswordMailbox(t, d, "-")
	if err != nil {
		t.Fatalf("account add --password - at a terminal: %v", err)
	}
	if sent := d.sent(); len(sent) != 1 || sent[0].body["password"] != " typed at the prompt" {
		t.Errorf("the daemon was asked %v", sent)
	}
	if !strings.Contains(stderr, "Password for ops@mail.example (not shown): ") {
		t.Errorf("no prompt: %q", stderr)
	}
	if strings.Contains(stderr, "typed at the prompt") {
		t.Errorf("the password was printed: %q", stderr)
	}
}

func TestPasswordDashWithNoPasswordToReadAddsNothing(t *testing.T) {
	d := newCLIDaemon(t, map[string]string{"POST /v1/accounts": passwordMailboxAdded})
	for _, c := range []struct {
		input string
		want  error
	}{
		{"", errNoPassword},
		{"\n", errNoPassword},
		{"\r\n", errNoPassword},
		{strings.Repeat("x", maxPasswordBytes+1) + "\n", errPasswordTooLong},
		{strings.Repeat("x", 1<<20), errPasswordTooLong},
	} {
		pipeStdin(t, c.input)
		if _, err := addPasswordMailbox(t, d, "-"); !errors.Is(err, c.want) {
			t.Errorf("standard input of %d bytes: %v, want %v", len(c.input), err, c.want)
		}
	}
	if sent := d.sent(); len(sent) != 0 {
		t.Errorf("the daemon was asked %d times", len(sent))
	}

	// The longest password it takes is sent.
	pipeStdin(t, strings.Repeat("x", maxPasswordBytes)+"\n")
	if _, err := addPasswordMailbox(t, d, "-"); err != nil {
		t.Errorf("a password of %d bytes: %v", maxPasswordBytes, err)
	}
}

func TestAddingAPasswordMailboxNamesTheCommandThatSwitchesItsSyncOn(t *testing.T) {
	// An operator mailbox syncs nothing until the operator switches it on:
	// "ready to sync" was not true.
	d := newCLIDaemon(t, map[string]string{"POST /v1/accounts": passwordMailboxAdded})
	pipeStdin(t, "a password\n")
	stderr, err := addPasswordMailbox(t, d, "-")
	if err != nil {
		t.Fatalf("account add: %v", err)
	}
	const want = "added ops@mail.example as acc_7; its sync is off until `mailserver account sync acc_7 on`\n"
	if !strings.Contains(stderr, want) {
		t.Errorf("account add said %q, want %q", stderr, want)
	}
	if strings.Contains(stderr, "ready to sync") {
		t.Errorf("account add said %q", stderr)
	}
}

func TestTheMailboxSyncNoteSaysWhatTheDaemonAnswered(t *testing.T) {
	for _, c := range []struct {
		name                  string
		enabled, held, paused bool
		want                  string
	}{
		{"off", false, false, false, "its sync is off until `mailserver account sync acc_1 on`"},
		{"on", true, false, false, "its sync is on; `mailserver account sync acc_1 status` shows how far it got"},
		{"paused", true, false, true, "its sync is paused until `mailserver account sync acc_1 resume`"},
		{"held", false, true, false, "its sync is held until its person turns it on"},
	} {
		var a accountJSON
		a.Sync.Enabled, a.Sync.Held, a.Sync.Paused = c.enabled, c.held, c.paused
		if got := mailboxSyncNote("acc_1", a); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestAuthorisingAMailboxNamesTheCommandThatSwitchesItsSyncOn(t *testing.T) {
	// The device flow: the daemon finishes consent, and the command polls
	// the mailbox until it is active.
	expires := time.Now().Add(time.Minute).Unix()
	d := newCLIDaemon(t, map[string]string{
		"POST /v1/accounts": fmt.Sprintf(`{"account":{"id":"acc_8","email":"ops@outlook.example",`+
			`"provider":"microsoft","auth_kind":"oauth2","state":"pending_auth","sync":{"enabled":false}},`+
			`"auth":{"user_code":"ABCD-EFGH","verification_uri":"https://login.example/device","expires_at":%d}}`,
			expires),
		"GET /v1/accounts/acc_8": `{"id":"acc_8","email":"ops@outlook.example","state":"active",` +
			`"sync":{"enabled":false,"state":"off"}}`,
	})
	_, stderr, err := captureOutput(t, func() error {
		return accountAdd(t.Context(), d.cfg, []string{"--email", "ops@outlook.example", "--provider", "microsoft",
			"--device"})
	})
	if err != nil {
		t.Fatalf("account add --device: %v", err)
	}
	const want = "authorised; its sync is off until `mailserver account sync acc_8 on`\n"
	if !strings.Contains(stderr, want) {
		t.Errorf("account add said %q, want %q", stderr, want)
	}
}

func TestAPastedAuthorisationSaysWhereSyncStandsAsTheDaemonAnswered(t *testing.T) {
	// Authorising again: the daemon answers the pasted address with the
	// mailbox it authorised, here paused.
	d := newCLIDaemon(t, map[string]string{
		"POST /v1/accounts/acc_9/oauth/start": `{"auth_url":"https://accounts.example/auth?state=s",` +
			`"state":"s","expires_at":1790000600}`,
		"POST /v1/accounts/oauth/callback": `{"id":"acc_9","email":"ops@gmail.example","state":"active",` +
			`"sync":{"enabled":true,"paused":true,"paused_since":1790000000}}`,
	})
	withOSStdin(t, "http://127.0.0.1:53682/oauth/callback?code=c&state=s\n")
	_, stderr, err := captureOutput(t, func() error {
		return accountAuthorize(t.Context(), d.cfg, []string{"acc_9", "--paste"})
	})
	if err != nil {
		t.Fatalf("account authorize --paste: %v", err)
	}
	const want = "authorised; its sync is paused until `mailserver account sync acc_9 resume`\n"
	if !strings.Contains(stderr, want) {
		t.Errorf("account authorize said %q, want %q", stderr, want)
	}
	sent := d.sent()
	if len(sent) != 2 || sent[1].body["redirect_url"] != "http://127.0.0.1:53682/oauth/callback?code=c&state=s" {
		t.Errorf("the daemon was asked %v", sent)
	}
}

// withOSStdin makes the process's standard input a pipe that carries input,
// for what reads os.Stdin itself.
func withOSStdin(t *testing.T, input string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	previous := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = previous
		_ = r.Close()
	})
}
