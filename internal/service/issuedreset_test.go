package service_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/service/issuertest"
)

// Waiting grants, the directory's reset and its pass, the creator's belt and
// this server's own stops of the keys its issuer makes.

// waitingOf lists what the issuer is owed as waiting, as "id account use".
func (w *keysWorld) waitingOf(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, g := range w.report(t).Waiting {
		out = append(out, g.ExternalID+" "+g.AccountID+" "+g.Use)
	}
	slices.Sort(out)
	return out
}

// heldOf is what the issuer is owed of a key held narrower.
func (w *keysWorld) heldOf(t *testing.T, id string) (service.IssuedHeld, bool) {
	t.Helper()
	for _, h := range w.report(t).Held {
		if h.ExternalID == id {
			return h, true
		}
	}
	return service.IssuedHeld{}, false
}

// stopsOf lists this server's own stops reported, as "id cause".
func (w *keysWorld) stopsOf(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, s := range w.report(t).Suspended {
		out = append(out, s.ExternalID+" "+string(s.Cause))
	}
	slices.Sort(out)
	return out
}

func TestAGrantWaitsUntilItsGiversDocumentApplies(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	// The directory starts again: until Ana's document of the new
	// generation applies, nothing she gives is judged.
	w.reset(t, "generation-2")
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	if w.flags(t, "tok-1", w.box) != "" || w.grantState(t, "tok-1", w.box, "read") != "waiting" {
		t.Fatalf("a grant of a giver not followed since the reset: %q", w.grantState(t, "tok-1", w.box, "read"))
	}
	if got := w.waitingOf(t); !slices.Equal(got, []string{"tok-1 " + w.box + " read"}) {
		t.Errorf("waiting %q", got)
	}
	w.anaDoc(t, 1)
	if w.flags(t, "tok-1", w.box) != "read" || len(w.waitingOf(t)) != 0 {
		t.Errorf("once her document applied: %q, waiting %q", w.flags(t, "tok-1", w.box), w.waitingOf(t))
	}
}

func TestAGrantAfterAResetWaitsUntilItsGiversDocumentApplies(t *testing.T) {
	// A reset sets every person's row to the new reset with no version: the
	// reset counter alone would take everyone as followed.
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	w.reset(t, "generation-2")
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub), w.given(w.other, "read", anaSub)))
	if w.grantState(t, "tok-1", w.other, "read") != "waiting" {
		t.Errorf("a new grant after the reset: %q", w.grantState(t, "tok-1", w.other, "read"))
	}
	if w.flags(t, "tok-1", w.box) != "read" {
		t.Error("a grant held before the reset was taken")
	}
	w.anaDoc(t, 1)
	if w.flags(t, "tok-1", w.other) != "read" {
		t.Error("the grant waiting was not judged at her document")
	}
}

func TestAWaitingGrantIsJudgedInItsGiversDocumentsWrite(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.reset(t, "generation-2")
	w.keys(t, w.record(t, "tok-1", 3, w.given(w.box, "read", anaSub)))
	raw := w.cred(t, "tok-1").raw
	// Her document says she is disabled: in its own write, the grant is
	// judged, and refused (she reads nothing now), owed at the key's version.
	w.suspendAna(t, 1)
	if w.grantState(t, "tok-1", w.box, "read") != "refused giver_not_reader" {
		t.Fatalf("judged in her document's write: %q", w.grantState(t, "tok-1", w.box, "read"))
	}
	if got := w.refusals(t); !slices.Equal(got, []string{"tok-1 v3 " + w.box + " read giver_not_reader"}) {
		t.Errorf("owed %q", got)
	}
	p, err := w.svc.Authenticate(t.Context(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.GetAccount(t.Context(), p, w.box); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("a key whose grant was refused reaches the mailbox: %v", err)
	}
}

func TestActBesideAWaitingReadWaitsAndIsJudgedWithIt(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.reset(t, "generation-2")
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub), w.given(w.box, "act", anaSub)))
	if w.grantState(t, "tok-1", w.box, "act") != "waiting" {
		t.Fatalf("act beside a waiting read: %q", w.grantState(t, "tok-1", w.box, "act"))
	}
	if got := w.waitingOf(t); !slices.Equal(got, []string{"tok-1 " + w.box + " act", "tok-1 " + w.box + " read"}) {
		t.Errorf("waiting %q", got)
	}
	w.anaDoc(t, 1)
	if w.flags(t, "tok-1", w.box) != "read,act" {
		t.Errorf("judged with read: %q", w.flags(t, "tok-1", w.box))
	}
}

