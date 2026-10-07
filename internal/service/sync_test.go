package service_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
)

// fakeSync stands in for the sync engine: it records what the service asks
// of it and answers statuses a test sets.
type fakeSync struct {
	mu         sync.Mutex
	reconciled []string
	triggered  []string
	status     map[string]service.SyncStatus
}

func newFakeSync() *fakeSync { return &fakeSync{status: map[string]service.SyncStatus{}} }

func (s *fakeSync) Trigger(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.status[id]; !ok {
		return service.ErrSyncNotRunning
	}
	s.triggered = append(s.triggered, id)
	return nil
}

func (s *fakeSync) Status(_ context.Context, id string) (service.SyncStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.status[id]
	if !ok {
		return service.SyncStatus{}, service.ErrSyncNotRunning
	}
	return st, nil
}

func (s *fakeSync) Reconcile(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconciled = append(s.reconciled, id)
}

func (s *fakeSync) running(id string, st service.SyncStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[id] = st
}

func (s *fakeSync) reconciledIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reconciled)
}

func newSyncFixture(t *testing.T) (*fixture, *fakeSync) {
	t.Helper()
	engine := newFakeSync()
	f := newFixtureWith(t, fixtureOptions{sync: engine})
	return f, engine
}

// eligible is the engine's own question: may it store anything for this
// account now? Asked the way its write transactions ask it.
func (f *fixture) eligible(t *testing.T, accountID string) bool {
	t.Helper()
	err := f.db.Write(t.Context(), func(tx *sql.Tx) error {
		return store.RequireSyncEligibleTx(t.Context(), tx, accountID)
	})
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrNotEligible):
		return false
	default:
		t.Fatalf("RequireSyncEligibleTx: %v", err)
		return false
	}
}

