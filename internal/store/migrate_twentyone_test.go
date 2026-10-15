package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
)

// Migration 0021: the journal rebuilt with strandings, their causes, the
// person each lost, a removal's end, and an index per workspace
// (docs/workspaces.md, "Removed mailboxes").

// addedColumn says what value a migration gives a column it adds to a row
// that was there, as dumpTable writes values.
type addedColumn func(row map[string]string) string

// sameValue is a column every row takes with the same value.
func sameValue(v string) addedColumn { return func(map[string]string) string { return v } }

// compareMigrated checks that every table of before is in after, row for row
// and value for value, gaining only the columns of added, each with the
// value it says; that after holds no table but before's and newTables, these
// empty; and that sqlite_sequence holds the same high-water marks, whatever
// the order of its rows. It returns how many rows of each table that gained
// columns it checked, for a test to say its seed tried the migration on them.
func compareMigrated(t *testing.T, before, after tableDump, added map[string]map[string]addedColumn, newTables []string) map[string]int {
	t.Helper()
	tried := map[string]int{}
	if len(after) != len(before)+len(newTables) {
		t.Errorf("the migration left %d tables, want the %d there were and %v", len(after), len(before), newTables)
	}
	for _, table := range newTables {
		if rows, ok := after[table]; !ok || len(rows) != 0 {
			t.Errorf("the migration made %s with %d rows (%t), want it empty", table, len(rows), ok)
		}
	}
	for table, rows := range before {
		got, ok := after[table]
		if !ok {
			t.Errorf("table %s is gone", table)
			continue
		}
		if len(got) != len(rows) {
			t.Errorf("%s held %d rows and holds %d", table, len(rows), len(got))
			continue
		}
		if table == "sqlite_sequence" {
			marks := func(rows []map[string]string) []string {
				var out []string
				for _, row := range rows {
					out = append(out, row["name"]+" "+row["seq"])
				}
				sort.Strings(out)
				return out
			}
			if !slices.Equal(marks(rows), marks(got)) {
				t.Errorf("the high-water marks were %v and are %v", marks(rows), marks(got))
			}
			continue
		}
		for i, row := range rows {
			for col, want := range row {
				if got[i][col] != want {
					t.Errorf("%s row %d column %s was %s and is %s", table, i, col, want, got[i][col])
				}
			}
			var gained []string
			for col := range got[i] {
				if _, ok := row[col]; !ok {
					gained = append(gained, col)
				}
			}
			sort.Strings(gained)
			var want []string
			for col := range added[table] {
				want = append(want, col)
			}
			sort.Strings(want)
			if !slices.Equal(gained, want) {
				t.Errorf("%s row %d gained the columns %v, want %v", table, i, gained, want)
				continue
			}
			for col, value := range added[table] {
				if got[i][col] != value(row) {
					t.Errorf("%s row %d took %s for %s, want %s", table, i, got[i][col], col, value(row))
				}
			}
			if len(want) > 0 {
				tried[table]++
			}
		}
	}
	return tried
}

// schemaChanges lists what a migration added to the schema, changed in it,
// and dropped from it, by sqlite_schema's text.
func schemaChanges(before, after map[string]string) (added, changed, dropped []string) {
	for name, text := range after {
		old, ok := before[name]
		switch {
		case !ok:
			added = append(added, name)
		case old != text:
			changed = append(changed, name)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			dropped = append(dropped, name)
		}
	}
	sort.Strings(added)
	sort.Strings(changed)
	sort.Strings(dropped)
	return added, changed, dropped
}

