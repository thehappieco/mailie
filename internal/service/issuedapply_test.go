package service_test

import (
	"database/sql"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/service/issuertest"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Applying an issuer's keys (issuedapply.go): a daemon that follows a
// directory and mirrors the keys its issuer makes, the fake one, page by
// page.

// testClock is a service clock a test moves.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// cred is a credential the fake issuer made, and when.
type cred struct {
	raw, selector, hash string
	created             time.Time
}

// keysWorld is a daemon whose workspaces come from the directory, whose
// issuer makes its keys: Ana, followed, with two mailboxes of her personal
// workspace, which she reads.
type keysWorld struct {
	*fixture
	issuer  *issuertest.Fake
	clock   *testClock
	ana     service.Principal
	ws      string
	box     string
	other   string
	creds   map[string]cred
	givenAt time.Time
	// gen is the directory's generation the documents are written at.
	gen string
}

const anaEmail = "ana@example.com"

var anaSub = subjectOf(anaEmail)

func newKeysWorld(t *testing.T, o fixtureOptions) *keysWorld {
	t.Helper()
	clock := &testClock{at: time.Now().UTC().Truncate(time.Second)}
	fake := issuertest.NewFor(dirIssuer)
	o.source, o.keyIssuer, o.now = workspace.Platform(), fake, clock.now
	o.registry = account.RegistryOptions{
		Google:    account.OAuthClient{ClientID: "google-client"},
		Microsoft: account.OAuthClient{ClientID: "ms-client", Tenant: "common"},
	}
	f := newFixtureWith(t, o)
	w := &keysWorld{fixture: f, issuer: fake, clock: clock, creds: map[string]cred{}, gen: dirGen}
	w.ana = f.followed(t, anaEmail)
	f.apply(t, document(anaEmail, 1))
	w.ws = personalOf(anaEmail)
	w.box = f.teamBox(t, w.ws, w.ana.UserID, "ana@mail.example")
	w.other = f.teamBox(t, w.ws, w.ana.UserID, "ana@other.example")
	f.connect(t, w.box)
	f.connect(t, w.other)
	w.givenAt = clock.now().Add(-time.Hour)
	return w
}

// cred is the credential of the key the issuer knows by id, made the first
// time it is asked for, two hours ago.
func (w *keysWorld) cred(t *testing.T, id string) cred {
	t.Helper()
	if c, ok := w.creds[id]; ok {
		return c
	}
	raw, selector, hash := issuertest.Credential(t)
	c := cred{raw: raw, selector: selector, hash: hash, created: w.clock.now().Add(-2 * time.Hour)}
	w.creds[id] = c
	return c
}

// record is the issuer's record of a key of Ana's at version, listing
// grants, under the key terms this server accepts.
func (w *keysWorld) record(t *testing.T, id string, version int64, grants ...service.IssuedGrant) service.IssuedKey {
	t.Helper()
	c := w.cred(t, id)
	return service.IssuedKey{
		ExternalID: id, Selector: c.selector, Hash: c.hash, WorkspaceID: w.ws, Creator: anaSub, Version: version,
		CreatedAt: c.created, ExpiresAt: c.created.Add(90 * 24 * time.Hour), Terms: w.consent().Keys, Grants: grants,
	}
}

// given is a use of a mailbox given by a subject, at the world's moment.
func (w *keysWorld) given(accountID, use, by string) service.IssuedGrant {
	return service.IssuedGrant{AccountID: accountID, Use: use, GivenBy: by, GivenAt: w.givenAt}
}

// page is a page of the feed after where it stands, at the directory's
// generation and reset; the last one unless more.
func (w *keysWorld) page(t *testing.T, more bool, keys ...service.IssuedKey) service.IssuedKeysPage {
	t.Helper()
	d, ok, err := w.svc.Directory(t.Context(), dirIssuer)
	if err != nil || !ok {
		t.Fatalf("the directory: %v %v", ok, err)
	}
	rep, err := w.svc.IssuedKeysReport(t.Context(), dirIssuer)
	if err != nil {
		t.Fatal(err)
	}
	return service.IssuedKeysPage{
		Issuer: dirIssuer, Generation: d.Generation, Reset: d.Reset, Keys: keys, Cursor: rep.Through + 1, Last: !more,
	}
}

// keys applies a last page of keys.
func (w *keysWorld) keys(t *testing.T, keys ...service.IssuedKey) service.IssuedKeysApplied {
	t.Helper()
	got, err := w.svc.ApplyIssuedKeys(t.Context(), w.page(t, false, keys...))
	if err != nil {
		t.Fatalf("applying a page: %v", err)
	}
	return got
}

// more applies a page with more behind it.
func (w *keysWorld) more(t *testing.T, keys ...service.IssuedKey) service.IssuedKeysApplied {
	t.Helper()
	got, err := w.svc.ApplyIssuedKeys(t.Context(), w.page(t, true, keys...))
	if err != nil {
		t.Fatalf("applying a page: %v", err)
	}
	return got
}

// works reports whether a credential authenticates now.
func (w *keysWorld) works(t *testing.T, raw string) bool {
	t.Helper()
	_, err := w.svc.Authenticate(t.Context(), raw, nil)
	return err == nil
}

// flags is what a key holds on a mailbox, as "read,act", "" for nothing.
func (w *keysWorld) flags(t *testing.T, id, accountID string) string {
	t.Helper()
	var read, act, send bool
	err := w.db.Reader().QueryRowContext(t.Context(), `SELECT read, act, send FROM key_access
		WHERE key_prefix = ? AND account_id = ?`, w.cred(t, id).selector, accountID).Scan(&read, &act, &send)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range []struct {
		on   bool
		name string
	}{{read, "read"}, {act, "act"}, {send, "send"}} {
		if f.on {
			out = append(out, f.name)
		}
	}
	return strings.Join(out, ",")
}

// grantState is a grant's state, with its cause, as "refused giver_not_reader";
// "" when there is no row.
func (w *keysWorld) grantState(t *testing.T, id, accountID, use string) string {
	t.Helper()
	var state, cause string
	err := w.db.Reader().QueryRowContext(t.Context(), `SELECT state, cause FROM issued_key_grants
		WHERE prefix = ? AND account_id = ? AND use = ?`, w.cred(t, id).selector, accountID, use).Scan(&state, &cause)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(state + " " + cause)
}

// report is what is owed to the issuer.
func (w *keysWorld) report(t *testing.T) service.IssuedKeysReport {
	t.Helper()
	rep, err := w.svc.IssuedKeysReport(t.Context(), dirIssuer)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// refusals are the refusals owed, as "id v1 account use cause".
func (w *keysWorld) refusals(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, r := range w.report(t).Refused {
		out = append(out, strings.Join([]string{r.ExternalID, "v" + itoa(r.Version), r.AccountID, r.Use, string(r.Cause)}, " "))
	}
	slices.Sort(out)
	return out
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// stored reports whether the key's full record is held.
func (w *keysWorld) stored(t *testing.T, id string) bool {
	t.Helper()
	return w.count(t, `SELECT count(*) FROM issued_keys WHERE prefix = ?`, w.cred(t, id).selector) == 1
}

// stub is what is left of a key under its selector, as "how", "" for none.
func (w *keysWorld) stub(t *testing.T, id string) string {
	t.Helper()
	var how string
	err := w.db.Reader().QueryRowContext(t.Context(), `SELECT how FROM issued_key_ends WHERE prefix = ?`,
		w.cred(t, id).selector).Scan(&how)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return how
}

// reset starts the directory again at gen.
func (w *keysWorld) reset(t *testing.T, gen string) {
	t.Helper()
	if _, err := w.svc.ResetDirectory(t.Context(), dirIssuer, gen); err != nil {
		t.Fatal(err)
	}
	w.gen = gen
}

// anaDoc applies Ana's document at version, at the world's generation.
func (w *keysWorld) anaDoc(t *testing.T, version int64) {
	t.Helper()
	doc := document(anaEmail, version)
	doc.Generation = w.gen
	w.apply(t, doc)
}

func TestAnIssuedKeyIsStoredFromItsRecordAndWorksAtOnce(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	got := w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub), w.given(w.box, "act", anaSub)))
	if got.Stored != 1 || got.Refused != 0 || got.Faults != 0 {
		t.Fatalf("applied %+v", got)
	}
	raw := w.cred(t, "tok-1").raw
	if !w.works(t, raw) {
		t.Fatal("the key stored does not work")
	}
	if f := w.flags(t, "tok-1", w.box); f != "read,act" {
		t.Errorf("it holds %q", f)
	}
	if w.grantState(t, "tok-1", w.box, "read") != "held" || w.grantState(t, "tok-1", w.box, "act") != "held" {
		t.Error("its grants are not held")
	}
	// What its row holds: never the key's name, never a time of use.
	var name string
	var lastUsed int64
	if err := w.db.Reader().QueryRowContext(t.Context(), `SELECT name, last_used_at FROM api_keys WHERE prefix = ?`,
		w.cred(t, "tok-1").selector).Scan(&name, &lastUsed); err != nil || name != "" || lastUsed != 0 {
		t.Errorf("the key's row: %q %d %v", name, lastUsed, err)
	}
}

