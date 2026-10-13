package store

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// Migration 0018: a person suspended by an extension of the daemon
// (docs/workspaces.md, "People on the instance").

func TestMigrationEighteenAddsTheSuspensionAndChangesNothingElse(t *testing.T) {
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	migrateTo(t, s, 17)
	before, objects := dumpTables(t, s), queryStrings(t, s, schemaObjectsSQL)
	triggers := queryStrings(t, s, schemaTriggersSQL)
	migrateTo(t, s, 18)
	after := dumpTables(t, s)

	if len(after) != len(before) {
		t.Errorf("0018 left %d tables, want the %d there were", len(after), len(before))
	}
	disabled := 0
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
			if table == "users" {
				want = []string{"suspended_at"}
				// Nobody was suspended before: a person disabled then stays
				// disabled here.
				if at := got[i]["suspended_at"]; at != "int64:0" {
					t.Errorf("%s is suspended since %s", row["email"], at)
				}
				if row["status"] != "string:active" {
					disabled++
				}
			}
			if !slices.Equal(added, want) {
				t.Errorf("%s row %d gained the columns %v, want %v", table, i, added, want)
			}
		}
	}
	if disabled == 0 {
		t.Error("the seed has nobody disabled: the migration was not tried on one")
	}
	var added []string
	for _, o := range queryStrings(t, s, schemaObjectsSQL) {
		if !slices.Contains(objects, o) {
			added = append(added, o)
		}
	}
	if len(added) != 0 {
		t.Errorf("0018 added %v, want only a column", added)
	}
	if now := queryStrings(t, s, schemaTriggersSQL); !slices.Equal(now, triggers) {
		t.Errorf("0018 changed the triggers:\n%v\nare now\n%v", triggers, now)
	}
}

func TestTheSchemaRefusesASuspendedPersonWhoIsActive(t *testing.T) {
	// The seed at schema 18: usr_0000000000000001 (Owner) and
	// usr_0000000000000006 (Fay) are active, usr_0000000000000005 (Eve) is
	// disabled.
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	migrateTo(t, s, 18)
	ctx := context.Background()
	exec := func(query string, args ...any) error {
		return s.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, query, args...)
			return err
		})
	}
	refused := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "CHECK") {
			t.Errorf("%s: %v, want the CHECK's refusal", what, err)
		}
	}
	accepted := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	state := func(id string) string {
		t.Helper()
		got := queryStrings(t, s, `SELECT status || ' ' || suspended_at FROM users WHERE id = ?`, id)
		if len(got) != 1 {
			t.Fatalf("no person %s", id)
		}
		return got[0]
	}
	const owner, eve, fay = "usr_0000000000000001", "usr_0000000000000005", "usr_0000000000000006"

	// An active person marked suspended, alone or among others.
	refused("an active person suspended without being disabled",
		exec(`UPDATE users SET suspended_at = 900 WHERE id = ?`, owner))
	refused("everyone marked suspended at once", exec(`UPDATE users SET suspended_at = 900`))
	refused("an active person inserted suspended", exec(`INSERT INTO users(id, email, name, password_hash, role,
		status, password_changed_at, created_at, updated_at, suspended_at)
		VALUES ('usr_00000000000000aa', 'cy@example.org', 'Cy', '', 'member', 'active', 0, 500, 500, 900)`))
	if got := state(owner); got != "active 0" {
		t.Errorf("a refused write left Owner %q", got)
	}

	// Suspending: both columns in one statement.
	accepted("Fay suspended", exec(`UPDATE users SET status = 'disabled', suspended_at = 900 WHERE id = ?`, fay))
	// A suspended person made active while still marked suspended.
	refused("a suspended person switched back on, still marked",
		exec(`UPDATE users SET status = 'active' WHERE id = ?`, fay))
	if got := state(fay); got != "disabled 900" {
		t.Errorf("a refused write left Fay %q", got)
	}
	// Resuming clears the mark in the statement that switches her on; a
	// disable here clears it and keeps her off.
	accepted("Fay resumed", exec(`UPDATE users SET status = 'active', suspended_at = 0 WHERE id = ?`, fay))
	if got := state(fay); got != "active 0" {
		t.Errorf("Fay resumed is %q", got)
	}
	accepted("Fay suspended again", exec(`UPDATE users SET status = 'disabled', suspended_at = 950 WHERE id = ?`, fay))
	accepted("Fay disabled here", exec(`UPDATE users SET suspended_at = 0 WHERE id = ?`, fay))
	if got := state(fay); got != "disabled 0" {
		t.Errorf("Fay disabled here over her suspension is %q", got)
	}
	// Eve, disabled before the migration, stays as she was.
	if got := state(eve); got != "disabled 0" {
		t.Errorf("Eve is %q", got)
	}
}