// seedJournal writes, at schema 20, a journal of every kind 0020 knew, with
// its readers and notices: Owner (usr_1) removed acc_t1, which Bea (usr_2)
// and Fay (usr_6) read and were told of; a key turned acc_t3's sync off,
// which Dan (usr_4) read and was told of; Owner recovered acc_t1, whose
// label, as 0020 kept it, holds its address's local part; the sweep
// deleted a mailbox for good; a removal whose remover was deleted since;
// and an entry swept by its retention, the last, so the journal's high-water
// mark is above its highest seq.
func seedJournal(t *testing.T, s *Store) {
	t.Helper()
	execAll(t, s,
		`INSERT INTO mailbox_events(seq, id, kind, workspace_id, account_id, label, hint, actor, created_at) VALUES
		 (1, 'mbe_0000000000000001', 'removed', 'wsp_t1', 'acc_t1', 'Support', 's…@example.org', 'usr_0000000000000001', 900),
		 (2, 'mbe_0000000000000002', 'sync_paused', 'wsp_t2', 'acc_t3', 'Operations', 'o…@team.example.org', 'key:abcd0001', 910),
		 (3, 'mbe_0000000000000003', 'recovered', 'wsp_t1', 'acc_t1', 'Support', 's…@example.org', 'usr_0000000000000001', 920),
		 (4, 'mbe_0000000000000004', 'purged', 'wsp_t1', 'acc_gone', '', '', 'sweep', 930),
		 (5, 'mbe_0000000000000005', 'removed', 'wsp_t1', 'acc_t2', '', 'b…@example.org', '', 940),
		 (6, 'mbe_0000000000000006', 'recovered', 'wsp_t1', 'acc_t2', '', '', 'cli', 950)`,
		`INSERT INTO mailbox_event_readers(event_seq, user_id) VALUES
		 (1, 'usr_0000000000000001'), (1, 'usr_0000000000000002'), (1, 'usr_0000000000000006'),
		 (2, 'usr_0000000000000004'), (3, 'usr_0000000000000002'), (5, 'usr_0000000000000006')`,
		`INSERT INTO notices(user_id, event_seq, created_at) VALUES
		 ('usr_0000000000000002', 1, 900), ('usr_0000000000000006', 1, 900), ('usr_0000000000000004', 2, 910),
		 ('usr_0000000000000006', 5, 940)`,
		`DELETE FROM mailbox_events WHERE seq = 6`,
	)
}

// thirtyDaysAfter is the end a removal recorded: its moment and 30 days.
func thirtyDaysAfter(row map[string]string) string {
	if row["kind"] != "string:removed" {
		return "int64:0"
	}
	var at int64
	if _, err := fmt.Sscanf(row["created_at"], "int64:%d", &at); err != nil {
		return "unreadable " + row["created_at"]
	}
	return fmt.Sprintf("int64:%d", at+thirtyDays)
}

var journalColumns = map[string]map[string]addedColumn{
	"mailbox_events": {"cause": sameValue("string:"), "lost_user": sameValue("string:"), "purge_after": thirtyDaysAfter},
}

func TestMigrationTwentyOneRebuildsTheJournalAndChangesNothingElse(t *testing.T) {
	s := newSchemaTwenty(t).s
	seedJournal(t, s)
	before, schema, fts := dumpTables(t, s), schemaSQL(t, s), ftsAnswers(t, s)
	if got := queryStrings(t, s, `SELECT seq FROM sqlite_sequence WHERE name = 'mailbox_events'`); !slices.Equal(got, []string{"6"}) {
		t.Fatalf("the seed's journal has the high-water mark %v, want 6", got)
	}
	migrateTo(t, s, 21)
	after := dumpTables(t, s)

	// The one value that changes: a label that holds its mailbox's local
	// part ("Support" for support@example.org) is not kept.
	for _, row := range before["mailbox_events"] {
		if row["account_id"] == "string:acc_t1" {
			row["label"] = "string:"
		}
	}
	tried := compareMigrated(t, before, after, journalColumns, nil)
	if tried["mailbox_events"] != 5 {
		t.Errorf("the migration was tried on %d entries of the journal, want the seed's 5", tried["mailbox_events"])
	}
	// The removals keep the end they recorded; a recovery, a deletion for
	// good and a sync turned off have none.
	if got := queryStrings(t, s, `SELECT seq || ' ' || kind || ' ' || purge_after FROM mailbox_events ORDER BY seq`); !slices.Equal(got, []string{
		"1 removed 2592900", "2 sync_paused 0", "3 recovered 0", "4 purged 0", "5 removed 2592940",
	}) {
		t.Errorf("the journal's ends: %v", got)
	}

	// The schema is as it was but for the journal's table, its new index,
	// and the trigger that writes it once; notices_to_readers, dropped to
	// let the rename through, is written again byte for byte.
	now := schemaSQL(t, s)
	added, changed, dropped := schemaChanges(schema, now)
	if !slices.Equal(added, []string{"index mailbox_events_workspace"}) || len(dropped) != 0 ||
		!slices.Equal(changed, []string{"table mailbox_events", "trigger mailbox_events_written_once"}) {
		t.Errorf("0021 added %v, changed %v and dropped %v", added, changed, dropped)
	}
	for _, clause := range []string{"NEW.cause IS NOT OLD.cause", "NEW.purge_after IS NOT OLD.purge_after",
		"(NEW.lost_user IS NOT OLD.lost_user AND NEW.lost_user <> '')", "(NEW.actor IS NOT OLD.actor AND NEW.actor <> '')"} {
		if !strings.Contains(now["trigger mailbox_events_written_once"], clause) {
			t.Errorf("the journal is written once without %q", clause)
		}
	}
	// Its readers and notices refer to the rebuilt journal, and go with an
	// entry deleted.
	for _, table := range []string{"mailbox_event_readers", "notices"} {
		if got := queryStrings(t, s, `SELECT "table" FROM pragma_foreign_key_list(?) WHERE "from" = 'event_seq'`, table); !slices.Equal(got, []string{"mailbox_events"}) {
			t.Errorf("%s refers to %v", table, got)
		}
	}
	execAll(t, s, `DELETE FROM mailbox_events WHERE seq = 1`)
	if n := queryInt(t, s, `SELECT (SELECT count(*) FROM mailbox_event_readers WHERE event_seq = 1)
		+ (SELECT count(*) FROM notices WHERE event_seq = 1)`); n != 0 {
		t.Errorf("an entry deleted left %d readers and notices", n)
	}
	if got := ftsAnswers(t, s); !mapsEqual(got, fts) {
		t.Errorf("the full-text index answers %v, and answered %v", got, fts)
	}
}