func TestAPageAndItsCursorMoveInOneWrite(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	// The cursor's write refused: nothing of the page stays.
	w.exec(t, `CREATE TRIGGER test_cursor_refused BEFORE UPDATE OF cursor ON directory_keys
		BEGIN SELECT RAISE(ABORT, 'test'); END`)
	page := w.page(t, false, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	if _, err := w.svc.ApplyIssuedKeys(t.Context(), page); err == nil {
		t.Fatal("a page whose cursor could not move was applied")
	}
	if w.stored(t, "tok-1") || w.report(t).Through != 0 {
		t.Fatal("a record stayed of a page that was not written")
	}
	w.exec(t, `DROP TRIGGER test_cursor_refused`)
	if _, err := w.svc.ApplyIssuedKeys(t.Context(), page); err != nil {
		t.Fatal(err)
	}
	if !w.stored(t, "tok-1") || w.report(t).Through != page.Cursor {
		t.Fatal("the page and its cursor did not move together")
	}
	// The same page again, after a crash before its answer: behind the
	// cursor? no, at it, and changing nothing.
	again, err := w.svc.ApplyIssuedKeys(t.Context(), page)
	if err != nil || again.Stored+again.Updated != 0 {
		t.Fatalf("the same page again: %+v %v", again, err)
	}
	// A page read before the cursor moved past it is refused whole.
	stale := page
	stale.Cursor = page.Cursor - 1
	_, err = w.svc.ApplyIssuedKeys(t.Context(), stale)
	wantIs(t, "a page behind the cursor", err, service.CodeConflict, service.ErrDirectoryMoved)
	// So is one of another generation or reset.
	other := w.page(t, false)
	other.Reset++
	_, err = w.svc.ApplyIssuedKeys(t.Context(), other)
	wantIs(t, "a page of another reset", err, service.CodeConflict, service.ErrDirectoryMoved)
	other = w.page(t, false)
	other.Issuer = "https://elsewhere.example.org"
	_, err = w.svc.ApplyIssuedKeys(t.Context(), other)
	wantIs(t, "a page of another issuer", err, service.CodeConflict, service.ErrNotKeyIssuer)
}

// suspendAna has the directory disable Ana with a mark older than every key
// of the world, which suspends her and stops none of them.
func (w *keysWorld) suspendAna(t *testing.T, version int64) {
	t.Helper()
	doc := document(anaEmail, version)
	doc.Generation, doc.Status, doc.DisabledAt = w.gen, service.DirectoryDisabled, w.clock.now().Add(-24*time.Hour)
	w.apply(t, doc)
}

// resumeAna has the directory say Ana is active again, knowing of that mark.
func (w *keysWorld) resumeAna(t *testing.T, version int64) {
	t.Helper()
	doc := document(anaEmail, version)
	doc.Generation, doc.DisabledAt = w.gen, w.clock.now().Add(-24*time.Hour)
	w.apply(t, doc)
}

func TestAGrantIsJudgedOnce(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	// Ana reads nothing while suspended: a grant she gives then is refused,
	// and the one she gave before keeps what it held.
	w.suspendAna(t, 2)
	w.keys(t, w.record(t, "tok-1", 2, w.given(w.box, "read", anaSub), w.given(w.other, "read", anaSub)))
	if w.grantState(t, "tok-1", w.box, "read") != "held" ||
		w.grantState(t, "tok-1", w.other, "read") != "refused giver_not_reader" {
		t.Fatal("the grants were not judged as given")
	}
	owed := w.refusals(t)
	// She reads again. The next version lists both grants as they were:
	// neither is judged again.
	w.resumeAna(t, 3)
	w.keys(t, w.record(t, "tok-1", 3, w.given(w.box, "read", anaSub), w.given(w.other, "read", anaSub)))
	if f := w.flags(t, "tok-1", w.box); f != "read" {
		t.Errorf("a grant held was judged again: it holds %q", f)
	}
	if w.flags(t, "tok-1", w.other) != "" || w.grantState(t, "tok-1", w.other, "read") != "refused giver_not_reader" {
		t.Error("a grant refused was judged again")
	}
	if got := w.refusals(t); !slices.Equal(got, owed) {
		t.Errorf("a refusal was owed again: %q, was %q", got, owed)
	}
}

func TestAGiverWhoStopsReadingTakesNoReadAway(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	w.suspendAna(t, 2)
	if w.readsNow(t, w.box, w.ana.UserID) {
		t.Fatal("Ana still reads her mailbox while suspended")
	}
	if w.flags(t, "tok-1", w.box) != "read" {
		t.Fatal("the key lost read when its giver did")
	}
	p, err := w.svc.Authenticate(t.Context(), w.cred(t, "tok-1").raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.GetAccount(t.Context(), p, w.box); err != nil {
		t.Errorf("the key no longer reaches the mailbox: %v", err)
	}
}

func TestReadIsHeldOnlyWhenItsGiverReadsTheMailbox(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.followed(t, "bea@example.com")
	w.apply(t, document("bea@example.com", 1))
	w.keys(t,
		w.record(t, "tok-ana", 1, w.given(w.box, "read", anaSub)),
		w.record(t, "tok-bea", 1, w.given(w.box, "read", subjectOf("bea@example.com"))),
		w.record(t, "tok-nobody", 1, w.given(w.box, "read", "sub-nobody")))
	if w.flags(t, "tok-ana", w.box) != "read" {
		t.Error("read given by its reader is not held")
	}
	for _, id := range []string{"tok-bea", "tok-nobody"} {
		if w.flags(t, id, w.box) != "" || w.grantState(t, id, w.box, "read") != "refused giver_not_reader" {
			t.Errorf("%s: read given by someone who does not read it here: %q %q", id, w.flags(t, id, w.box),
				w.grantState(t, id, w.box, "read"))
		}
	}
	if got, want := w.refusals(t), []string{"tok-bea v1 " + w.box + " read giver_not_reader",
		"tok-nobody v1 " + w.box + " read giver_not_reader"}; !slices.Equal(got, want) {
		t.Errorf("owed %q, want %q", got, want)
	}
}

func TestActIsRefusedBesideARefusedRead(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", "sub-nobody"), w.given(w.box, "act", anaSub)))
	if w.flags(t, "tok-1", w.box) != "" {
		t.Fatal("act held without read")
	}
	if w.grantState(t, "tok-1", w.box, "act") != "refused giver_not_reader" {
		t.Errorf("act beside a refused read: %q", w.grantState(t, "tok-1", w.box, "act"))
	}
	if got := w.refusals(t); len(got) != 2 {
		t.Errorf("owed %q, want read and act", got)
	}
}

func TestAnIssuedKeyNeverHoldsSend(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub), w.given(w.box, "send", anaSub)))
	if f := w.flags(t, "tok-1", w.box); f != "read" {
		t.Errorf("it holds %q", f)
	}
	if w.grantState(t, "tok-1", w.box, "send") != "refused not_offered" {
		t.Errorf("send: %q", w.grantState(t, "tok-1", w.box, "send"))
	}
}