func TestAWaitingGrantTheRecordNoLongerListsIsNeverJudged(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.reset(t, "generation-2")
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub), w.given(w.other, "read", anaSub)))
	// The issuer took Read on the second mailbox back before Ana's document.
	w.keys(t, w.record(t, "tok-1", 2, w.given(w.box, "read", anaSub)))
	if got := w.waitingOf(t); !slices.Equal(got, []string{"tok-1 " + w.box + " read"}) {
		t.Errorf("waiting %q", got)
	}
	w.anaDoc(t, 1)
	if w.flags(t, "tok-1", w.other) != "" || w.grantState(t, "tok-1", w.other, "read") != "" {
		t.Error("a grant no longer listed was judged")
	}
	if w.flags(t, "tok-1", w.box) != "read" {
		t.Error("the grant still listed was not")
	}
}

func TestTheKeysResetIsInTheDirectoryResetsTransaction(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 2, w.given(w.box, "read", "sub-nobody")))
	if len(w.refusals(t)) != 1 {
		t.Fatal("no refusal owed")
	}
	// The keys' part refused: the directory's reset is undone with it.
	w.exec(t, `CREATE TRIGGER test_keys_reset BEFORE UPDATE OF version ON issued_keys
		BEGIN SELECT RAISE(ABORT, 'test'); END`)
	if _, err := w.svc.ResetDirectory(t.Context(), dirIssuer, "generation-2"); err == nil {
		t.Fatal("a reset whose keys could not start again was made")
	}
	if d, _, _ := w.svc.Directory(t.Context(), dirIssuer); d.Generation != dirGen || d.Reset != 0 {
		t.Fatalf("the directory reset without its keys: %+v", d)
	}
	w.exec(t, `DROP TRIGGER test_keys_reset`)
	w.reset(t, "generation-2")
	rep := w.report(t)
	if rep.Generation != "generation-2" || rep.Reset != 1 || rep.Through != 0 || !rep.PassOpen || len(rep.Refused) != 0 {
		t.Errorf("after the reset: %+v", rep)
	}
	if w.count(t, `SELECT count(*) FROM issued_keys WHERE version <> 0 OR listed <> 0`) != 0 {
		t.Error("a key's version stayed")
	}
}

func TestAResetDropsTheRefusalsOwed(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	never := w.record(t, "tok-2", 1, w.given(w.box, "read", anaSub))
	never.WorkspaceID = "personal-sub-nobody"
	w.keys(t, w.record(t, "tok-1", 2, w.given(w.box, "read", "sub-nobody")), never)
	if len(w.refusals(t)) != 2 {
		t.Fatalf("owed %q", w.refusals(t))
	}
	w.reset(t, "generation-2")
	if got := w.refusals(t); len(got) != 0 {
		t.Errorf("owed after the reset %q", got)
	}
	if w.grantState(t, "tok-1", w.box, "read") != "refused giver_not_reader" {
		t.Error("the grant refused was judged again by the reset")
	}
}

// publishedThrough applies a last page that says the issuer published
// everything up to at.
func (w *keysWorld) publishedThrough(t *testing.T, at time.Time) {
	t.Helper()
	page := w.page(t, false)
	page.PublishedThrough = at
	if _, err := w.svc.ApplyIssuedKeys(t.Context(), page); err != nil {
		t.Fatal(err)
	}
}

func TestAResetNeverWidensAnIssuedKey(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	far := w.cred(t, "tok-short").created.Add(90 * 24 * time.Hour)
	short := w.record(t, "tok-short", 1, w.given(w.box, "read", anaSub))
	w.keys(t, w.record(t, "tok-revoked", 1, w.given(w.box, "read", anaSub)), short,
		w.record(t, "tok-grants", 1, w.given(w.box, "read", anaSub)))
	revoked := w.record(t, "tok-revoked", 2)
	revoked.Revoked = true
	shortened := w.record(t, "tok-short", 2, w.given(w.box, "read", anaSub))
	shortened.ExpiresAt = w.clock.now().Add(48 * time.Hour)
	w.keys(t, revoked, shortened)
	w.publishedThrough(t, w.clock.now())
	// The issuer restored from before: its pass lists the keys wider.
	w.reset(t, "generation-2")
	old := service.IssuedGrant{AccountID: w.other, Use: "read", GivenBy: anaSub, GivenAt: w.givenAt}
	fresh := service.IssuedGrant{AccountID: w.box, Use: "read", GivenBy: anaSub, GivenAt: w.clock.now().Add(time.Second)}
	wide := w.record(t, "tok-short", 1, w.given(w.box, "read", anaSub))
	wide.ExpiresAt = far
	w.more(t, w.record(t, "tok-revoked", 1, w.given(w.box, "read", anaSub)), wide,
		w.record(t, "tok-grants", 1, w.given(w.box, "read", anaSub), old),
		w.record(t, "tok-new", 1, fresh))
	if w.works(t, w.cred(t, "tok-revoked").raw) {
		t.Error("a key held revoked works again")
	}
	var expires int64
	if err := w.db.Reader().QueryRowContext(t.Context(), `SELECT expires_at FROM api_keys WHERE prefix = ?`,
		w.cred(t, "tok-short").selector).Scan(&expires); err != nil || expires != shortened.ExpiresAt.Unix() {
		t.Errorf("the earlier expiry: %d %v", expires, err)
	}
	if w.flags(t, "tok-grants", w.other) != "" || w.grantState(t, "tok-grants", w.other, "read") != "removed reset" {
		t.Errorf("an old grant not held at the reset: %q %q", w.flags(t, "tok-grants", w.other),
			w.grantState(t, "tok-grants", w.other, "read"))
	}
	if w.grantState(t, "tok-new", w.box, "read") != "waiting" {
		t.Errorf("a new grant during the pass: %q", w.grantState(t, "tok-new", w.box, "read"))
	}
	if w.flags(t, "tok-grants", w.box) != "read" {
		t.Error("a grant held at the reset was taken")
	}
}

