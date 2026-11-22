package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// Migration 0029: the requests an extension carries for a person, each run
// once and recorded by the write that decides it, and when a mailbox access
// row last gained a flag (docs/workspaces.md, "Acts an extension runs for a
// person").

func TestMigration0029ChangesNothingElse(t *testing.T) {
	f := newSchemaAt(t, 28)
	seedJournalAt24(t, f)
	grants := queryInt(t, f.s, `SELECT count(*) FROM mailbox_access`)
	if grants == 0 {
		t.Fatal("the seed holds no access row; the test would prove nothing of raised_at")
	}
	marks := queryStrings(t, f.s, `SELECT workspace_id || ' ' || seq FROM workspace_changes ORDER BY seq`)
	people := queryStrings(t, f.s, `SELECT user_id || ' ' || seq FROM person_changes ORDER BY seq`)
	before, schema, fts := dumpTables(t, f.s), schemaSQL(t, f.s), ftsAnswers(t, f.s)
	migrateTo(t, f.s, 29)

	// Every row was last given a flag before any request could be asked.
	tried := compareMigrated(t, before, dumpTables(t, f.s), map[string]map[string]addedColumn{
		"mailbox_access": {"raised_at": sameValue("int64:0")},
	}, []string{"extension_requests"})
	if tried["mailbox_access"] != int(grants) {
		t.Errorf("the seed tried the migration on %d access rows of %d", tried["mailbox_access"], grants)
	}
	if got := queryStrings(t, f.s, `SELECT workspace_id || ' ' || seq FROM workspace_changes ORDER BY seq`); !slices.Equal(got, marks) {
		t.Errorf("the workspace marks are %v, and were %v", got, marks)
	}
	if got := queryStrings(t, f.s, `SELECT user_id || ' ' || seq FROM person_changes ORDER BY seq`); !slices.Equal(got, people) {
		t.Errorf("the person marks are %v, and were %v", got, people)
	}
	added, changed, dropped := schemaChanges(schema, schemaSQL(t, f.s))
	// WITHOUT ROWID: its primary key is the table itself, with no index of
	// its own.
	if want := []string{"index extension_requests_keep", "table extension_requests"}; !slices.Equal(added, want) {
		t.Errorf("0029 added\n%v\nwant\n%v", added, want)
	}
	if want := []string{"table mailbox_access"}; !slices.Equal(changed, want) || len(dropped) != 0 {
		t.Errorf("0029 changed %v and dropped %v", changed, dropped)
	}
	if got := ftsAnswers(t, f.s); !mapsEqual(got, fts) {
		t.Errorf("the full-text index answers %v, and answered %v", got, fts)
	}
	if got := queryStrings(t, f.s, `PRAGMA foreign_key_check`); len(got) != 0 {
		t.Errorf("foreign_key_check: %v", got)
	}
	if got := queryStrings(t, f.s, `PRAGMA integrity_check`); !slices.Equal(got, []string{"ok"}) {
		t.Errorf("integrity_check: %v", got)
	}
}