func TestKeyTermsAreJudgedOnceAndForAll(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	old := w.record(t, "tok-old", 1, w.given(w.box, "read", anaSub))
	old.Terms = "terms-of-before"
	none := w.record(t, "tok-none", 1, w.given(w.box, "read", anaSub))
	none.Terms = ""
	w.keys(t, old, none)
	for _, id := range []string{"tok-old", "tok-none"} {
		if w.flags(t, id, w.box) != "" || w.grantState(t, id, w.box, "read") != "refused terms_not_current" {
			t.Errorf("%s: a key under terms not current holds %q", id, w.flags(t, id, w.box))
		}
	}
	// A later version naming the current terms changes nothing: judged
	// once, at the first storing, for the key's life.
	later := w.record(t, "tok-old", 2, w.given(w.box, "read", anaSub), w.given(w.other, "read", anaSub))
	w.keys(t, later)
	if w.flags(t, "tok-old", w.box) != "" || w.flags(t, "tok-old", w.other) != "" ||
		w.grantState(t, "tok-old", w.other, "read") != "refused terms_not_current" {
		t.Error("a key refused for its terms was given something later")
	}
}

func TestAGrantOnARemovedMailboxIsRefusedAndARecoveryGivesNothingBack(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub), w.given(w.box, "act", anaSub)))
	w.removed(t, w.ana, w.box)
	if w.flags(t, "tok-1", w.box) != "" || w.grantState(t, "tok-1", w.box, "read") != "removed mailbox_removed" ||
		w.grantState(t, "tok-1", w.box, "act") != "removed mailbox_removed" {
		t.Fatal("a removed mailbox's grants are not tombstones")
	}
	if _, err := w.svc.RecoverAccount(t.Context(), w.ana, w.box); err != nil {
		t.Fatal(err)
	}
	// The same grant listed again: kept off.
	w.keys(t, w.record(t, "tok-1", 2, w.given(w.box, "read", anaSub), w.given(w.box, "act", anaSub)))
	if w.flags(t, "tok-1", w.box) != "" {
		t.Error("a recovery gave an issued key back what it held")
	}
	// A new key's grant on a mailbox removed: refused, owed.
	w.removed(t, w.ana, w.other)
	w.keys(t, w.record(t, "tok-2", 1, w.given(w.other, "read", anaSub)))
	if w.grantState(t, "tok-2", w.other, "read") != "refused mailbox_unknown" {
		t.Errorf("a grant on a removed mailbox: %q", w.grantState(t, "tok-2", w.other, "read"))
	}
	// And one on a mailbox this server never had: owed, with no row.
	w.keys(t, w.record(t, "tok-3", 1, w.given("acc_ffffffffffffffff", "read", anaSub)))
	if !slices.Contains(w.refusals(t), "tok-3 v1 acc_ffffffffffffffff read mailbox_unknown") {
		t.Errorf("owed %q", w.refusals(t))
	}
}

