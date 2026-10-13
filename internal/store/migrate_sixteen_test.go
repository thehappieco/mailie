package store

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// Migration 0016: a workspace's kind, source and person never change
// (docs/workspaces.md, "Workspaces" and "The workspace source").

// schemaObjectsSQL lists every object of the schema by type and name.
const schemaObjectsSQL = `SELECT type || ' ' || name FROM sqlite_schema ORDER BY type, name`

func TestMigrationSixteenAddsOneTriggerAndChangesNothingElse(t *testing.T) {
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	migrateTo(t, s, 15)
	before, objects := dumpTables(t, s), queryStrings(t, s, schemaObjectsSQL)
	migrateTo(t, s, 16)
	after := dumpTables(t, s)

	if len(after) != len(before) {
		t.Errorf("0016 left %d tables, want the %d there were", len(after), len(before))
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
		for i, row := range rows {
			if len(got[i]) != len(row) {
				t.Errorf("%s row %d had %d columns and has %d", table, i, len(row), len(got[i]))
			}
			for col, want := range row {
				if got[i][col] != want {
					t.Errorf("%s row %d column %s was %s and is %s", table, i, col, want, got[i][col])
				}
			}
		}
	}
	var added []string
	for _, o := range queryStrings(t, s, schemaObjectsSQL) {
		if !slices.Contains(objects, o) {
			added = append(added, o)
		}
	}
	if want := []string{"trigger workspaces_fixed"}; !slices.Equal(added, want) {
		t.Errorf("0016 added %v, want %v", added, want)
	}
}

func TestAWorkspacesKindSourceAndPersonNeverChange(t *testing.T) {
	// The seed at schema 16: usr_0000000000000001 to 7 are people, each with
	// a personal workspace; wsp_t1 and wsp_t2 are local teams.
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	migrateTo(t, s, 16)
	ctx := context.Background()
	exec := func(query string, args ...any) error {
		return s.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, query, args...)
			return err
		})
	}
	refused := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "kind, source and person never change") {
			t.Errorf("%s: %v, want the trigger's refusal", what, err)
		}
	}
	accepted := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	// A team from another source, and a person without a personal
	// workspace, whom nothing but the trigger keeps from receiving one.
	accepted("a team from another source", exec(`INSERT INTO workspaces(id, kind, source, name, created_at, updated_at)
		VALUES ('0b4a3c5e-0000-4000-8000-000000000001', 'team', 'platform', 'Elsewhere', 500, 500)`))
	accepted("a person without a personal workspace", exec(`INSERT INTO users(id, email, name, password_hash, role,
		status, password_changed_at, created_at, updated_at)
		VALUES ('usr_00000000000000aa', 'cy@example.org', 'Cy', '', 'member', 'active', 0, 500, 500)`))
	var personal string
	if err := s.Reader().QueryRowContext(ctx, `SELECT id FROM workspaces WHERE person_id = 'usr_0000000000000001'`).
		Scan(&personal); err != nil {
		t.Fatal(err)
	}

	// The source, which a CHECK alone would let pass either way.
	refused("a local team moved to another source", exec(`UPDATE workspaces SET source = 'platform' WHERE id = 'wsp_t1'`))
	refused("a team from another source made local",
		exec(`UPDATE workspaces SET source = 'local' WHERE id = '0b4a3c5e-0000-4000-8000-000000000001'`))
	refused("a personal workspace moved to another source",
		exec(`UPDATE workspaces SET source = 'platform' WHERE id = ?`, personal))
	// The person, to someone the UNIQUE and the CHECKs would accept.
	refused("a personal workspace passed to another person",
		exec(`UPDATE workspaces SET person_id = 'usr_00000000000000aa' WHERE id = ?`, personal))
	// The kind, and the kind with every column a CHECK ties to it.
	refused("a personal workspace made a team",
		exec(`UPDATE workspaces SET kind = 'team', name = 'Mine', person_id = NULL WHERE id = ?`, personal))
	refused("a team made a personal workspace",
		exec(`UPDATE workspaces SET kind = 'personal', name = '', person_id = 'usr_00000000000000aa' WHERE id = 'wsp_t1'`))
	refused("the operator workspace made a team",
		exec(`UPDATE workspaces SET kind = 'team', name = 'Ops' WHERE id = 'wsp_operator'`))
	refused("every workspace at once", exec(`UPDATE workspaces SET source = 'platform'`))

	if n := queryInt(t, s, `SELECT count(*) FROM workspaces WHERE source = 'platform'`); n != 1 {
		t.Errorf("%d workspaces come from another source, want the one inserted so", n)
	}
	if n := queryInt(t, s, `SELECT count(*) FROM workspaces WHERE person_id = 'usr_00000000000000aa' OR kind = 'team' AND
		id NOT IN ('wsp_t1', 'wsp_t2', '0b4a3c5e-0000-4000-8000-000000000001')`); n != 0 {
		t.Errorf("a refused change left %d workspaces changed", n)
	}

	// What a workspace's row does change: a team's name and its time, and
	// an update that names the three columns without changing them.
	accepted("a team renamed", exec(`UPDATE workspaces SET name = 'Help', updated_at = 900 WHERE id = 'wsp_t1'`))
	accepted("a team from another source renamed",
		exec(`UPDATE workspaces SET name = 'Still elsewhere', updated_at = 900 WHERE id = '0b4a3c5e-0000-4000-8000-000000000001'`))
	accepted("the three columns set to what they are",
		exec(`UPDATE workspaces SET kind = kind, source = source, person_id = person_id, updated_at = 901`))
	if n := queryInt(t, s, `SELECT count(*) FROM workspaces WHERE id = 'wsp_t1' AND name = 'Help' AND updated_at = 901`); n != 1 {
		t.Error("the rename did not stay")
	}
}