func TestNothingIsStoredBeforeConsent(t *testing.T) {
	// The privacy policy: nothing about a person's messages is kept until
	// they agree to it in the console. Adding and authorising a mailbox is
	// not agreeing; only the person, signed in, naming the policy they were
	// shown, is.
	f, engine := newSyncFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	anas := f.mailbox(t, ana, "ana@mail.example")
	_, anasKey := f.issue(t, ana.UserID)
	byKey, err := f.svc.Authenticate(t.Context(), anasKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Adding the mailbox tells the engine to look at it once; the engine
	// finds it ineligible, and nothing below may change that.
	told := len(engine.reconciledIDs())

	if f.eligible(t, anas) {
		t.Fatal("a mailbox is eligible to sync the moment it is added, before anyone consented")
	}
	if eligible, err := f.db.SyncEligibleAccounts(t.Context()); err != nil || len(eligible) != 0 {
		t.Fatalf("eligible accounts before consent: %v (%v)", eligible, err)
	}
	status, err := f.svc.SyncStatus(t.Context(), ana, anas)
	if err != nil {
		t.Fatal(err)
	}
	if status.Enabled || status.State != "off" {
		t.Errorf("status before consent = %+v, want off and not enabled", status)
	}
	if err := f.svc.TriggerSync(t.Context(), ana, anas); service.CodeOf(err) != service.CodeConflict {
		t.Errorf("a pass asked for before consent: %v, want a conflict", err)
	}
	consent, err := f.svc.SyncConsent(t.Context(), ana)
	if err != nil {
		t.Fatal(err)
	}
	if consent.Consented || consent.CurrentVersion != service.DefaultSyncConsentVersion {
		t.Errorf("consent before asking = %+v", consent)
	}

	// Not by a key acting as her, not to an older or missing text, and not
	// by her mailbox's being authorised again.
	if _, err := f.svc.GrantSyncConsent(t.Context(), byKey, service.DefaultSyncConsentVersion); service.CodeOf(err) != service.CodeNotAuthorized {
		t.Errorf("a key consented for its person: %v", err)
	}
	for _, version := range []string{"", "2025-01-privacy", strings.ToUpper(service.DefaultSyncConsentVersion)} {
		if _, err := f.svc.GrantSyncConsent(t.Context(), ana, version); service.CodeOf(err) != service.CodeBadRequest {
			t.Errorf("consent to %q: %v, want bad_request", version, err)
		}
	}
	if _, err := f.svc.GrantSyncConsent(t.Context(), admin(), service.DefaultSyncConsentVersion); service.CodeOf(err) != service.CodeNotAuthorized {
		t.Errorf("an instance key consented for nobody: %v", err)
	}
	if f.eligible(t, anas) {
		t.Fatal("a refused consent made the mailbox eligible")
	}
	if got := engine.reconciledIDs(); len(got) != told {
		t.Errorf("a refused consent told the engine to reconsider: %v", got[told:])
	}

	given, err := f.svc.GrantSyncConsent(t.Context(), ana, service.DefaultSyncConsentVersion)
	if err != nil {
		t.Fatalf("GrantSyncConsent: %v", err)
	}
	if !given.Consented || given.Version != service.DefaultSyncConsentVersion || given.ConsentedAt == 0 {
		t.Errorf("consent = %+v", given)
	}
	if !f.eligible(t, anas) {
		t.Fatal("consent did not make her mailbox eligible")
	}
	if got := engine.reconciledIDs(); !slices.Contains(got[told:], anas) {
		t.Errorf("the engine was not told her mailbox may start: %v", got[told:])
	}
	status, err = f.svc.SyncStatus(t.Context(), ana, anas)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Enabled {
		t.Errorf("status after consent = %+v, want enabled", status)
	}
	listed, err := f.svc.ListAccounts(t.Context(), ana, "")
	if err != nil || len(listed) != 1 || !listed[0].Sync.Enabled {
		t.Errorf("the account listing does not show sync on: %+v (%v)", listed, err)
	}

	// Agreeing again keeps the date she first agreed.
	again, err := f.svc.GrantSyncConsent(t.Context(), ana, service.DefaultSyncConsentVersion)
	if err != nil || again.ConsentedAt != given.ConsentedAt {
		t.Errorf("consenting again moved the date: %+v (%v), first %+v", again, err, given)
	}
}

func TestAMailboxNobodyOwnsSyncsOnlyWhenAnInstanceAdminSwitchesItOn(t *testing.T) {
	f, engine := newSyncFixture(t)
	owner := f.person(t, "owner@example.com", auth.RoleOwner)
	shared := f.mailbox(t, admin(), "shared@mail.example")
	mine := f.mailbox(t, owner, "owner@mail.example")

	if f.eligible(t, shared) {
		t.Fatal("a mailbox nobody owns syncs before anyone switched it on")
	}
	// Switching sync on for one is the operator's: not a restricted key's,
	// not a reader's, and not an owner of the instance, who does not even
	// see the operator workspace's mailboxes.
	restricted := service.Principal{KeyPrefix: "dddddddd", Scope: auth.ScopeAdmin, AccountIDs: []string{shared}}
	for name, p := range map[string]service.Principal{"restricted key": restricted, "reader": reader()} {
		if _, err := f.svc.SetMailboxSync(t.Context(), p, shared, switchSync(true)); service.CodeOf(err) != service.CodeNotAuthorized {
			t.Errorf("%s switched sync on: %v", name, err)
		}
	}
	if _, err := f.svc.SetMailboxSync(t.Context(), owner, shared, switchSync(true)); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("an owner of the instance switched sync on: %v", err)
	}
	// Nor may the operator decide for a person, whose mailbox it does not
	// reach.
	if _, err := f.svc.SetMailboxSync(t.Context(), admin(), mine, switchSync(true)); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("the operator switched sync on for a person's mailbox: %v", err)
	}
	if f.eligible(t, mine) {
		t.Fatal("a person's mailbox became eligible without their consent")
	}

	status, err := f.svc.SetMailboxSync(t.Context(), admin(), shared, switchSync(true))
	if err != nil {
		t.Fatalf("EnableInstanceAccountSync: %v", err)
	}
	if !status.Enabled || !f.eligible(t, shared) {
		t.Fatalf("switched on, but enabled=%t eligible=%t", status.Enabled, f.eligible(t, shared))
	}
	if !slices.Contains(engine.reconciledIDs(), shared) {
		t.Error("the engine was not told")
	}
	var by string
	var at int64
	if err := f.db.Reader().QueryRowContext(t.Context(),
		`SELECT sync_enabled_by, sync_enabled_at FROM accounts WHERE id = ?`, shared).Scan(&by, &at); err != nil {
		t.Fatal(err)
	}
	if by != "key:"+admin().KeyPrefix || at == 0 {
		t.Errorf("recorded %q at %d, want who and when", by, at)
	}

	f.seedIndex(t, shared, "Walrus")
	if _, err := f.svc.SetMailboxSync(t.Context(), admin(), shared, switchSync(false)); err != nil {
		t.Fatal(err)
	}
	if f.eligible(t, shared) {
		t.Error("switched off, still eligible")
	}
	if n := f.count(t, `SELECT count(*) FROM messages WHERE account_id = ?`, shared); n != 0 {
		t.Errorf("switching sync off left %d indexed messages", n)
	}
}

