package service_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// How long this server keeps what it holds of the keys an issuer makes, and
// what it reports of them besides what is owed: the snapshot's keys and the
// counts of their uses.

const day = 24 * time.Hour

// sweep runs the housekeeping's sweep of issued keys at the world's now.
func (w *keysWorld) sweep(t *testing.T) service.SweptIssuedKeys {
	t.Helper()
	if _, err := w.svc.EndExpiredIssuedKeys(t.Context(), w.clock.now()); err != nil {
		t.Fatal(err)
	}
	got, err := w.svc.SweepIssuedKeys(t.Context(), w.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// revoke applies a record revoking a key held, at the moment given.
func (w *keysWorld) revoke(t *testing.T, id string, version int64, at time.Time) {
	t.Helper()
	r := w.record(t, id, version)
	r.Revoked, r.RevokedAt = true, at
	w.keys(t, r)
}

func TestAnIssuedKeysRecordGoesThirtyDaysAfterItEnds(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	// Revoked ten days ago, as its record says, though heard of only now:
	// its 30 days count from then.
	w.revoke(t, "tok-1", 2, w.clock.now().Add(-10*day))
	w.clock.advance(19 * day)
	w.sweep(t)
	if !w.stored(t, "tok-1") {
		t.Fatal("the record went before its 30 days")
	}
	w.clock.advance(2 * day)
	if got := w.sweep(t); got.Keys != 1 {
		t.Fatalf("swept %+v", got)
	}
	if w.stored(t, "tok-1") || w.stub(t, "tok-1") != "revoked" {
		t.Errorf("after its 30 days: stored %v, stub %q", w.stored(t, "tok-1"), w.stub(t, "tok-1"))
	}
	for _, table := range []string{"api_keys", "key_access"} {
		column := map[string]string{"api_keys": "prefix", "key_access": "key_prefix"}[table]
		if w.count(t, `SELECT count(*) FROM `+table+` WHERE `+column+` = ?`, w.cred(t, "tok-1").selector) != 0 {
			t.Errorf("%s kept the key", table)
		}
	}
}

func TestARecordEndedLongAgoIsNeverStoredInFull(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	// A feed replayed from an old cursor: a key revoked 40 days ago, never
	// held here, leaves only what is left of it.
	old := w.record(t, "tok-old", 1, w.given(w.box, "read", anaSub))
	old.CreatedAt = w.clock.now().Add(-60 * day)
	old.Revoked, old.RevokedAt = true, w.clock.now().Add(-40*day)
	w.keys(t, old)
	if w.count(t, `SELECT count(*) FROM api_keys WHERE prefix = ?`, w.cred(t, "tok-old").selector) != 0 ||
		w.stub(t, "tok-old") != "revoked" {
		t.Errorf("a key revoked long ago: stored %v, stub %q", w.stored(t, "tok-old"), w.stub(t, "tok-old"))
	}
	// A key held whose record says it was revoked 40 days ago: its full
	// record goes in that write.
	w.keys(t, w.record(t, "tok-held", 1, w.given(w.box, "read", anaSub)))
	w.revoke(t, "tok-held", 2, w.clock.now().Add(-40*day))
	if w.stored(t, "tok-held") || w.stub(t, "tok-held") != "revoked" {
		t.Errorf("a key held, ended long ago: stored %v, stub %q", w.stored(t, "tok-held"), w.stub(t, "tok-held"))
	}
}

func TestAnIssuedKeyGoesWithItsCreatorsAccount(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-live", 1, w.given(w.box, "read", anaSub)),
		w.record(t, "tok-ended", 1, w.given(w.box, "read", anaSub)))
	w.revoke(t, "tok-ended", 2, w.clock.now().Add(-day))
	never := w.record(t, "tok-team", 1, w.given(w.box, "read", anaSub))
	never.WorkspaceID = "personal-sub-nobody"
	w.keys(t, never)
	doc := document(anaEmail, 2)
	doc.Status, doc.Workspaces = service.DirectoryClosed, nil
	if _, err := w.svc.ForgetExternalPerson(t.Context(), doc); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"issued_keys", "issued_key_grants", "issued_key_stops", "issued_key_uses"} {
		if n := w.count(t, `SELECT count(*) FROM `+table); n != 0 {
			t.Errorf("%s kept %d rows of a person deleted", table, n)
		}
	}
	if n := w.count(t, `SELECT count(*) FROM api_keys WHERE issuer <> ''`); n != 0 {
		t.Errorf("%d issued keys stayed", n)
	}
	if w.stub(t, "tok-live") != "revoked" || w.stub(t, "tok-ended") != "revoked" {
		t.Errorf("stubs %q %q", w.stub(t, "tok-live"), w.stub(t, "tok-ended"))
	}
}

