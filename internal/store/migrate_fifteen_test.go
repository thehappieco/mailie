package store

import (
	"bytes"
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// Migration 0015: the platform wraps of people who sign in through an
// identity provider, and their first sign-ins' enrolment tickets
// (docs/key-scheme.md sections 6.3 and 12.10).

func TestMigrationFifteenAddsThePlatformWrapTablesEmptyAndChangesNothingElse(t *testing.T) {
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	migrateTo(t, s, 14)
	before := dumpTables(t, s)
	migrateTo(t, s, 15)
	after := dumpTables(t, s)

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
		}
	}
	var added []string
	for table, rows := range after {
		if _, ok := before[table]; ok {
			continue
		}
		added = append(added, table)
		if len(rows) != 0 {
			t.Errorf("%s holds %d rows; nobody has a platform wrap yet", table, len(rows))
		}
	}
	slices.Sort(added)
	if want := []string{"external_enrolments", "platform_wraps"}; !slices.Equal(added, want) {
		t.Errorf("0015 added the tables %v, want %v", added, want)
	}
	if views := queryInt(t, s, `SELECT count(*) FROM sqlite_schema WHERE type = 'view'`); views != 0 {
		t.Errorf("0015 left %d views, which a later rebuild would refuse", views)
	}
}

// platformWrapShape is 61 bytes starting with b: what the schema holds of a
// platform wrap is its length and its first byte.
func platformWrapShape(b byte) []byte {
	return append([]byte{b}, bytes.Repeat([]byte{0xa5}, 60)...)
}

func TestThePlatformWrapTablesRefuseWhatTheSchemaForbids(t *testing.T) {
	// The seed at schema 15: usr_0000000000000001 to 7 are people.
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	migrateTo(t, s, 15)
	ctx := context.Background()
	exec := func(query string, args ...any) error {
		return s.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, query, args...)
			return err
		})
	}
	wrap := func(user, keyID string, blob []byte) error {
		return exec(`INSERT INTO platform_wraps(user_id, product_key_id, wrap, created_at) VALUES (?, ?, ?, 500)`,
			user, keyID, blob)
	}
	ticket := func(hash []byte, user, keyID string, ttl int) error {
		return exec(`INSERT INTO external_enrolments(hash, user_id, issuer, subject, product_key_id, auth_time,
			session_ttl, created_at, expires_at) VALUES (?, ?, 'https://accounts.example.com', 'subject-1', ?, 0, ?, 500, 1100)`,
			hash, user, keyID, ttl)
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

	// Mailie's product key ids, in their one spelling, and only those.
	for _, id := range []string{"mailie:1", "mailie:42", "mailie:2147483647"} {
		accepted("the product key id "+id, wrap("usr_0000000000000001", id, platformWrapShape(0x03)))
	}
	for _, id := range []string{"", "mailie:", "mailie:0", "mailie:01", "mailie:-1", "mailie:1 ", "Mailie:1", "wappie:1",
		"mailie:2147483648", "mailie:99999999999", "mailie:1a"} {
		refused("the product key id "+id, wrap("usr_0000000000000002", id, platformWrapShape(0x03)), "CHECK")
	}
	// A platform wrap's shape: 61 bytes starting with 0x03, never an
	// account wrap's 0x02.
	refused("an account wrap", wrap("usr_0000000000000002", "mailie:1", platformWrapShape(0x02)), "CHECK")
	refused("60 bytes", wrap("usr_0000000000000002", "mailie:1", platformWrapShape(0x03)[:60]), "CHECK")
	refused("a wrap as text", exec(`INSERT INTO platform_wraps(user_id, product_key_id, wrap, created_at)
		VALUES ('usr_0000000000000002', 'mailie:1', ?, 500)`, string(platformWrapShape(0x03))), "CHECK")
	refused("a person who does not exist", wrap("usr_0000000000000099", "mailie:1", platformWrapShape(0x03)), "FOREIGN KEY")

	// Insert only: never changed, never replaced.
	refused("a second wrap at the same id", wrap("usr_0000000000000001", "mailie:1", platformWrapShape(0x03)), "written once")
	refused("a wrap replaced", exec(`INSERT OR REPLACE INTO platform_wraps(user_id, product_key_id, wrap, created_at)
		VALUES ('usr_0000000000000001', 'mailie:1', ?, 600)`, platformWrapShape(0x03)), "written once")
	refused("a wrap changed", exec(`UPDATE platform_wraps SET wrap = ? WHERE user_id = 'usr_0000000000000001'`,
		platformWrapShape(0x03)), "written once")
	refused("a wrap moved to another person", exec(`UPDATE platform_wraps SET user_id = 'usr_0000000000000002'`), "written once")

	// Tickets: stored as a hash, for a session of at most 14 days.
	hash := bytes.Repeat([]byte{1}, 32)
	refused("a ticket stored as 31 bytes", ticket(hash[:31], "usr_0000000000000003", "mailie:1", 3600), "CHECK")
	refused("a ticket under another product's key id", ticket(hash, "usr_0000000000000003", "wappie:1", 3600), "CHECK")
	refused("a ticket for a session of 15 days", ticket(hash, "usr_0000000000000003", "mailie:1", 15*86400), "CHECK")
	refused("a ticket for a session of nothing", ticket(hash, "usr_0000000000000003", "mailie:1", 0), "CHECK")
	accepted("a ticket", ticket(hash, "usr_0000000000000003", "mailie:1", 86400))

	// Both go with their person, by the schema itself.
	accepted("a person", exec(`INSERT INTO users(id, email, name, password_hash, role, status, password_changed_at,
		created_at, updated_at) VALUES ('usr_00000000000000aa', 'cy@example.org', 'Cy', '', 'member', 'active', 0, 500, 500)`))
	accepted("her wrap", wrap("usr_00000000000000aa", "mailie:1", platformWrapShape(0x03)))
	accepted("her ticket", ticket(bytes.Repeat([]byte{2}, 32), "usr_00000000000000aa", "mailie:1", 86400))
	accepted("her deletion", exec(`DELETE FROM users WHERE id = 'usr_00000000000000aa'`))
	if n := queryInt(t, s, `SELECT (SELECT count(*) FROM platform_wraps WHERE user_id = 'usr_00000000000000aa') +
		(SELECT count(*) FROM external_enrolments WHERE user_id = 'usr_00000000000000aa')`); n != 0 {
		t.Errorf("a deleted person left %d wraps and tickets", n)
	}
}