func TestTheSchemaKeepsARequestsRecordToItsShape(t *testing.T) {
	f := newSchemaLatest(t)
	insert := `INSERT INTO extension_requests(extension, id, user_id, act, outcome, done_at, keep_until)
		VALUES (?, ?, 'usr_0000000000000001', ?, ?, ?, ?)`
	f.accepted("a record", f.exec(insert, "ext", "r1", "pause_sync", `{"changed":true}`, 1, 2))
	f.refused("the same request twice", "UNIQUE constraint failed",
		f.exec(insert, "ext", "r1", "pause_sync", `{"changed":true}`, 1, 2))
	f.accepted("another extension's request of the same id", f.exec(insert, "other", "r1", "pause_sync", `{}`, 1, 2))
	for what, args := range map[string][]any{
		"no extension":          {"", "r2", "pause_sync", `{}`, 1, 2},
		"an extension too long": {strings.Repeat("e", 65), "r2", "pause_sync", `{}`, 1, 2},
		"no id":                 {"ext", "", "pause_sync", `{}`, 1, 2},
		"an id too long":        {"ext", strings.Repeat("i", 65), "pause_sync", `{}`, 1, 2},
		"no act":                {"ext", "r2", "", `{}`, 1, 2},
		"an outcome not JSON":   {"ext", "r2", "pause_sync", `changed`, 1, 2},
		"an outcome no object":  {"ext", "r2", "pause_sync", `[true]`, 1, 2},
		"no moment":             {"ext", "r2", "pause_sync", `{}`, 0, 2},
		"no end":                {"ext", "r2", "pause_sync", `{}`, 1, 0},
	} {
		f.refused(what, "CHECK constraint failed", f.exec(insert, args...))
	}
	f.refused("a negative raise", "CHECK constraint failed",
		f.exec(`UPDATE mailbox_access SET raised_at = -1 WHERE rowid = (SELECT min(rowid) FROM mailbox_access)`))
}

// requestFor is a request of usr_1's, kept a day.
func requestFor(id, act, workspaceID string) *Request {
	return &Request{Extension: "ext", ID: id, UserID: "usr_0000000000000001", Act: act, WorkspaceID: workspaceID,
		FirstIssued: time.Unix(1_000, 0), KeepUntil: time.Now().Add(24 * time.Hour)}
}

func TestARequestRunsOnceAndItsRecordAnswersWhatItDid(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	runs := 0
	write := func(req *Request) error {
		return f.s.WriteRequest(ctx, req, "", func(*sql.Tx) error {
			runs++
			return nil
		}, func() RequestOutcome { return RequestOutcome{Changed: true, SessionsEnded: 2} })
	}
	if err := write(requestFor("r1", "sign_out", "")); err != nil {
		t.Fatal(err)
	}
	err := write(requestFor("r1", "sign_out", ""))
	var done *RequestDone
	if !errors.As(err, &done) || !errors.Is(err, ErrRequestDone) || done.Outcome != (RequestOutcome{Changed: true, SessionsEnded: 2}) {
		t.Fatalf("the request again: %v (%+v)", err, done)
	}
	if runs != 1 {
		t.Errorf("the request ran %d times", runs)
	}
	out, ran, err := f.s.RequestOutcomeOf(ctx, "ext", "r1")
	if err != nil || !ran || out != (RequestOutcome{Changed: true, SessionsEnded: 2}) {
		t.Errorf("its record: %+v, %v, %v", out, ran, err)
	}
	if _, ran, err := f.s.RequestOutcomeOf(ctx, "ext", "r2"); err != nil || ran {
		t.Errorf("a request never run: ran %v, %v", ran, err)
	}

	// A refusal in the write rolls its record back with it.
	refused := errors.New("refused")
	err = f.s.WriteRequest(ctx, requestFor("r3", "pause_sync", ""), "", func(*sql.Tx) error { return refused },
		func() RequestOutcome { return RequestOutcome{Changed: true} })
	if !errors.Is(err, refused) {
		t.Fatalf("a refused request: %v", err)
	}
	if _, ran, _ := f.s.RequestOutcomeOf(ctx, "ext", "r3"); ran {
		t.Error("a refused request was recorded")
	}

	// A request whose effect held already is recorded as changing nothing,
	// while it still holds; once it no longer does, nothing is written.
	if done, err := f.s.RecordNoOp(ctx, requestFor("r4", "pause_sync", ""), "", func(*sql.Tx) (bool, error) { return false, nil }); err != nil || done {
		t.Fatalf("a no-op that no longer holds: %v, %v", done, err)
	}
	if _, ran, _ := f.s.RequestOutcomeOf(ctx, "ext", "r4"); ran {
		t.Error("a no-op that no longer held was recorded")
	}
	if done, err := f.s.RecordNoOp(ctx, requestFor("r4", "pause_sync", ""), "", func(*sql.Tx) (bool, error) { return true, nil }); err != nil || !done {
		t.Fatalf("a no-op: %v, %v", done, err)
	}
	if out, ran, _ := f.s.RequestOutcomeOf(ctx, "ext", "r4"); !ran || out.Changed {
		t.Errorf("a no-op's record: %+v, ran %v", out, ran)
	}
}