func TestAnAccountDeletedWithALiveIssuedKeyLeavesARevokedStub(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	before := time.Now().Unix()
	doc := document(anaEmail, 2)
	doc.Status, doc.Workspaces = service.DirectoryClosed, nil
	if _, err := w.svc.ForgetExternalPerson(t.Context(), doc); err != nil {
		t.Fatal(err)
	}
	var how string
	var ended, forget int64
	if err := w.db.Reader().QueryRowContext(t.Context(), `SELECT how, ended_at, forget_at FROM issued_key_ends
		WHERE prefix = ?`, w.cred(t, "tok-1").selector).Scan(&how, &ended, &forget); err != nil {
		t.Fatal(err)
	}
	if how != "revoked" || ended < before || ended > time.Now().Unix()+1 ||
		forget != w.cred(t, "tok-1").created.Add(365*day).Unix() {
		t.Errorf("the stub: %s %d %d", how, ended, forget)
	}
}

func TestTheSweepDeletesKeyByKey(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	expiring := w.record(t, "tok-expired", 1, w.given(w.box, "read", anaSub))
	expiring.ExpiresAt = w.clock.now().Add(time.Hour)
	w.keys(t, w.record(t, "tok-revoked", 1, w.given(w.box, "read", anaSub)), expiring,
		w.record(t, "tok-unlisted", 1, w.given(w.box, "read", anaSub)))
	w.revoke(t, "tok-revoked", 2, w.clock.now())
	w.clock.advance(2 * time.Hour)
	if _, err := w.svc.EndExpiredIssuedKeys(t.Context(), w.clock.now()); err != nil {
		t.Fatal(err)
	}
	w.reset(t, "generation-2")
	w.keys(t)
	w.clock.advance(31 * day)
	w.exec(t, `CREATE TRIGGER test_keep BEFORE DELETE ON api_keys WHEN OLD.prefix = '`+
		w.cred(t, "tok-expired").selector+`' BEGIN SELECT RAISE(ABORT, 'test'); END`)
	got := w.sweep(t)
	if got.Keys != 2 || got.Failed != 1 {
		t.Fatalf("swept %+v", got)
	}
	if w.stub(t, "tok-revoked") != "revoked" || w.stub(t, "tok-unlisted") != "revoked" || !w.stored(t, "tok-expired") {
		t.Errorf("stubs %q %q, the failing one stored %v", w.stub(t, "tok-revoked"), w.stub(t, "tok-unlisted"),
			w.stored(t, "tok-expired"))
	}
	if !strings.Contains(w.logs.String(), "past its retention failed") {
		t.Error("the failure was not logged")
	}
	w.exec(t, `DROP TRIGGER test_keep`)
	if got := w.sweep(t); got.Keys != 1 || w.stub(t, "tok-expired") != "expired" {
		t.Errorf("the next sweep: %+v, stub %q", got, w.stub(t, "tok-expired"))
	}
}

func TestAKeysRefusalsOwedGoWithItsEnd(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub), w.given(w.other, "read", "sub-nobody")))
	never := w.record(t, "tok-2", 1, w.given(w.box, "read", anaSub))
	never.WorkspaceID = "personal-sub-nobody"
	w.keys(t, never)
	if len(w.refusals(t)) != 2 {
		t.Fatalf("owed %q", w.refusals(t))
	}
	w.revoke(t, "tok-1", 2, w.clock.now())
	// A key never stored that arrives ended: what was owed of it goes too.
	ended := w.record(t, "tok-2", 2)
	ended.WorkspaceID, ended.Revoked = "personal-sub-nobody", true
	w.keys(t, ended)
	if got := w.refusals(t); len(got) != 0 {
		t.Errorf("owed after the keys ended: %q", got)
	}
}

