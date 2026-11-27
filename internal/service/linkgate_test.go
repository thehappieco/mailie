package service_test

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// The link gate (linkgate.go): asked before a mailbox is linked — added by
// its address, linked at the provider, or authorized for the first time —
// and told once the link committed; never about a re-authorization.

// icloudPassword is the app-specific password the stand-in for Apple's
// server takes.
const icloudPassword = "abcd-efgh-ijkl-mnop"

// linkGateHook records what it is asked and told, answers with answer when
// set, and runs onTold when it is told.
type linkGateHook struct {
	mu     sync.Mutex
	asked  []service.LinkAttempt
	told   []service.LinkAttempt
	answer func(ctx context.Context, link service.LinkAttempt) error
	onTold func(ctx context.Context, link service.LinkAttempt)
}

func (h *linkGateHook) BeforeLink(ctx context.Context, link service.LinkAttempt) error {
	h.mu.Lock()
	h.asked = append(h.asked, link)
	answer := h.answer
	h.mu.Unlock()
	if answer == nil {
		return nil
	}
	return answer(ctx, link)
}

func (h *linkGateHook) Linked(ctx context.Context, link service.LinkAttempt) {
	h.mu.Lock()
	h.told = append(h.told, link)
	onTold := h.onTold
	h.mu.Unlock()
	if onTold != nil {
		onTold(ctx, link)
	}
}

// take returns what the gate was asked and told since the last take.
func (h *linkGateHook) take() (asked, told []service.LinkAttempt) {
	h.mu.Lock()
	defer h.mu.Unlock()
	asked, told = h.asked, h.told
	h.asked, h.told = nil, nil
	return asked, told
}

func (h *linkGateHook) answerWith(fn func(ctx context.Context, link service.LinkAttempt) error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.answer = fn
}

func (h *linkGateHook) whenTold(fn func(ctx context.Context, link service.LinkAttempt)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onTold = fn
}

// gateWorld is a linkWorld whose daemon has gate as its link gate, and
// whose iCloud mailboxes log in to a stand-in for Apple's server, which
// counts its logins.
type gateWorld struct {
	*linkWorld
	icloud *providertest.IMAPServer
	// icloudLogins counts the logins the stand-in for Apple's server took.
	icloudLogins *atomic.Int64
	personal     string
}

func newGateWorld(t *testing.T, gate service.LinkGate) *gateWorld {
	t.Helper()
	idp := newFakeIDP(t)
	gmail := providertest.NewIMAPServer(t, providertest.IMAPOptions{AnyUser: true})
	idp.accept = gmail.AcceptToken
	icloud := providertest.NewIMAPServer(t, providertest.IMAPOptions{User: "ana@icloud.com", Password: icloudPassword})
	logins := &atomic.Int64{}
	icloud.OnLogin(func() { logins.Add(1) })
	registry := linkRegistry(idp)
	// Gmail's and Microsoft's servers and Apple's: the fixture redirects
	// the first two only when it is given one server for both.
	registry.RedirectIMAP = map[string]string{"imap.mail.me.com:993": icloud.Addr}
	for _, host := range providerHosts {
		registry.RedirectIMAP[host] = gmail.Addr
	}
	f := newFixtureWith(t, fixtureOptions{publicURL: linkConsole, registry: registry, linkGate: gate})
	w := &gateWorld{linkWorld: &linkWorld{fixture: f, idp: idp, imap: gmail}, icloud: icloud, icloudLogins: logins}
	w.ana = w.person(t, "ana@example.test", auth.RoleMember)
	w.personal = authtest.Personal(t, w.db, w.ana.UserID)
	return w
}

// byAddress adds a Gmail mailbox by its address as Ana, which writes it
// waiting for its first authorization.
func (w *gateWorld) byAddress(t *testing.T, email string) service.AddAccountResult {
	t.Helper()
	added, err := w.svc.AddAccount(t.Context(), w.ana, keyed(w.ana, service.AddAccountRequest{Email: email}))
	if err != nil {
		t.Fatalf("adding %s by its address: %v", email, err)
	}
	return added
}

