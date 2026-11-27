package service_test

import (
	"testing"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
)

// Whether a mailbox was ever authorized, as a snapshot reports it
// (SnapshotMailbox.Pending): until its first credential, whatever its state
// says, and never again after it.

// pendingConsole is the console these tests' OAuth returns come back to.
const pendingConsole = "http://localhost:5174"

// pendingIn is whether a followed workspace's snapshot says a mailbox is
// pending, as its report carries it; the snapshot read on its own and the
// bytes read say the same, or the test fails.
func (d directoryTeam) pendingIn(t *testing.T, workspaceID, accountID string) bool {
	t.Helper()
	inSnapshot := func(s service.AccessSnapshot) bool {
		t.Helper()
		for _, mb := range s.Mailboxes {
			if mb.ID == accountID {
				return mb.Pending
			}
		}
		t.Fatalf("%s is not in the snapshot of %s", accountID, workspaceID)
		return false
	}
	reported := inSnapshot(d.report(t, workspaceID).Snapshot)
	alone, err := d.svc.AccessSnapshot(t.Context(), workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if read := inSnapshot(alone); read != reported {
		t.Errorf("%s: the snapshot says pending %v, its report %v", accountID, read, reported)
	}
	if _, bytes := d.workspaceBytes(t, workspaceID, bytesSince); bytes[accountID].Pending != reported {
		t.Errorf("%s: the bytes read says pending %v, the report %v", accountID, bytes[accountID].Pending, reported)
	}
	return reported
}

func TestAMailboxIsPendingOnlyUntilItsFirstCredential(t *testing.T) {
	idp := newFakeIDP(t)
	standIn := providertest.NewIMAPServer(t, providertest.IMAPOptions{AnyUser: true})
	idp.accept = standIn.AcceptToken
	d := newDirectoryTeam(t, fixtureOptions{
		publicURL: pendingConsole, providerIMAP: standIn.Addr,
		registry: account.RegistryOptions{
			Google:    account.OAuthClient{ClientID: "google-installed", Endpoint: idp.endpoint()},
			GoogleWeb: account.OAuthClient{ClientID: "google-web", ClientSecret: "google-web-secret", Endpoint: idp.endpoint()},
		},
	})
	ctx := t.Context()
	home := personalOf("ana@example.com")
	// is fails the test unless the mailbox is in state and pending as said.
	is := func(what, id string, state account.State, pending bool) {
		t.Helper()
		a := d.stateOfAccount(t, id)
		if got := d.pendingIn(t, home, id); a.State != state || got != pending || got == a.Connected {
			t.Errorf("%s: %s, pending %v, connected %v; want %s, pending %v", what, a.State, got, a.Connected, state, pending)
		}
	}

	// A password mailbox stores its credential in the write that links it.
	if d.pendingIn(t, dirTeamID, d.box) {
		t.Error("a password mailbox is pending")
	}

	// Added by its address, an OAuth mailbox is written before its first
	// consent, and waits for it with no credential.
	added, err := d.svc.AddAccount(ctx, d.ana, keyed(d.ana, service.AddAccountRequest{Email: "ana@gmail.com"}))
	if err != nil {
		t.Fatal(err)
	}
	id := added.Account.ID
	is("added by its address", id, account.StatePendingAuth, true)

	// Its first consent declined: in error, and still never authorized.
	if _, err := d.svc.CompleteOAuth(ctx, d.ana, returnTo(pendingConsole+"/oauth/return?error=access_denied&state="+
		stateOf(t, added.Auth))); service.CodeOf(err) != service.CodeBadRequest {
		t.Fatalf("a declined first consent: %v, want bad_request", err)
	}
	is("its first consent declined", id, account.StateError, true)

	// Asked again, and granted: its first credential ends it.
	first, err := d.svc.StartOAuth(ctx, d.ana, id, "")
	if err != nil {
		t.Fatal(err)
	}
	is("its first consent asked again", id, account.StatePendingAuth, true)
	if _, err := d.svc.CompleteOAuth(ctx, d.ana, returnTo(pendingConsole+"/oauth/return?code=first&state="+
		stateOf(t, first))); err != nil {
		t.Fatal(err)
	}
	is("authorized", id, account.StateActive, false)

	// With its credential and no moment of first connection, as every OAuth
	// mailbox connected before that moment was recorded: not pending.
	d.exec(t, `UPDATE accounts SET connected_at = 0 WHERE id = ?`, id)
	if n := d.count(t, `SELECT count(*) FROM accounts WHERE id = ? AND connected_at = 0`, id); n != 1 {
		t.Fatalf("the moment of its first connection is still kept")
	}
	is("connected with no moment of first connection", id, account.StateActive, false)

	// Its grant dies: a re-authorization puts it in pending_auth, and a
	// declined one in error, and neither makes it pending.
	if err := d.repo.MarkNeedsReauth(ctx, id, account.ReasonRefreshTokenInvalid); err != nil {
		t.Fatal(err)
	}
	again, err := d.svc.StartOAuth(ctx, d.ana, id, "")
	if err != nil {
		t.Fatal(err)
	}
	is("re-authorizing", id, account.StatePendingAuth, false)
	if _, err := d.svc.CompleteOAuth(ctx, d.ana, returnTo(pendingConsole+"/oauth/return?error=access_denied&state="+
		stateOf(t, again))); service.CodeOf(err) != service.CodeBadRequest {
		t.Fatalf("a declined re-authorization: %v, want bad_request", err)
	}
	is("its re-authorization declined", id, account.StateError, false)
}