func TestARefusalOwedBeforeAKeyWasStoredGoesWithItsEnd(t *testing.T) {
	// v1 refused before the key could be stored (its creator not the
	// workspace's person), v2 stores it.
	owedThenStored := func(t *testing.T) *keysWorld {
		t.Helper()
		w := newKeysWorld(t, fixtureOptions{})
		first := w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub))
		first.Creator = "sub-nobody"
		w.keys(t, first)
		w.keys(t, w.record(t, "tok-1", 2, w.given(w.box, "read", anaSub)))
		if got := w.refusals(t); !slices.Equal(got, []string{"tok-1 v1 " + w.box + " read not_offered"}) {
			t.Fatalf("owed %q", got)
		}
		return w
	}
	forgetAna := func(t *testing.T, w *keysWorld) {
		t.Helper()
		doc := document(anaEmail, 2)
		doc.Status, doc.Workspaces = service.DirectoryClosed, nil
		if _, err := w.svc.ForgetExternalPerson(t.Context(), doc); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("the key's revocation", func(t *testing.T) {
		w := owedThenStored(t)
		w.revoke(t, "tok-1", 3, w.clock.now())
		if got := w.refusals(t); len(got) != 0 {
			t.Errorf("owed after the key ended: %q", got)
		}
	})
	t.Run("its creator's account deleted", func(t *testing.T) {
		w := owedThenStored(t)
		forgetAna(t, w)
		if got := w.refusals(t); len(got) != 0 {
			t.Errorf("owed after the account went: %q", got)
		}
	})
	t.Run("a forgotten key's revocation", func(t *testing.T) {
		w := newKeysWorld(t, fixtureOptions{})
		w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
		forgetAna(t, w)
		// The issuer still lists it, live: refused, as nothing of it is
		// held here; then it revokes it.
		w.keys(t, w.record(t, "tok-1", 2, w.given(w.box, "read", anaSub)))
		if got := w.refusals(t); !slices.Equal(got, []string{"tok-1 v2 " + w.box + " read mailbox_unknown"}) {
			t.Fatalf("owed %q", got)
		}
		w.revoke(t, "tok-1", 3, w.clock.now())
		if got := w.refusals(t); len(got) != 0 {
			t.Errorf("owed after the key ended: %q", got)
		}
	})
}

func TestAnIssuedKeyEndsAtTheEarliestOfWhatEndsIt(t *testing.T) {
	ended := func(t *testing.T, w *keysWorld, id string) (int64, string) {
		t.Helper()
		var at int64
		var kind string
		if err := w.db.Reader().QueryRowContext(t.Context(), `SELECT ended_at, end_kind FROM issued_keys WHERE prefix = ?`,
			w.cred(t, id).selector).Scan(&at, &kind); err != nil {
			t.Fatal(err)
		}
		return at, kind
	}
	t.Run("an expiry recorded, then an earlier revocation", func(t *testing.T) {
		w := newKeysWorld(t, fixtureOptions{})
		t0 := w.clock.now()
		k := w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub))
		k.CreatedAt, k.ExpiresAt = t0.Add(-25*day), t0.Add(time.Hour)
		w.keys(t, k)
		w.clock.advance(2 * time.Hour)
		if _, err := w.svc.EndExpiredIssuedKeys(t.Context(), w.clock.now()); err != nil {
			t.Fatal(err)
		}
		// Heard late (a feed behind, or replayed after a restore): revoked
		// 20 days ago.
		k.Version, k.Grants, k.Revoked, k.RevokedAt = 2, nil, true, t0.Add(-20*day)
		w.keys(t, k)
		if at, kind := ended(t, w, "tok-1"); at != k.RevokedAt.Unix() || kind != "revoked" {
			t.Errorf("ended %d %s, want its revocation %d", at, kind, k.RevokedAt.Unix())
		}
		w.clock.advance(k.RevokedAt.Add(30*day + time.Hour).Sub(w.clock.now()))
		if got := w.sweep(t); got.Keys != 1 || w.stub(t, "tok-1") != "revoked" {
			t.Errorf("30 days after its revocation: %+v, stub %q", got, w.stub(t, "tok-1"))
		}
	})
	t.Run("the belt's revocation, then the issuer's earlier one", func(t *testing.T) {
		w := newKeysWorld(t, fixtureOptions{})
		t0 := w.clock.now()
		k := w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub))
		k.CreatedAt = t0.Add(-25 * day)
		w.keys(t, k)
		doc := document(anaEmail, 2)
		doc.CredentialsRevokedAt = t0.Add(-time.Minute)
		w.apply(t, doc)
		if at, _ := ended(t, w, "tok-1"); at != t0.Unix() {
			t.Fatalf("the belt ended it at %d", at)
		}
		k.Version, k.Grants, k.Revoked, k.RevokedAt = 2, nil, true, t0.Add(-20*day)
		w.keys(t, k)
		if at, kind := ended(t, w, "tok-1"); at != k.RevokedAt.Unix() || kind != "revoked" {
			t.Errorf("ended %d %s, want its revocation %d", at, kind, k.RevokedAt.Unix())
		}
		w.clock.advance(k.RevokedAt.Add(30*day + time.Hour).Sub(w.clock.now()))
		if got := w.sweep(t); got.Keys != 1 {
			t.Errorf("30 days after its revocation: %+v", got)
		}
	})
	t.Run("an expiry not yet recorded, then a later revocation", func(t *testing.T) {
		w := newKeysWorld(t, fixtureOptions{})
		t0 := w.clock.now()
		k := w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub))
		k.ExpiresAt = t0.Add(time.Hour)
		w.keys(t, k)
		w.clock.advance(3 * time.Hour)
		k.Version, k.Grants, k.Revoked, k.RevokedAt = 2, nil, true, t0.Add(2*time.Hour)
		w.keys(t, k)
		if at, kind := ended(t, w, "tok-1"); at != k.ExpiresAt.Unix() || kind != "revoked" {
			t.Errorf("ended %d %s, want its expiry %d", at, kind, k.ExpiresAt.Unix())
		}
	})
	t.Run("a key never held, revoked after its expiry", func(t *testing.T) {
		w := newKeysWorld(t, fixtureOptions{})
		t0 := w.clock.now()
		k := w.record(t, "tok-1", 1)
		k.ExpiresAt, k.Revoked, k.RevokedAt = t0.Add(-2*time.Hour), true, t0.Add(-time.Hour)
		w.keys(t, k)
		var at int64
		if err := w.db.Reader().QueryRowContext(t.Context(), `SELECT ended_at FROM issued_key_ends WHERE prefix = ?`,
			w.cred(t, "tok-1").selector).Scan(&at); err != nil || at != k.ExpiresAt.Unix() {
			t.Errorf("its stub ended %d, want its expiry %d: %v", at, k.ExpiresAt.Unix(), err)
		}
	})
}