func TestMigrationTwentyOneKeepsNoLabelThatHoldsTheAddresssLocalPart(t *testing.T) {
	// The seed at schema 20: acc_t1 is support@example.org, acc_t2
	// billing@example.org, renamed al@example.org below, and acc_t3
	// ops@team.example.org. Each label is what 0020 kept, which left out
	// only a name holding an address.
	s := newSchemaTwenty(t).s
	execAll(t, s, `UPDATE accounts SET email = 'al@example.org' WHERE id = 'acc_t2'`)
	labels := []struct{ account, label, want string }{
		{"acc_t1", "Support", ""},
		{"acc_t1", "The SUPPORT desk", ""},
		{"acc_t1", "Helpdesk", "Helpdesk"},
		// SQLite folds the case of ASCII only: past it, nothing is kept.
		{"acc_t1", "Équipe", ""},
		{"acc_t3", "Operations", "Operations"},
		{"acc_t3", "OPS", ""},
		// A local part under three characters says nothing.
		{"acc_t2", "Al's box", "Al's box"},
		{"acc_t2", "Équipe d'Al", "Équipe d'Al"},
		// A mailbox deleted for good keeps no label.
		{"acc_gone", "Old", ""},
		{"acc_t1", "", ""},
	}
	for i, l := range labels {
		execAll(t, s, fmt.Sprintf(`INSERT INTO mailbox_events(id, kind, workspace_id, account_id, label, hint, actor,
			created_at) VALUES ('mbe_%016x', 'sync_paused', 'wsp_t1', '%s', '%s', '', 'cli', 900)`,
			i+1, l.account, strings.ReplaceAll(l.label, "'", "''")))
	}
	migrateTo(t, s, 21)
	got := queryStrings(t, s, `SELECT label FROM mailbox_events ORDER BY seq`)
	if len(got) != len(labels) {
		t.Fatalf("the journal holds %d entries, want %d", len(got), len(labels))
	}
	for i, l := range labels {
		if got[i] != l.want {
			t.Errorf("the label %q of %s became %q, want %q", l.label, l.account, got[i], l.want)
		}
		// What is kept is what the journal's writer would keep now.
		if email := map[string]string{"acc_t1": "support@example.org", "acc_t2": "al@example.org",
			"acc_t3": "ops@team.example.org"}[l.account]; email != "" && got[i] != "" && MailboxLabel(l.label, email) != got[i] {
			t.Errorf("the label %q of %s was kept, which MailboxLabel refuses", l.label, l.account)
		}
	}
}

