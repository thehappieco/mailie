package store

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Migration 0013: every person's account key, and a password the server
// never receives (docs/key-scheme.md).

// thirteenNewColumns are the columns 0013 adds to the tables that were there.
var thirteenNewColumns = map[string][]string{
	"users": {"auth_verifier", "kdf_m", "kdf_p", "kdf_salt", "kdf_t", "key_replaced_at", "password_wrap",
		"public_key", "recovery_verifier", "recovery_wrap", "seal_id", "zk_enrolled_at"},
	"sessions": {"authenticated_at", "stepup_mark_at"},
	"invites":  {"seal_id"},
}

var lowercaseUUIDv4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestMigrationThirteenGivesEveryPersonASealIDAndChangesNothingElse(t *testing.T) {
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	migrateTo(t, s, 12)
	before := dumpTables(t, s)
	migrateTo(t, s, 13)
	after := dumpTables(t, s)

	for table, rows := range before {
		got := after[table]
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
			var extra []string
			for col := range got[i] {
				if _, ok := row[col]; !ok {
					extra = append(extra, col)
				}
			}
			slices.Sort(extra)
			if want := thirteenNewColumns[table]; !slices.Equal(extra, want) {
				t.Errorf("%s gained columns %v, want %v", table, extra, want)
			}
		}
	}
	for _, table := range []string{"auth_tickets", "reset_invites"} {
		if rows, ok := after[table]; !ok || len(rows) != 0 {
			t.Errorf("%s: created %v, holding %d rows; want it new and empty", table, ok, len(rows))
		}
	}

	// A seal id for every person who was there, each its own and spelled
	// one way; nobody enrolled, and every old password hash where it was.
	seen := map[string]bool{}
	for _, row := range after["users"] {
		id := strings.TrimPrefix(row["seal_id"], "string:")
		if !lowercaseUUIDv4.MatchString(id) || seen[id] {
			t.Errorf("%s has the seal id %q", row["id"], id)
		}
		seen[id] = true
		if row["zk_enrolled_at"] != "int64:0" || row["public_key"] != "<nil>:<nil>" || row["auth_verifier"] != "string:" ||
			!strings.HasPrefix(row["password_hash"], "string:$argon2id$") {
			t.Errorf("%s was enrolled by the migration: %v", row["id"], row)
		}
	}
	if len(seen) == 0 || len(after["sessions"]) == 0 {
		t.Fatal("the seed has nobody, or no session")
	}
	for _, row := range after["sessions"] {
		if row["authenticated_at"] != "int64:0" || row["stepup_mark_at"] != "int64:0" {
			t.Errorf("a session open before the migration has a step-up time: %v", row)
		}
	}
}