func TestABadRecordsRevocationOfAKeyNeverHeldLeavesItsStub(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	bad := w.record(t, "tok-1", 0)
	bad.Partial, bad.Revoked = true, true
	w.keys(t, bad)
	var ended, forget int64
	if err := w.db.Reader().QueryRowContext(t.Context(), `SELECT ended_at, forget_at FROM issued_key_ends WHERE prefix = ?`,
		w.cred(t, "tok-1").selector).Scan(&ended, &forget); err != nil {
		t.Fatal(err)
	}
	if ended != w.clock.now().Unix() || forget != w.cred(t, "tok-1").created.Add(365*day).Unix() {
		t.Errorf("the stub: ended %d, forget %d", ended, forget)
	}
	// With no creation to count a year from, or one in the future: nothing.
	undated := w.record(t, "tok-2", 0)
	undated.Partial, undated.Revoked, undated.CreatedAt = true, true, time.Time{}
	future := w.record(t, "tok-3", 0)
	future.Partial, future.Revoked, future.CreatedAt = true, true, w.clock.now().Add(day)
	w.keys(t, undated, future)
	if w.stub(t, "tok-2") != "" || w.stub(t, "tok-3") != "" {
		t.Errorf("stubs %q %q", w.stub(t, "tok-2"), w.stub(t, "tok-3"))
	}
}

func TestWhatIsLeftOfAnIssuedKeyGoesAYearAfterItsCreation(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	w.revoke(t, "tok-1", 2, w.clock.now())
	w.clock.advance(31 * day)
	w.sweep(t)
	if w.stub(t, "tok-1") != "revoked" {
		t.Fatal("no stub")
	}
	created := w.cred(t, "tok-1").created
	w.clock.advance(created.Add(365*day).Sub(w.clock.now()) - time.Second)
	if w.sweep(t); w.stub(t, "tok-1") == "" {
		t.Fatal("the stub went before a year")
	}
	w.clock.advance(time.Second)
	if got := w.sweep(t); got.Ends != 1 || w.stub(t, "tok-1") != "" {
		t.Errorf("after a year: %+v", got)
	}
}

func TestWhatIsOwedToTheIssuerGoesAfterThirtyDays(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", "sub-nobody")))
	w.clock.advance(30*day - time.Second)
	if w.sweep(t); len(w.refusals(t)) != 1 {
		t.Fatal("owed went before its 30 days")
	}
	w.clock.advance(time.Second)
	if got := w.sweep(t); got.Owed != 1 || len(w.refusals(t)) != 0 {
		t.Errorf("after 30 days: %+v, owed %q", got, w.refusals(t))
	}
}