func TestReadLeavingTakesActInTheSameWrite(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	both := func(v int64) service.IssuedKey {
		return w.record(t, "tok-1", v, w.given(w.box, "read", anaSub), w.given(w.box, "act", anaSub),
			w.given(w.other, "read", anaSub), w.given(w.other, "act", anaSub))
	}
	w.keys(t, both(1))
	// Unlisted: both go, and the row with them, never left at no flag.
	w.keys(t, w.record(t, "tok-1", 2, w.given(w.other, "read", anaSub), w.given(w.other, "act", anaSub)))
	if w.flags(t, "tok-1", w.box) != "" || w.grantState(t, "tok-1", w.box, "act") != "" {
		t.Errorf("read unlisted left act: %q", w.flags(t, "tok-1", w.box))
	}
	// Read listed anew, by someone who does not read it: refused, act with it.
	w.keys(t, w.record(t, "tok-1", 3, w.given(w.other, "act", anaSub),
		service.IssuedGrant{AccountID: w.other, Use: "read", GivenBy: "sub-nobody", GivenAt: w.givenAt.Add(time.Second)}))
	if w.flags(t, "tok-1", w.other) != "" || w.grantState(t, "tok-1", w.other, "act") != "refused giver_not_reader" {
		t.Errorf("read refused left act: %q %q", w.flags(t, "tok-1", w.other), w.grantState(t, "tok-1", w.other, "act"))
	}
	// A mailbox removed: both, by the schema's trigger.
	w.keys(t, w.record(t, "tok-2", 1, w.given(w.box, "read", anaSub), w.given(w.box, "act", anaSub)))
	w.removed(t, w.ana, w.box)
	if w.grantState(t, "tok-2", w.box, "act") != "removed mailbox_removed" {
		t.Error("a mailbox removed left act")
	}
}

