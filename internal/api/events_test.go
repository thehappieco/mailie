package api_test

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/api"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/service"
)

// engine stands in for the sync engine: accounts it was told are running
// answer a status, and the rest are not running.
type engine struct {
	mu     sync.Mutex
	status map[string]service.SyncStatus
}

func newEngine() *engine { return &engine{status: map[string]service.SyncStatus{}} }

func (e *engine) Trigger(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.status[id]; !ok {
		return service.ErrSyncNotRunning
	}
	return nil
}

func (e *engine) Status(_ context.Context, id string) (service.SyncStatus, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.status[id]
	if !ok {
		return service.SyncStatus{}, service.ErrSyncNotRunning
	}
	return st, nil
}

func (e *engine) Reconcile(string) {}

func (e *engine) running(id string, st service.SyncStatus) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.status[id] = st
}

// person makes a person and returns their session token.
func (h *harness) person(t *testing.T, email string, role auth.Role) string {
	t.Helper()
	authtest.NewUser(t, h.store, email, role)
	return authtest.SignIn(t, h.users, email)
}

// mailbox adds a password account with the caller's credential and returns
// its id.
func (h *harness) mailbox(t *testing.T, token, email string) string {
	t.Helper()
	resp := h.do(t, http.MethodPost, "/v1/accounts", token, h.passwordAccount(t, email))
	if resp.StatusCode != http.StatusCreated {
		code, msg := decodeError(t, resp)
		t.Fatalf("add %s: %d %s %s", email, resp.StatusCode, code, msg)
	}
	var added struct {
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&added); err != nil {
		t.Fatal(err)
	}
	return added.Account.ID
}