func TestAnIssuedKeysUsesAreKeptAsAbsoluteCountsPerDay(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	today := w.clock.now().Format(time.DateOnly)
	yesterday := w.clock.now().Add(-day).Format(time.DateOnly)
	record := func(u service.IssuedKeyUses) {
		t.Helper()
		if err := w.svc.RecordIssuedKeyUses(t.Context(), dirIssuer, u); err != nil {
			t.Fatal(err)
		}
	}
	record(service.IssuedKeyUses{
		Used: []service.IssuedKeyUsed{{ExternalID: "tok-1", Day: yesterday}, {ExternalID: "tok-unknown", Day: today}},
		Refused: []service.IssuedKeyRefusals{
			{ExternalID: "tok-1", Day: today, AccountID: w.box, Use: "act", Refused: service.UseCreatorConsent, Count: 2},
			{ExternalID: "tok-1", Day: today, Refused: service.UseHeld, Count: 1},
		},
	})
	record(service.IssuedKeyUses{
		Used:    []service.IssuedKeyUsed{{ExternalID: "tok-1", Day: today}},
		Refused: []service.IssuedKeyRefusals{{ExternalID: "tok-1", Day: today, AccountID: w.box, Use: "act", Refused: service.UseCreatorConsent, Count: 3}},
	})
	got, err := w.svc.IssuedKeyUsage(t.Context(), dirIssuer, w.clock.now().Add(-30*day))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Used, []service.IssuedKeyUsed{{ExternalID: "tok-1", Day: today}}) {
		t.Errorf("used %+v", got.Used)
	}
	want := []service.IssuedKeyRefusals{
		{ExternalID: "tok-1", Day: today, Refused: service.UseHeld, Count: 1},
		{ExternalID: "tok-1", Day: today, AccountID: w.box, Use: "act", Refused: service.UseCreatorConsent, Count: 5},
	}
	if !slices.Equal(got.Refused, want) {
		t.Errorf("refused %+v, want %+v", got.Refused, want)
	}
	// Never a time of use: only the day.
	var used string
	if err := w.db.Reader().QueryRowContext(t.Context(), `SELECT used_day FROM issued_keys WHERE external_id = 'tok-1'`).
		Scan(&used); err != nil || used != today {
		t.Errorf("used_day %q: %v", used, err)
	}
}

func TestUseCountsOlderThanYesterdayGoOnceReported(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	days := map[string]time.Time{"today": w.clock.now(), "yesterday": w.clock.now().Add(-day),
		"three days ago": w.clock.now().Add(-3 * day), "31 days ago": w.clock.now().Add(-31 * day)}
	var uses service.IssuedKeyUses
	for _, at := range days {
		uses.Refused = append(uses.Refused, service.IssuedKeyRefusals{ExternalID: "tok-1", Day: at.Format(time.DateOnly),
			AccountID: w.box, Use: "read", Refused: service.UseNoFlag, Count: 1})
	}
	if err := w.svc.RecordIssuedKeyUses(t.Context(), dirIssuer, uses); err != nil {
		t.Fatal(err)
	}
	have := func() []string {
		got, err := w.svc.IssuedKeyUsage(t.Context(), dirIssuer, w.clock.now().Add(-60*day))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for name, at := range days {
			for _, r := range got.Refused {
				if r.Day == at.Format(time.DateOnly) {
					out = append(out, name)
				}
			}
		}
		slices.Sort(out)
		return out
	}
	w.sweep(t)
	if got := have(); !slices.Equal(got, []string{"three days ago", "today", "yesterday"}) {
		t.Errorf("before any report: %q", got)
	}
	if err := w.svc.IssuedKeyUsesReported(t.Context(), dirIssuer, w.clock.now().Format(time.DateOnly)); err != nil {
		t.Fatal(err)
	}
	w.sweep(t)
	if got := have(); !slices.Equal(got, []string{"today", "yesterday"}) {
		t.Errorf("once reported: %q", got)
	}
}