// seedIndex writes what sync stores for an account: a folder named after
// word, a message whose subject and sender carry it, a part, and the event
// announcing the message.
func (f *fixture) seedIndex(t *testing.T, accountID, word string) int64 {
	t.Helper()
	now := time.Now().Unix()
	var folder int64
	if err := f.db.Writer().QueryRowContext(t.Context(),
		`INSERT INTO folders(account_id, name, display_name, role, sync_state)
		 VALUES (?, ?, ?, '', 'live') RETURNING id`, accountID, "Labels/"+word+"Label", word+"Label").Scan(&folder); err != nil {
		t.Fatal(err)
	}
	var msg int64
	if err := f.db.Writer().QueryRowContext(t.Context(),
		`INSERT INTO messages(account_id, folder_id, uidvalidity, uid, message_id, group_key, subject, from_json,
		 from_addr, from_text, to_text, internal_date, size, first_seen_at, updated_at)
		 VALUES (?, ?, 7, 1, ?, ?, ?, ?, ?, ?, 'someone', ?, 10, ?, ?) RETURNING id`,
		accountID, folder, word+"@mid.example", "mid:"+word+"@mid.example", word+" quarterly report",
		`[{"name":"`+word+` Sender","address":"`+strings.ToLower(word)+`@sender.example"}]`,
		strings.ToLower(word)+"@sender.example", word+" Sender", now, now, now).Scan(&msg); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO parts(msg_id, path, mime_type, filename, size) VALUES (?, '2', 'application/pdf', ?, 100)`,
		msg, word+"-invoice.pdf")
	payload, err := json.Marshal(map[string]any{
		"account_id": accountID, "message_id": msg, "folder_id": folder, "folder_role": "",
		"subject": word + " quarterly report", "first_copy": true, "first_inbox_copy": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	if err := f.db.Writer().QueryRowContext(t.Context(),
		`INSERT INTO events(type, account_id, payload_json, created_at) VALUES ('message.new', ?, ?, ?) RETURNING seq`,
		accountID, string(payload), now).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

func TestWithdrawingConsentDeletesTheIndex(t *testing.T) {
	// Turning sync off deletes what it stored — from the database's files,
	// not only its tables — and stops the mailboxes; the mailboxes
	// themselves, and everybody else's index, stay.
	f, engine := newSyncFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	bob := f.person(t, "bob@example.com", auth.RoleMember)
	anaHome := f.mailbox(t, ana, "ana@mail.example")
	anaWork := f.mailbox(t, ana, "ana.work@mail.example")
	bobs := f.mailbox(t, bob, "bob@mail.example")
	for _, p := range []service.Principal{ana, bob} {
		if _, err := f.svc.GrantSyncConsent(t.Context(), p, service.DefaultSyncConsentVersion); err != nil {
			t.Fatal(err)
		}
	}
	f.seedIndex(t, anaHome, "Xylophone")
	f.seedIndex(t, anaWork, "Quokka")
	bobEvent := f.seedIndex(t, bobs, "Giraffe")
	// Enough rows that the full-text index has more than one segment to
	// merge, as it would after a real initial sync.
	for i := range 40 {
		f.exec(t, `INSERT INTO messages(account_id, folder_id, uidvalidity, uid, group_key, subject, internal_date,
			first_seen_at, updated_at) SELECT account_id, id, 7, ?, ?, 'Xylophone filler', 1, 1, 1
			FROM folders WHERE account_id = ?`, 100+i, "h:filler"+strings.Repeat("x", i), anaHome)
	}

	withdrawn, err := f.svc.WithdrawSyncConsent(t.Context(), ana)
	if err != nil {
		t.Fatalf("WithdrawSyncConsent: %v", err)
	}
	if withdrawn.Consented {
		t.Errorf("after withdrawing: %+v", withdrawn)
	}

	for what, query := range map[string]string{
		"messages": `SELECT count(*) FROM messages WHERE account_id IN (?, ?)`,
		"folders":  `SELECT count(*) FROM folders WHERE account_id IN (?, ?)`,
		"events":   `SELECT count(*) FROM events WHERE account_id IN (?, ?)`,
		"parts":    `SELECT count(*) FROM parts WHERE msg_id IN (SELECT id FROM messages WHERE account_id IN (?, ?))`,
	} {
		if n := f.count(t, query, anaHome, anaWork); n != 0 {
			t.Errorf("%s: %d of her rows left", what, n)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM parts`); n != 1 {
		t.Errorf("%d parts left, want only bob's", n)
	}
	for _, word := range []string{"xylophone", "quokka"} {
		if n := f.count(t, `SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?`, word); n != 0 {
			t.Errorf("the full-text index still finds %q", word)
		}
	}
	if found := f.onDisk(t, "Xylophone", "xylophone", "Quokka", "quokka"); len(found) > 0 {
		t.Errorf("withdrawn, but still on disk:\n  %s", strings.Join(found, "\n  "))
	}
	if f.eligible(t, anaHome) || f.eligible(t, anaWork) {
		t.Error("her mailboxes are still eligible to sync")
	}
	got := engine.reconciledIDs()
	if !slices.Contains(got[len(got)-2:], anaHome) || !slices.Contains(got[len(got)-2:], anaWork) {
		t.Errorf("the engine was not told to stop her mailboxes: %v", got)
	}

	// The mailboxes, their credentials and the other person are untouched.
	if n := f.count(t, `SELECT count(*) FROM accounts a JOIN credentials c ON c.account_id = a.id
		WHERE a.id IN (?, ?)`, anaHome, anaWork); n != 2 {
		t.Errorf("%d of her mailboxes kept their credentials, want 2", n)
	}
	if n := f.count(t, `SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'giraffe'`); n != 1 {
		t.Error("bob's index went too")
	}
	if n := f.count(t, `SELECT count(*) FROM events WHERE seq = ?`, bobEvent); n != 1 {
		t.Error("bob's event went too")
	}
	if !f.eligible(t, bobs) {
		t.Error("bob's mailbox stopped")
	}

	// Only she can do it, signed in.
	_, bobsKey := f.issue(t, bob.UserID)
	byKey, err := f.svc.Authenticate(t.Context(), bobsKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.WithdrawSyncConsent(t.Context(), byKey); service.CodeOf(err) != service.CodeNotAuthorized {
		t.Errorf("a key withdrew its person's consent: %v", err)
	}
}