func TestMigrationTwentyOneOnAJournalNeverWrittenChangesNothingElse(t *testing.T) {
	s := newSchemaTwenty(t).s
	mark := `SELECT count(*) FROM sqlite_sequence WHERE name IN ('mailbox_events', 'mailbox_events_new')`
	if n := queryInt(t, s, mark); n != 0 {
		t.Fatalf("a journal never written has %d high-water marks", n)
	}
	before := dumpTables(t, s)
	migrateTo(t, s, 21)
	compareMigrated(t, before, dumpTables(t, s), nil, nil)
	if n := queryInt(t, s, mark); n != 0 {
		t.Errorf("the rebuild left %d high-water marks for a journal never written", n)
	}
	if n := queryInt(t, s, `SELECT count(*) FROM sqlite_schema WHERE name LIKE 'mailbox_events_new%'`); n != 0 {
		t.Error("the rebuild left its new table behind")
	}
}

func TestTheRebuiltJournalNeverReusesASeq(t *testing.T) {
	// Entries the retention swept before the upgrade: the journal is empty,
	// and its high-water mark is the last seq it handed out.
	s := newSchemaTwenty(t).s
	seedJournal(t, s)
	execAll(t, s, `DELETE FROM mailbox_events`)
	migrateTo(t, s, latestVersion(t))
	f := seededSchema{t: t, s: s}
	id := f.journal(MailboxSyncPaused, "acc_t3", "usr_0000000000000004", 1000, false)
	if seq := queryInt(t, s, `SELECT seq FROM mailbox_events WHERE id = ?`, id); seq != 7 {
		t.Errorf("the first entry after the rebuild is %d, want 7, above every seq handed out", seq)
	}
}

func TestTheSchemaRecordsAStrandingWholeAndOnlyAsOne(t *testing.T) {
	// The seed: acc_t1 is a mailbox of the team wsp_t1, which Fay (usr_6)
	// reads.
	f := newSchemaLatest(t)
	const fay = "usr_0000000000000006"
	n := 0
	entry := func(kind, cause, lostUser string, purgeAfter int64) error {
		n++
		return f.exec(`INSERT INTO mailbox_events(id, kind, workspace_id, account_id, actor, created_at, cause, lost_user,
			purge_after) VALUES (?, ?, 'wsp_t1', 'acc_t1', 'directory', 900, ?, ?, ?)`,
			fmt.Sprintf("mbe_%016x", n), kind, cause, lostUser, purgeAfter)
	}
	// A stranding always says why, with one of its causes, and whom it lost.
	for _, cause := range []string{CauseMembershipEnded, CauseForgotten, CauseDisabled, CauseDeleted, CauseReset} {
		f.accepted("stranded, "+cause, entry("stranded", cause, fay, 0))
	}
	f.refused("stranded without a cause", "CHECK", entry("stranded", "", fay, 0))
	f.refused("stranded for a sync's cause", "CHECK", entry("stranded", CauseBoundStopped, fay, 0))
	f.refused("stranded for no cause it has", "CHECK", entry("stranded", "lost", fay, 0))
	f.refused("stranded with an end", "CHECK", entry("stranded", CauseForgotten, fay, 900+thirtyDays))
	// A sync turned off says why only when a consent bound to a person
	// stopped with them.
	f.accepted("a sync turned off", entry("sync_paused", "", "", 0))
	f.accepted("a sync stopped with its person", entry("sync_paused", CauseBoundStopped, "", 0))
	f.refused("a sync turned off for a stranding's cause", "CHECK", entry("sync_paused", CauseForgotten, "", 0))
	// Nothing but a stranding loses a person, and nothing but a removal has
	// an end, which it always has.
	for _, kind := range []string{"removed", "sync_paused", "recovered", "purged"} {
		end := map[bool]int64{true: 900 + thirtyDays}[kind == "removed"]
		f.refused(kind+" losing someone", "CHECK", entry(kind, "", fay, end))
		if kind != "sync_paused" {
			f.refused(kind+" for a cause", "CHECK", entry(kind, CauseDeleted, "", end))
		}
	}
	f.accepted("a removal with its end", entry("removed", "", "", 900+thirtyDays))
	f.refused("a removal without its end", "CHECK", entry("removed", "", "", 0))
	for _, kind := range []string{"sync_paused", "recovered", "purged"} {
		f.refused(kind+" with an end", "CHECK", entry(kind, "", "", 900+thirtyDays))
	}

	// The journal's own writer says whom a stranding lost, and tells nobody.
	ctx := context.Background()
	write := func(ev MailboxEvent, notify bool) error {
		return f.s.Write(ctx, func(tx *sql.Tx) error {
			_, _, err := JournalMailboxTx(ctx, tx, ev, []string{fay}, notify)
			return err
		})
	}
	if err := write(MailboxEvent{Kind: MailboxStranded, AccountID: "acc_t1", Actor: ActorDirectory, At: 900,
		Cause: CauseMembershipEnded}, false); !errors.Is(err, ErrNoLostPerson) {
		t.Errorf("a stranding naming nobody: %v, want ErrNoLostPerson", err)
	}
	if err := write(MailboxEvent{Kind: MailboxStranded, AccountID: "acc_t1", Actor: ActorDirectory, At: 900,
		Cause: CauseMembershipEnded, LostUser: fay}, true); !errors.Is(err, ErrNotNotifying) {
		t.Errorf("a stranding told its reader: %v, want ErrNotNotifying", err)
	}
	if err := write(MailboxEvent{Kind: MailboxStranded, AccountID: "acc_t1", Actor: ActorDirectory, At: 950,
		Cause: CauseMembershipEnded, LostUser: fay}, false); err != nil {
		t.Fatal(err)
	}
	if got := queryStrings(t, f.s, `SELECT e.kind || ' ' || e.cause || ' ' || e.lost_user || ' ' || e.actor || ' '
		|| e.label || ' ' || e.purge_after || ' ' || r.user_id FROM mailbox_events e
		JOIN mailbox_event_readers r ON r.event_seq = e.seq WHERE e.created_at = 950`); !slices.Equal(got, []string{
		"stranded membership_ended " + fay + " directory Name of acc_t1 0 " + fay,
	}) {
		t.Errorf("the stranding journaled: %v", got)
	}
}