func TestWhatIsHeldNarrowerIsOwedToTheIssuer(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-revoked", 1, w.given(w.box, "read", anaSub)),
		w.record(t, "tok-short", 1, w.given(w.box, "read", anaSub)),
		w.record(t, "tok-grants", 1, w.given(w.box, "read", anaSub)))
	revoked := w.record(t, "tok-revoked", 2)
	revoked.Revoked = true
	shortened := w.record(t, "tok-short", 2, w.given(w.box, "read", anaSub))
	shortened.ExpiresAt = w.clock.now().Add(48 * time.Hour)
	w.keys(t, revoked, shortened)
	w.publishedThrough(t, w.clock.now())
	w.reset(t, "generation-2")
	w.more(t, w.record(t, "tok-revoked", 1, w.given(w.box, "read", anaSub)),
		w.record(t, "tok-short", 1, w.given(w.box, "read", anaSub)),
		w.record(t, "tok-grants", 1, w.given(w.box, "read", anaSub), w.given(w.other, "read", anaSub)))
	if h, ok := w.heldOf(t, "tok-revoked"); !ok || !h.Revoked {
		t.Errorf("a key held revoked: %+v %v", h, ok)
	}
	if h, ok := w.heldOf(t, "tok-short"); !ok || h.Revoked || !h.ExpiresAt.Equal(shortened.ExpiresAt.Truncate(time.Second)) {
		t.Errorf("a key held to an earlier expiry: %+v %v", h, ok)
	}
	if h, ok := w.heldOf(t, "tok-grants"); !ok || h.Revoked || !h.ExpiresAt.IsZero() ||
		!slices.Equal(h.Removed, []service.IssuedGrantRef{{AccountID: w.other, Use: "read"}}) {
		t.Errorf("a key held without a grant: %+v %v", h, ok)
	}
	// Told: forgotten.
	var ids []int64
	for _, h := range w.report(t).Held {
		ids = append(ids, h.ID)
	}
	if err := w.svc.IssuedKeysTold(t.Context(), dirIssuer, ids); err != nil {
		t.Fatal(err)
	}
	if got := w.report(t).Held; len(got) != 0 {
		t.Errorf("held after it was told: %+v", got)
	}
}

func TestASecondResetDuringThePassNeverWidens(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	w.publishedThrough(t, w.clock.now())
	// Two resets before the pass's last page: the trusted moment of the
	// first stays.
	w.reset(t, "generation-2")
	w.reset(t, "generation-3")
	w.more(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub), w.given(w.other, "read", anaSub)))
	if w.flags(t, "tok-1", w.other) != "" || w.grantState(t, "tok-1", w.other, "read") != "removed reset" {
		t.Errorf("a grant held off: %q %q", w.flags(t, "tok-1", w.other), w.grantState(t, "tok-1", w.other, "read"))
	}
	if h, ok := w.heldOf(t, "tok-1"); !ok || !slices.Equal(h.Removed, []service.IssuedGrantRef{{AccountID: w.other, Use: "read"}}) {
		t.Errorf("held %+v %v", h, ok)
	}
}