func TestAnActWithoutReadRecordTakesBothFlagsAndTheFeedMovesOn(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub), w.given(w.box, "act", anaSub)))
	// The kit's vector: a record listing act without read, which fails its
	// checks and narrows.
	bad := w.record(t, "tok-1", 2, w.given(w.box, "act", anaSub))
	got := w.keys(t, bad)
	if got.Faults != 0 || !got.Narrowed {
		t.Fatalf("applied %+v", got)
	}
	if w.flags(t, "tok-1", w.box) != "" {
		t.Errorf("it still holds %q", w.flags(t, "tok-1", w.box))
	}
	if got := w.refusals(t); !slices.Equal(got, []string{"tok-1 v2 " + w.box + " act invalid"}) {
		t.Errorf("owed %q", got)
	}
	if w.report(t).Through != 2 {
		t.Error("the feed did not move on")
	}
}

func TestAPartialRecordOnlyNarrows(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub), w.given(w.other, "read", anaSub)))
	partial := w.record(t, "tok-1", 2, w.given(w.box, "read", anaSub),
		service.IssuedGrant{AccountID: w.other, Use: "act", GivenBy: anaSub, GivenAt: w.givenAt},
		service.IssuedGrant{AccountID: w.box, Use: "act", GivenBy: anaSub, GivenAt: w.givenAt})
	partial.Partial = true
	partial.ExpiresAt = w.clock.now().Add(24 * time.Hour)
	w.keys(t, partial)
	if w.flags(t, "tok-1", w.box) != "read" {
		t.Errorf("a grant still listed: %q", w.flags(t, "tok-1", w.box))
	}
	if w.flags(t, "tok-1", w.other) != "" {
		t.Error("a grant no longer listed stayed")
	}
	if w.grantState(t, "tok-1", w.box, "act") != "refused invalid" {
		t.Errorf("a new grant of a partial record: %q", w.grantState(t, "tok-1", w.box, "act"))
	}
	var expires int64
	if err := w.db.Reader().QueryRowContext(t.Context(), `SELECT expires_at FROM api_keys WHERE prefix = ?`,
		w.cred(t, "tok-1").selector).Scan(&expires); err != nil || expires != w.clock.now().Add(24*time.Hour).Unix() {
		t.Errorf("the earlier expiry: %d %v", expires, err)
	}
	// A partial record never moves an expiry later, and its revocation
	// applies.
	revoking := w.record(t, "tok-1", 3)
	revoking.Partial, revoking.Revoked = true, true
	w.keys(t, revoking)
	if w.works(t, w.cred(t, "tok-1").raw) {
		t.Error("a partial record's revocation did not apply")
	}
}

