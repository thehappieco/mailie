package service_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// supportTeam is a team, Support, whose owner Ana links its mailboxes, and
// Bea, a member who holds nothing on them until a test grants her something.
type supportTeam struct {
	ws       *workspace.Repository
	ana, bea service.Principal
	id       string
}

func newSupportTeam(t *testing.T, f *fixture) supportTeam {
	t.Helper()
	tm := supportTeam{ws: workspace.NewRepository(f.db, nil)}
	tm.ana = f.person(t, "ana@example.com", auth.RoleMember)
	tm.bea = f.person(t, "bea@example.com", auth.RoleMember)
	w, err := tm.ws.CreateTeam(t.Context(), "Support", tm.ana.UserID, nil)
	if err != nil {
		t.Fatal(err)
	}
	tm.id = w.ID
	if err := f.db.Write(t.Context(), func(tx *sql.Tx) error {
		return tm.ws.AddMemberTx(t.Context(), tx, w.ID, tm.bea.UserID, workspace.RoleMember, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	return tm
}

// link registers an active password mailbox Ana linked in the team.
func (tm supportTeam) link(t *testing.T, f *fixture, id, email string) {
	t.Helper()
	if _, err := f.repo.Create(t.Context(), account.Account{
		ID: id, WorkspaceID: tm.id, Email: email, Provider: provider.KindIMAP,
		AuthKind: "password", IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example",
		SMTPPort: 465, SMTPTLS: "implicit", LoginUser: email, State: account.StateActive,
	}, tm.ana.UserID); err != nil {
		t.Fatal(err)
	}
}

// syncOn gives the team's consent to sync one of its mailboxes, as Ana, its
// owner, to the current text.
func (tm supportTeam) syncOn(t *testing.T, f *fixture, accountID string) {
	t.Helper()
	on := true
	if _, err := f.svc.SetMailboxSync(t.Context(), tm.ana, accountID,
		service.MailboxSyncRequest{Enabled: &on, Version: f.consent().Sync}); err != nil {
		t.Fatalf("turning the team's sync on: %v", err)
	}
}

// grant sets exactly what Bea holds on a mailbox of the team: read given on
// a mailbox that has a key comes with her grant at its current epoch, as
// Ana's browser would seal it.
func (tm supportTeam) grant(t *testing.T, accountID string, flags workspace.Flags) {
	t.Helper()
	if _, err := tm.ws.SetGrantSealed(t.Context(), accountID, tm.bea.UserID, flags,
		sealedWith(t, tm.ws, accountID, tm.bea.UserID, flags), tm.ana.UserID, nil); err != nil {
		t.Fatalf("grant %+v: %v", flags, err)
	}
}

// sealedWith is the grant a change of flags gives a person on a mailbox:
// one at the mailbox key's current epoch when it gives read on a mailbox
// that has a key, none otherwise.
func sealedWith(t *testing.T, ws *workspace.Repository, accountID, userID string, flags workspace.Flags) []byte {
	t.Helper()
	if !flags.Read {
		return nil
	}
	before, err := ws.Grant(t.Context(), accountID, userID)
	switch {
	case err == nil && before.Read:
		return nil
	case err != nil && !errors.Is(err, workspace.ErrNoGrant):
		t.Fatal(err)
	}
	key, err := ws.CurrentKey(t.Context(), accountID)
	switch {
	case errors.Is(err, workspace.ErrKeyless):
		return nil
	case err != nil:
		t.Fatal(err)
	}
	return grantAt(key.Epoch)
}

func TestStorageCountsOnlyTheMailboxesTheCallerMayRead(t *testing.T) {
	// Bea may send from the team's mailbox, and so sees it; what its index
	// holds is read access, which she does not have.
	m := newMailFixture(t)
	tm := newSupportTeam(t, m.fixture)
	const shared = "acc_00000000000000aa"
	tm.link(t, m.fixture, shared, "support@mail.example")
	tm.syncOn(t, m.fixture, shared)
	box := providertest.NewFakeMailbox(providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", providertest.FakeMessage{
		MessageID: "support-1", Subject: "Refund", From: "client@example.org", To: []string{"support@mail.example"},
		InternalDate: time.Now().Add(-time.Hour), Size: 4000,
	})
	m.index(t, shared, box)

	tm.grant(t, shared, workspace.Flags{Send: true})
	if _, err := m.svc.GetAccount(t.Context(), tm.bea, shared); err != nil {
		t.Fatalf("Bea does not see the mailbox she may send from: %v", err)
	}
	// A key Ana gives send alone: what it may send from, it does not read.
	sender := m.authenticate(t, authtest.NewWorkspaceKey(t, m.db, auth.ScopeSend, tm.id, tm.ana.UserID,
		workspace.KeyGrant{AccountID: shared, Flags: workspace.Flags{Send: true}}))
	for name, p := range map[string]service.Principal{"her session": tm.bea, "a key holding send alone": sender} {
		st, err := m.svc.Storage(t.Context(), p, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Mailboxes) != 0 || st.Total != (service.StorageTotal{}) {
			t.Errorf("%s, without read, is told what the index holds: %+v", name, st)
		}
	}

	tm.grant(t, shared, workspace.Flags{Read: true})
	st, err := m.svc.Storage(t.Context(), tm.bea, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := mailboxes(st); len(got) != 1 || got[shared].Bytes != 4000 || st.Total.Bytes != 4000 {
		t.Errorf("with read, Bea's storage is %+v", st)
	}
}

func TestOnlyACallerWhoMayReadAMailboxMayFollowItsNewMail(t *testing.T) {
	// An MCP subscription to an inbox announces its new mail: reading, not
	// merely seeing the mailbox.
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	const shared = "acc_00000000000000aa"
	tm.link(t, f, shared, "support@mail.example")
	follow := func(p service.Principal) service.Code {
		t.Helper()
		if err := f.svc.MayFollow(t.Context(), p, shared); err != nil {
			return service.CodeOf(err)
		}
		return ""
	}
	if got := follow(tm.bea); got != service.CodeNotFound {
		t.Errorf("without a grant: %q, want not_found", got)
	}
	tm.grant(t, shared, workspace.Flags{Send: true, Manage: true})
	if got := follow(tm.bea); got != service.CodeNotAuthorized {
		t.Errorf("with send and manage but not read: %q, want not_authorized", got)
	}
	tm.grant(t, shared, workspace.Flags{Read: true})
	if got := follow(tm.bea); got != "" {
		t.Errorf("with read: %q", got)
	}
	if got := follow(admin()); got != service.CodeNotFound {
		t.Errorf("an instance key following a person's mailbox: %q, want not_found", got)
	}
}

func TestAConsentFinishingAfterItsStarterStoppedManagingTheMailboxStoresNoGrant(t *testing.T) {
	// Bea manages the team's Gmail mailbox and re-authorises it in a browser
	// on this machine. The daemon finishes that by itself when the browser
	// comes back, and no request asks again who she is then. Her manage is
	// taken away while the code is being exchanged: nothing is stored on her
	// say.
	f, idp := consentFixture(t, "")
	tm := newSupportTeam(t, f)
	added, err := f.svc.AddAccount(t.Context(), tm.ana, keyed(tm.ana, service.AddAccountRequest{
		Email: "support@gmail.com", WorkspaceID: tm.id, Flow: "loopback",
	}))
	if err != nil {
		t.Fatal(err)
	}
	shared := added.Account.ID
	tm.grant(t, shared, workspace.Flags{Read: true, Manage: true})
	flow, err := f.svc.StartOAuth(t.Context(), tm.bea, shared, "loopback")
	if err != nil {
		t.Fatal(err)
	}
	revoked := make(chan error, 1)
	idp.set(func(idp *fakeIDP) {
		idp.onIssue = func(string) {
			_, err := tm.ws.Revoke(context.Background(), shared, tm.bea.UserID, workspace.Flags{Manage: true}, nil)
			revoked <- err
		}
	})

	reply := getRedirect(t, flow, url.Values{"code": {"the-code"}, "state": {stateOf(t, flow)}})
	if err := <-revoked; err != nil {
		t.Fatalf("taking manage away during the exchange: %v", err)
	}
	if n := idp.exchangeCount(); n != 1 {
		t.Fatalf("the code was exchanged %d times; the race needs exactly one", n)
	}
	if reply.status == http.StatusOK {
		t.Errorf("the browser was told the authorisation completed: %q", reply.body)
	}
	if n := f.count(t, `SELECT count(*) FROM credentials WHERE account_id = ?`, shared); n != 0 {
		t.Errorf("a grant was stored for someone who no longer manages the mailbox")
	}
	if a := f.stateOfAccount(t, shared); a.State == account.StateActive {
		t.Errorf("the account became active on the say of someone who no longer manages it")
	}
}

func TestLosingManageOfAMailboxEndsTheConsentAttemptsStartedOnIt(t *testing.T) {
	// Taking manage away, or the person's place in the team, ends what they
	// started: the browser coming back afterwards finds nothing, and no
	// code is exchanged.
	for _, lose := range []struct {
		name string
		do   func(t *testing.T, tm supportTeam, accountID string) error
	}{
		{"manage revoked", func(t *testing.T, tm supportTeam, accountID string) error {
			_, err := tm.ws.Revoke(t.Context(), accountID, tm.bea.UserID, workspace.Flags{Manage: true}, nil)
			return err
		}},
		{"removed from the team", func(t *testing.T, tm supportTeam, _ string) error {
			return tm.ws.RemoveMember(t.Context(), tm.id, tm.bea.UserID, nil)
		}},
	} {
		t.Run(lose.name, func(t *testing.T) {
			f, idp := consentFixture(t, "")
			tm := newSupportTeam(t, f)
			added, err := f.svc.AddAccount(t.Context(), tm.ana, keyed(tm.ana, service.AddAccountRequest{
				Email: "support@gmail.com", WorkspaceID: tm.id, Flow: "loopback",
			}))
			if err != nil {
				t.Fatal(err)
			}
			shared := added.Account.ID
			tm.grant(t, shared, workspace.Flags{Read: true, Manage: true})
			flow, err := f.svc.StartOAuth(t.Context(), tm.bea, shared, "loopback")
			if err != nil {
				t.Fatal(err)
			}
			if err := lose.do(t, tm, shared); err != nil {
				t.Fatal(err)
			}
			if n := f.count(t, `SELECT count(*) FROM oauth_pending WHERE account_id = ?`, shared); n != 0 {
				t.Errorf("%d consent attempts are still open on the mailbox", n)
			}
			query := url.Values{"code": {"the-code"}, "state": {stateOf(t, flow)}}
			pasted := redirectURIOf(t, flow.AuthURL) + "?" + query.Encode()
			if _, err := f.svc.CompleteOAuth(t.Context(), tm.bea, pasted); service.CodeOf(err) != service.CodeNotFound {
				t.Errorf("pasting the redirect afterwards: %v, want not_found", err)
			}
			// Nor does the browser reaching the listener the daemon keeps.
			getRedirect(t, flow, query)
			if n := idp.exchangeCount(); n != 0 {
				t.Errorf("the code was exchanged %d times", n)
			}
			if n := f.count(t, `SELECT count(*) FROM credentials WHERE account_id = ?`, shared); n != 0 {
				t.Errorf("a grant was stored")
			}
		})
	}
}

func TestASendFinishedReachesOnlyItsSender(t *testing.T) {
	// Several people send from one shared mailbox, and each send's key and
	// how it ended are that sender's record, as GET /v1/sends/{key} keeps
	// them: Bea, who reads the mailbox, never hears how Ana's or Cid's sends
	// went, and Cid, who may send and not read, hears how his own did.
	b, tm := teamSends(t)
	f := b.m.fixture
	ctx := t.Context()
	tm.grant(t, b.id, workspace.Flags{Read: true})
	cid := tm.join(t, f, "cid@example.com", workspace.RoleMember)
	if _, err := tm.ws.SetGrant(ctx, b.id, cid.UserID, workspace.Flags{Send: true}, tm.ana.UserID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.GrantSendConsent(ctx, cid, f.consent().Send); err != nil {
		t.Fatal(err)
	}
	const cidOwn = "acc_00000000000000cc"
	if _, err := f.repo.Create(ctx, account.Account{
		ID: cidOwn, Email: "cid@mail.example", Provider: provider.KindIMAP,
		AuthKind: "password", IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example",
		SMTPPort: 465, SMTPTLS: "implicit", LoginUser: "cid@mail.example", State: account.StateActive,
	}, cid.UserID); err != nil {
		t.Fatal(err)
	}

	open := func(p service.Principal) *service.Stream {
		t.Helper()
		st, err := f.svc.Subscribe(ctx, p, 0, service.EventFilter{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(st.Close)
		return st
	}
	readKey := keyOf(t, f, tm.ana, auth.ScopeRead, tm.id)
	sendKey := keyOf(t, f, tm.ana, auth.ScopeSend, tm.id)
	streams := map[string]*service.Stream{
		"Ana": open(tm.ana), "Bea": open(tm.bea), "Cid": open(cid), "a read key": open(readKey),
		"a send key": open(sendKey),
	}
	// sendsBefore reads a stream up to the marker published last, and
	// returns the sends it heard of, as "key by user".
	sendsBefore := func(name string, marker string) []string {
		t.Helper()
		var heard []string
		deadline := time.After(3 * time.Second)
		for {
			select {
			case ev, ok := <-streams[name].Events():
				if !ok {
					t.Fatalf("%s: the stream ended", name)
				}
				if ev.Type == "send.finished" {
					var sent struct {
						Key    string `json:"key"`
						UserID string `json:"user_id"`
					}
					if err := json.Unmarshal(ev.Payload, &sent); err != nil {
						t.Fatal(err)
					}
					heard = append(heard, sent.Key+" by "+sent.UserID)
					continue
				}
				if m, ok := ev.NewMail(); ok && m.Subject == marker {
					return heard
				}
			case <-deadline:
				t.Fatalf("%s: the marker never came, after %v", name, heard)
			}
		}
	}
	markers := func(subject string) {
		t.Helper()
		f.publish(t, newMail(t, b.id, "inbox", subject), newMail(t, cidOwn, "inbox", subject))
	}

	if _, err := b.send(t, tm.ana, "ana-key", b.compose("client@example.org")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.send(t, cid, "cid-key", b.compose("other@example.org")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.send(t, sendKey, "by-key", b.compose("third@example.org")); err != nil {
		t.Fatal(err)
	}
	markers("first")
	for name, want := range map[string][]string{
		"Ana":        {"ana-key by " + tm.ana.UserID},
		"Bea":        nil,
		"Cid":        {"cid-key by " + cid.UserID},
		"a read key": nil,            // neither a person's sends nor another key's
		"a send key": {"by-key by "}, // its own, which names no person
	} {
		if got := sendsBefore(name, "first"); !slices.Equal(got, want) {
			t.Errorf("%s heard of the sends %v, want %v", name, got, want)
		}
	}

	// The send flag decides, as it stands when the event is delivered.
	if err := f.svc.RevokeAccess(ctx, tm.ana, b.id, cid.UserID, workspace.Flags{}); err != nil {
		t.Fatal(err)
	}
	late, err := events.New(events.TypeSendFinished, b.id, time.Now(), map[string]any{
		"account_id": b.id, "key": "cid-late", "state": "unknown", "user_id": cid.UserID,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.publish(t, late)
	markers("second")
	if got := sendsBefore("Cid", "second"); len(got) != 0 {
		t.Errorf("Cid, without send, heard of %v", got)
	}
	if got := sendsBefore("Bea", "second"); len(got) != 0 {
		t.Errorf("Bea heard of %v", got)
	}
}

// proxyTo forwards connections to target, and runs before, once, when the
// first one arrives: a moment inside a request, after its checks and before
// its transaction.
func proxyTo(t *testing.T, target string, before func()) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var once sync.Once
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			once.Do(before)
			upstream, err := net.Dial("tcp", target)
			if err != nil {
				_ = conn.Close()
				continue
			}
			go func() { _, _ = io.Copy(upstream, conn); _ = upstream.Close() }()
			go func() { _, _ = io.Copy(conn, upstream); _ = conn.Close() }()
		}
	}()
	return splitHostPort(t, ln.Addr().String())
}

func TestALinkRefusedInsideItsTransactionKeepsTheRefusalsCode(t *testing.T) {
	// Bea, an admin of the team, links a mailbox into it. Her place in the
	// team is checked before the login to the mail server and again in the
	// transaction that creates the mailbox; what changed in between is the
	// refusal she gets, not an internal error.
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	ctx := t.Context()
	setRole := func(role workspace.Role) {
		t.Helper()
		if _, err := tm.ws.SetMember(ctx, tm.id, tm.bea.UserID, workspace.MemberChange{Role: &role}, nil); err != nil {
			t.Fatal(err)
		}
	}
	link := func(email string, meanwhile func()) error {
		t.Helper()
		req := f.passwordAccount(t, email)
		req.WorkspaceID = tm.id
		req.IMAPHost, req.IMAPPort = proxyTo(t, f.mailServer(t).Addr, meanwhile)
		_, err := f.svc.AddAccount(ctx, tm.bea, keyed(tm.bea, req))
		return err
	}

	setRole(workspace.RoleAdmin)
	err := link("demoted@mail.example", func() { setRole(workspace.RoleMember) })
	wantCode(t, "a link by an admin demoted meanwhile", err, service.CodeNotAuthorized)
	if msg := service.MessageOf(err); !strings.Contains(msg, "owner or an admin") {
		t.Errorf("the refusal says %q", msg)
	}

	setRole(workspace.RoleAdmin)
	err = link("removed@mail.example", func() {
		if err := tm.ws.RemoveMember(ctx, tm.id, tm.bea.UserID, nil); err != nil {
			t.Error(err)
		}
	})
	wantCode(t, "a link by an admin removed meanwhile", err, service.CodeNotFound)
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE email IN ('demoted@mail.example', 'removed@mail.example')`); n != 0 {
		t.Errorf("%d mailboxes were linked anyway", n)
	}
}