func TestTheSnapshotListsTheLiveIssuedKeysOnEachMailbox(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.keys(t,
		w.record(t, "tok-both", 1, w.given(w.box, "read", anaSub), w.given(w.box, "act", anaSub)),
		w.record(t, "tok-other", 1, w.given(w.other, "read", anaSub)),
		w.record(t, "tok-held", 1, w.given(w.box, "read", anaSub)),
		w.record(t, "tok-revoked", 1, w.given(w.other, "read", anaSub)))
	w.revoke(t, "tok-revoked", 2, w.clock.now())
	if err := w.svc.HoldIssuedKey(t.Context(), admin(), w.cred(t, "tok-held").selector); err != nil {
		t.Fatal(err)
	}
	snap, err := w.svc.AccessSnapshot(t.Context(), w.ws)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]service.SnapshotKey{}
	for _, mb := range snap.Mailboxes {
		if mb.Keys == nil {
			t.Errorf("%s: keys absent rather than empty", mb.ID)
		}
		got[mb.ID] = mb.Keys
		for _, p := range mb.People {
			if strings.HasPrefix(p.UserID, "tok") {
				t.Errorf("a key among the people: %+v", p)
			}
		}
	}
	want := map[string][]service.SnapshotKey{
		w.box: {{ExternalID: "tok-both", Flags: workspace.Flags{Read: true, Act: true}, Reads: true},
			{ExternalID: "tok-held", Flags: workspace.Flags{Read: true}, Reads: true}},
		w.other: {{ExternalID: "tok-other", Flags: workspace.Flags{Read: true}, Reads: true}},
	}
	for id, keys := range want {
		if !slices.Equal(got[id], keys) {
			t.Errorf("%s: %+v, want %+v", id, got[id], keys)
		}
	}
}

func TestAnExpiredIssuedKeyMarksItsWorkspaceAtItsExpiry(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	soon := w.record(t, "tok-soon", 1, w.given(w.box, "read", anaSub))
	soon.ExpiresAt = w.clock.now().Add(time.Hour)
	later := w.record(t, "tok-later", 1, w.given(w.box, "read", anaSub))
	later.ExpiresAt = w.clock.now().Add(5 * time.Hour)
	w.keys(t, soon, later)
	next, err := w.svc.EndExpiredIssuedKeys(t.Context(), w.clock.now())
	if err != nil || !next.Equal(soon.ExpiresAt) {
		t.Fatalf("the next expiry: %v %v", next, err)
	}
	w.exec(t, `DELETE FROM workspace_changes`)
	w.clock.advance(2 * time.Hour)
	next, err = w.svc.EndExpiredIssuedKeys(t.Context(), w.clock.now())
	if err != nil || !next.Equal(later.ExpiresAt) {
		t.Fatalf("the next expiry: %v %v", next, err)
	}
	if w.count(t, `SELECT count(*) FROM workspace_changes WHERE workspace_id = ?`, w.ws) != 1 {
		t.Error("the workspace was not marked at the expiry")
	}
	var ended int64
	if err := w.db.Reader().QueryRowContext(t.Context(), `SELECT ended_at FROM issued_keys WHERE external_id = 'tok-soon'`).
		Scan(&ended); err != nil || ended != soon.ExpiresAt.Unix() {
		t.Errorf("ended at %d, want its expiry: %v", ended, err)
	}
}

func TestAnIssuedKeyEndedAtItsExpiryChecksOpenRequestsAgain(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	expiring := w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub))
	expiring.ExpiresAt = w.clock.now().Add(time.Hour)
	w.keys(t, expiring)
	// Nothing ended: nothing to check.
	quiet := w.svc.AccessChanged()
	if _, err := w.svc.EndExpiredIssuedKeys(t.Context(), w.clock.now()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-quiet:
		t.Error("ending no key woke what holds a request open")
	default:
	}
	w.clock.advance(2 * time.Hour)
	if _, err := w.svc.EndExpiredIssuedKeys(t.Context(), w.clock.now()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-quiet:
	default:
		t.Error("a key that ended at its expiry left open streams and downloads unchecked")
	}
}

func TestAnIssuedKeysChangeMarksItsWorkspaceFromItsRecord(t *testing.T) {
	w := newKeysWorld(t, fixtureOptions{})
	w.exec(t, `DELETE FROM workspace_changes`)
	w.keys(t, w.record(t, "tok-1", 1, w.given(w.box, "read", anaSub)))
	if w.count(t, `SELECT count(*) FROM workspace_changes WHERE workspace_id = ?`, w.ws) != 1 {
		t.Fatal("a key stored did not mark its workspace")
	}
	w.exec(t, `DELETE FROM workspace_changes`)
	w.revoke(t, "tok-1", 2, w.clock.now())
	if w.count(t, `SELECT count(*) FROM workspace_changes WHERE workspace_id = ?`, w.ws) != 1 {
		t.Error("a key revoked did not mark its workspace")
	}
}