// icloudRequest is Ana's request to add her iCloud mailbox.
func (w *gateWorld) icloudRequest() service.AddAccountRequest {
	return keyed(w.ana, service.AddAccountRequest{Email: "ana@icloud.com", Provider: "icloud", Password: icloudPassword})
}

// authorize finishes a consent attempt as Ana with a code.
func (w *gateWorld) authorize(t *testing.T, flow *service.AuthFlow) (service.Account, error) {
	t.Helper()
	return w.svc.CompleteOAuth(t.Context(), w.ana, returnTo(linkConsole+"/oauth/return?code=granted&state="+stateOf(t, flow)))
}

// asks fails the test unless the gate was asked exactly want, and told
// nothing, since the last take.
func asks(t *testing.T, gate *linkGateHook, what string, want ...service.LinkAttempt) {
	t.Helper()
	asked, told := gate.take()
	if !slices.Equal(asked, want) {
		t.Errorf("%s: the gate was asked %+v, want %+v", what, asked, want)
	}
	if len(told) != 0 {
		t.Errorf("%s: the gate was told %+v", what, told)
	}
}

// tells fails the test unless the gate was told exactly want since the last
// take, whatever it was asked.
func tells(t *testing.T, gate *linkGateHook, what string, want ...service.LinkAttempt) {
	t.Helper()
	if _, told := gate.take(); !slices.Equal(told, want) {
		t.Errorf("%s: the gate was told %+v, want %+v", what, told, want)
	}
}