func TestTheSchemaNeverGivesANoticeOfAStranding(t *testing.T) {
	// The seed: Bea (usr_2) and Fay (usr_6) read acc_t1.
	f := newSchemaLatest(t)
	const bea, fay = "usr_0000000000000002", "usr_0000000000000006"
	f.accepted("a stranding", f.exec(`INSERT INTO mailbox_events(id, kind, workspace_id, account_id, actor, created_at,
		cause, lost_user) VALUES ('mbe_0000000000000001', 'stranded', 'wsp_t1', 'acc_t1', 'usr_0000000000000001', 900,
		'disabled', ?)`, fay))
	seq := queryInt(t, f.s, `SELECT seq FROM mailbox_events WHERE id = 'mbe_0000000000000001'`)
	f.accepted("its readers", f.exec(`INSERT INTO mailbox_event_readers(event_seq, user_id) VALUES (?, ?), (?, ?)`,
		seq, fay, seq, bea))
	for _, user := range []string{fay, bea} {
		f.refused("a notice of it", "a notice goes to a reader of the mailbox, never to who acted",
			f.exec(`INSERT INTO notices(user_id, event_seq, created_at) VALUES (?, ?, 900)`, user, seq))
	}
}

func TestAStrandingIsWrittenOnceButItsLostPersonIsReleased(t *testing.T) {
	f := newSchemaLatest(t)
	const fay, bea = "usr_0000000000000006", "usr_0000000000000002"
	const once = "a mailbox event is written once"
	f.accepted("a stranding", f.exec(`INSERT INTO mailbox_events(id, kind, workspace_id, account_id, label, hint, actor,
		created_at, cause, lost_user) VALUES ('mbe_0000000000000001', 'stranded', 'wsp_t1', 'acc_t1', 'Support',
		's…@example.org', 'directory', 900, 'forgotten', ?)`, fay))
	f.accepted("a removal", f.exec(`INSERT INTO mailbox_events(id, kind, workspace_id, account_id, actor, created_at,
		purge_after) VALUES ('mbe_0000000000000002', 'removed', 'wsp_t1', 'acc_t2', ?, 900, 900 + ?)`, bea, thirtyDays))
	for what, set := range map[string]string{
		"another cause":        "cause = 'disabled'",
		"another lost person":  "lost_user = '" + bea + "'",
		"another kind":         "kind = 'removed', purge_after = 1, cause = ''",
		"another moment":       "created_at = 901",
		"another workspace":    "workspace_id = 'wsp_t2'",
		"another actor":        "actor = 'cli'",
		"an end it never had":  "purge_after = 1",
		"another label":        "label = 'Sales'",
		"another seq":          "seq = 99",
		"its cause taken away": "cause = ''",
	} {
		f.refused(what, once, f.exec(`UPDATE mailbox_events SET `+set+` WHERE id = 'mbe_0000000000000001'`))
	}
	f.refused("a removal's end moved", once, f.exec(`UPDATE mailbox_events SET purge_after = purge_after + 1
		WHERE id = 'mbe_0000000000000002'`))
	// Its lost person, its actor and its names are forgotten, and only to
	// nothing: never written again.
	f.accepted("its lost person and actor released", f.exec(`UPDATE mailbox_events SET lost_user = '', actor = ''
		WHERE id = 'mbe_0000000000000001'`))
	f.accepted("its names forgotten", f.exec(`UPDATE mailbox_events SET label = '', hint = ''
		WHERE id = 'mbe_0000000000000001'`))
	f.refused("its lost person written again", once, f.exec(`UPDATE mailbox_events SET lost_user = ?
		WHERE id = 'mbe_0000000000000001'`, fay))
	if got := queryStrings(t, f.s, `SELECT kind || '|' || cause || '|' || lost_user || '|' || actor || '|' || label
		FROM mailbox_events WHERE id = 'mbe_0000000000000001'`); !slices.Equal(got, []string{"stranded|forgotten|||"}) {
		t.Errorf("after the refused writes the stranding is %v", got)
	}
}