func TestAPartialRecordOfAKeyNeverHeldRefusesItsGrants(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	partial := w.record(t, "tok-1", 4, w.given(w.box, "read", anaSub))
	partial.Partial = true
	w.keys(t, partial)
	if w.stored(t, "tok-1") {
		t.Fatal("a partial record of a key never held was stored")
	}
	if got := w.refusals(t); !slices.Equal(got, []string{"tok-1 v4 " + w.box + " read invalid"}) {
		t.Errorf("owed %q", got)
	}
}

func TestNoRefusalIsOwedAtVersionZero(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	partial := w.record(t, "tok-1", 0, w.given(w.box, "read", anaSub))
	partial.Partial = true
	w.keys(t, partial)
	w.keys(t, w.record(t, "tok-2", 1, w.given(w.box, "read", anaSub)))
	undecoded := w.record(t, "tok-2", 0, w.given(w.box, "read", anaSub), w.given(w.box, "act", anaSub))
	undecoded.Partial = true
	w.keys(t, undecoded)
	if got := w.refusals(t); len(got) != 0 {
		t.Errorf("owed at version 0: %q", got)
	}
}

func TestAZeroExpiryEndsAnIssuedKeyAtOnce(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	// The kit's record whose expiry did not decode: the earliest.
	zero := w.record(t, "tok-1", 2)
	zero.Partial, zero.ExpiresAt = true, time.Time{}
	w.keys(t, zero)
	if w.works(t, w.cred(t, "tok-1").raw) {
		t.Error("a key past a zero expiry still works")
	}
	var expires, ended int64
	if err := w.db.Reader().QueryRowContext(t.Context(), `SELECT k.expires_at, i.ended_at FROM api_keys k
		JOIN issued_keys i ON i.prefix = k.prefix WHERE k.prefix = ?`, w.cred(t, "tok-1").selector).
		Scan(&expires, &ended); err != nil || expires == 0 || ended == 0 {
		t.Errorf("expires %d, ended %d: %v; never 0, which never expires", expires, ended, err)
	}
	// A key never held that arrives with one leaves only its stub.
	never := w.record(t, "tok-2", 1, w.given(w.box, "read", anaSub))
	never.ExpiresAt = time.Time{}
	w.keys(t, never)
	if w.stored(t, "tok-2") || w.stub(t, "tok-2") != "expired" {
		t.Errorf("a key never held, expired: stored %v, stub %q", w.stored(t, "tok-2"), w.stub(t, "tok-2"))
	}
}