func TestAResetReportsTheRefusedAndRemovedGrantsThePassLists(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub), w.given(w.other, "read", "sub-nobody")))
	w.removed(t, w.ana, w.box)
	w.reset(t, "generation-2")
	// A restore lost the refusal and the mailbox's removal: the pass lists
	// both grants as they were given.
	w.more(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub), w.given(w.other, "read", "sub-nobody")))
	h, ok := w.heldOf(t, "tok-1")
	want := []service.IssuedGrantRef{{AccountID: w.box, Use: "read"}, {AccountID: w.other, Use: "read"}}
	slices.SortFunc(h.Removed, func(a, b service.IssuedGrantRef) int {
		if a.AccountID < b.AccountID {
			return -1
		}
		return 1
	})
	slices.SortFunc(want, func(a, b service.IssuedGrantRef) int {
		if a.AccountID < b.AccountID {
			return -1
		}
		return 1
	})
	if !ok || !slices.Equal(h.Removed, want) {
		t.Errorf("held %+v %v, want %+v", h, ok, want)
	}
	if len(w.refusals(t)) != 0 {
		t.Errorf("owed as refusals %q", w.refusals(t))
	}
}

func TestWhatIsOwedAsKeptOffNeverNamesAGrantGivenAgainSince(t *testing.T) {
	for name, before := range map[string]func(w *keysWorld) []service.IssuedGrant{
		// An old grant not held at the reset: a tombstone.
		"an old grant": func(w *keysWorld) []service.IssuedGrant { return nil },
		// A grant refused before the reset, which the pass lists again.
		"a grant refused": func(w *keysWorld) []service.IssuedGrant {
			return []service.IssuedGrant{w.given(w.other, "read", "sub-nobody")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newKeysWorld(t, fixtureOptions{})
			first := append([]service.IssuedGrant{w.given(w.box, "read", anaSub)}, before(w)...)
			w.keys(t, w.record(t, "tok-1", 1, first...))
			w.publishedThrough(t, w.clock.now())
			w.reset(t, "generation-2")
			w.anaDoc(t, 1)
			off := w.given(w.other, "read", anaSub)
			if len(before(w)) > 0 {
				off = before(w)[0]
			}
			w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub), off))
			if h, ok := w.heldOf(t, "tok-1"); !ok ||
				!slices.Equal(h.Removed, []service.IssuedGrantRef{{AccountID: w.other, Use: "read"}}) {
				t.Fatalf("held %+v %v", h, ok)
			}
			// Before the issuer took that, Ana gives read on the mailbox
			// again: held, and no longer owed as kept off.
			again := service.IssuedGrant{AccountID: w.other, Use: "read", GivenBy: anaSub, GivenAt: w.clock.now()}
			w.keys(t, w.record(t, "tok-1", 2, w.given(w.box, "read", anaSub), again))
			if w.flags(t, "tok-1", w.other) != "read" {
				t.Fatalf("the grant given again: %q", w.grantState(t, "tok-1", w.other, "read"))
			}
			if h, ok := w.heldOf(t, "tok-1"); ok {
				t.Errorf("still owed as held narrower: %+v", h)
			}
		})
	}
	// What a key is held narrower by besides stays owed: its revocation,
	// its earlier expiry.
	w := newKeysWorld(t, fixtureOptions{})
	short := w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub))
	w.keys(t, short)
	short.Version, short.ExpiresAt = 2, w.clock.now().Add(48*time.Hour)
	w.keys(t, short)
	w.publishedThrough(t, w.clock.now())
	w.reset(t, "generation-2")
	w.anaDoc(t, 1)
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub), w.given(w.other, "read", anaSub)))
	again := service.IssuedGrant{AccountID: w.other, Use: "read", GivenBy: anaSub, GivenAt: w.clock.now()}
	w.keys(t, w.record(t, "tok-1", 2, w.given(w.box, "read", anaSub), again))
	if h, ok := w.heldOf(t, "tok-1"); !ok || len(h.Removed) != 0 || !h.ExpiresAt.Equal(short.ExpiresAt) {
		t.Errorf("held %+v %v", h, ok)
	}
}

func TestAnOldGrantNotHeldAtTheResetIsNeverJudgedLaterInTheGeneration(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	w.publishedThrough(t, w.clock.now())
	w.reset(t, "generation-2")
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub), w.given(w.other, "read", anaSub)))
	w.anaDoc(t, 1)
	// A later record of the generation, published before the issuer took
	// what it was told, still lists it: never judged.
	w.keys(t, w.record(t, "tok-1", 2, w.given(w.box, "read", anaSub), w.given(w.other, "read", anaSub)))
	if w.flags(t, "tok-1", w.other) != "" || w.grantState(t, "tok-1", w.other, "read") != "removed reset" {
		t.Errorf("an old grant: %q %q", w.flags(t, "tok-1", w.other), w.grantState(t, "tok-1", w.other, "read"))
	}
}