func TestAPersonWhoMayNoLongerActIsRefusedInTheRequestsWrite(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	const person = "usr_0000000000000001"
	try := func(id string, own bool) error {
		req := requestFor(id, "withdraw_consent", "")
		req.OwnNarrowing = own
		return f.s.WriteRequest(ctx, req, "", func(*sql.Tx) error { return nil },
			func() RequestOutcome { return RequestOutcome{Changed: true} })
	}
	f.accepted("suspended", f.exec(`UPDATE users SET status = 'disabled', suspended_at = 900 WHERE id = ?`, person))
	if err := try("s1", false); !errors.Is(err, ErrActorInactive) {
		t.Errorf("a suspended person's act: %v, want ErrActorInactive", err)
	}
	if err := try("s2", true); err != nil {
		t.Errorf("a suspended person's own narrowing: %v", err)
	}
	f.accepted("disabled here", f.exec(`UPDATE users SET status = 'disabled', suspended_at = 0 WHERE id = ?`, person))
	for _, own := range []bool{false, true} {
		if err := try("d1", own); !errors.Is(err, ErrActorInactive) {
			t.Errorf("a person disabled here, own narrowing %v: %v, want ErrActorInactive", own, err)
		}
	}
	req := requestFor("g1", "pause_sync", "")
	req.UserID = "usr_nobody"
	if err := f.s.WriteRequest(ctx, req, "", func(*sql.Tx) error { return nil },
		func() RequestOutcome { return RequestOutcome{} }); !errors.Is(err, ErrActorInactive) {
		t.Errorf("a person nobody knows: %v, want ErrActorInactive", err)
	}
}

func TestARequestsMailboxOutsideItsWorkspaceIsRefusedInTheWrite(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	var box, home string
	if err := f.s.Reader().QueryRowContext(ctx, `SELECT id, workspace_id FROM accounts ORDER BY id LIMIT 1`).
		Scan(&box, &home); err != nil {
		t.Fatal(err)
	}
	write := func(id, workspaceID string) error {
		return f.s.WriteRequest(ctx, requestFor(id, "pause_sync", workspaceID), box, func(*sql.Tx) error { return nil },
			func() RequestOutcome { return RequestOutcome{Changed: true} })
	}
	if err := write("w1", "wsp_elsewhere"); !errors.Is(err, ErrNotInWorkspace) {
		t.Errorf("a mailbox of another workspace: %v, want ErrNotInWorkspace", err)
	}
	if err := write("w2", home); err != nil {
		t.Errorf("a mailbox of the request's workspace: %v", err)
	}
}

func TestRequestRecordsGoAtTheirKeepUntil(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	now := time.Now()
	for id, keep := range map[string]time.Time{"old": now.Add(-time.Second), "due": now, "kept": now.Add(time.Hour)} {
		f.accepted(id, f.exec(`INSERT INTO extension_requests(extension, id, user_id, act, outcome, done_at, keep_until)
			VALUES ('ext', ?, 'usr_0000000000000001', 'pause_sync', '{}', 1, ?)`, id, keep.Unix()))
	}
	n, err := f.s.SweepRequests(ctx, now)
	if err != nil || n != 2 {
		t.Fatalf("the sweep deleted %d records (%v), want the 2 whose time was over", n, err)
	}
	if got := queryStrings(t, f.s, `SELECT id FROM extension_requests`); !slices.Equal(got, []string{"kept"}) {
		t.Errorf("the records kept are %v", got)
	}
}

