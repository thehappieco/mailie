package service_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
)

// revised is a deployment that changed every text since the defaults.
var revised = config.ConsentVersions{Sync: "sync-2", Actions: "actions-2", Send: "send-2", Keys: "keys-2"}

func TestTheConfiguredConsentVersionIsTheCurrentOneAndTheOnlyOneAccepted(t *testing.T) {
	m := newMailFixtureWith(t, fixtureOptions{consent: revised})
	ctx := t.Context()
	ana := m.person(t, "ana@example.com", auth.RoleMember)

	syncC, err := m.svc.SyncConsent(ctx, ana)
	if err != nil || syncC.CurrentVersion != revised.Sync {
		t.Fatalf("sync consent = %+v, %v; want current %s", syncC, err, revised.Sync)
	}
	actionsC, err := m.svc.ActionsConsent(ctx, ana)
	if err != nil || actionsC.CurrentVersion != revised.Actions {
		t.Fatalf("actions consent = %+v, %v; want current %s", actionsC, err, revised.Actions)
	}
	sendC, err := m.svc.SendConsent(ctx, ana)
	if err != nil || sendC.CurrentVersion != revised.Send {
		t.Fatalf("send consent = %+v, %v; want current %s", sendC, err, revised.Send)
	}

	// The defaults are texts this deployment no longer shows.
	_, err = m.svc.GrantSyncConsent(ctx, ana, service.DefaultSyncConsentVersion)
	wantNotCurrent(t, "sync", err, revised.Sync)
	_, err = m.svc.GrantActionsConsent(ctx, ana, service.DefaultActionsConsentVersion)
	wantNotCurrent(t, "actions", err, revised.Actions)
	_, err = m.svc.GrantSendConsent(ctx, ana, service.DefaultSendConsentVersion)
	wantNotCurrent(t, "sending", err, revised.Send)
	_, err = m.svc.CreateMyAPIKey(ctx, ana, service.PersonalKeyRequest{
		Name: "assistant", Scope: "read", TermsVersion: service.DefaultKeyTermsVersion,
	})
	wantCode(t, "a key under the default terms", err, service.CodeConflict)
	if msg := service.MessageOf(err); !strings.Contains(msg, "current key terms version, "+revised.Keys) {
		t.Errorf("message = %q", msg)
	}

	if c, err := m.svc.GrantSyncConsent(ctx, ana, revised.Sync); err != nil || !c.Consented || c.Version != revised.Sync {
		t.Fatalf("GrantSyncConsent(%s) = %+v, %v", revised.Sync, c, err)
	}
	if c, err := m.svc.GrantActionsConsent(ctx, ana, revised.Actions); err != nil || c.Version != revised.Actions {
		t.Fatalf("GrantActionsConsent(%s) = %+v, %v", revised.Actions, c, err)
	}
	if c, err := m.svc.GrantSendConsent(ctx, ana, revised.Send); err != nil || c.Version != revised.Send {
		t.Fatalf("GrantSendConsent(%s) = %+v, %v", revised.Send, c, err)
	}
	key, err := m.svc.CreateMyAPIKey(ctx, ana, service.PersonalKeyRequest{Name: "assistant", Scope: "read", TermsVersion: revised.Keys})
	if err != nil || key.TermsVersion != revised.Keys {
		t.Fatalf("CreateMyAPIKey(%s) = %+v, %v", revised.Keys, key.PersonalKey, err)
	}
}

func wantNotCurrent(t *testing.T, what string, err error, current string) {
	t.Helper()
	wantCode(t, what+" under the default text", err, service.CodeBadRequest)
	// Neutral words: a deployment of its own has its own texts, not a
	// company's privacy policy.
	if msg := service.MessageOf(err); msg != "consent must name the current text version, "+current+"; reload and review it" {
		t.Errorf("%s: message = %q", what, msg)
	}
}