func TestAKeyThePassDidNotListIsRevokedWhenThePassEnds(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-listed", 1, w.given(w.box, "read", anaSub)),
		w.record(t, "tok-gone", 1, w.given(w.box, "read", anaSub)))
	w.reset(t, "generation-2")
	w.more(t, w.record(t, "tok-listed", 1, w.given(w.box, "read", anaSub)))
	if !w.works(t, w.cred(t, "tok-gone").raw) {
		t.Fatal("a key not listed yet stopped before the pass ended")
	}
	got := w.keys(t)
	if !got.PassClosed || got.Unlisted != 1 {
		t.Fatalf("the pass's end: %+v", got)
	}
	if w.works(t, w.cred(t, "tok-gone").raw) || !w.works(t, w.cred(t, "tok-listed").raw) {
		t.Error("the pass's end revoked the wrong key")
	}
	if _, ok := w.heldOf(t, "tok-gone"); ok || w.report(t).PassOpen {
		t.Error("a key the issuer no longer has was owed, or the pass is open")
	}
	var kind string
	if err := w.db.Reader().QueryRowContext(t.Context(), `SELECT end_kind FROM issued_keys WHERE prefix = ?`,
		w.cred(t, "tok-gone").selector).Scan(&kind); err != nil || kind != "unlisted" {
		t.Errorf("ended %q: %v", kind, err)
	}
}

func TestAKeyStoppedForARecordThePassCouldNotApplyIsNotRevokedAsUnlisted(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-stuck", 1, w.given(w.box, "read", anaSub)),
		w.record(t, "tok-stopped", 1, w.given(w.box, "read", anaSub)))
	w.reset(t, "generation-2")
	// tok-stuck's record cannot be written even narrowed (a test's trigger
	// on its version); tok-stopped's could not be applied, and the extension
	// stopped its key by itself, as it does a record it applies alone.
	w.exec(t, `CREATE TRIGGER test_stuck BEFORE UPDATE OF version ON issued_keys WHEN NEW.prefix = '`+
		w.cred(t, "tok-stuck").selector+`' BEGIN SELECT RAISE(ABORT, 'test'); END`)
	if stopped, err := w.svc.StopIssuedKey(t.Context(), dirIssuer, "tok-stopped", w.cred(t, "tok-stopped").selector); err != nil || !stopped {
		t.Fatalf("stopping: %v %v", stopped, err)
	}
	got := w.keys(t, w.record(t, "tok-stuck", 1, w.given(w.box, "read", anaSub)))
	if got.Faults != 1 || !got.PassClosed || got.Unlisted != 0 {
		t.Fatalf("applied %+v", got)
	}
	for _, id := range []string{"tok-stuck", "tok-stopped"} {
		if w.count(t, `SELECT count(*) FROM api_keys WHERE prefix = ? AND revoked_at = 0`, w.cred(t, id).selector) != 1 {
			t.Errorf("%s, a key the pass named, was revoked as unlisted", id)
		}
	}
	if got := w.stopsOf(t); !slices.Equal(got, []string{"tok-stopped operator", "tok-stuck operator"}) {
		t.Errorf("stops %q", got)
	}
}

func TestUnderTheEditionsWithdrawalEachKeyIsStoppedOnceAndTheListSaysWhenThereIsMore(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{issuedKeysRefused: true})
	var page []service.IssuedKey
	for i := range service.MaxIssuedReportEntries + 1 {
		page = append(page, w.record(t, fmt.Sprintf("tok-%04d", i), 1))
	}
	w.keys(t, page...)
	// The operator's own hold beside the edition's withdrawal: one stop.
	if err := w.svc.HoldIssuedKey(t.Context(), admin(), w.cred(t, "tok-0000").selector); err != nil {
		t.Fatal(err)
	}
	rep := w.report(t)
	seen := map[string]bool{}
	for _, st := range rep.Suspended {
		if seen[st.ExternalID] {
			t.Errorf("%s stopped twice", st.ExternalID)
		}
		seen[st.ExternalID] = true
	}
	if len(rep.Suspended) != service.MaxIssuedReportEntries || !rep.SuspendedMore {
		t.Errorf("%d stops, more %v", len(rep.Suspended), rep.SuspendedMore)
	}
}

