package service_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/service/issuertest"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Keys an issuer makes (issuedkeys.go): an extension's keys, mirrored here,
// checked without Argon2id and refused with one answer, and enforced as
// workspace keys are; and where they are, this server's own keys withdrawn.

// issuedWorld is a daemon whose keys an issuer makes, the fake one: Ana, who
// allows actions, with two mailboxes of her personal workspace, indexed.
type issuedWorld struct {
	*mailFixture
	issuer *issuertest.Fake
	ana    service.Principal
	box    string // ana's first mailbox, with a message
	other  string // her second
	ws     string // her personal workspace
}

func newIssuedWorld(t *testing.T, o fixtureOptions) *issuedWorld {
	t.Helper()
	fake := issuertest.New()
	o.keyIssuer = fake
	m := newMailFixtureWith(t, o)
	w := &issuedWorld{mailFixture: m, issuer: fake}
	w.ana = m.person(t, "ana@example.com", auth.RoleMember)
	w.ws = authtest.Personal(t, m.db, w.ana.UserID)
	var box *providertest.FakeMailbox
	w.box, box = m.ownedBox(t, w.ana, "ana@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.Deliver("INBOX", message("a", "Lunch on Friday"))
	m.index(t, w.box, box)
	w.other, _ = m.ownedBox(t, w.ana, "ana@other.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	if _, err := m.svc.GrantActionsConsent(t.Context(), w.ana, m.consent().Actions); err != nil {
		t.Fatal(err)
	}
	return w
}

// issue stores a key the issuer made for Ana, holding what grants say on her
// mailboxes, and returns its credential and selector.
func (w *issuedWorld) issue(t *testing.T, k issuertest.Key) (string, string) {
	t.Helper()
	if k.WorkspaceID == "" {
		k.WorkspaceID = w.ws
	}
	if k.CreatorID == "" {
		k.CreatorID = w.ana.UserID
	}
	return issuertest.Store(t, w.db, k)
}

func holds(accountID string, read, act bool) workspace.KeyGrant {
	return workspace.KeyGrant{AccountID: accountID, Flags: workspace.Flags{Read: read, Act: act}}
}

// wantInvalidKey is the one answer every refusal of an issued key gets.
func wantInvalidKey(t *testing.T, what string, err error) {
	t.Helper()
	if service.CodeOf(err) != service.CodeUnauthorized || service.MessageOf(err) != "invalid or expired api key" {
		t.Errorf("%s = %v, want the one unauthorized answer", what, err)
	}
}

func TestEveryRefusalOfAnIssuedKeyIsOneUnauthorized(t *testing.T) {
	w := newIssuedWorld(t, fixtureOptions{})
	ctx := t.Context()
	good, _ := w.issue(t, issuertest.Key{Grants: []workspace.KeyGrant{holds(w.box, true, false)}})
	unknown, _, _ := issuertest.Credential(t)
	_, selector, _ := issuertest.Credential(t)
	revoked, _ := w.issue(t, issuertest.Key{Revoked: true})
	expired, _ := w.issue(t, issuertest.Key{Created: time.Now().Add(-2 * time.Hour), Expires: time.Now().Add(-time.Hour)})
	held, _ := w.issue(t, issuertest.Key{Stops: []string{"operator"}})
	disabledHere, _ := w.issue(t, issuertest.Key{Stops: []string{"creator"}})
	wrong := good[:len(good)-43] + unknown[len(unknown)-43:]
	for what, raw := range map[string]string{
		"an unknown key":            unknown,
		"a malformed one":           issuertest.Prefix + selector + ".short",
		"one in capitals":           strings.ToUpper(issuertest.Prefix) + good[len(issuertest.Prefix):],
		"a wrong secret":            wrong,
		"a revoked one":             revoked,
		"an expired one":            expired,
		"one held by the operator":  held,
		"one whose creator stopped": disabledHere,
		"a space more before it":    " " + good,
		"a space more after it":     good + " ",
		"a tab after it":            good + "\t",
	} {
		_, err := w.svc.Authenticate(ctx, raw, nil)
		wantInvalidKey(t, what+" over REST", err)
		_, err = w.svc.AuthenticateTool(ctx, raw, nil)
		wantInvalidKey(t, what+" as a tool's", err)
	}
	if _, err := w.svc.Authenticate(ctx, good, nil); err != nil {
		t.Fatalf("the good key: %v", err)
	}
}

func TestAWrongSecretCountsForNoKey(t *testing.T) {
	w := newIssuedWorld(t, fixtureOptions{})
	ctx := t.Context()
	good, selector := w.issue(t, issuertest.Key{ExternalID: "tok-good", Grants: []workspace.KeyGrant{holds(w.box, true, false)}})
	revoked, _ := w.issue(t, issuertest.Key{ExternalID: "tok-revoked", Revoked: true})
	held, _ := w.issue(t, issuertest.Key{ExternalID: "tok-held", Stops: []string{"operator", "creator"}})
	unknown, _, _ := issuertest.Credential(t)
	wrong := good[:len(good)-43] + unknown[len(unknown)-43:]
	for _, raw := range []string{wrong, unknown, issuertest.Prefix + selector + ".x"} {
		_, err := w.svc.Authenticate(ctx, raw, nil)
		wantInvalidKey(t, "a secret that matched nothing", err)
	}
	if uses := w.issuer.Uses(); len(uses) != 0 {
		t.Fatalf("a secret that matched nothing was reported: %+v", uses)
	}
	_, err := w.svc.Authenticate(ctx, revoked, nil)
	wantInvalidKey(t, "the revoked key", err)
	_, err = w.svc.Authenticate(ctx, held, nil)
	wantInvalidKey(t, "the held key", err)
	if _, err := w.svc.Authenticate(ctx, good, nil); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, u := range w.issuer.Uses() {
		if u.At.IsZero() || u.AccountID != "" || u.Use != "" {
			t.Errorf("a use of the key itself: %+v", u)
		}
		got = append(got, u.ExternalID+" "+string(u.Refused))
	}
	if want := []string{"tok-revoked revoked", "tok-held held", "tok-good "}; !slices.Equal(got, want) {
		t.Errorf("the issuer heard %q, want %q", got, want)
	}
}

func TestTheIssuerSeesTheBearerUntrimmed(t *testing.T) {
	w := newIssuedWorld(t, fixtureOptions{})
	good, _ := w.issue(t, issuertest.Key{Grants: []workspace.KeyGrant{holds(w.box, true, false)}})
	for _, raw := range []string{" " + good, good + " ", "\t" + good, good + "\t", good} {
		_, _ = w.svc.Authenticate(t.Context(), raw, nil)
		if claimed := w.issuer.Claimed(); !slices.Contains(claimed, raw) {
			t.Errorf("the issuer was asked %q, not %q as presented", claimed, raw)
		}
	}
	// And a transport hands it on as it came, after "Bearer" and one space.
	for header, want := range map[string]string{
		"Bearer " + good:        good,
		"bearer " + good:        good,
		"BEARER  " + good:       " " + good,
		"Bearer " + good + " ":  good + " ",
		"Bearer " + good + "\t": good + "\t",
	} {
		raw, ok := service.Bearer([]string{header})
		if !ok || raw != want {
			t.Errorf("Bearer(%q) = %q, %v; want %q", header, raw, ok, want)
		}
	}
	for _, values := range [][]string{
		nil, {"Bearer\t" + good}, {"Basic " + good}, {good}, {"Bearer"}, {"Bearer "}, {"Bearer   "},
		{"Bearer " + good, "Bearer " + good}, {"Bearer " + strings.Repeat("a", service.MaxBearerLen+1)},
	} {
		if raw, ok := service.Bearer(values); ok {
			t.Errorf("Bearer(%q) took %q", values, raw)
		}
	}
}

func TestAnExpiredIssuedKeyIsRefusedByThisServersClock(t *testing.T) {
	w := newIssuedWorld(t, fixtureOptions{})
	ctx := t.Context()
	now := time.Now()
	clock := func() time.Time { return now }
	// The server's keys read this server's clock, whatever the issuer's
	// says of the key.
	w.keys = auth.NewKeysWithClock(w.db, func() time.Time { return clock() })
	w.svc = w.rebuild(t, w.opts.registry)
	raw, _ := w.issue(t, issuertest.Key{ExternalID: "tok-1", Created: now.Add(-time.Hour), Expires: now.Add(time.Minute),
		Grants: []workspace.KeyGrant{holds(w.box, true, false)}})
	p, err := w.svc.Authenticate(ctx, raw, nil)
	if err != nil {
		t.Fatalf("a minute before its expiry: %v", err)
	}
	clock = func() time.Time { return now.Add(time.Minute) }
	_, err = w.svc.Authenticate(ctx, raw, nil)
	wantInvalidKey(t, "at its expiry", err)
	wantInvalidKey(t, "a principal held since, rechecked at its expiry", w.svc.Recheck(ctx, p))
	if uses := w.issuer.Uses(); len(uses) != 2 || uses[1].Refused != service.UseExpired {
		t.Errorf("the issuer heard %+v", uses)
	}
}

func TestAnIssuedKeyNeverWritesItsLastUse(t *testing.T) {
	w := newIssuedWorld(t, fixtureOptions{})
	raw, selector := w.issue(t, issuertest.Key{Grants: []workspace.KeyGrant{holds(w.box, true, false)}})
	own := authtest.NewWorkspaceKey(t, w.db, auth.ScopeRead, w.ws, w.ana.UserID, holds(w.box, true, false))
	for range 3 {
		w.authenticate(t, raw)
		w.authenticate(t, own)
	}
	// An own key records its use from a detached write; wait for it, and an
	// issued key's would have been there by then.
	deadline := time.Now().Add(5 * time.Second)
	for w.count(t, `SELECT count(*) FROM api_keys WHERE prefix = ? AND last_used_at <> 0`, authtest.Prefix(own)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the own key never recorded its use")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := w.count(t, `SELECT count(*) FROM api_keys WHERE prefix = ? AND last_used_at <> 0`, selector); n != 0 {
		t.Error("an issued key recorded when it was used")
	}
	if n := w.count(t, `SELECT count(*) FROM issued_keys WHERE prefix = ? AND used_day <> ''`, selector); n != 0 {
		t.Error("authenticating an issued key wrote its day of use; only its issuer's report does")
	}
}

func TestASessionTokenThatBeginsLikeAnIssuedKeyStillAuthenticates(t *testing.T) {
	w := newIssuedWorld(t, fixtureOptions{})
	// One session token in about two million begins as the issuer's keys
	// do: 43 base64url characters, no dot. It stays a session, and the
	// issuer is never asked.
	token := issuertest.Prefix + strings.Repeat("A", 39)
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		t.Fatalf("the token does not decode: %v", err)
	}
	sum := sha256.Sum256(raw)
	now := time.Now().Unix()
	if err := w.db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO sessions(id, user_id, token_hash, user_agent, created_at,
			last_seen_at, expires_at) VALUES ('ses_lookalike', ?, ?, 'test', ?, ?, ?)`, w.ana.UserID, sum[:], now, now, now+3600)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p, err := w.svc.Authenticate(t.Context(), token, nil)
	if err != nil || !p.IsSession() || p.UserID != w.ana.UserID {
		t.Fatalf("the session = %+v, %v", p, err)
	}
	if p, err := w.svc.Authenticate(t.Context(), " "+token+" ", nil); err != nil || !p.IsSession() {
		t.Fatalf("the session with spaces around it = %+v, %v", p, err)
	}
	if claimed := w.issuer.Claimed(); len(claimed) != 0 {
		t.Errorf("the issuer was asked about a session: %q", claimed)
	}
	if got := w.svc.CredentialSubject(token); got != "" {
		t.Errorf("a session's rate-limit subject is %q", got)
	}
}

func TestTheRateLimitChargesAnIssuedKeysFailuresToItsSelector(t *testing.T) {
	w := newIssuedWorld(t, fixtureOptions{})
	raw, selector := w.issue(t, issuertest.Key{})
	own := authtest.NewWorkspaceKey(t, w.db, auth.ScopeRead, w.ws, w.ana.UserID)
	for got, want := range map[string]string{
		w.svc.CredentialSubject(raw):                                "key:" + selector,
		w.svc.CredentialSubject(" " + raw):                          "",
		w.svc.CredentialSubject(issuertest.Prefix + "nothing.else"): "",
		w.svc.CredentialSubject(own):                                "key:" + authtest.Prefix(own),
		w.svc.CredentialSubject(" " + own + " "):                    "key:" + authtest.Prefix(own),
	} {
		if got != want {
			t.Errorf("a subject is %q, want %q", got, want)
		}
	}
}

func TestAnIssuedKeyReachesOnlyWhatItsIssuerGranted(t *testing.T) {
	w := newIssuedWorld(t, fixtureOptions{})
	ctx := t.Context()
	raw, selector := w.issue(t, issuertest.Key{Grants: []workspace.KeyGrant{holds(w.box, true, false)}})
	k := w.authenticate(t, raw)
	if !k.IsWorkspaceKey() || k.IsSession() || k.UserID != "" || k.KeyPrefix != selector || k.Issuer != issuertest.Issuer ||
		k.WorkspaceID != w.ws || k.CreatedBy != w.ana.UserID || k.Scope != auth.ScopeWrite || k.Actor() != "key:"+selector {
		t.Errorf("an issued key authenticates as %+v", k)
	}
	accounts, err := w.svc.ListAccounts(ctx, k, "")
	if err != nil || len(accounts) != 1 || accounts[0].ID != w.box || !accounts[0].Access.Read || accounts[0].Access.Act ||
		accounts[0].Access.Send || accounts[0].Access.Manage {
		t.Fatalf("the key lists %+v (%v)", accounts, err)
	}
	if page, err := w.svc.SearchMessages(ctx, k, service.SearchRequest{AccountID: w.box}); err != nil || len(page.Messages) != 1 {
		t.Errorf("the key reads %d messages (%v)", len(page.Messages), err)
	}
	_, err = w.svc.GetAccount(ctx, k, w.other)
	wantCode(t, "the mailbox it holds nothing on", err, service.CodeNotFound)
	_, err = w.svc.SetFlags(ctx, k, service.SetFlagsRequest{IDs: []int64{w.messageID(t, w.box, "INBOX", 1)}, Seen: yes()})
	wantCode(t, "acting with read alone", err, service.CodeNotAuthorized)
	// What it holds is read live: taken away by its issuer, gone at once.
	w.exec(t, `DELETE FROM key_access WHERE key_prefix = ?`, selector)
	if accounts, err := w.svc.ListAccounts(ctx, k, ""); err != nil || len(accounts) != 0 {
		t.Errorf("once taken away the key lists %+v (%v)", accounts, err)
	}
}

func TestAnIssuedKeyNeverHoldsSendAndNeverSends(t *testing.T) {
	for _, mayNotSend := range []bool{false, true} {
		w := newIssuedWorld(t, fixtureOptions{keysMayNotSend: mayNotSend})
		raw, selector := w.issue(t, issuertest.Key{Grants: []workspace.KeyGrant{holds(w.box, true, true)}})
		w.exec(t, `INSERT INTO issued_key_grants(prefix, account_id, use, given_by, given_at_us, state, cause, version)
			VALUES (?, ?, 'send', 'sub-ana', 1, 'refused', 'not_offered', 1)`, selector, w.box)
		if err := w.db.Write(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), `UPDATE key_access SET send = 1 WHERE key_prefix = ?`, selector)
			return err
		}); err == nil {
			t.Fatal("the schema let an issued key hold send")
		}
		k := w.authenticate(t, raw)
		_, err := w.svc.SendMessage(t.Context(), k, service.SendRequest{Compose: service.Compose{AccountID: w.box,
			Confirm: true, To: []service.Address{{Email: "bea@example.org"}}, Subject: "Hi", Text: "Hello"}})
		wantCode(t, "a send with an issued key", err, service.CodeNotAuthorized)
		if a, err := w.svc.GetAccount(t.Context(), k, w.box); err != nil || a.Access.Send || a.Send.Available {
			t.Errorf("the key's card says it sends: %+v (%v)", a, err)
		}
	}
}

func TestAnIssuedKeyActsOnlyWhileItsCreatorAllowsActions(t *testing.T) {
	w := newIssuedWorld(t, fixtureOptions{keysActUnderCreator: true})
	ctx := t.Context()
	raw, _ := w.issue(t, issuertest.Key{ExternalID: "tok-act", Grants: []workspace.KeyGrant{holds(w.box, true, true)}})
	k := w.authenticate(t, raw)
	id := w.messageID(t, w.box, "INBOX", 1)
	if _, err := w.svc.SetFlags(ctx, k, service.SetFlagsRequest{IDs: []int64{id}, Seen: yes()}); err != nil {
		t.Fatalf("with its creator allowing actions: %v", err)
	}
	if _, err := w.svc.WithdrawActionsConsent(ctx, w.ana); err != nil {
		t.Fatal(err)
	}
	_, err := w.svc.SetFlags(ctx, k, service.SetFlagsRequest{IDs: []int64{id}, Seen: no()})
	wantCode(t, "once its creator withdrew", err, service.CodeConflict)
	uses := w.issuer.Uses()
	if last := uses[len(uses)-1]; last.ExternalID != "tok-act" || last.AccountID != w.box || last.Use != "act" ||
		last.Refused != service.UseCreatorConsent {
		t.Errorf("the issuer heard %+v", last)
	}
}

func TestAnIssuedKeyActsOnlyUnderItsCreatorsConsentWhateverTheSwitch(t *testing.T) {
	// MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT off: an own key acts under its key
	// terms alone; an issued key still only while its creator allows
	// actions, at the current revision.
	w := newIssuedWorld(t, fixtureOptions{keysActUnderCreator: false})
	ctx := t.Context()
	raw, _ := w.issue(t, issuertest.Key{Grants: []workspace.KeyGrant{holds(w.box, true, true)}})
	k := w.authenticate(t, raw)
	own := w.authenticate(t, authtest.NewWorkspaceKey(t, w.db, auth.ScopeWrite, w.ws, w.ana.UserID, holds(w.box, true, true)))
	id := w.messageID(t, w.box, "INBOX", 1)
	if _, err := w.svc.WithdrawActionsConsent(ctx, w.ana); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.SetFlags(ctx, own, service.SetFlagsRequest{IDs: []int64{id}, Seen: yes()}); err != nil {
		t.Fatalf("an own key under its key terms: %v", err)
	}
	_, err := w.svc.SetFlags(ctx, k, service.SetFlagsRequest{IDs: []int64{id}, Seen: no()})
	wantCode(t, "the issued key without its creator's consent", err, service.CodeConflict)
	// Consent to an older text is not consent to this one.
	w.exec(t, `UPDATE users SET actions_consent_at = 5, actions_consent_version = '2025-01-older' WHERE id = ?`, w.ana.UserID)
	_, err = w.svc.SetFlags(ctx, k, service.SetFlagsRequest{IDs: []int64{id}, Seen: no()})
	wantCode(t, "the issued key under an older text", err, service.CodeConflict)
	if _, err := w.svc.GrantActionsConsent(ctx, w.ana, w.consent().Actions); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.SetFlags(ctx, k, service.SetFlagsRequest{IDs: []int64{id}, Seen: no()}); err != nil {
		t.Errorf("the issued key once its creator allows actions again: %v", err)
	}
}

func TestAnIssuedKeyNeverReachesATeamsMailbox(t *testing.T) {
	// Whatever the edition says of keys: an issued key reaches personal
	// workspaces alone, even one a database holds in a team.
	m := newMailFixtureWith(t, fixtureOptions{keyIssuer: issuertest.New()})
	tm := newKeysTeam(t, m.fixture)
	ctx := t.Context()
	raw, _ := issuertest.Store(t, m.db, issuertest.Key{WorkspaceID: tm.id, CreatorID: tm.ana.UserID,
		Grants: []workspace.KeyGrant{holds(tm.shared, true, false)}})
	k := m.authenticate(t, raw)
	if accounts, err := m.svc.ListAccounts(ctx, k, ""); err != nil || len(accounts) != 0 {
		t.Errorf("an issued key in a team lists %+v (%v)", accounts, err)
	}
	_, err := m.svc.GetAccount(ctx, k, tm.shared)
	wantCode(t, "the team's mailbox by name", err, service.CodeNotAuthorized)
	_, err = m.svc.ListAccounts(ctx, k, tm.id)
	wantCode(t, "the team's listing", err, service.CodeNotAuthorized)
	_, err = m.svc.SearchMessages(ctx, k, service.SearchRequest{AccountID: tm.shared})
	wantCode(t, "searching the team's mailbox", err, service.CodeNotAuthorized)
	if ws, err := m.svc.ListWorkspaces(ctx, k); err != nil || len(ws) != 0 {
		t.Errorf("an issued key in a team lists the workspaces %+v (%v)", ws, err)
	}
}

func TestAnIssuedKeyIsNeverAReaderOrABackup(t *testing.T) {
	m := newMailFixtureWith(t, fixtureOptions{keyIssuer: issuertest.New()})
	tm := newKeysTeam(t, m.fixture)
	ctx := t.Context()
	// A key holding read on the team's mailbox, as only a database could
	// hold one: Ana is still its last reader, and keeps reading it.
	issuertest.Store(t, m.db, issuertest.Key{WorkspaceID: tm.id, CreatorID: tm.ana.UserID,
		Grants: []workspace.KeyGrant{holds(tm.shared, true, false)}})
	if err := m.svc.RevokeAccess(ctx, tm.ana, tm.shared, tm.ana.UserID, workspace.Flags{Read: true}); err == nil {
		t.Fatal("the last reader gave up read while only a key held it")
	}
	dir, err := m.svc.AccessDirectory(ctx, tm.ana, tm.id)
	if err != nil {
		t.Fatal(err)
	}
	for _, mb := range dir {
		if mb.AccountID == tm.shared && (mb.Readers != 1 || len(mb.Keys) != 0) {
			t.Errorf("the directory says %d readers and keys %+v; want Ana alone, and no issued key", mb.Readers, mb.Keys)
		}
	}
}

func TestAnIssuedKeyNeverPausesResumesOrTurnsOffSync(t *testing.T) {
	w := newIssuedWorld(t, fixtureOptions{})
	ctx := t.Context()
	raw, _ := w.issue(t, issuertest.Key{Grants: []workspace.KeyGrant{holds(w.box, true, true)}})
	k := w.authenticate(t, raw)
	_, err := w.svc.PauseMailboxSync(ctx, k, w.box)
	wantCode(t, "a pause", err, service.CodeNotAuthorized)
	_, err = w.svc.ResumeMailboxSync(ctx, k, w.box, service.SyncResumeRequest{})
	wantCode(t, "a resume", err, service.CodeNotAuthorized)
	off := false
	_, err = w.svc.SetMailboxSync(ctx, k, w.box, service.MailboxSyncRequest{Enabled: &off})
	wantCode(t, "a turn-off", err, service.CodeNotAuthorized)
	_, err = w.svc.StopMailboxSync(ctx, k, w.box)
	wantCode(t, "the operator's stop", err, service.CodeNotAuthorized)
	_, err = w.svc.WithdrawSyncConsent(ctx, k)
	wantCode(t, "a withdrawal", err, service.CodeNotAuthorized)
	if a, err := w.svc.GetAccount(ctx, k, w.box); err != nil || !a.Sync.Enabled || a.Sync.Paused {
		t.Errorf("the mailbox's sync after a key's tries: %+v (%v)", a.Sync, err)
	}
}

func TestAnIssuedKeyReachesNoMemberKeyOrConsentRoute(t *testing.T) {
	w := newIssuedWorld(t, fixtureOptions{})
	ctx := t.Context()
	raw, selector := w.issue(t, issuertest.Key{Grants: []workspace.KeyGrant{holds(w.box, true, true)}})
	k := w.authenticate(t, raw)
	for what, err := range map[string]error{
		"listing the workspace's keys": func() error { _, err := w.svc.ListWorkspaceKeys(ctx, k, w.ws); return err }(),
		"creating one": func() error {
			_, err := w.svc.CreateWorkspaceKey(ctx, k, w.ws, keyRequest("more", "read"))
			return err
		}(),
		"widening itself": func() error {
			_, err := w.svc.SetKeyAccess(ctx, k, w.ws, selector, w.other, keyAccess(true, false, false))
			return err
		}(),
		"its creator's keys":  func() error { _, err := w.svc.ListMyAPIKeys(ctx, k); return err }(),
		"the operator's keys": func() error { _, err := w.svc.ListAPIKeys(ctx, k); return err }(),
		"allowing actions":    func() error { _, err := w.svc.GrantActionsConsent(ctx, k, w.consent().Actions); return err }(),
		"allowing sending":    func() error { _, err := w.svc.GrantSendConsent(ctx, k, w.consent().Send); return err }(),
		"the members":         func() error { _, err := w.svc.ListMembers(ctx, k, w.ws); return err }(),
		"who holds what":      func() error { _, err := w.svc.AccessDirectory(ctx, k, w.ws); return err }(),
		"giving someone read": func() error {
			_, err := w.svc.SetAccess(ctx, k, w.box, w.ana.UserID, grantRequest(true, true, true, false))
			return err
		}(),
		"removing the mailbox": func() error {
			_, err := w.svc.RemoveAccount(ctx, k, w.box, service.RemoveAccountRequest{Confirm: w.box})
			return err
		}(),
	} {
		if code := service.CodeOf(err); code != service.CodeNotAuthorized && code != service.CodeNotFound {
			t.Errorf("%s with an issued key = %v", what, err)
		}
	}
}

func TestAnIssuedKeysActionIsLoggedUnderItsSelector(t *testing.T) {
	w := newIssuedWorld(t, fixtureOptions{logLevel: "info"})
	raw, selector := w.issue(t, issuertest.Key{Grants: []workspace.KeyGrant{holds(w.box, true, true)}})
	k := w.authenticate(t, raw)
	if _, err := w.svc.SetFlags(t.Context(), k, service.SetFlagsRequest{IDs: []int64{w.messageID(t, w.box, "INBOX", 1)},
		Seen: yes()}); err != nil {
		t.Fatal(err)
	}
	logs := w.logs.String()
	if !strings.Contains(logs, "message flags changed") || !strings.Contains(logs, "by=key:"+selector) {
		t.Errorf("the action is not logged under the key's selector:\n%s", logs)
	}
	if strings.Contains(logs, raw[len(raw)-43:]) {
		t.Error("the log holds the key's secret")
	}
}

func TestAUseRefusalIsReportedOnlyForAMailboxOfTheKeysWorkspace(t *testing.T) {
	w := newIssuedWorld(t, fixtureOptions{})
	ctx := t.Context()
	bea := w.person(t, "bea@example.com", auth.RoleMember)
	beas, _ := w.ownedBox(t, bea, "bea@mail.example", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	raw, _ := w.issue(t, issuertest.Key{ExternalID: "tok-1", Grants: []workspace.KeyGrant{holds(w.box, true, false)}})
	k := w.authenticate(t, raw)
	before := len(w.issuer.Uses())
	for _, id := range []string{beas, "acc_madeupmadeup0001", w.box + "x"} {
		_, err := w.svc.GetAccount(ctx, k, id)
		wantCode(t, "another's mailbox", err, service.CodeNotFound)
	}
	if uses := w.issuer.Uses()[before:]; len(uses) != 0 {
		t.Fatalf("mailboxes outside the key's workspace were reported: %+v", uses)
	}
	_, err := w.svc.ListFolders(ctx, k, w.other)
	wantCode(t, "a mailbox of its workspace it holds nothing on", err, service.CodeNotFound)
	_, err = w.svc.SetFlags(ctx, k, service.SetFlagsRequest{IDs: []int64{w.messageID(t, w.box, "INBOX", 1)}, Seen: yes()})
	wantCode(t, "acting where it reads", err, service.CodeNotAuthorized)
	var got []string
	for _, u := range w.issuer.Uses()[before:] {
		got = append(got, u.ExternalID+" "+u.AccountID+" "+u.Use+" "+string(u.Refused))
	}
	if want := []string{"tok-1 " + w.other + " read no_flag", "tok-1 " + w.box + " act no_flag"}; !slices.Equal(got, want) {
		t.Errorf("the issuer heard %q, want %q", got, want)
	}
	// An own key reports nothing to anyone.
	own := w.authenticate(t, authtest.NewWorkspaceKey(t, w.db, auth.ScopeRead, w.ws, w.ana.UserID, holds(w.box, true, false)))
	before = len(w.issuer.Uses())
	_, _ = w.svc.ListFolders(ctx, own, w.other)
	if len(w.issuer.Uses()) != before {
		t.Error("an own key's refusal reached the issuer")
	}
}

func TestIssuedKeysRefusedRefusesEveryKeyAtAuthenticationAndRecheck(t *testing.T) {
	w := newIssuedWorld(t, fixtureOptions{})
	raw, _ := w.issue(t, issuertest.Key{ExternalID: "tok-1", Grants: []workspace.KeyGrant{holds(w.box, true, false)}})
	p := w.authenticate(t, raw)
	refused := newFixtureLike(t, w, fixtureOptions{keyIssuer: w.issuer, issuedKeysRefused: true})
	_, err := refused.Authenticate(t.Context(), raw, nil)
	wantInvalidKey(t, "an issued key where the edition refuses them", err)
	wantInvalidKey(t, "a principal held since", refused.Recheck(t.Context(), p))
	uses := w.issuer.Uses()
	if last := uses[len(uses)-1]; last.ExternalID != "tok-1" || last.Refused != service.UseHeld {
		t.Errorf("the issuer heard %+v", last)
	}
	// This server's own keys are as they were.
	own := authtest.NewWorkspaceKey(t, w.db, auth.ScopeRead, w.ws, w.ana.UserID)
	if _, err := refused.Authenticate(t.Context(), own, nil); err != nil {
		t.Errorf("an own key: %v", err)
	}
}

// newFixtureLike is another service over w's database, with o.
func newFixtureLike(t *testing.T, w *issuedWorld, o fixtureOptions) *service.Service {
	t.Helper()
	f := *w.fixture
	f.opts = o
	svc, _ := f.build(t, o.registry)
	return svc
}

// Own keys, where an issuer makes the keys.

func TestOwnWorkspaceKeysAreRevokedAtStartWhereKeysAreIssuedElsewhere(t *testing.T) {
	w := newIssuedWorld(t, fixtureOptions{keysIssuedElsewhere: true})
	ctx := t.Context()
	live := authtest.NewWorkspaceKey(t, w.db, auth.ScopeRead, w.ws, w.ana.UserID, holds(w.box, true, false))
	expired := authtest.NewWorkspaceKey(t, w.db, auth.ScopeRead, w.ws, w.ana.UserID)
	w.exec(t, `UPDATE api_keys SET expires_at = 1000 WHERE prefix = ?`, authtest.Prefix(expired))
	stopped := authtest.NewWorkspaceKey(t, w.db, auth.ScopeRead, w.ws, w.ana.UserID)
	w.exec(t, `UPDATE api_keys SET revoked_at = 77, stop_reason = 'creator_left' WHERE prefix = ?`, authtest.Prefix(stopped))
	operator := authtest.NewKey(t, w.db, auth.ScopeAdmin)
	issued, selector := w.issue(t, issuertest.Key{Grants: []workspace.KeyGrant{holds(w.box, true, false)}})

	got, err := w.svc.RevokeOwnWorkspaceKeys(ctx)
	if err != nil || got.Revoked != 2 || got.Unexpired != 1 || got.At.IsZero() {
		t.Fatalf("the revocation = %+v, %v; want the live and the expired one, one of them unexpired", got, err)
	}
	for prefix, want := range map[string]string{
		authtest.Prefix(live): "manual", authtest.Prefix(expired): "manual", authtest.Prefix(stopped): "creator_left",
		authtest.Prefix(operator): "", selector: "",
	} {
		var reason string
		var revoked int64
		if err := w.db.Reader().QueryRowContext(ctx, `SELECT stop_reason, revoked_at FROM api_keys WHERE prefix = ?`, prefix).
			Scan(&reason, &revoked); err != nil {
			t.Fatal(err)
		}
		if reason != want || (want == "") != (revoked == 0) {
			t.Errorf("%s stopped for %q at %d, want %q", prefix, reason, revoked, want)
		}
	}
	if again, err := w.svc.RevokeOwnWorkspaceKeys(ctx); err != nil || again.Revoked != 0 || again.Unexpired != 0 {
		t.Errorf("a second start revoked %+v (%v)", again, err)
	}
	w.authenticate(t, operator)
	w.authenticate(t, issued)
}

func TestAnOwnKeyRestoredWhereKeysAreIssuedElsewhereIsRefused(t *testing.T) {
	w := newIssuedWorld(t, fixtureOptions{})
	own := authtest.NewWorkspaceKey(t, w.db, auth.ScopeRead, w.ws, w.ana.UserID, holds(w.box, true, false))
	held := w.authenticate(t, own)
	operator := authtest.NewKey(t, w.db, auth.ScopeAdmin)
	// The same database under an edition whose keys an issuer makes, with
	// its own key still live, as a restore would leave it.
	elsewhere := newFixtureLike(t, w, fixtureOptions{keyIssuer: w.issuer, keysIssuedElsewhere: true})
	_, err := elsewhere.Authenticate(t.Context(), own, nil)
	wantInvalidKey(t, "an own key of a workspace", err)
	_, err = elsewhere.AuthenticateTool(t.Context(), own, nil)
	wantInvalidKey(t, "an own key of a workspace as a tool's", err)
	wantInvalidKey(t, "a principal held since", elsewhere.Recheck(t.Context(), held))
	if _, err := elsewhere.Authenticate(t.Context(), operator, nil); err != nil {
		t.Errorf("the operator's key: %v", err)
	}
}

func TestNoOwnWorkspaceKeyIsCreatedOrWidenedWhereKeysAreIssuedElsewhere(t *testing.T) {
	hook := &freshHook{}
	w := newIssuedWorld(t, fixtureOptions{keysIssuedElsewhere: true, freshness: hook})
	ctx := t.Context()
	prefix := authtest.Prefix(authtest.NewWorkspaceKey(t, w.db, auth.ScopeWrite, w.ws, w.ana.UserID, holds(w.box, true, true)))
	hook.take()
	const why = "this server's API keys are made by their issuer; none is created or given more here"
	_, err := w.svc.CreateWorkspaceKey(ctx, w.ana, w.ws, keyRequest("mine", "read", reads(w.box)))
	if service.CodeOf(err) != service.CodeNotAuthorized || service.MessageOf(err) != why {
		t.Errorf("creating a key = %v", err)
	}
	err = w.svc.CreateMyAPIKey(ctx, w.ana)
	if service.CodeOf(err) != service.CodeNotAuthorized || service.MessageOf(err) != why {
		t.Errorf("creating one of hers = %v", err)
	}
	_, err = w.svc.SetKeyAccess(ctx, w.ana, w.ws, prefix, w.other, keyAccess(true, false, false))
	if service.CodeOf(err) != service.CodeNotAuthorized || service.MessageOf(err) != why {
		t.Errorf("giving a key another mailbox = %v", err)
	}
	if asked := hook.take(); len(asked) != 0 {
		t.Errorf("the freshness hook was asked %+v before the refusal", asked)
	}
	// Taking away, listing and revoking stay.
	if _, err := w.svc.SetKeyAccess(ctx, w.ana, w.ws, prefix, w.box, keyAccess(true, false, false)); err != nil {
		t.Errorf("taking act away: %v", err)
	}
	if keys, err := w.svc.ListWorkspaceKeys(ctx, w.ana, w.ws); err != nil || len(keys) != 1 {
		t.Errorf("listing: %+v (%v)", keys, err)
	}
	if err := w.svc.RevokeKeyAccess(ctx, w.ana, w.ws, prefix, w.box); err != nil {
		t.Errorf("taking the mailbox out: %v", err)
	}
	if err := w.svc.RevokeWorkspaceKey(ctx, w.ana, w.ws, prefix); err != nil {
		t.Errorf("revoking: %v", err)
	}
}

func TestOwnKeyListingsNeverShowIssuedKeys(t *testing.T) {
	w := newIssuedWorld(t, fixtureOptions{})
	ctx := t.Context()
	own := authtest.Prefix(authtest.NewWorkspaceKey(t, w.db, auth.ScopeRead, w.ws, w.ana.UserID, holds(w.box, true, false)))
	_, selector := w.issue(t, issuertest.Key{Grants: []workspace.KeyGrant{holds(w.box, true, false)}})
	prefixes := func(keys []service.WorkspaceKey) []string {
		var out []string
		for _, k := range keys {
			out = append(out, k.Prefix)
		}
		return out
	}
	if keys, err := w.svc.ListWorkspaceKeys(ctx, w.ana, w.ws); err != nil || !slices.Equal(prefixes(keys), []string{own}) {
		t.Errorf("the workspace's keys: %v (%v)", prefixes(keys), err)
	}
	if keys, err := w.svc.ListMyAPIKeys(ctx, w.ana); err != nil || !slices.Equal(prefixes(keys), []string{own}) {
		t.Errorf("her keys: %v (%v)", prefixes(keys), err)
	}
	all, err := w.svc.ListAPIKeys(ctx, admin())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range all {
		if k.Prefix == selector {
			t.Errorf("the operator's listing shows the issued key: %+v", k)
		}
	}
	dir, err := w.svc.AccessDirectory(ctx, w.ana, w.ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, mb := range dir {
		for _, k := range mb.Keys {
			if k.Prefix == selector {
				t.Errorf("the access directory shows the issued key on %s", mb.AccountID)
			}
		}
	}
}

func TestOnlyItsIssuerChangesAnIssuedKey(t *testing.T) {
	w := newIssuedWorld(t, fixtureOptions{})
	ctx := t.Context()
	raw, selector := w.issue(t, issuertest.Key{Grants: []workspace.KeyGrant{holds(w.box, true, true)}})
	for what, err := range map[string]error{
		"revoking it in its workspace": w.svc.RevokeWorkspaceKey(ctx, w.ana, w.ws, selector),
		"revoking it as hers":          w.svc.RevokeMyAPIKey(ctx, w.ana, selector),
		"the operator revoking it":     w.svc.RevokeAPIKey(ctx, admin(), selector),
		"taking a mailbox out of it":   w.svc.RevokeKeyAccess(ctx, w.ana, w.ws, selector, w.box),
		"narrowing it": func() error {
			_, err := w.svc.SetKeyAccess(ctx, w.ana, w.ws, selector, w.box, keyAccess(true, false, false))
			return err
		}(),
		"widening it": func() error {
			_, err := w.svc.SetKeyAccess(ctx, w.ana, w.ws, selector, w.other, keyAccess(true, false, false))
			return err
		}(),
		"listing its sends": func() error { _, err := w.svc.ListKeySends(ctx, w.ana, w.ws, selector); return err }(),
	} {
		wantCode(t, what, err, service.CodeNotFound)
	}
	k := w.authenticate(t, raw)
	if a, err := w.svc.GetAccount(ctx, k, w.box); err != nil || !a.Access.Read || !a.Access.Act {
		t.Errorf("the key after every try: %+v (%v)", a.Access, err)
	}
}
