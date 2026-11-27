package store

import (
	"context"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// Migration 0030: each mailbox's bytes under usage rule 1, kept by UTC day in
// usage_days and usage_fixed by triggers, and the index every delete of a
// message looks its copies up by (docs/workspaces.md, "Migration 0030").

// usageTrigger is what a usage trigger counts: the table it is on, the rows
// whose bytes under the rule its text spells (the table's columns under an
// alias: NEW, OLD, or p for the parts of a message), and the internal dates
// whose UTC day it writes on.
type usageTrigger struct {
	table string
	rows  []string
	days  []string
}

// usageTriggers are migration 0030's triggers, every one of them: each table
// the rule counts follows its inserts, deletes and updates, and accounts its
// deletes.
var usageTriggers = map[string]usageTrigger{
	"usage_messages_insert": {"messages", []string{"messages NEW"}, []string{"NEW"}},
	"usage_messages_delete": {"messages", []string{"messages OLD", "parts p"}, []string{"OLD"}},
	"usage_messages_revive": {"messages", []string{"messages NEW", "parts p"}, []string{"NEW"}},
	"usage_messages_vanish": {"messages", []string{"messages OLD", "parts p"}, []string{"OLD"}},
	"usage_messages_resize": {"messages", []string{"messages NEW", "messages OLD"}, []string{"NEW"}},
	"usage_messages_move":   {"messages", []string{"messages NEW", "messages OLD", "parts p"}, []string{"NEW", "OLD"}},
	"usage_parts_insert":    {"parts", []string{"parts NEW"}, []string{"m"}},
	"usage_parts_delete":    {"parts", []string{"parts OLD"}, []string{"m"}},
	"usage_parts_update":    {"parts", []string{"parts NEW", "parts OLD"}, []string{"m"}},
	"usage_folders_insert":  {"folders", []string{"folders NEW"}, nil},
	"usage_folders_delete":  {"folders", []string{"folders OLD"}, nil},
	"usage_folders_resize":  {"folders", []string{"folders NEW", "folders OLD"}, nil},
	"usage_folders_move":    {"folders", []string{"folders NEW", "folders OLD"}, nil},
	"usage_drafts_insert":   {"drafts", []string{"drafts NEW"}, nil},
	"usage_drafts_delete":   {"drafts", []string{"drafts OLD"}, nil},
	"usage_drafts_update":   {"drafts", []string{"drafts NEW", "drafts OLD"}, nil},
	"usage_accounts_delete": {"accounts", nil, nil},
}

// latestUsageTriggers opens a database every migration ran on and reads its
// usage triggers: each one's table and text, its whitespace made single
// spaces.
func latestUsageTriggers(t *testing.T) (*Store, map[string][2]string) {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "mail.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	rows, err := s.Reader().QueryContext(ctx, `SELECT name, tbl_name, sql FROM sqlite_schema
		WHERE type = 'trigger' AND name LIKE 'usage\_%' ESCAPE '\'`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][2]string{}
	for rows.Next() {
		var name, table, text string
		if err := rows.Scan(&name, &table, &text); err != nil {
			t.Fatal(err)
		}
		out[name] = [2]string{table, strings.Join(strings.Fields(text), " ")}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return s, out
}

func TestTheUsageTriggersAreOnEveryTableTheRuleCounts(t *testing.T) {
	// A rebuild of messages, parts, folders, drafts or accounts drops their
	// triggers with the old table, silently: the totals would stop moving.
	// It must create them again, and this fails until it does.
	_, got := latestUsageTriggers(t)
	for name, want := range usageTriggers {
		if trigger, ok := got[name]; !ok || trigger[0] != want.table {
			t.Errorf("the trigger %s is on %q (%t), want it on %s", name, trigger[0], ok, want.table)
		}
	}
	for name := range got {
		if _, ok := usageTriggers[name]; !ok {
			t.Errorf("the schema has a usage trigger %s this test does not know", name)
		}
	}
}

// ofList reads the columns an UPDATE OF trigger names.
var ofList = regexp.MustCompile(`AFTER UPDATE OF (.+?) ON (\w+)`)

func TestTheUsageTriggersCountEveryColumnTheRuleCounts(t *testing.T) {
	// Each trigger spells the rule's expression of a row over the columns
	// usageTables lists, in their order, exactly as the rule's scans build
	// it (rowBytesSQL): a column added to a counted table fails here as it
	// fails TestTheUsageRuleCoversEveryColumnOfTheTablesItCounts, until the
	// triggers are written again with it.
	s, got := latestUsageTriggers(t)
	tables := map[string]usageTable{}
	for _, table := range usageTables {
		tables[table.name] = table
	}
	for name, want := range usageTriggers {
		text := got[name][1]
		var spelt []string
		for _, table := range usageTables {
			for _, alias := range []string{"NEW", "OLD", "p", "m"} {
				if strings.Contains(text, table.rowBytesSQL(alias)) {
					spelt = append(spelt, table.name+" "+alias)
				}
			}
		}
		slices.Sort(spelt)
		if rows := slices.Sorted(slices.Values(want.rows)); !slices.Equal(spelt, rows) {
			t.Errorf("%s spells the rule's bytes of %v, want %v", name, spelt, rows)
		}
		// Every column of each row it counts, named on that row.
		for _, row := range want.rows {
			table, alias, _ := strings.Cut(row, " ")
			for _, column := range tables[table].columns {
				if !strings.Contains(text, alias+"."+column) {
					t.Errorf("%s never reads %s.%s", name, alias, column)
				}
			}
		}
		for _, alias := range want.days {
			if !strings.Contains(text, utcDaySQL(alias+".internal_date")) {
				t.Errorf("%s does not date %s's bytes by the UTC day of its internal_date", name, alias)
			}
		}
		if dated := strings.Contains(text, "INSERT INTO usage_days"); dated != (len(want.days) > 0) {
			t.Errorf("%s adds to usage_days: %t, want %t", name, dated, len(want.days) > 0)
		}
		if strings.Contains(text, "INSERT OR REPLACE") {
			t.Errorf("%s replaces a row, which fires no delete", name)
		}
	}

	// The columns an update can change a row's bytes by: every TEXT column
	// (its length), every nullable one (NULL counts nothing), and, for a
	// message, internal_date (its day). The UPDATE OF lists of resize and
	// move name all of them, and nothing the table does not have.
	for _, table := range []string{"messages", "folders"} {
		need, have := columnsChangingBytes(t, s, table)
		if table == "messages" {
			need["internal_date"] = true
		}
		listed := map[string]bool{}
		for _, name := range []string{"usage_" + table + "_resize", "usage_" + table + "_move"} {
			m := ofList.FindStringSubmatch(got[name][1])
			if m == nil || m[2] != table {
				t.Fatalf("%s is no UPDATE OF trigger on %s: %s", name, table, got[name][1])
			}
			for column := range strings.SplitSeq(m[1], ",") {
				column = strings.TrimSpace(column)
				if !have[column] {
					t.Errorf("%s names %s, which %s has not", name, column, table)
				}
				listed[column] = true
			}
		}
		for column := range need {
			if !listed[column] {
				t.Errorf("no usage trigger follows an update of %s.%s, which changes its bytes", table, column)
			}
		}
		if extra := slices.Sorted(maps.Keys(listed)); len(extra) != len(need) {
			t.Errorf("%s's update triggers name %v, want exactly %v", table, extra, slices.Sorted(maps.Keys(need)))
		}
	}
}

// TestAMomentsUTCDayIsTheDayItsRowsAreKeptUnder holds UTCDay, which a reader
// of the totals asks a day of, to utcDaySQL, which the triggers and the scan
// keep a row under: the same floor division, before 1970 and around each
// day's edge, whatever zone the moment is written in.
func TestAMomentsUTCDayIsTheDayItsRowsAreKeptUnder(t *testing.T) {
	s, _ := latestUsageTriggers(t)
	west := time.FixedZone("-03", -3*3600)
	for _, unix := range []int64{-86401, -86400, -86399, -1, 0, 1, 86399, 86400, 1792022400 - 1, 1792022400, 1792108799} {
		var want int64
		if err := s.Reader().QueryRowContext(t.Context(), `SELECT `+utcDaySQL("?1"), unix).Scan(&want); err != nil {
			t.Fatal(err)
		}
		for _, at := range []time.Time{time.Unix(unix, 0).UTC(), time.Unix(unix, 0).In(west)} {
			if got := UTCDay(at); got != want {
				t.Errorf("UTCDay(%s) = %d, and the triggers keep it under day %d", at.Format(time.RFC3339), got, want)
			}
		}
	}
}

// columnsChangingBytes reads a table's columns, and those whose value can
// change a row's bytes under the rule: a TEXT one by its length, a nullable
// one by NULL.
func columnsChangingBytes(t *testing.T, s *Store, table string) (need, have map[string]bool) {
	t.Helper()
	need, have = map[string]bool{}, map[string]bool{}
	rows, err := s.Reader().QueryContext(t.Context(), `SELECT name, type, "notnull" FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var column, kind string
		var notNull bool
		if err := rows.Scan(&column, &kind, &notNull); err != nil {
			t.Fatal(err)
		}
		have[column] = true
		if strings.EqualFold(kind, "TEXT") || (!notNull && column != "id") {
			need[column] = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return need, have
}

func TestMigration0030ChangesNothingElse(t *testing.T) {
	f := newSchemaAt(t, 29)
	seedJournalAt24(t, f)
	for _, table := range usageTables {
		if n := queryInt(t, f.s, `SELECT count(*) FROM "`+table.name+`"`); n == 0 {
			t.Fatalf("the seed holds no %s; the test would prove nothing of its triggers", table.name)
		}
	}
	marks := queryStrings(t, f.s, `SELECT workspace_id || ' ' || seq FROM workspace_changes ORDER BY seq`)
	people := queryStrings(t, f.s, `SELECT user_id || ' ' || seq FROM person_changes ORDER BY seq`)
	before, schema, fts := dumpTables(t, f.s), schemaSQL(t, f.s), ftsAnswers(t, f.s)
	migrateTo(t, f.s, 30)

	// Every row as it was, and both tables empty until a reconcile fills
	// them.
	compareMigrated(t, before, dumpTables(t, f.s), nil, []string{"usage_days", "usage_fixed"})
	if got := queryStrings(t, f.s, `SELECT workspace_id || ' ' || seq FROM workspace_changes ORDER BY seq`); !slices.Equal(got, marks) {
		t.Errorf("the workspace marks are %v, and were %v", got, marks)
	}
	if got := queryStrings(t, f.s, `SELECT user_id || ' ' || seq FROM person_changes ORDER BY seq`); !slices.Equal(got, people) {
		t.Errorf("the person marks are %v, and were %v", got, people)
	}
	added, changed, dropped := schemaChanges(schema, schemaSQL(t, f.s))
	// WITHOUT ROWID: a table's primary key is the table itself, with no
	// index of its own.
	want := []string{"index messages_dup_of", "table usage_days", "table usage_fixed"}
	for name := range usageTriggers {
		want = append(want, "trigger "+name)
	}
	slices.Sort(want)
	if !slices.Equal(added, want) {
		t.Errorf("0030 added\n%v\nwant\n%v", added, want)
	}
	if len(changed) != 0 || len(dropped) != 0 {
		t.Errorf("0030 changed %v and dropped %v", changed, dropped)
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

func TestTheFirstReconcileAfterMigration0030FillsEveryMailboxsTotals(t *testing.T) {
	f := newSchemaAt(t, 29)
	migrateTo(t, f.s, 30)
	ctx := context.Background()
	accounts := queryStrings(t, f.s, `SELECT id FROM accounts ORDER BY id`)
	usage, err := f.s.Usage(ctx, accounts)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) < 3 {
		t.Fatalf("the seed indexed %d mailboxes; the test would prove little", len(usage))
	}
	if drifted, err := f.s.CheckUsageTotals(ctx); err != nil || len(drifted) != len(usage) {
		t.Fatalf("before the first reconcile the check finds %v (%v), want every mailbox that holds anything", drifted, err)
	}

	n, err := f.s.ReconcileUsageTotals(ctx)
	if err != nil || n != len(usage) {
		t.Fatalf("the first reconcile filled %d mailboxes (%v), want the %d that hold anything", n, err, len(usage))
	}
	for _, id := range accounts {
		kept := queryInt(t, f.s, `SELECT (SELECT coalesce(sum(bytes), 0) FROM usage_days WHERE account_id = ?1)
			+ (SELECT coalesce(sum(bytes), 0) FROM usage_fixed WHERE account_id = ?1)`, id)
		if kept != usage[id].Bytes {
			t.Errorf("%s keeps %d bytes, and Usage scans %d", id, kept, usage[id].Bytes)
		}
	}
	if drifted, err := f.s.CheckUsageTotals(ctx); err != nil || len(drifted) != 0 {
		t.Errorf("after the first reconcile the check finds %v drifted (%v)", drifted, err)
	}
	if n, err := f.s.ReconcileUsageTotals(ctx); err != nil || n != 0 {
		t.Errorf("a second reconcile repaired %d mailboxes (%v), want none", n, err)
	}
}