func TestAnExpiredStubIsReportedAsItsEndNotAsARevocation(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	end := w.clock.now().Add(-time.Hour)
	for id, how := range map[string]string{"tok-expired": "expired", "tok-held": "held", "tok-revoked": "revoked"} {
		w.exec(t, `INSERT INTO issued_key_ends(prefix, how, ended_at, forget_at) VALUES (?, ?, ?, ?)`,
			w.cred(t, id).selector, how, end.Unix(), w.clock.now().Add(300*24*time.Hour).Unix())
	}
	w.reset(t, "generation-2")
	w.more(t, w.record(t, "tok-expired", 1, w.given(w.box, "read", anaSub)),
		w.record(t, "tok-held", 1, w.given(w.box, "read", anaSub)),
		w.record(t, "tok-revoked", 1, w.given(w.box, "read", anaSub)))
	for _, id := range []string{"tok-expired", "tok-held"} {
		if h, ok := w.heldOf(t, id); !ok || h.Revoked || !h.ExpiresAt.Equal(end) {
			t.Errorf("%s: %+v %v, want its end, never a revocation", id, h, ok)
		}
	}
	if h, ok := w.heldOf(t, "tok-revoked"); !ok || !h.Revoked {
		t.Errorf("a revoked stub: %+v %v", h, ok)
	}
	for _, id := range []string{"tok-expired", "tok-held", "tok-revoked"} {
		if w.stored(t, id) {
			t.Errorf("%s was stored again", id)
		}
	}
}

func TestTheBeltStopsOnlyIssuedKeysCreatedBeforeTheDirectorysMoment(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	mark := w.clock.now().Add(-time.Hour).Truncate(time.Second).Add(500 * time.Microsecond)
	before := w.record(t, "tok-before", 1, w.given(w.box, "read", anaSub))
	before.CreatedAt = mark.Add(-time.Microsecond)
	at := w.record(t, "tok-at", 1, w.given(w.box, "read", anaSub))
	at.CreatedAt = mark
	w.keys(t, before, at)
	doc := document(anaEmail, 2)
	doc.CredentialsRevokedAt = mark
	w.apply(t, doc)
	if w.works(t, w.cred(t, "tok-before").raw) || !w.works(t, w.cred(t, "tok-at").raw) {
		t.Fatal("the credentials' mark stopped the wrong keys")
	}
	var reason string
	if err := w.db.Reader().QueryRowContext(t.Context(), `SELECT stop_reason FROM api_keys WHERE prefix = ?`,
		w.cred(t, "tok-before").selector).Scan(&reason); err != nil || reason != "account_recovered" {
		t.Errorf("stopped for %q: %v", reason, err)
	}
	// A disable later than the key: it stops, as creator_suspended.
	doc = document(anaEmail, 3)
	doc.Status, doc.CredentialsRevokedAt, doc.DisabledAt = service.DirectoryDisabled, mark, mark.Add(time.Microsecond)
	w.apply(t, doc)
	if w.works(t, w.cred(t, "tok-at").raw) {
		t.Fatal("a disable after the key's creation left it working")
	}
	if err := w.db.Reader().QueryRowContext(t.Context(), `SELECT stop_reason FROM api_keys WHERE prefix = ?`,
		w.cred(t, "tok-at").selector).Scan(&reason); err != nil || reason != "creator_suspended" {
		t.Errorf("stopped for %q: %v", reason, err)
	}
}

func TestTheBeltHoldsWhicheverArrivesFirst(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	mark := w.clock.now().Add(-time.Hour)
	// The record first, then the document.
	w.keys(t, w.record(t, "tok-first", 1, w.given(w.box, "read", anaSub)))
	doc := document(anaEmail, 2)
	doc.CredentialsRevokedAt = mark
	w.apply(t, doc)
	if w.works(t, w.cred(t, "tok-first").raw) {
		t.Error("the document after the record left the key working")
	}
	// The document first, then the record of a key created before it.
	got := w.keys(t, w.record(t, "tok-after", 1, w.given(w.box, "read", anaSub)))
	if got.Revoked != 1 || w.works(t, w.cred(t, "tok-after").raw) {
		t.Errorf("a record after the document: %+v", got)
	}
	// A reset's pass listing a key created before the mark as live, the
	// issuer restored from before it knew.
	w.reset(t, "generation-2")
	w.more(t, w.record(t, "tok-first", 1, w.given(w.box, "read", anaSub)),
		w.record(t, "tok-new", 1, w.given(w.box, "read", anaSub)))
	if w.works(t, w.cred(t, "tok-first").raw) || w.works(t, w.cred(t, "tok-new").raw) {
		t.Error("the pass widened a key the mark stops")
	}
	if h, ok := w.heldOf(t, "tok-first"); !ok || !h.Revoked {
		t.Errorf("held %+v %v", h, ok)
	}
}

func TestARecoveryStopsOwnKeysWithItsMarginAndIssuedKeysWithout(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	mark := time.Now().Add(-2 * time.Second)
	own := w.keyOf(t, w.ws, w.ana.UserID)
	after := w.record(t, "tok-after", 1, w.given(w.box, "read", anaSub))
	after.CreatedAt = mark.Add(time.Microsecond)
	w.keys(t, after)
	doc := document(anaEmail, 2)
	doc.CredentialsRevokedAt = mark
	w.apply(t, doc)
	if w.count(t, `SELECT count(*) FROM api_keys WHERE prefix = ? AND revoked_at <> 0`, own) != 1 {
		t.Error("an own key made within the margin was spared")
	}
	if !w.works(t, w.cred(t, "tok-after").raw) {
		t.Error("an issued key made a microsecond after the mark was stopped")
	}
}