func TestTheJournalKeepsNoLabelThatHoldsTheAddresssLocalPart(t *testing.T) {
	for _, c := range []struct{ name, email, want string }{
		{"Support", "support@example.org", ""},
		{"The SUPPORT desk", "support@example.org", ""},
		{"Help", "support@example.org", "Help"},
		{"Support <support@example.org>", "billing@example.org", ""},
		{"me@home", "ana@example.org", ""},
		{"Al's box", "al@example.org", "Al's box"},
		{"Bobby", "bob@example.org", ""},
		{"Élodie", "élodie@exemple.fr", ""},
		{"ÉLODIE", "élodie@exemple.fr", ""},
		{"Sales", "odd@local@example.org", "Sales"},
		{"odd@local", "odd@local@example.org", ""},
		{"The odd@local box", "odd@local@example.org", ""},
		{"Nobody's", "nobody", ""},
		{"Inbox", "", "Inbox"},
		{"", "support@example.org", ""},
		// A character nobody sees, a decomposed accent or a fullwidth at sign
		// never hides the address.
		{"Queue john\u200b.smith", "john.smith@example.com", ""},
		{"jo\u2060hn.smith desk", "john.smith@example.com", ""},
		{"jo\u00adhn.smith", "john.smith@example.com", ""},
		{"Jose\u0301 inbox", "jos\u00e9@example.com", ""},
		{"Jos\u00e9 inbox", "jose\u0301@example.com", ""},
		{"john  \t smith", "john smith@example.com", ""},
		{"me\uff20home", "ana@example.org", ""},
		{"me\ufe6bhome", "ana@example.org", ""},
		{"Caf\u00e9 desk", "ana@example.org", "Caf\u00e9 desk"},
	} {
		if got := MailboxLabel(c.name, c.email); got != c.want {
			t.Errorf("MailboxLabel(%q, %q) = %q, want %q", c.name, c.email, got, c.want)
		}
	}

	// The seed: acc_t1 is support@example.org; Bea (usr_2) reads it.
	f := newSchemaLatest(t)
	f.accepted("named after its address", f.exec(`UPDATE accounts SET display_name = 'Support desk' WHERE id = 'acc_t1'`))
	f.journal(MailboxSyncPaused, "acc_t1", "usr_0000000000000002", 900, true)
	f.accepted("named otherwise", f.exec(`UPDATE accounts SET display_name = 'Helpdesk' WHERE id = 'acc_t1'`))
	f.journal(MailboxSyncPaused, "acc_t1", "usr_0000000000000002", 901, true)
	if got := queryStrings(t, f.s, `SELECT label || '|' || hint FROM mailbox_events ORDER BY seq`); !slices.Equal(got, []string{
		"|s…@example.org", "Helpdesk|s…@example.org",
	}) {
		t.Errorf("the journal names the mailbox %v", got)
	}
}