func TestARevocationAtAnUnknownTimeStillRevokes(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	revoked := w.record(t, "tok-1", 2)
	revoked.Partial, revoked.Revoked, revoked.RevokedAt = true, true, time.Time{}
	got := w.keys(t, revoked)
	if got.Revoked != 1 || w.works(t, w.cred(t, "tok-1").raw) {
		t.Fatalf("revoked at a time unknown: %+v", got)
	}
	var at int64
	var reason string
	if err := w.db.Reader().QueryRowContext(t.Context(), `SELECT revoked_at, stop_reason FROM api_keys WHERE prefix = ?`,
		w.cred(t, "tok-1").selector).Scan(&at, &reason); err != nil || at != w.clock.now().Unix() || reason != "manual" {
		t.Errorf("revoked at %d (%s): %v", at, reason, err)
	}
	// A revocation dated later than now is now.
	w.keys(t, w.record(t, "tok-2", 1, w.given(w.box, "read", anaSub)))
	later := w.record(t, "tok-2", 2)
	later.Revoked, later.RevokedAt, later.Stop = true, w.clock.now().Add(time.Hour), workspace.KeyStopCreatorLeft
	w.keys(t, later)
	if err := w.db.Reader().QueryRowContext(t.Context(), `SELECT revoked_at, stop_reason FROM api_keys WHERE prefix = ?`,
		w.cred(t, "tok-2").selector).Scan(&at, &reason); err != nil || at != w.clock.now().Unix() || reason != "creator_left" {
		t.Errorf("revoked at %d (%s): %v", at, reason, err)
	}
}

func TestAForgottenSelectorIsNeverStoredAgain(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	revoked := w.record(t, "tok-1", 2)
	revoked.Revoked, revoked.RevokedAt = true, w.clock.now()
	w.keys(t, revoked)
	w.clock.advance(31 * 24 * time.Hour)
	if _, err := w.svc.SweepIssuedKeys(t.Context(), w.clock.now()); err != nil {
		t.Fatal(err)
	}
	if w.stored(t, "tok-1") || w.stub(t, "tok-1") != "revoked" {
		t.Fatalf("after its 30 days: stored %v, stub %q", w.stored(t, "tok-1"), w.stub(t, "tok-1"))
	}
	// Its record sent again, live, by an issuer restored from before: never
	// stored again.
	live := w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub))
	live.ExpiresAt = w.clock.now().Add(30 * 24 * time.Hour)
	got := w.keys(t, live)
	if got.Forgotten != 1 || w.stored(t, "tok-1") || w.works(t, w.cred(t, "tok-1").raw) {
		t.Fatalf("a forgotten selector was stored again: %+v", got)
	}
}