// disableAna disables Ana here, as the operator.
func (w *keysWorld) disableAna(t *testing.T) {
	t.Helper()
	if _, err := w.svc.DisableUser(t.Context(), admin(), service.CloseUserRequest{Email: anaEmail, Force: true}); err != nil {
		t.Fatal(err)
	}
}

func TestADisableHereHoldsIssuedKeysAndRevokesOwnOnes(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	own := w.keyOf(t, w.ws, w.ana.UserID)
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	w.disableAna(t)
	if w.count(t, `SELECT count(*) FROM api_keys WHERE prefix = ? AND stop_reason = 'creator_disabled'`, own) != 1 {
		t.Error("the own key was not revoked")
	}
	if w.works(t, w.cred(t, "tok-1").raw) {
		t.Error("the issued key works while its creator is disabled here")
	}
	if w.count(t, `SELECT count(*) FROM api_keys WHERE prefix = ? AND revoked_at = 0`, w.cred(t, "tok-1").selector) != 1 {
		t.Error("the issued key was revoked here")
	}
	if got := w.stopsOf(t); !slices.Equal(got, []string{"tok-1 creator"}) {
		t.Errorf("stops %q", got)
	}
}

func TestAKeyStoredForACreatorDisabledHereIsHeld(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.disableAna(t)
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	if !w.stored(t, "tok-1") || w.works(t, w.cred(t, "tok-1").raw) {
		t.Fatal("a key stored for a creator disabled here works")
	}
	if got := w.stopsOf(t); !slices.Equal(got, []string{"tok-1 creator"}) {
		t.Errorf("stops %q", got)
	}
}

func TestTheTwoStopsLiftApart(t *testing.T) {
	selectorOf := func(w *keysWorld) string { return w.cred(t, "tok-1").selector }
	// Disabled, then held, then released: refused throughout.
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	w.disableAna(t)
	if err := w.svc.HoldIssuedKey(t.Context(), admin(), selectorOf(w)); err != nil {
		t.Fatal(err)
	}
	if got := w.stopsOf(t); !slices.Equal(got, []string{"tok-1 creator", "tok-1 operator"}) {
		t.Errorf("stops %q", got)
	}
	if err := w.svc.ReleaseIssuedKey(t.Context(), admin(), selectorOf(w)); err != nil {
		t.Fatal(err)
	}
	if w.works(t, w.cred(t, "tok-1").raw) {
		t.Error("the operator's release lifted the disable's stop")
	}
	// Held, then disabled, then released: refused throughout.
	w = newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	if err := w.svc.HoldIssuedKey(t.Context(), admin(), selectorOf(w)); err != nil {
		t.Fatal(err)
	}
	w.disableAna(t)
	if err := w.svc.ReleaseIssuedKey(t.Context(), admin(), selectorOf(w)); err != nil {
		t.Fatal(err)
	}
	if w.works(t, w.cred(t, "tok-1").raw) {
		t.Error("the operator's release lifted the disable's stop")
	}
	if got := w.stopsOf(t); !slices.Equal(got, []string{"tok-1 creator"}) {
		t.Errorf("stops %q", got)
	}
}

func TestTheOperatorHoldsAndReleasesAnIssuedKeyBySelector(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	raw, selector := w.cred(t, "tok-1").raw, w.cred(t, "tok-1").selector
	p, err := w.svc.Authenticate(t.Context(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.svc.HoldIssuedKey(t.Context(), admin(), selector); err != nil {
		t.Fatal(err)
	}
	if w.works(t, raw) {
		t.Fatal("a held key works")
	}
	wantInvalidKey(t, "a principal held since, rechecked", w.svc.Recheck(t.Context(), p))
	if got := w.stopsOf(t); !slices.Equal(got, []string{"tok-1 operator"}) {
		t.Errorf("stops %q", got)
	}
	// Twice is no error; and nothing of the key itself changed.
	if err := w.svc.HoldIssuedKey(t.Context(), admin(), selector); err != nil {
		t.Fatal(err)
	}
	if w.flags(t, "tok-1", w.box) != "read" {
		t.Error("a hold changed what the key holds")
	}
	if err := w.svc.ReleaseIssuedKey(t.Context(), admin(), selector); err != nil {
		t.Fatal(err)
	}
	if !w.works(t, raw) || len(w.stopsOf(t)) != 0 {
		t.Error("the release did not lift the hold")
	}
}

func TestAHoldOfASelectorNotHeldIsNotFound(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	_, unknown, _ := issuertest.Credential(t)
	err := w.svc.HoldIssuedKey(t.Context(), admin(), unknown)
	if service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("a selector never held: %v", err)
	}
	if err := w.svc.ReleaseIssuedKey(t.Context(), admin(), unknown); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("released: %v", err)
	}
	w.keys(t, w.record(t, "tok-2", 1, w.given(w.box, "read", anaSub)))
	r2 := w.record(t, "tok-2", 2)
	r2.Revoked = true
	w.keys(t, r2)
	if err := w.svc.HoldIssuedKey(t.Context(), admin(), w.cred(t, "tok-2").selector); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("a revoked key held: %v", err)
	}
	// An own key's prefix is no issued key.
	own := w.keyOf(t, w.ws, w.ana.UserID)
	if err := w.svc.HoldIssuedKey(t.Context(), admin(), own); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("an own key held: %v", err)
	}
}

