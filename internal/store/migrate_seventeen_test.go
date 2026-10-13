package store

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// Migration 0017: why an API key stopped (docs/workspaces.md, "API keys").

// schemaTriggersSQL is every trigger of the schema with its definition.
const schemaTriggersSQL = `SELECT name || ': ' || sql FROM sqlite_schema WHERE type = 'trigger' ORDER BY name`

func TestMigrationSeventeenAddsTheStopReasonAndChangesNothingElse(t *testing.T) {
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	migrateTo(t, s, 16)
	before, objects := dumpTables(t, s), queryStrings(t, s, schemaObjectsSQL)
	triggers := queryStrings(t, s, schemaTriggersSQL)
	migrateTo(t, s, 17)
	after := dumpTables(t, s)

	if len(after) != len(before) {
		t.Errorf("0017 left %d tables, want the %d there were", len(after), len(before))
	}
	revoked := 0
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
		for i, row := range rows {
			for col, want := range row {
				if got[i][col] != want {
					t.Errorf("%s row %d column %s was %s and is %s", table, i, col, want, got[i][col])
				}
			}
			var added []string
			for col := range got[i] {
				if _, ok := row[col]; !ok {
					added = append(added, col)
				}
			}
			want := []string(nil)
			if table == "api_keys" {
				want = []string{"stop_reason"}
				// Nothing recorded why a key stopped before: none says.
				if reason := got[i]["stop_reason"]; reason != "string:" {
					t.Errorf("key %s says it stopped for %s", row["prefix"], reason)
				}
				if row["revoked_at"] != "int64:0" {
					revoked++
				}
			}
			if !slices.Equal(added, want) {
				t.Errorf("%s row %d gained the columns %v, want %v", table, i, added, want)
			}
		}
	}
	if revoked == 0 {
		t.Error("the seed has no revoked key: the migration was not tried on one")
	}

	var added []string
	for _, o := range queryStrings(t, s, schemaObjectsSQL) {
		if !slices.Contains(objects, o) {
			added = append(added, o)
		}
	}
	if want := []string{"trigger api_keys_stop_reason_fixed", "trigger api_keys_stop_says_why"}; !slices.Equal(added, want) {
		t.Errorf("0017 added %v, want %v", added, want)
	}
	// The two triggers created again are what they were but for the reason
	// they write; every other trigger is untouched.
	recreated := []string{"api_key_accounts_last", "key_access_carried_over_last"}
	now := queryStrings(t, s, schemaTriggersSQL)
	for _, old := range triggers {
		name, _, _ := strings.Cut(old, ":")
		i := slices.IndexFunc(now, func(n string) bool { return strings.HasPrefix(n, name+":") })
		if i < 0 {
			t.Errorf("the trigger %s is gone", name)
			continue
		}
		if !slices.Contains(recreated, name) {
			if now[i] != old {
				t.Errorf("the trigger %s changed:\n%s\nis now\n%s", name, old, now[i])
			}
			continue
		}
		was := strings.Join(strings.Fields(old), " ")
		is := strings.Join(strings.Fields(now[i]), " ")
		if want := strings.Replace(was, "SET revoked_at = unixepoch()",
			"SET revoked_at = unixepoch(), stop_reason = 'restriction_emptied'", 1); is != want {
			t.Errorf("the trigger %s is\n%s\nwant\n%s", name, is, want)
		}
	}
}