// publish journals events the way the sync engine does, in a transaction,
// and fans them out after it commits.
func (f *fixture) publish(t *testing.T, evs ...events.Event) []events.Event {
	t.Helper()
	var written []events.Event
	err := f.db.Write(t.Context(), func(tx *sql.Tx) error {
		var err error
		written, err = f.bus.Journal().Append(t.Context(), tx, evs)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f.bus.Publish(written...)
	return written
}

func newMail(t *testing.T, accountID, role, subject string) events.Event {
	t.Helper()
	ev, err := events.New(events.TypeMessageNew, accountID, time.Now(), map[string]any{
		"account_id": accountID, "message_id": 1, "folder_id": 1, "folder_role": role,
		"subject": subject, "first_copy": true, "first_inbox_copy": role == "inbox",
	})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// collect reads events from a stream until want have arrived.
func collect(t *testing.T, st *service.Stream, want int) []service.Event {
	t.Helper()
	var got []service.Event
	deadline := time.After(3 * time.Second)
	for len(got) < want {
		select {
		case ev, ok := <-st.Events():
			if !ok {
				t.Fatalf("the stream closed after %d of %d events", len(got), want)
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timed out after %d of %d events", len(got), want)
		}
	}
	return got
}

func subjects(evs []service.Event) []string {
	var out []string
	for _, ev := range evs {
		var payload struct {
			Subject string `json:"subject"`
		}
		_ = json.Unmarshal(ev.Payload, &payload)
		out = append(out, payload.Subject)
	}
	return out
}

func TestAPersonNeverSeesAnotherPersonsEvents(t *testing.T) {
	// Events quote subjects and senders. Ownership decides who gets them, in
	// the replay, live, and in the long poll, and a mailbox connected while
	// a stream is open is its owner's from its first event.
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	bob := f.person(t, "bob@example.com", auth.RoleMember)
	owner := f.person(t, "owner@example.com", auth.RoleOwner)
	anas := f.mailbox(t, ana, "ana@mail.example")
	bobs := f.mailbox(t, bob, "bob@mail.example")
	shared := f.mailbox(t, admin(), "shared@mail.example")

	// A cursor to resume from: zero would mean "from now".
	start := f.publish(t, newMail(t, bobs, "inbox", "bob, long ago"))[0].Seq
	past := f.publish(t, newMail(t, anas, "inbox", "ana before"), newMail(t, bobs, "inbox", "bob before"),
		newMail(t, shared, "inbox", "shared before"))

	mail := service.EventFilter{Types: []string{"message.new"}}
	stream, err := f.svc.Subscribe(t.Context(), ana, start, mail)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	ownerStream, err := f.svc.Subscribe(t.Context(), owner, start, mail)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerStream.Close()
	if got := subjects(collect(t, stream, 1)); !slices.Equal(got, []string{"ana before"}) {
		t.Errorf("ana's replay = %v", got)
	}
	// The owner role sees neither a member's mailbox nor the operator's.
	operatorStream, err := f.svc.Subscribe(t.Context(), admin(), start, mail)
	if err != nil {
		t.Fatal(err)
	}
	defer operatorStream.Close()
	if got := subjects(collect(t, operatorStream, 1)); !slices.Equal(got, []string{"shared before"}) {
		t.Errorf("the operator's replay = %v", got)
	}

	later := f.mailbox(t, ana, "ana.later@mail.example")
	f.publish(t, newMail(t, bobs, "inbox", "bob live"), newMail(t, anas, "inbox", "ana live"),
		newMail(t, later, "inbox", "ana's new mailbox"), newMail(t, shared, "inbox", "shared live"))
	if got := subjects(collect(t, stream, 2)); !slices.Equal(got, []string{"ana live", "ana's new mailbox"}) {
		t.Errorf("ana's live events = %v", got)
	}
	if got := subjects(collect(t, operatorStream, 1)); !slices.Equal(got, []string{"shared live"}) {
		t.Errorf("the operator's live events = %v", got)
	}
	for name, st := range map[string]*service.Stream{"ana": stream, "the owner": ownerStream} {
		select {
		case ev := <-st.Events():
			t.Errorf("%s received %s of account %s", name, ev.Type, ev.AccountID)
		case <-time.After(100 * time.Millisecond):
		}
	}

	// Naming someone else's mailbox is not_found, as everywhere else.
	if _, err := f.svc.Subscribe(t.Context(), ana, 0, service.EventFilter{AccountIDs: []string{bobs}}); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("subscribing to bob's mailbox: %v", err)
	}
	if _, err := f.svc.WaitForNewMail(t.Context(), ana, 0, time.Second, service.EventFilter{AccountIDs: []string{bobs}}); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("waiting on bob's mailbox: %v", err)
	}

	// The long poll: bob's mail never ends ana's wait.
	cursor := past[len(past)-1].Seq
	result, err := f.svc.WaitForNewMail(t.Context(), ana, cursor, time.Second, service.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := subjects(result.Events); !slices.Equal(got, []string{"ana live", "ana's new mailbox"}) {
		t.Errorf("ana's long poll = %v", got)
	}
}

func TestTheLongPollAnswersWithNewMailAndACursorToResumeFrom(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	anas := f.mailbox(t, ana, "ana@mail.example")

	// From now, with nothing arriving: a timeout that says where to resume.
	f.publish(t, newMail(t, anas, "inbox", "before the wait"))
	quiet, err := f.svc.WaitForNewMail(t.Context(), ana, 0, time.Second, service.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if !quiet.TimedOut || len(quiet.Events) != 0 || quiet.NextCursor == 0 {
		t.Fatalf("a quiet wait = %+v, want a timeout with a cursor", quiet)
	}

	done := make(chan service.WaitResult, 1)
	go func() {
		result, err := f.svc.WaitForNewMail(context.Background(), ana, quiet.NextCursor, 5*time.Second, service.EventFilter{})
		if err != nil {
			t.Error(err)
		}
		done <- result
	}()
	time.Sleep(100 * time.Millisecond)
	// A copy filed in Sent is not mail arriving; the inbox copy is.
	f.publish(t, newMail(t, anas, "sent", "my own reply"))
	arrived := f.publish(t, newMail(t, anas, "inbox", "hello"))
	var result service.WaitResult
	select {
	case result = <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("the wait did not end when mail arrived")
	}
	if result.TimedOut || !slices.Equal(subjects(result.Events), []string{"hello"}) {
		t.Fatalf("the wait = %+v", result)
	}
	if result.NextCursor != arrived[0].Seq {
		t.Errorf("next_cursor = %d, want %d", result.NextCursor, arrived[0].Seq)
	}

	// Resuming from it finds nothing new.
	again, err := f.svc.WaitForNewMail(t.Context(), ana, result.NextCursor, time.Second, service.EventFilter{})
	if err != nil || !again.TimedOut {
		t.Errorf("resuming replayed something: %+v (%v)", again, err)
	}
	if _, err := f.svc.WaitForNewMail(t.Context(), ana, 0, 56*time.Second, service.EventFilter{}); service.CodeOf(err) != service.CodeBadRequest {
		t.Errorf("a 56-second wait: %v", err)
	}
}

func TestTriggeringASyncNeedsConsentWriteScopeAndARunningEngine(t *testing.T) {
	f, engine := newSyncFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	anas := f.mailbox(t, ana, "ana@mail.example")
	if _, err := f.svc.GrantSyncConsent(t.Context(), ana, service.DefaultSyncConsentVersion); err != nil {
		t.Fatal(err)
	}
	secret, _, err := f.keys.Issue(t.Context(), auth.NewKeyRequest{Name: "ro", Scope: auth.ScopeRead, UserID: ana.UserID,
		TermsVersion: service.DefaultKeyTermsVersion})
	if err != nil {
		t.Fatal(err)
	}
	readKey, err := f.svc.Authenticate(t.Context(), secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.TriggerSync(t.Context(), readKey, anas); service.CodeOf(err) != service.CodeNotAuthorized {
		t.Errorf("a read key asked for a pass: %v", err)
	}
	// Consented, but the engine has not picked it up yet.
	if err := f.svc.TriggerSync(t.Context(), ana, anas); !errors.Is(err, service.ErrSyncNotRunning) {
		t.Errorf("a pass for an account not running: %v", err)
	}

	engine.running(anas, service.SyncStatus{
		Running: true, State: "initial", Tier: "uidpoll", FoldersTotal: 5, FoldersSynced: 2,
		Messages: 120, InitialProgress: 40, LastSyncedAt: time.Unix(1_790_000_000, 0),
	})
	if err := f.svc.TriggerSync(t.Context(), ana, anas); err != nil {
		t.Fatalf("TriggerSync: %v", err)
	}
	status, err := f.svc.SyncStatus(t.Context(), ana, anas)
	if err != nil {
		t.Fatal(err)
	}
	want := service.AccountSync{
		Enabled: true, Running: true, State: "initial", Tier: "uidpoll", FoldersSynced: 2, FoldersTotal: 5,
		Messages: 120, InitialProgress: 40, LastSyncedAt: 1_790_000_000,
	}
	if status != want {
		t.Errorf("status = %+v, want %+v", status, want)
	}

	// Without an engine at all, the answer says so rather than pretending.
	bare := newFixture(t)
	bea := bare.person(t, "bea@example.com", auth.RoleMember)
	beas := bare.mailbox(t, bea, "bea@mail.example")
	if _, err := bare.svc.GrantSyncConsent(t.Context(), bea, service.DefaultSyncConsentVersion); err != nil {
		t.Fatal(err)
	}
	if err := bare.svc.TriggerSync(t.Context(), bea, beas); !errors.Is(err, service.ErrSyncUnavailable) {
		t.Errorf("a pass with no engine: %v", err)
	}
}

func TestTheFolderListComesFromTheIndexOnlyWhileSyncIsOn(t *testing.T) {
	f, _ := newSyncFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	anas := f.mailbox(t, ana, "ana@mail.example")
	f.seedIndex(t, anas, "Walrus")

	names := func() map[string]service.Folder {
		t.Helper()
		folders, err := f.svc.ListFolders(t.Context(), ana, anas)
		if err != nil {
			t.Fatalf("ListFolders: %v", err)
		}
		out := map[string]service.Folder{}
		for _, folder := range folders {
			out[folder.Name] = folder
		}
		return out
	}

	// Rows in the index are not what a person without sync sees: the
	// server answers.
	live := names()
	if _, ok := live["INBOX"]; !ok {
		t.Fatalf("the live listing has no INBOX: %v", live)
	}
	if _, ok := live["Labels/WalrusLabel"]; ok {
		t.Fatal("the listing came from the index before consent")
	}

	if _, err := f.svc.GrantSyncConsent(t.Context(), ana, service.DefaultSyncConsentVersion); err != nil {
		t.Fatal(err)
	}
	indexed := names()
	walrus, ok := indexed["Labels/WalrusLabel"]
	if !ok || len(indexed) != 1 {
		t.Fatalf("with sync on, the listing = %v, want the index", indexed)
	}
	if walrus.Messages != 1 || walrus.SyncState != "live" || walrus.DisplayName != "WalrusLabel" {
		t.Errorf("indexed folder = %+v", walrus)
	}

	if _, err := f.svc.WithdrawSyncConsent(t.Context(), ana); err != nil {
		t.Fatal(err)
	}
	if again := names(); again["INBOX"].SyncState != "" || len(again) != len(live) {
		t.Errorf("after withdrawing, the listing = %v, want the server's again", again)
	}
}