func TestAConsentStoppedWithItsPersonIsJournaledAsBoundStopped(t *testing.T) {
	// The seed: Bea (usr_2) and Fay (usr_6) read acc_t1, of the team
	// wsp_t1, whose consent is bound to Bea here, as migration 0011 left
	// it; Dan (usr_4) reads acc_t3, of the team wsp_t2, which he turns off.
	f := newSchemaLatest(t)
	ctx := context.Background()
	const bea, dan = "usr_0000000000000002", "usr_0000000000000004"
	f.accepted("bound to Bea", f.exec(`UPDATE accounts SET sync_enabled_via = 'migration', sync_enabled_by = ?
		WHERE id = 'acc_t1'`, bea))
	if _, err := f.s.WithdrawSyncConsent(ctx, bea); err != nil {
		t.Fatal(err)
	}
	f.accepted("acc_t3 on", f.exec(`UPDATE accounts SET sync_enabled_at = 800, sync_enabled_by = ? WHERE id = 'acc_t3'`, dan))
	if _, err := f.s.SetMailboxSync(ctx, "acc_t3", false, dan, "", nil); err != nil {
		t.Fatal(err)
	}
	if got := queryStrings(t, f.s, `SELECT account_id || ' ' || kind || ' ' || actor || ' ' || cause
		FROM mailbox_events ORDER BY seq`); !slices.Equal(got, []string{
		"acc_t1 sync_paused " + bea + " bound_stopped", "acc_t3 sync_paused " + dan + " ",
	}) {
		t.Errorf("the journal: %v", got)
	}
}

func TestAHeldMemberIsNeitherAReaderNorASuspendedReaderButAHeldOne(t *testing.T) {
	// A team from another source, ext_team, with a mailbox without a key
	// (acc_e1) and one with a key (acc_e2), each of which Bea (usr_2), Dan
	// (usr_4), Fay (usr_6) and Gil (usr_7) hold read on; Bea, Fay and Gil
	// hold a grant on acc_e2. Their memberships, but Dan's, are held; Fay
	// is suspended by an extension and Gil disabled here.
	f := newSchemaLatest(t)
	const bea, dan, fay, gil = "usr_0000000000000002", "usr_0000000000000004", "usr_0000000000000006", "usr_0000000000000007"
	seedExternalTeam(t, f, "ext_team", bea, dan, fay, gil)
	for _, account := range []string{"acc_e1", "acc_e2"} {
		externalMailbox(t, f, account, "ext_team")
		for _, user := range []string{bea, dan, fay, gil} {
			f.accepted("read for "+user, f.exec(`INSERT INTO mailbox_access(account_id, workspace_id, user_id, read,
				granted_by, created_at, updated_at) VALUES (?, 'ext_team', ?, 1, 'directory', 900, 900)`, account, user))
		}
	}
	keyMailbox(t, f, "acc_e2", "ext_team", bea, fay, gil)
	who := func(account string) []string {
		t.Helper()
		return queryStrings(t, f.s, `SELECT g.user_id || ' ' || (`+ReaderSQL("g")+`) || (`+SuspendedReaderSQL("g")+`)
			|| (`+HeldReaderSQL("g")+`) FROM mailbox_access g WHERE g.account_id = ? ORDER BY g.user_id`, account)
	}
	// Before the holds: everyone but Fay and Gil reads; Fay would once
	// resumed; Dan reads acc_e1 alone, having no grant on acc_e2.
	f.accepted("Fay suspended", f.exec(`UPDATE users SET status = 'disabled', suspended_at = 900 WHERE id = ?`, fay))
	f.accepted("Gil disabled", f.exec(`UPDATE users SET status = 'disabled' WHERE id = ?`, gil))
	if got := who("acc_e2"); !slices.Equal(got, []string{bea + " 100", dan + " 000", fay + " 010", gil + " 000"}) {
		t.Errorf("before the holds, acc_e2: %v", got)
	}
	for _, user := range []string{bea, fay, gil} {
		f.accepted("held", f.exec(`UPDATE workspace_members SET status = 'disabled', held_at = 950
			WHERE workspace_id = 'ext_team' AND user_id = ?`, user))
	}
	// Held, Bea and Fay read nothing and wait for nothing; the hold keeps
	// each mailbox for them, a key's with their grant. Gil, disabled here,
	// counts for nothing; Dan's flag, without a grant, is no read.
	if got := who("acc_e1"); !slices.Equal(got, []string{bea + " 001", dan + " 100", fay + " 001", gil + " 000"}) {
		t.Errorf("held, acc_e1: %v", got)
	}
	if got := who("acc_e2"); !slices.Equal(got, []string{bea + " 001", dan + " 000", fay + " 001", gil + " 000"}) {
		t.Errorf("held, acc_e2: %v", got)
	}
	// A disabled membership not held keeps nothing, and neither does a
	// removed mailbox, nor a flag that is not read.
	f.accepted("Bea's hold let go", f.exec(`UPDATE workspace_members SET held_at = 0 WHERE workspace_id = 'ext_team'
		AND user_id = ?`, bea))
	f.accepted("Fay's read taken", f.exec(`UPDATE mailbox_access SET read = 0, manage = 1 WHERE account_id = 'acc_e1'
		AND user_id = ?`, fay))
	if got := who("acc_e1"); !slices.Equal(got, []string{bea + " 000", dan + " 100", fay + " 000", gil + " 000"}) {
		t.Errorf("a hold let go and a read taken, acc_e1: %v", got)
	}
	f.remove("acc_e2", dan, 1000)
	if got := who("acc_e2"); !slices.Equal(got, []string{bea + " 000", dan + " 000", fay + " 000", gil + " 000"}) {
		t.Errorf("removed, acc_e2: %v", got)
	}
}