func TestARequestOutOfShapeIsRefused(t *testing.T) {
	now := time.Now()
	good := Request{Extension: "ext", ID: "0b5e7c1a-6f0e-4b8e-9a51-6b1d2e3f4a5b", UserID: "usr_1", Act: "pause_sync",
		KeepUntil: now.Add(30 * 24 * time.Hour)}
	if err := good.Check(now); err != nil {
		t.Fatalf("a request in shape: %v", err)
	}
	for what, change := range map[string]func(*Request){
		"no extension":             func(r *Request) { r.Extension = "" },
		"an extension too long":    func(r *Request) { r.Extension = strings.Repeat("e", 65) },
		"no id":                    func(r *Request) { r.ID = "" },
		"an id too long":           func(r *Request) { r.ID = strings.Repeat("i", 65) },
		"an id with a slash":       func(r *Request) { r.ID = "cmd/1" },
		"an id with a space":       func(r *Request) { r.ID = "cmd 1" },
		"no person":                func(r *Request) { r.UserID = "" },
		"no act":                   func(r *Request) { r.Act = "" },
		"a record already over":    func(r *Request) { r.KeepUntil = now },
		"a record kept past a day": func(r *Request) { r.KeepUntil = now.Add(MaxRequestKept + time.Second) },
	} {
		r := good
		change(&r)
		if err := r.Check(now); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: %v, want ErrBadRequest", what, err)
		}
	}
}

func TestAWideningIsStampedWithItsRequestsFirstMomentAndANarrowingComparesWithItsSecond(t *testing.T) {
	var none *Request
	if none.Stamp(500) != 500 || none.Since() != 0 || none.widenedSince(500) {
		t.Error("no request stamps now and compares with nothing")
	}
	r := &Request{FirstIssued: time.Unix(400, 900_000_000)}
	if got := r.Stamp(500); got != 400 {
		t.Errorf("a widening asked at 400.9 and run at 500 is stamped %d", got)
	}
	if got := r.Stamp(300); got != 300 {
		t.Errorf("a stamp is never after now: %d", got)
	}
	if !r.widenedSince(400) || !r.widenedSince(401) || r.widenedSince(399) {
		t.Error("a narrowing asked at 400.9 is refused for a widening at or after second 400, and only those")
	}
}