func TestOnlyAnUnrestrictedInstanceAdminKeyHoldsAKey(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	selector := w.cred(t, "tok-1").selector
	issued, err := w.svc.Authenticate(t.Context(), w.cred(t, "tok-1").raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	restricted := admin()
	restricted.AccountIDs = []string{"acc_1"}
	owner := w.person(t, "owner@example.com", auth.RoleOwner)
	for what, p := range map[string]service.Principal{
		"a read instance key": reader(), "a restricted admin key": restricted, "a person signed in": w.ana,
		"an owner signed in": owner, "the key itself": issued,
	} {
		if err := w.svc.HoldIssuedKey(t.Context(), p, selector); service.CodeOf(err) != service.CodeNotAuthorized {
			t.Errorf("%s held a key: %v", what, err)
		}
		if err := w.svc.ReleaseIssuedKey(t.Context(), p, selector); service.CodeOf(err) != service.CodeNotAuthorized {
			t.Errorf("%s released a key: %v", what, err)
		}
	}
	if !w.works(t, w.cred(t, "tok-1").raw) {
		t.Error("the key was held")
	}
}

func TestASuspensionOutsideDocumentsStopsIssuedKeysUntilTheResume(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	raw := w.cred(t, "tok-1").raw
	if _, err := w.svc.SuspendPerson(t.Context(), w.ana.UserID); err != nil {
		t.Fatal(err)
	}
	if w.works(t, raw) {
		t.Fatal("a key works while its creator is suspended outside the directory's documents")
	}
	if got := w.stopsOf(t); !slices.Equal(got, []string{"tok-1 creator"}) {
		t.Errorf("stops %q", got)
	}
	// A key stored meanwhile is stopped too.
	w.keys(t, w.record(t, "tok-2", 1, w.given(w.box, "read", anaSub)))
	if w.works(t, w.cred(t, "tok-2").raw) {
		t.Error("a key stored for a creator suspended outside documents works")
	}
	if _, err := w.svc.ResumePerson(t.Context(), w.ana.UserID); err != nil {
		t.Fatal(err)
	}
	if !w.works(t, raw) || !w.works(t, w.cred(t, "tok-2").raw) || len(w.stopsOf(t)) != 0 {
		t.Error("the resume did not lift the stop")
	}
}

func TestIssuedKeysRefusedRefusesEveryKeyAndReportsItStopped(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{issuedKeysRefused: true})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)),
		w.record(t, "tok-2", 1, w.given(w.box, "read", anaSub)))
	if err := w.svc.HoldIssuedKey(t.Context(), admin(), w.cred(t, "tok-2").selector); err != nil {
		t.Fatal(err)
	}
	if w.works(t, w.cred(t, "tok-1").raw) {
		t.Fatal("a key works where the edition refuses them")
	}
	// The operator's hold stands under the edition's withdrawal: each key is
	// stopped once.
	if got := w.stopsOf(t); !slices.Equal(got, []string{"tok-1 withdrawn", "tok-2 withdrawn"}) {
		t.Errorf("stops %q", got)
	}
	// Its records still apply: a revocation ends its report.
	revoked := w.record(t, "tok-1", 2)
	revoked.Revoked = true
	w.keys(t, revoked)
	if got := w.stopsOf(t); !slices.Equal(got, []string{"tok-2 withdrawn"}) {
		t.Errorf("stops after a revocation %q", got)
	}
	if w.count(t, `SELECT count(*) FROM issued_key_stops WHERE cause NOT IN ('operator', 'creator')`) != 0 {
		t.Error("the edition's stop was stored")
	}
}