func TestAForgottenSelectorsGrantsAreRefusedOutsideAPass(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	c := w.cred(t, "tok-1")
	w.exec(t, `INSERT INTO issued_key_ends(prefix, how, ended_at, forget_at) VALUES (?, 'revoked', ?, ?)`,
		c.selector, w.clock.now().Unix(), w.clock.now().Add(300*24*time.Hour).Unix())
	w.keys(t, w.record(t, "tok-1", 3, w.given(w.box, "read", anaSub)))
	// Its workspace gone from here (its creator closed here while the
	// issuer keeps it): item unknown.
	gone := w.record(t, "tok-1", 4, w.given(w.box, "act", anaSub))
	gone.WorkspaceID = "personal-sub-nobody"
	w.keys(t, gone)
	want := []string{"tok-1 v3 " + w.box + " read not_offered", "tok-1 v4 " + w.box + " act mailbox_unknown"}
	slices.Sort(want)
	if got := w.refusals(t); !slices.Equal(got, want) {
		t.Errorf("owed %q, want %q", got, want)
	}
	// One revoked, or past its expiry, changes nothing.
	revoked := w.record(t, "tok-1", 5, w.given(w.box, "read", anaSub))
	revoked.Revoked = true
	w.keys(t, revoked)
	if got := w.refusals(t); !slices.Equal(got, want) {
		t.Errorf("owed %q after a revoked record", got)
	}
}

func TestARecordTheSchemaRefusesNeverFailsItsPage(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-held", 1, w.given(w.box, "read", anaSub)),
		w.record(t, "tok-stuck", 1, w.given(w.box, "read", anaSub)),
		w.record(t, "tok-late", 1, w.given(w.box, "read", anaSub)))
	// A selector this server holds as one of its own keys: the schema
	// refuses the record whole; it is refused narrowed.
	own := w.keyOf(t, w.ws, w.ana.UserID)
	collides := w.record(t, "tok-collides", 1, w.given(w.box, "read", anaSub))
	collides.Selector = own
	// A new grant the schema refuses (a test's trigger): the record applies
	// as a partial one, the grant refused.
	w.exec(t, `CREATE TRIGGER test_no_other BEFORE INSERT ON key_access WHEN NEW.account_id = '`+w.other+`'
		BEGIN SELECT RAISE(ABORT, 'test'); END`)
	refusedGrant := w.record(t, "tok-held", 2, w.given(w.box, "read", anaSub), w.given(w.other, "read", anaSub))
	// A record that cannot be written even narrowed (a test's trigger on
	// its version): its key is stopped by the operator's stop instead.
	w.exec(t, `CREATE TRIGGER test_stuck BEFORE UPDATE OF version ON issued_keys WHEN NEW.prefix = '`+
		w.cred(t, "tok-stuck").selector+`' BEGIN SELECT RAISE(ABORT, 'test'); END`)
	stuck := w.record(t, "tok-stuck", 2)
	// And a revocation later in the same page.
	late := w.record(t, "tok-late", 2)
	late.Revoked = true
	got := w.keys(t, collides, refusedGrant, stuck, late)
	if got.Faults != 1 || !got.Narrowed {
		t.Fatalf("applied %+v", got)
	}
	if w.stored(t, "tok-collides") || !slices.Contains(w.refusals(t), "tok-collides v1 "+w.box+" read invalid") {
		t.Errorf("the colliding record: stored %v, owed %q", w.stored(t, "tok-collides"), w.refusals(t))
	}
	if w.flags(t, "tok-held", w.box) != "read" || w.grantState(t, "tok-held", w.other, "read") != "refused invalid" {
		t.Errorf("the record refused a new grant: %q %q", w.flags(t, "tok-held", w.box),
			w.grantState(t, "tok-held", w.other, "read"))
	}
	if w.works(t, w.cred(t, "tok-stuck").raw) ||
		w.count(t, `SELECT count(*) FROM issued_key_stops WHERE prefix = ? AND cause = 'operator'`,
			w.cred(t, "tok-stuck").selector) != 1 {
		t.Error("a record that could not be written left its key working")
	}
	if w.works(t, w.cred(t, "tok-late").raw) {
		t.Error("a revocation later in the page did not apply")
	}
	if w.report(t).Through != 2 {
		t.Error("the cursor did not move")
	}
	if !strings.Contains(w.logs.String(), "issued key record not applied") {
		t.Error("the fault was not logged")
	}
}