func TestTurningOffARemovedMailboxsIndexNeverReachesOneLiveAgain(t *testing.T) {
	// The seed: acc_t1 is a mailbox of the team wsp_t1, under its consent;
	// a personal mailbox is its person's.
	f := newSchemaLatest(t)
	ctx := context.Background()
	const owner = "usr_0000000000000001"
	personal := queryStrings(t, f.s, `SELECT id FROM accounts WHERE owner_user_id IS NOT NULL ORDER BY id LIMIT 1`)[0]
	indexed := func(id string) int64 {
		t.Helper()
		return queryInt(t, f.s, `SELECT count(*) FROM folders WHERE account_id = ?`, id)
	}
	for _, id := range []string{"acc_t1", personal} {
		f.accepted(id+"'s folder", f.exec(`INSERT INTO folders(account_id, name, display_name, role, role_source, uidvalidity)
			VALUES (?, 'Kept', 'Kept', '', '', 7)`, id))
	}
	keptTeam, keptOwn := indexed("acc_t1"), indexed(personal)
	consent := queryInt(t, f.s, `SELECT sync_enabled_at FROM accounts WHERE id = 'acc_t1'`)
	if consent == 0 {
		t.Fatal("the seed's team mailbox is not under its team's consent")
	}

	// Live, recovered since a request asked: nothing of it is touched, and
	// the request is done.
	switched, err := f.s.SyncOffRemovedFor(ctx, requestFor("t1", "removed_index_off", ""), "acc_t1", owner, nil)
	if err != nil || switched.Changed || len(switched.Told) != 0 {
		t.Fatalf("turning off a live team mailbox's kept index: %+v, %v", switched, err)
	}
	forgot, err := f.s.ForgetRemovedIndexFor(ctx, requestFor("p1", "removed_index_off", ""), personal, nil)
	if err != nil || forgot {
		t.Fatalf("deleting a live personal mailbox's kept index: %v, %v", forgot, err)
	}
	if queryInt(t, f.s, `SELECT sync_enabled_at FROM accounts WHERE id = 'acc_t1'`) != consent ||
		indexed("acc_t1") != keptTeam || indexed(personal) != keptOwn {
		t.Error("a live mailbox's consent or index was touched")
	}
	for _, id := range []string{"t1", "p1"} {
		if out, ran, _ := f.s.RequestOutcomeOf(ctx, "ext", id); !ran || out.Changed {
			t.Errorf("the request %s: %+v, ran %v; want done, nothing changed", id, out, ran)
		}
	}

	// Removed: its index goes, and a personal one's consent and removal
	// stay, with nothing journaled.
	f.remove("acc_t1", owner, 1000)
	f.remove(personal, owner, 1000)
	journal := queryInt(t, f.s, `SELECT count(*) FROM mailbox_events`)
	if switched, err := f.s.SyncOffRemovedFor(ctx, nil, "acc_t1", owner, nil); err != nil || !switched.Changed {
		t.Errorf("turning off a removed team mailbox's kept index: %+v, %v", switched, err)
	}
	if forgot, err := f.s.ForgetRemovedIndexFor(ctx, nil, personal, nil); err != nil || !forgot {
		t.Errorf("deleting a removed personal mailbox's kept index: %v, %v", forgot, err)
	}
	if indexed("acc_t1") != 0 || indexed(personal) != 0 {
		t.Error("a removed mailbox keeps its index")
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM accounts WHERE id = ? AND removed_at <> 0`, personal); n != 1 {
		t.Error("the personal mailbox is no longer removed")
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM mailbox_events`); n != journal+1 {
		t.Errorf("the journal grew by %d, want the team's turn-off alone", n-journal)
	}
	if _, err := f.s.ForgetRemovedIndexFor(ctx, nil, "acc_t1", nil); !errors.Is(err, ErrNoSuchMailbox) {
		t.Errorf("the personal write on a team mailbox: %v, want ErrNoSuchMailbox", err)
	}
}

func TestDismissingNoticesTakesOnlyThePersonsOwn(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	const ana, bea = "usr_0000000000000001", "usr_0000000000000002"
	var ids []string
	for range 2 {
		var id string
		if err := f.s.Write(ctx, func(tx *sql.Tx) error {
			var err error
			_, id, err = JournalMailboxTx(ctx, tx, MailboxEvent{Kind: MailboxSyncPaused, AccountID: "acc_t1", Actor: "cli", At: 900},
				[]string{ana, bea}, true)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	n, err := f.s.DismissNoticesFor(ctx, requestFor("d1", "dismiss_notices", ""), ana, append(ids, "mbe_gone", ids[0]))
	if err != nil || n != 2 {
		t.Fatalf("Ana dismissing her two notices: %d, %v", n, err)
	}
	if got := queryInt(t, f.s, `SELECT count(*) FROM notices WHERE user_id = ?`, bea); got != 2 {
		t.Errorf("Bea holds %d notices, want her two", got)
	}
	if out, ran, _ := f.s.RequestOutcomeOf(ctx, "ext", "d1"); !ran || out != (RequestOutcome{Changed: true, Dismissed: 2}) {
		t.Errorf("the dismissal's record: %+v, ran %v", out, ran)
	}
	if n, err := f.s.DismissNoticesFor(ctx, nil, ana, ids); err != nil || n != 0 {
		t.Errorf("dismissing them again: %d, %v", n, err)
	}
}