func TestTheSchemaRefusesAKeyThatStopsWithoutSayingWhy(t *testing.T) {
	// The seed at schema 17: 11111111 is Fay's live key of her team, and
	// 22222222 Owner's live key; bbbbbbbb an instance key restricted to
	// acc_5 alone; dddddddd Bea's key carried over, holding mailboxes of
	// three workspaces; ffffffff Owner's key, revoked before any reason was
	// recorded.
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	migrateTo(t, s, 17)
	ctx := context.Background()
	exec := func(query string, args ...any) error {
		return s.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, query, args...)
			return err
		})
	}
	refused := func(what string, err error, says string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v, want a refusal saying %q", what, err, says)
		}
	}
	accepted := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	state := func(prefix string) string {
		t.Helper()
		got := queryStrings(t, s, `SELECT (revoked_at <> 0) || ' ' || stop_reason FROM api_keys WHERE prefix = ?`, prefix)
		if len(got) != 1 {
			t.Fatalf("no key %s", prefix)
		}
		return got[0]
	}
	for _, prefix := range []string{"11111111", "22222222", "bbbbbbbb", "dddddddd"} {
		if got := state(prefix); got != "0 " {
			t.Fatalf("the seed's key %s is %q, want live", prefix, got)
		}
	}
	if got := state("ffffffff"); got != "1 " {
		t.Fatalf("the seed's key ffffffff is %q, want revoked with no reason", got)
	}

	// Stopping a key without saying why, alone or among others.
	refused("a key revoked for no reason", exec(`UPDATE api_keys SET revoked_at = 900 WHERE prefix = '11111111'`),
		"a key that stops says why")
	refused("every key revoked at once, for no reason", exec(`UPDATE api_keys SET revoked_at = 900 WHERE revoked_at = 0`),
		"a key that stops says why")
	// A reason the schema does not know, and a reason for a key that works.
	refused("an unknown reason", exec(`UPDATE api_keys SET revoked_at = 900, stop_reason = 'expired' WHERE prefix = '11111111'`),
		"CHECK")
	refused("a reason for a live key", exec(`UPDATE api_keys SET stop_reason = 'manual' WHERE prefix = '11111111'`), "CHECK")
	refused("a live key inserted with a reason", exec(`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at,
		workspace_id, stop_reason) VALUES ('0a0a0a0a', 'h', 'n', 'read', 1, 0, 'wsp_operator', 'manual')`), "CHECK")
	for _, prefix := range []string{"11111111", "22222222"} {
		if got := state(prefix); got != "0 " {
			t.Errorf("a refused write left the key %s %q", prefix, got)
		}
	}

	// A key that stops with its reason; a key revoked before 0017 changed
	// without one, as deleting its creator changes it.
	accepted("a key revoked for its reason",
		exec(`UPDATE api_keys SET revoked_at = 900, stop_reason = 'manual' WHERE prefix = '11111111'`))
	accepted("a key stopped before, changed without a reason",
		exec(`UPDATE api_keys SET created_by = '', revoked_at = revoked_at WHERE prefix = 'ffffffff'`))
	if got := state("11111111"); got != "1 manual" {
		t.Errorf("the revoked key is %q", got)
	}

	// The schema's own triggers say why they stop a key: an instance key's
	// last restriction, and a carried-over key's last mailbox, removed.
	accepted("an instance key's last restriction removed",
		exec(`DELETE FROM api_key_accounts WHERE key_prefix = 'bbbbbbbb'`))
	if got := state("bbbbbbbb"); got != "1 restriction_emptied" {
		t.Errorf("the instance key with no restriction left is %q", got)
	}
	held := queryStrings(t, s, `SELECT account_id FROM key_access WHERE key_prefix = 'dddddddd' ORDER BY account_id`)
	if len(held) < 2 {
		t.Fatalf("the carried-over key holds %v, want mailboxes of several workspaces", held)
	}
	for i, id := range held {
		accepted("a carried-over key losing "+id, exec(`DELETE FROM key_access WHERE key_prefix = 'dddddddd' AND account_id = ?`, id))
		want := "0 "
		if i == len(held)-1 {
			want = "1 restriction_emptied"
		}
		if got := state("dddddddd"); got != want {
			t.Errorf("the carried-over key, having lost %s, is %q, want %q", id, got, want)
		}
	}
}

func TestWhyAKeyStoppedNeverChanges(t *testing.T) {
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	migrateTo(t, s, 17)
	ctx := context.Background()
	exec := func(query string) error {
		return s.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, query)
			return err
		})
	}
	if err := exec(`UPDATE api_keys SET revoked_at = 900, stop_reason = 'creator_left' WHERE prefix = '11111111'`); err != nil {
		t.Fatal(err)
	}
	for what, query := range map[string]string{
		"another reason":                `UPDATE api_keys SET stop_reason = 'manual' WHERE prefix = '11111111'`,
		"no reason":                     `UPDATE api_keys SET stop_reason = '' WHERE prefix = '11111111'`,
		"another reason, revoked again": `UPDATE api_keys SET revoked_at = 950, stop_reason = 'manual' WHERE prefix = '11111111'`,
		"every key given one reason":    `UPDATE api_keys SET revoked_at = 950, stop_reason = 'migration'`,
	} {
		if err := exec(query); err == nil || !strings.Contains(err.Error(), "why a key stopped never changes") {
			t.Errorf("%s: %v, want the trigger's refusal", what, err)
		}
	}
	// Back to live, keeping or dropping the reason: a key never restarts.
	if err := exec(`UPDATE api_keys SET revoked_at = 0 WHERE prefix = '11111111'`); err == nil ||
		!strings.Contains(err.Error(), "CHECK") {
		t.Errorf("a stopped key made live again: %v", err)
	}
	if got := queryStrings(t, s, `SELECT revoked_at || ' ' || stop_reason FROM api_keys WHERE prefix = '11111111'`); !slices.Equal(got,
		[]string{"900 creator_left"}) {
		t.Errorf("the key is %v, want it as it stopped", got)
	}
	// Writing the reason it has is no change.
	if err := exec(`UPDATE api_keys SET stop_reason = stop_reason, name = 'Renamed' WHERE prefix = '11111111'`); err != nil {
		t.Errorf("a write keeping the reason: %v", err)
	}
}