// seedExternalTeam writes a team from another source with these members,
// active members all, at the latest schema.
func seedExternalTeam(t *testing.T, f seededSchema, id string, members ...string) {
	t.Helper()
	f.accepted("the team "+id, f.exec(`INSERT INTO workspaces(id, kind, source, name, created_at, updated_at, source_reset)
		VALUES (?, 'team', 'platform', 'Team '||?, 800, 800, (SELECT coalesce(sum(reset), 0) FROM directories))`, id, id))
	for _, user := range members {
		f.accepted("a member of "+id, f.exec(`INSERT INTO workspace_members(workspace_id, user_id, role, status,
			created_at, updated_at) VALUES (?, ?, 'member', 'active', 800, 800)`, id, user))
	}
}

// externalMailbox links a mailbox into a team, with credentials, live.
func externalMailbox(t *testing.T, f seededSchema, id, team string) {
	t.Helper()
	f.accepted("the mailbox "+id, f.exec(`INSERT INTO accounts(id, workspace_id, email, display_name, provider, auth_kind,
		imap_host, imap_port, smtp_host, smtp_port, smtp_tls, login_user, save_sent_copy, state, state_changed_at,
		created_at, updated_at) VALUES (?1, ?2, ?1 || '@ext.example.org', 'Box ' || ?1, 'imap', 'password',
		'imap.example.org', 993, 'smtp.example.org', 587, 'starttls', ?1, 1, 'active', 800, 800, 800)`, id, team))
}

// keyMailbox writes a mailbox's first key, and a grant sealed with it to
// each of these people.
func keyMailbox(t *testing.T, f seededSchema, account, team string, holders ...string) {
	t.Helper()
	ns := fmt.Sprintf("%08x-2222-4333-8444-555555555555", len(account)+int(account[len(account)-1]))
	f.accepted("the key of "+account, f.exec(`INSERT INTO mailbox_keys(account_id, epoch, public_key, namespace,
		created_by, created_at) VALUES (?, 1, randomblob(32), ?, '', 800)`, account, ns))
	sealed := "x'4d4c" + strings.Repeat("ab", 86) + "'"
	for _, user := range holders {
		f.accepted("a grant to "+user, f.exec(`INSERT INTO mailbox_grants(account_id, workspace_id, user_id, epoch,
			grant, granted_by, created_at) VALUES (?, ?, ?, 1, `+sealed+`, '', 800)`, account, team, user))
	}
}