func TestAnActionsOrSendConsentGivenToAnEarlierTextIsRefusedUntilAgreedAgain(t *testing.T) {
	// The deployment moved the texts on: what its people agreed to before is
	// not agreement to the new words, so actions and sends are refused until
	// they agree again.
	m := newMailFixtureWith(t, fixtureOptions{consent: revised})
	ctx := t.Context()
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	b := m.sendingBox(t, ana, "ana@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	b.box.CreateFolder("Archive", imap.MailboxAttrArchive)
	b.box.Deliver("INBOX", message("a", "Lunch on Friday"))
	m.index(t, b.id, b.box)
	if _, err := m.svc.GrantActionsConsent(ctx, ana, revised.Actions); err != nil {
		t.Fatal(err)
	}
	a := (&actionBox{m: m, owner: ana, id: b.id, box: b.box}).row(t, "INBOX", "a")

	// What she agreed to is now the earlier texts.
	if _, err := m.db.Writer().ExecContext(ctx, `UPDATE users SET actions_consent_version = ?, send_consent_version = ?
		WHERE id = ?`, service.DefaultActionsConsentVersion, service.DefaultSendConsentVersion, ana.UserID); err != nil {
		t.Fatal(err)
	}

	// The console sees her answers and the revisions asked for, and asks
	// again.
	actionsC, err := m.svc.ActionsConsent(ctx, ana)
	if err != nil || actionsC.Version != service.DefaultActionsConsentVersion || actionsC.CurrentVersion != revised.Actions {
		t.Fatalf("actions consent = %+v, %v", actionsC, err)
	}
	sendC, err := m.svc.SendConsent(ctx, ana)
	if err != nil || sendC.Version != service.DefaultSendConsentVersion || sendC.CurrentVersion != revised.Send {
		t.Fatalf("send consent = %+v, %v", sendC, err)
	}

	// Actions and sends are refused before anything reaches a server.
	_, err = m.svc.SetFlags(ctx, ana, service.SetFlagsRequest{IDs: []int64{a}, Seen: yes()})
	wantCode(t, "acting on a consent to an earlier text", err, service.CodeConflict)
	_, err = b.send(t, ana, "k-1", b.compose("bea@example.org"))
	wantCode(t, "sending on a consent to an earlier text", err, service.CodeConflict)
	if n := b.box.Opens(provider.RoleInteractive); n != 0 {
		t.Fatalf("refused actions opened %d connections", n)
	}
	if n := b.smtp.DataCommands(); n != 0 || len(b.smtp.Auths()) != 0 {
		t.Fatalf("the server saw %d DATA and AUTH %v on a consent to an earlier text", n, b.smtp.Auths())
	}

	// Agreeing to the current texts brings them back.
	if _, err := m.svc.GrantActionsConsent(ctx, ana, revised.Actions); err != nil {
		t.Fatal(err)
	}
	if _, err := m.svc.GrantSendConsent(ctx, ana, revised.Send); err != nil {
		t.Fatal(err)
	}
	if _, err := m.svc.SetFlags(ctx, ana, service.SetFlagsRequest{IDs: []int64{a}, Seen: yes()}); err != nil {
		t.Fatalf("acting after agreeing to the current text: %v", err)
	}
	if res, err := b.send(t, ana, "k-2", b.compose("bea@example.org")); err != nil || res.State != service.SendStateSent {
		t.Fatalf("sending after agreeing to the current text: %+v, %v", res, err)
	}
}

func TestASyncConsentGivenToAnEarlierTextKeepsTheMailboxSyncingAfterTheTextChanges(t *testing.T) {
	// The rule docs/console.md states: having agreed to an earlier revision
	// of the sync text does not stop sync, because eligibility reads only
	// that the person consented. A deployment that moves
	// MAIL_CONSENT_VERSION_SYNC on asks everybody again, and keeps their
	// mailboxes syncing meanwhile.
	m := newMailFixture(t)
	ctx := t.Context()
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	id, box := m.ownedBox(t, ana, "ana@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("a", "Lunch on Friday"))
	m.index(t, id, box)
	agreed, err := m.svc.SyncConsent(ctx, ana)
	if err != nil || agreed.Version != service.DefaultSyncConsentVersion {
		t.Fatalf("sync consent before the change = %+v, %v", agreed, err)
	}

	// The daemon restarts with another revision of the sync text.
	m.opts.consent = config.ConsentVersions{Sync: "sync-2"}
	m.svc, m.registry = m.build(t, m.opts.registry)

	// The console is told to ask again...
	c, err := m.svc.SyncConsent(ctx, ana)
	if err != nil || !c.Consented || c.Version != service.DefaultSyncConsentVersion || c.CurrentVersion != "sync-2" ||
		c.ConsentedAt != agreed.ConsentedAt {
		t.Fatalf("sync consent after the change = %+v, %v", c, err)
	}
	// ...and sync goes on: the engine may still store mail for her, the
	// account says sync is on, and what arrives is indexed and served.
	if !m.eligible(t, id) {
		t.Fatal("a consent to an earlier sync text stopped sync")
	}
	listed, err := m.db.SyncEligibleAccounts(ctx)
	if err != nil || !slices.Contains(listed, id) {
		t.Fatalf("eligible accounts = %v, %v; want %s among them", listed, err, id)
	}
	shown, err := m.svc.GetAccount(ctx, ana, id)
	if err != nil || !shown.Sync.Enabled {
		t.Fatalf("account sync = %+v, %v; want on", shown.Sync, err)
	}
	box.Deliver("INBOX", message("b", "Quarterly numbers"))
	m.index(t, id, box)
	page, err := m.svc.SearchMessages(ctx, ana, service.SearchRequest{AccountID: id})
	if err != nil || len(page.Messages) != 2 {
		t.Fatalf("searching after the change = %+v, %v; want both messages", page.Messages, err)
	}
	newest := (&actionBox{m: m, owner: ana, id: id, box: box}).row(t, "INBOX", "b")
	if msg, err := m.svc.GetMessage(ctx, ana, service.GetMessageRequest{ID: newest}); err != nil || msg.Subject != "Quarterly numbers" {
		t.Fatalf("reading after the change = %+v, %v", msg.MessageSummary, err)
	}

	// Agreeing again takes only the current text, and sync stays on.
	_, err = m.svc.GrantSyncConsent(ctx, ana, service.DefaultSyncConsentVersion)
	wantNotCurrent(t, "sync", err, "sync-2")
	if c, err := m.svc.GrantSyncConsent(ctx, ana, "sync-2"); err != nil || !c.Consented || c.Version != "sync-2" {
		t.Fatalf("GrantSyncConsent(sync-2) = %+v, %v", c, err)
	}
	if !m.eligible(t, id) {
		t.Fatal("sync stopped after agreeing to the current text")
	}
}

func TestAKeyKeepsWorkingUnderTheTermsItWasCreatedWith(t *testing.T) {
	// New key terms are asked for on the next key; a key a person already
	// created under the earlier terms is theirs until they revoke it.
	f := newFixtureWith(t, fixtureOptions{consent: revised})
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	secret, _, err := f.keys.Issue(t.Context(), auth.NewKeyRequest{
		Name: "assistant", Scope: auth.ScopeRead, UserID: ana.UserID, TermsVersion: service.DefaultKeyTermsVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.svc.Authenticate(t.Context(), secret, nil)
	if err != nil || p.TermsVersion != service.DefaultKeyTermsVersion {
		t.Fatalf("a key under the earlier terms: %+v, %v", p, err)
	}
}