// publish journals events as the sync engine does, and fans them out once
// the transaction has committed.
func (h *harness) publish(t *testing.T, evs ...events.Event) []events.Event {
	t.Helper()
	var written []events.Event
	err := h.store.Write(t.Context(), func(tx *sql.Tx) error {
		var err error
		written, err = h.bus.Journal().Append(t.Context(), tx, evs)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	h.bus.Publish(written...)
	return written
}

func mailEvent(t *testing.T, accountID, subject string) events.Event {
	t.Helper()
	ev, err := events.New(events.TypeMessageNew, accountID, time.Unix(1_790_000_000, 0), map[string]any{
		"account_id": accountID, "message_id": 1, "folder_id": 1, "folder_role": "inbox",
		"subject": subject, "first_copy": true, "first_inbox_copy": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// frame is one server-sent event, or a comment.
type frame struct {
	id, event, data, comment string
}

// sseStream reads a text/event-stream response frame by frame.
type sseStream struct {
	resp   *http.Response
	frames chan frame
}

// openStream connects to the event stream. lastEventID, when set, is sent as
// a reconnecting browser would send it.
func (h *harness) openStream(t *testing.T, path, token, lastEventID string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	s := &sseStream{resp: resp, frames: make(chan frame, 64)}
	if resp.StatusCode != http.StatusOK {
		close(s.frames)
		return s
	}
	go func() {
		defer close(s.frames)
		scanner := bufio.NewScanner(resp.Body)
		var f frame
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				s.frames <- f
				f = frame{}
			case strings.HasPrefix(line, ": "):
				f.comment = strings.TrimPrefix(line, ": ")
			case strings.HasPrefix(line, "id: "):
				f.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return s
}

// next returns the next event, skipping comments. ok is false when the
// stream ended.
func (s *sseStream) next(t *testing.T) (frame, bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case f, ok := <-s.frames:
			if !ok {
				return frame{}, false
			}
			if f.event == "" {
				continue
			}
			return f, true
		case <-deadline:
			t.Fatal("no event within 3 s")
			return frame{}, false
		}
	}
}

// subjectOf reads the subject out of an event's data line.
func subjectOf(t *testing.T, f frame) string {
	t.Helper()
	var ev struct {
		Seq     int64  `json:"seq"`
		Type    string `json:"type"`
		Account string `json:"account_id"`
		Payload struct {
			Subject string `json:"subject"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(f.data), &ev); err != nil {
		t.Fatalf("data %q: %v", f.data, err)
	}
	if strconv.FormatInt(ev.Seq, 10) != f.id || ev.Type != f.event {
		t.Errorf("the frame says id %s, event %s; its data says seq %d, type %s", f.id, f.event, ev.Seq, ev.Type)
	}
	return ev.Payload.Subject
}

func TestSSEReplaysFromLastEventID(t *testing.T) {
	h := newHarness(t, false)
	ana := h.person(t, "ana@example.com", auth.RoleMember)
	bob := h.person(t, "bob@example.com", auth.RoleMember)
	anas := h.mailbox(t, ana, "ana@mail.example")
	bobs := h.mailbox(t, bob, "bob@mail.example")

	seen := h.publish(t, mailEvent(t, anas, "one"), mailEvent(t, anas, "two"), mailEvent(t, bobs, "bob's"),
		mailEvent(t, anas, "three"))

	stream := h.openStream(t, "/v1/events?types=message.new", ana, strconv.FormatInt(seen[0].Seq, 10))
	if stream.resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", stream.resp.StatusCode)
	}
	if ct := stream.resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type %q", ct)
	}
	var got []string
	var last frame
	for range 2 {
		f, ok := stream.next(t)
		if !ok {
			t.Fatal("the stream ended during the replay")
		}
		got = append(got, subjectOf(t, f))
		last = f
	}
	if !slices.Equal(got, []string{"two", "three"}) {
		t.Fatalf("replayed %v, want what came after the Last-Event-ID, and none of bob's", got)
	}

	// Live, after the replay, in order.
	h.publish(t, mailEvent(t, bobs, "bob's live"), mailEvent(t, anas, "four"))
	f, ok := stream.next(t)
	if !ok || subjectOf(t, f) != "four" {
		t.Fatalf("live event = %+v", f)
	}

	// A reconnect resumes after the last id it received, whether the cursor
	// comes as the header or as ?since=, and the header wins.
	again := h.openStream(t, "/v1/events?since=1", ana, last.id)
	if f, ok := again.next(t); !ok || subjectOf(t, f) != "four" {
		t.Fatalf("resumed with %+v", f)
	}
	bySince := h.openStream(t, "/v1/events?since="+last.id, ana, "")
	if f, ok := bySince.next(t); !ok || subjectOf(t, f) != "four" {
		t.Fatalf("resumed by since with %+v", f)
	}

	for _, bad := range []string{"/v1/events?since=-1", "/v1/events?since=x", "/v1/events?types=message.nope",
		"/v1/events?account=" + bobs, "/v1/events?unknown=1"} {
		resp := h.do(t, http.MethodGet, bad, ana, "")
		if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", bad, resp.StatusCode)
		}
	}
}

func TestSSESaysLaggedWhenTheCursorIsOlderThanTheJournal(t *testing.T) {
	h := newHarness(t, false)
	ana := h.person(t, "ana@example.com", auth.RoleMember)
	anas := h.mailbox(t, ana, "ana@mail.example")
	cursor := h.publish(t, mailEvent(t, anas, "seen"))[0].Seq
	pruned := h.publish(t, mailEvent(t, anas, "old"), mailEvent(t, anas, "older"))
	kept := h.publish(t, mailEvent(t, anas, "kept"))
	// What retention does after a week.
	if _, err := h.store.Writer().ExecContext(t.Context(), `DELETE FROM events WHERE seq <= ?`, pruned[1].Seq); err != nil {
		t.Fatal(err)
	}

	stream := h.openStream(t, "/v1/events", ana, strconv.FormatInt(cursor, 10))
	f, ok := stream.next(t)
	if !ok || f.event != "lagged" || f.id != "" {
		t.Fatalf("first frame = %+v, want a lagged event without an id", f)
	}
	f, ok = stream.next(t)
	if !ok || f.id != strconv.FormatInt(kept[0].Seq, 10) {
		t.Fatalf("after lagged = %+v, want what survived", f)
	}
}

func TestAnEventStreamEndsWhenItsSessionEnds(t *testing.T) {
	h := newHarnessWith(t, func(h *api.Handler) { h.EventPing = 20 * time.Millisecond }, serviceOptions{})
	ana := h.person(t, "ana@example.com", auth.RoleMember)
	stream := h.openStream(t, "/v1/events", ana, "")

	// Pings keep coming while the session lives.
	pings := 0
	for pings < 2 {
		select {
		case f := <-stream.frames:
			if f.comment == "ping" {
				pings++
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no pings")
		}
	}
	if resp := h.do(t, http.MethodPost, "/v1/auth/logout", ana, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	f, ok := stream.next(t)
	if !ok || f.event != "error" || !strings.Contains(f.data, `"code":"unauthorized"`) {
		t.Fatalf("after signing out = %+v, want an error event saying unauthorized", f)
	}
	if _, ok := stream.next(t); ok {
		t.Fatal("the stream went on after its session ended")
	}
}

func TestTheLongPollRouteWaitsForNewMailAndReturnsACursor(t *testing.T) {
	h := newHarness(t, false)
	ana := h.person(t, "ana@example.com", auth.RoleMember)
	anas := h.mailbox(t, ana, "ana@mail.example")
	start := h.publish(t, mailEvent(t, anas, "before"))[0].Seq

	type waitResult struct {
		TimedOut   bool              `json:"timed_out"`
		Lagged     bool              `json:"lagged"`
		Events     []json.RawMessage `json:"events"`
		NextCursor int64             `json:"next_cursor"`
	}
	answered := make(chan waitResult, 1)
	go func() {
		resp := h.do(t, http.MethodGet, fmt.Sprintf("/v1/events/wait?since=%d&timeout=5", start), ana, "")
		var r waitResult
		if resp.StatusCode != http.StatusOK {
			t.Errorf("wait: %d", resp.StatusCode)
		} else if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			t.Error(err)
		}
		answered <- r
	}()
	time.Sleep(100 * time.Millisecond)
	arrived := h.publish(t, mailEvent(t, anas, "hello"))
	select {
	case r := <-answered:
		if r.TimedOut || len(r.Events) != 1 || r.NextCursor != arrived[0].Seq {
			t.Fatalf("wait = %+v", r)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("the long poll did not answer when mail arrived")
	}

	resp := h.do(t, http.MethodGet, fmt.Sprintf("/v1/events/wait?since=%d&timeout=1", arrived[0].Seq), ana, "")
	var quiet waitResult
	if err := json.NewDecoder(resp.Body).Decode(&quiet); err != nil {
		t.Fatal(err)
	}
	if !quiet.TimedOut || quiet.Events == nil || len(quiet.Events) != 0 || quiet.NextCursor != arrived[0].Seq {
		t.Errorf("a quiet wait = %+v, want timed out, an empty list and the same cursor", quiet)
	}

	for _, bad := range []string{"timeout=56", "timeout=-1", "timeout=soon", "since=-4", "folder=inbox"} {
		if resp := h.do(t, http.MethodGet, "/v1/events/wait?"+bad, ana, ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", bad, resp.StatusCode)
		}
	}
}

func TestConsentToSyncIsGivenAndTakenBackOnlyByThePersonSignedIn(t *testing.T) {
	h := newHarness(t, false)
	ana := h.person(t, "ana@example.com", auth.RoleMember)
	anas := h.mailbox(t, ana, "ana@mail.example")
	var user struct{ ID string }
	if err := h.store.Reader().QueryRowContext(t.Context(), `SELECT id FROM users WHERE email = 'ana@example.com'`).Scan(&user.ID); err != nil {
		t.Fatal(err)
	}
	hersByKey := authtest.NewWorkspaceKey(t, h.store, auth.ScopeSend, authtest.Personal(t, h.store, user.ID), user.ID)
	instance := h.key(t, auth.ScopeAdmin)

	consent := func(resp *http.Response) service.SyncConsent {
		t.Helper()
		var c service.SyncConsent
		if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	resp := h.do(t, http.MethodGet, "/v1/me/sync-consent", ana, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read: %d", resp.StatusCode)
	}
	if c := consent(resp); c.Consented || c.CurrentVersion != service.DefaultSyncConsentVersion {
		t.Errorf("before = %+v", c)
	}
	for name, try := range map[string]struct {
		token, body string
		want        int
	}{
		"an older text":      {ana, `{"version":"2025-01-privacy"}`, http.StatusBadRequest},
		"no text":            {ana, `{}`, http.StatusBadRequest},
		"a key of hers":      {hersByKey, `{"version":"` + service.DefaultSyncConsentVersion + `"}`, http.StatusForbidden},
		"an instance key":    {instance, `{"version":"` + service.DefaultSyncConsentVersion + `"}`, http.StatusForbidden},
		"a misspelt field":   {ana, `{"versio":"` + service.DefaultSyncConsentVersion + `"}`, http.StatusBadRequest},
		"nobody signed in":   {"", `{"version":"` + service.DefaultSyncConsentVersion + `"}`, http.StatusUnauthorized},
		"withdrawn by a key": {hersByKey, "", http.StatusForbidden},
	} {
		method := http.MethodPost
		if try.body == "" {
			method = http.MethodDelete
		}
		if resp := h.do(t, method, "/v1/me/sync-consent", try.token, try.body); resp.StatusCode != try.want {
			t.Errorf("%s: %d, want %d", name, resp.StatusCode, try.want)
		}
	}
	if resp := h.do(t, http.MethodGet, "/v1/me/sync-consent", instance, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("an instance key read a person's consent: %d", resp.StatusCode)
	}

	resp = h.do(t, http.MethodPost, "/v1/me/sync-consent", ana, `{"version":"`+service.DefaultSyncConsentVersion+`"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("consent: %d", resp.StatusCode)
	}
	if c := consent(resp); !c.Consented || c.Version != service.DefaultSyncConsentVersion {
		t.Errorf("after consenting = %+v", c)
	}
	var status service.AccountSync
	resp = h.do(t, http.MethodGet, "/v1/accounts/"+anas+"/sync", ana, "")
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if !status.Enabled {
		t.Errorf("status after consenting = %+v", status)
	}

	resp = h.do(t, http.MethodDelete, "/v1/me/sync-consent", ana, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("withdraw: %d", resp.StatusCode)
	}
	if c := consent(resp); c.Consented {
		t.Errorf("after withdrawing = %+v", c)
	}
}

func TestTheSyncConsentRoutesNameTheConfiguredTextAndAnEarlierConsentStillSyncs(t *testing.T) {
	// MAIL_CONSENT_VERSION_SYNC is the text the console must show: GET
	// reports it as current_version, and POST records a consent to it and to
	// no other. A consent recorded under an earlier text is shown for the
	// console to ask again, and the mailbox keeps syncing meanwhile.
	h := newHarnessWith(t, nil, serviceOptions{consent: config.ConsentVersions{Sync: "sync-2"}})
	ana := h.person(t, "ana@example.com", auth.RoleMember)
	anas := h.mailbox(t, ana, "ana@mail.example")
	var userID string
	if err := h.store.Reader().QueryRowContext(t.Context(), `SELECT id FROM users WHERE email = 'ana@example.com'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	read := func(resp *http.Response) service.SyncConsent {
		t.Helper()
		var c service.SyncConsent
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&c) != nil {
			t.Fatalf("sync consent answered %d", resp.StatusCode)
		}
		return c
	}
	syncOn := func() bool {
		t.Helper()
		var status service.AccountSync
		resp := h.do(t, http.MethodGet, "/v1/accounts/"+anas+"/sync", ana, "")
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&status) != nil {
			t.Fatalf("account sync answered %d", resp.StatusCode)
		}
		return status.Enabled
	}

	if c := read(h.do(t, http.MethodGet, "/v1/me/sync-consent", ana, "")); c.Consented || c.CurrentVersion != "sync-2" {
		t.Fatalf("before consenting = %+v", c)
	}

	// She agreed before the deployment moved its text on.
	if _, _, err := h.store.GrantSyncConsent(t.Context(), userID, service.DefaultSyncConsentVersion); err != nil {
		t.Fatal(err)
	}
	c := read(h.do(t, http.MethodGet, "/v1/me/sync-consent", ana, ""))
	if !c.Consented || c.Version != service.DefaultSyncConsentVersion || c.CurrentVersion != "sync-2" {
		t.Fatalf("an earlier consent = %+v", c)
	}
	if !syncOn() {
		t.Fatal("a consent to an earlier sync text stopped sync")
	}

	resp := h.do(t, http.MethodPost, "/v1/me/sync-consent", ana, `{"version":"`+service.DefaultSyncConsentVersion+`"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("agreeing to a text this deployment no longer shows answered %d, want 400", resp.StatusCode)
	}
	c = read(h.do(t, http.MethodPost, "/v1/me/sync-consent", ana, `{"version":"sync-2"}`))
	if !c.Consented || c.Version != "sync-2" || c.CurrentVersion != "sync-2" {
		t.Fatalf("agreeing to the configured text = %+v", c)
	}
	if !syncOn() {
		t.Fatal("sync is off after agreeing to the configured text")
	}
}

func TestSyncRoutesNeedTheRightScopeAndOnlyTheOperatorSwitchesAnInstanceMailbox(t *testing.T) {
	sync := newEngine()
	h := newHarnessWith(t, nil, serviceOptions{sync: sync})
	owner := h.person(t, "owner@example.com", auth.RoleOwner)
	instance := h.key(t, auth.ScopeAdmin)
	shared := h.mailbox(t, instance, "shared@mail.example")
	reader := h.key(t, auth.ScopeRead)

	if resp := h.do(t, http.MethodPost, "/v1/accounts/"+shared+"/sync", instance, ""); resp.StatusCode != http.StatusConflict {
		t.Errorf("a pass before sync was switched on: %d, want 409", resp.StatusCode)
	}
	for name, try := range map[string]struct {
		token, body string
		want        int
	}{
		"the owner role":   {owner, `{"enabled":true}`, http.StatusNotFound},
		"a read key":       {reader, `{"enabled":true}`, http.StatusForbidden},
		"no enabled field": {instance, `{}`, http.StatusBadRequest},
	} {
		if resp := h.do(t, http.MethodPut, "/v1/accounts/"+shared+"/sync", try.token, try.body); resp.StatusCode != try.want {
			t.Errorf("%s: %d, want %d", name, resp.StatusCode, try.want)
		}
	}
	resp := h.do(t, http.MethodPut, "/v1/accounts/"+shared+"/sync", instance, `{"enabled":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("switch on: %d", resp.StatusCode)
	}

	sync.running(shared, service.SyncStatus{Running: true, State: "live", Tier: "condstore"})
	if resp := h.do(t, http.MethodPost, "/v1/accounts/"+shared+"/sync", reader, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a read key asked for a pass: %d", resp.StatusCode)
	}
	resp = h.do(t, http.MethodPost, "/v1/accounts/"+shared+"/sync", instance, "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a pass: %d, want 202", resp.StatusCode)
	}
	var status service.AccountSync
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if !status.Enabled || !status.Running || status.State != "live" || status.Tier != "condstore" {
		t.Errorf("status = %+v", status)
	}
}

func TestARevokedKeysEventStreamReceivesNoEventAfterTheRevocation(t *testing.T) {
	// Revoking a key in the console stops it at once: a tool holding an
	// open stream is not handed the next message's subject and sender while
	// the stream waits for its next ping.
	h := newHarness(t, false)
	ana := h.person(t, "ana@example.com", auth.RoleMember)
	anas := h.mailbox(t, ana, "ana@mail.example")
	anaUser, err := h.users.GetByEmail(t.Context(), "ana@example.com")
	if err != nil {
		t.Fatal(err)
	}
	created := h.createKey(t, ana, authtest.Personal(t, h.store, anaUser.ID), keyBody("read", holding(anas, true, false, false)))
	stream := h.openStream(t, "/v1/events?types=message.new", created.Key, "")
	h.publish(t, mailEvent(t, anas, "before"))
	if f, ok := stream.next(t); !ok || subjectOf(t, f) != "before" {
		t.Fatalf("before the revocation the stream sent %+v", f)
	}
	if resp := h.do(t, http.MethodDelete, "/v1/me/apikeys/"+created.Prefix, ana, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoking: %d", resp.StatusCode)
	}
	h.publish(t, mailEvent(t, anas, "after"))
	f, ok := stream.next(t)
	if !ok || f.event != "error" || !strings.Contains(f.data, `"code":"unauthorized"`) {
		t.Fatalf("after the revocation the stream sent %+v, want an error saying unauthorized", f)
	}
	if f, ok := stream.next(t); ok {
		t.Fatalf("the stream went on after its key was revoked: %+v", f)
	}
}