func TestTheLinkGateIsAskedBeforeEveryLinkAndFirstAuthorization(t *testing.T) {
	gate := &linkGateHook{}
	w := newGateWorld(t, gate)
	ctx := t.Context()
	ana := func(way service.LinkWay) service.LinkAttempt {
		return service.LinkAttempt{ActorID: w.ana.UserID, WorkspaceID: w.personal, Way: way}
	}

	// By its address, through OAuth: the request names no workspace, and
	// the gate is asked about the person's personal one.
	added := w.byAddress(t, "ana.lima@gmail.com")
	asks(t, gate, "an OAuth mailbox added by its address", ana(service.LinkByAddress))

	// Its first authorization, started again: asked; its completion asks
	// nothing more.
	first, err := w.svc.StartOAuth(ctx, w.ana, added.Account.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	asks(t, gate, "a first authorization", ana(service.LinkFirstAuthorization))
	if _, err := w.authorize(t, first); err != nil {
		t.Fatal(err)
	}
	gate.take()

	// A re-authorization of the mailbox that connected is no link.
	again, err := w.svc.StartOAuth(ctx, w.ana, added.Account.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	asks(t, gate, "a re-authorization")
	if _, err := w.authorize(t, again); err != nil {
		t.Fatal(err)
	}
	tells(t, gate, "a re-authorization completed")

	// By its address, with a password: iCloud's.
	if _, err := w.svc.AddAccount(ctx, w.ana, w.icloudRequest()); err != nil {
		t.Fatal(err)
	}
	if asked, _ := gate.take(); !slices.Equal(asked, []service.LinkAttempt{ana(service.LinkByAddress)}) {
		t.Errorf("an iCloud mailbox: the gate was asked %+v", asked)
	}

	// At the provider.
	w.start(t, w.ana, service.LinkRequest{})
	asks(t, gate, "a link at the provider", ana(service.LinkAtProvider))

	// Into a team, by its admin, either way: the team's id.
	team, bea := w.team(t)
	inTeam := func(way service.LinkWay) service.LinkAttempt {
		return service.LinkAttempt{ActorID: bea.UserID, WorkspaceID: team, Team: true, Way: way}
	}
	w.start(t, bea, service.LinkRequest{WorkspaceID: team})
	asks(t, gate, "a link at the provider into a team", inTeam(service.LinkAtProvider))
	req := keyed(bea, w.passwordAccount(t, "support@example.test"))
	req.WorkspaceID = team
	if _, err := w.svc.AddAccount(ctx, bea, req); err != nil {
		t.Fatal(err)
	}
	if asked, _ := gate.take(); !slices.Equal(asked, []service.LinkAttempt{inTeam(service.LinkByAddress)}) {
		t.Errorf("a mailbox added into a team: the gate was asked %+v", asked)
	}

	// An instance key links into the operator workspace, and is nobody.
	if _, err := w.svc.AddAccount(ctx, admin(), w.passwordAccount(t, "cli@example.test")); err != nil {
		t.Fatal(err)
	}
	if asked, _ := gate.take(); !slices.Equal(asked, []service.LinkAttempt{
		{WorkspaceID: workspace.OperatorID, Way: service.LinkByAddress},
	}) {
		t.Errorf("an instance key's mailbox: the gate was asked %+v", asked)
	}

	// Refused before the gate, by who may link: the gate hears nothing of
	// a link that cannot happen.
	member := w.person(t, "cy@example.test", auth.RoleMember)
	if _, err := w.svc.StartLink(ctx, member, service.LinkRequest{Provider: "gmail", WorkspaceID: team}); err == nil {
		t.Fatal("somebody outside the team started a link into it")
	}
	asks(t, gate, "a link refused by who may link")
}

func TestALinkGatesRefusalStopsTheLinkBeforeAnythingIsStored(t *testing.T) {
	refusal := service.E(service.CodeNotAuthorized, "no mailbox may be linked here now", nil)
	down := errors.New("the gate's own trouble")
	for _, c := range []struct {
		name   string
		answer error
		check  func(t *testing.T, what string, err error)
	}{
		{"a refusal of its own", refusal, func(t *testing.T, what string, err error) {
			if !errors.Is(err, refusal) || service.CodeOf(err) != service.CodeNotAuthorized ||
				service.MessageOf(err) != "no mailbox may be linked here now" {
				t.Errorf("%s: %v", what, err)
			}
		}},
		{"a gate that could not be asked", down, func(t *testing.T, what string, err error) {
			if service.CodeOf(err) != service.CodeInternal || !strings.HasPrefix(service.MessageOf(err), "upstream: ") ||
				service.RetryAfterOf(err) != 30*time.Second || !errors.Is(err, down) ||
				strings.Contains(service.MessageOf(err), "trouble") {
				t.Errorf("%s: %v (retry %s)", what, err, service.RetryAfterOf(err))
			}
		}},
		{"a context that ended", context.DeadlineExceeded, func(t *testing.T, what string, err error) {
			if !errors.Is(err, context.DeadlineExceeded) || service.RetryAfterOf(err) != 0 {
				t.Errorf("%s: %v", what, err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			gate := &linkGateHook{}
			w := newGateWorld(t, gate)
			ctx := t.Context()
			// A mailbox added by its address before the gate refuses, waiting
			// for its first authorization.
			waiting := w.byAddress(t, "ana.lima@gmail.com")
			flowBefore := w.flowsOf(t, waiting.Account.ID)
			gate.take()
			gate.answerWith(func(context.Context, service.LinkAttempt) error { return c.answer })

			_, err := w.svc.AddAccount(ctx, w.ana, keyed(w.ana, service.AddAccountRequest{Email: "ana.work@gmail.com"}))
			c.check(t, "an OAuth mailbox by its address", err)
			_, err = w.svc.AddAccount(ctx, w.ana, w.icloudRequest())
			c.check(t, "an iCloud mailbox", err)
			_, err = w.svc.StartLink(ctx, w.ana, service.LinkRequest{Provider: "gmail"})
			c.check(t, "a link at the provider", err)
			_, err = w.svc.StartOAuth(ctx, w.ana, waiting.Account.ID, "")
			c.check(t, "a first authorization", err)

			if asked, told := gate.take(); len(asked) != 4 || len(told) != 0 {
				t.Errorf("asked %+v and told %+v, want four questions and nothing told", asked, told)
			}
			if n := w.count(t, `SELECT count(*) FROM accounts`); n != 1 {
				t.Errorf("%d mailboxes stored, want only the one waiting", n)
			}
			for _, table := range []string{"credentials", "oauth_links"} {
				if n := w.count(t, `SELECT count(*) FROM `+table); n != 0 {
					t.Errorf("%d rows in %s", n, table)
				}
			}
			if after := w.flowsOf(t, waiting.Account.ID); after != flowBefore {
				t.Errorf("the waiting mailbox's consent attempt changed from %q to %q", flowBefore, after)
			}
			if n := w.icloudLogins.Load(); n != 0 {
				t.Errorf("Apple's server was logged in to %d times", n)
			}
			if n := len(w.imap.AuthAttempts()); n != 0 {
				t.Errorf("Gmail's server was tried %d times", n)
			}
			if n := w.idp.exchangeCount(); n != 0 {
				t.Errorf("%d codes were exchanged", n)
			}
		})
	}
}

// stateIn is stateOf for a goroutine, which may not stop the test: an
// attempt with no address has no state, which its return then lacks.
func stateIn(flow *service.AuthFlow) string {
	if flow == nil {
		return ""
	}
	u, err := url.Parse(flow.AuthURL)
	if err != nil {
		return ""
	}
	return u.Query().Get("state")
}

// flowsOf is a mailbox's consent attempts, their states joined.
func (w *gateWorld) flowsOf(t *testing.T, accountID string) string {
	t.Helper()
	rows, err := w.db.Reader().QueryContext(t.Context(),
		`SELECT state FROM oauth_pending WHERE account_id = ? ORDER BY state`, accountID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var states []string
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			t.Fatal(err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(states, ",")
}

func TestANilLinkGateAsksNothing(t *testing.T) {
	// No gate: every way links as it always did.
	w := newGateWorld(t, nil)
	ctx := t.Context()

	added := w.byAddress(t, "ana.lima@gmail.com")
	if a, err := w.authorize(t, added.Auth); err != nil || !a.Connected {
		t.Fatalf("a first authorization: %+v, %v", a, err)
	}
	again, err := w.svc.StartOAuth(ctx, w.ana, added.Account.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.authorize(t, again); err != nil {
		t.Fatalf("a re-authorization: %v", err)
	}
	if icloud, err := w.svc.AddAccount(ctx, w.ana, w.icloudRequest()); err != nil || icloud.Account.State != "active" {
		t.Fatalf("an iCloud mailbox: %+v, %v", icloud, err)
	}
	w.googleNames("ana.work@gmail.com")
	if linked, err := w.finish(t, w.ana, w.start(t, w.ana, service.LinkRequest{})); err != nil || !linked.Connected {
		t.Fatalf("a link at the provider: %+v, %v", linked, err)
	}
	if _, err := w.svc.AddAccount(ctx, admin(), w.passwordAccount(t, "cli@example.test")); err != nil {
		t.Fatalf("an instance key's mailbox: %v", err)
	}
	if n := w.count(t, `SELECT count(*) FROM accounts a WHERE EXISTS (SELECT 1 FROM credentials c WHERE c.account_id = a.id)`); n != 4 {
		t.Errorf("%d mailboxes connected, want 4", n)
	}
}

func TestTheLinkGateIsAskedOutsideEveryWrite(t *testing.T) {
	// Each answer writes through the store, as an extension that records a
	// link would: asked or told inside a write, it would wait for the one
	// writer forever.
	gate := &linkGateHook{}
	w := newGateWorld(t, gate)
	var writes atomic.Int64
	write := func(ctx context.Context, key string) error {
		writes.Add(1)
		return w.db.WriteExtensionState(ctx, "linkgate", map[string][]byte{key: []byte("seen")}, nil)
	}
	gate.answerWith(func(ctx context.Context, _ service.LinkAttempt) error { return write(ctx, "asked") })
	gate.whenTold(func(ctx context.Context, _ service.LinkAttempt) {
		if err := write(ctx, "told"); err != nil {
			t.Errorf("the gate's write when told: %v", err)
		}
	})
	ctx := t.Context()
	var added service.AddAccountResult
	var first *service.AuthFlow
	var started service.LinkStarted
	for _, step := range []struct {
		name string
		do   func() error
	}{
		{"an OAuth mailbox added by its address", func() (err error) {
			added, err = w.svc.AddAccount(ctx, w.ana, keyed(w.ana, service.AddAccountRequest{Email: "ana.lima@gmail.com"}))
			return err
		}},
		{"its first authorization started", func() (err error) {
			first, err = w.svc.StartOAuth(ctx, w.ana, added.Account.ID, "")
			return err
		}},
		{"its first authorization completed", func() error {
			_, err := w.svc.CompleteOAuth(ctx, w.ana, returnTo(linkConsole+"/oauth/return?code=granted&state="+
				stateIn(first)))
			return err
		}},
		{"an iCloud mailbox added", func() error {
			_, err := w.svc.AddAccount(ctx, w.ana, w.icloudRequest())
			return err
		}},
		{"a link at the provider started", func() (err error) {
			started, err = w.svc.StartLink(ctx, w.ana, service.LinkRequest{Provider: "gmail"})
			return err
		}},
		{"a link at the provider finished", func() error {
			w.googleNames("ana.work@gmail.com")
			_, err := w.svc.CompleteOAuth(ctx, w.ana, withKey(stateIn(started.Auth)))
			return err
		}},
	} {
		done := make(chan error, 1)
		go func() { done <- step.do() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s, the gate's answer a write: %v", step.name, err)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("%s never returned: the gate was asked or told inside a write", step.name)
		}
	}
	// Asked four times, told three.
	if n := writes.Load(); n != 7 {
		t.Errorf("the gate wrote %d times, want 7", n)
	}
}

func TestTheLinkGateIsToldOfEveryLinkOnceItCommitted(t *testing.T) {
	gate := &linkGateHook{}
	w := newGateWorld(t, gate)
	ctx := t.Context()
	// What the gate sees when it is told: the mailboxes of the workspace
	// that connected, read on the reader pool, which sees only what
	// committed.
	var seen []int
	gate.whenTold(func(ctx context.Context, link service.LinkAttempt) {
		var n int
		if err := w.db.Reader().QueryRowContext(ctx, `SELECT count(*) FROM accounts a WHERE a.workspace_id = ?
			AND EXISTS (SELECT 1 FROM credentials c WHERE c.account_id = a.id)`, link.WorkspaceID).Scan(&n); err != nil {
			t.Errorf("reading what committed: %v", err)
		}
		seen = append(seen, n)
	})
	// once fails the test unless the gate was told want alone, with
	// connected mailboxes in its workspace when it was.
	once := func(what string, want service.LinkAttempt, connected int) {
		t.Helper()
		tells(t, gate, what, want)
		if len(seen) != 1 || seen[0] != connected {
			t.Errorf("%s: the gate saw %v connected mailboxes, want %d: told before the commit", what, seen, connected)
		}
		seen = nil
	}
	ana := func(way service.LinkWay) service.LinkAttempt {
		return service.LinkAttempt{ActorID: w.ana.UserID, WorkspaceID: w.personal, Way: way}
	}

	// Added by its address for OAuth: written, waiting, and no link yet.
	added := w.byAddress(t, "ana.lima@gmail.com")
	tells(t, gate, "an OAuth mailbox added by its address")

	// Its first consent declined: a failed first authorization.
	if _, err := w.svc.CompleteOAuth(ctx, w.ana, returnTo(linkConsole+"/oauth/return?error=access_denied&state="+
		stateOf(t, added.Auth))); service.CodeOf(err) != service.CodeBadRequest {
		t.Fatalf("a declined first consent: %v", err)
	}
	tells(t, gate, "a declined first authorization")

	// A grant the mail server refuses: failed too.
	refused, err := w.svc.StartOAuth(ctx, w.ana, added.Account.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	w.idp.set(func(f *fakeIDP) { f.accept = nil })
	if _, err := w.authorize(t, refused); service.CodeOf(err) != service.CodeBadRequest {
		t.Fatalf("a first grant the mail server refuses: %v", err)
	}
	w.idp.set(func(f *fakeIDP) { f.accept = w.imap.AcceptToken })
	tells(t, gate, "a first authorization the mail server refused")

	// Granted: linked, once.
	first, err := w.svc.StartOAuth(ctx, w.ana, added.Account.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.authorize(t, first); err != nil {
		t.Fatal(err)
	}
	once("a first authorization", ana(service.LinkFirstAuthorization), 1)

	// A re-authorization, completed: no link.
	again, err := w.svc.StartOAuth(ctx, w.ana, added.Account.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.authorize(t, again); err != nil {
		t.Fatal(err)
	}
	tells(t, gate, "a re-authorization")

	// A password the mail server refuses: nothing linked.
	wrong := keyed(w.ana, service.AddAccountRequest{Email: "ana.two@icloud.com", Provider: "icloud",
		LoginUser: "ana@icloud.com", Password: "wxyz-wxyz-wxyz-wxyz"})
	if _, err := w.svc.AddAccount(ctx, w.ana, wrong); err == nil {
		t.Fatal("a password the mail server refuses linked a mailbox")
	}
	tells(t, gate, "a password the mail server refused")

	// A password mailbox, stored with its password: linked, once.
	if _, err := w.svc.AddAccount(ctx, w.ana, w.icloudRequest()); err != nil {
		t.Fatal(err)
	}
	once("an iCloud mailbox", ana(service.LinkByAddress), 2)

	// Links at the provider that fail: declined there, and one whose grant
	// the mail server refuses.
	declined := w.start(t, w.ana, service.LinkRequest{})
	if _, err := w.svc.CompleteOAuth(ctx, w.ana, returnTo(linkConsole+"/oauth/return?error=access_denied&state="+
		stateOf(t, declined.Auth))); service.CodeOf(err) != service.CodeBadRequest {
		t.Fatalf("a declined link: %v", err)
	}
	tells(t, gate, "a link declined at the provider")
	w.idp.set(func(f *fakeIDP) { f.accept = nil })
	w.googleNames("ana.other@gmail.com")
	if _, err := w.finish(t, w.ana, w.start(t, w.ana, service.LinkRequest{})); service.CodeOf(err) != service.CodeBadRequest {
		t.Fatalf("a link whose grant the mail server refuses: %v", err)
	}
	w.idp.set(func(f *fakeIDP) { f.accept = w.imap.AcceptToken })
	tells(t, gate, "a link whose grant the mail server refused")

	// A link at the provider finished: linked, once.
	w.googleNames("ana.work@gmail.com")
	if _, err := w.finish(t, w.ana, w.start(t, w.ana, service.LinkRequest{})); err != nil {
		t.Fatal(err)
	}
	once("a link at the provider", ana(service.LinkAtProvider), 3)

	// Into a team, by its admin, and by an instance key into the operator
	// workspace.
	team, bea := w.team(t)
	w.googleNames("support@example.test")
	if _, err := w.finish(t, bea, w.start(t, bea, service.LinkRequest{WorkspaceID: team})); err != nil {
		t.Fatal(err)
	}
	once("a link into a team", service.LinkAttempt{ActorID: bea.UserID, WorkspaceID: team, Team: true,
		Way: service.LinkAtProvider}, 1)
	if _, err := w.svc.AddAccount(ctx, admin(), w.passwordAccount(t, "cli@example.test")); err != nil {
		t.Fatal(err)
	}
	once("an instance key's mailbox", service.LinkAttempt{WorkspaceID: workspace.OperatorID,
		Way: service.LinkByAddress}, 1)
}
