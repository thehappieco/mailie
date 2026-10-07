package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// Migration 0012: an API key belongs to its workspace.

// personKey is an INSERT of a schema-11 key acting as a person.
func personKey(prefix, user, scope, terms, createdBy string, restricted, revoked, expires int) string {
	return fmt.Sprintf(`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at, revoked_at, last_used_at,
	  user_id, terms_version, created_by, restricted)
	  VALUES ('%[1]s', '$argon2id$%[1]s', 'Key %[1]s', '%[3]s', 200, %[7]d, %[6]d, 0, '%[2]s', '%[4]s', '%[5]s', %[8]d)`,
		prefix, user, scope, terms, createdBy, revoked, expires, restricted)
}

// seedKeys is a self-hosted schema-11 database with every kind of key, on top
// of seedTeams: the people and mailboxes there, their keys from
// seedSelfHosted, and these, made at schema 11:
//
//	11111111 Fay's write key for acc_t1, which she reads without act
//	22222222 Owner's write key for acc_t2 (read, no act) and acc_1 (her own)
//	33333333 Dan's write key for every mailbox of his: acc_t3 alone
//	44444444 a read key an administrator made for Bea, nobody agreeing
//	55555555 Eve's write key for acc_t2, revoked when she was disabled
//	66666666 Owner's read key for acc_2, expired
//	77777777 Fay's write key for acc_t1 and acc_t2, the second of which she
//	         does not read
//	88888888 Gil's read key for every mailbox of his: he reads none
//	99999999 a write key an administrator made for Fay, for every mailbox
//	         of hers (acc_t1 alone), nobody agreeing
//
// and seedSelfHosted's: dddddddd Bea's write key for every mailbox of hers
// (acc_3, acc_t1, acc_t3: three workspaces), eeeeeeee and hhhhhhhh Owner's
// read keys for acc_1, ffffffff Owner's revoked write key for every mailbox
// of hers, gggggggg Owner's key 0009 revoked, and the instance keys
// aaaaaaaa, bbbbbbbb (for acc_5) and cccccccc (for acc_1, a person's, which
// it never reached).
func seedKeys(t *testing.T, s *Store) {
	t.Helper()
	seedTeams(t, s)
	migrateTo(t, s, 11)
	stmts := person("usr_0000000000000007", "gil@example.org", "member", "active", 0, "")
	stmts = append(stmts,
		personKey("11111111", "usr_0000000000000006", "write", "terms-1", "usr_0000000000000006", 1, 0, 9999999999),
		personKey("22222222", "usr_0000000000000001", "write", "terms-1", "usr_0000000000000001", 1, 0, 9999999999),
		personKey("33333333", "usr_0000000000000004", "write", "terms-1", "usr_0000000000000004", 0, 0, 9999999999),
		personKey("44444444", "usr_0000000000000002", "read", "", "key:aaaaaaaa", 1, 0, 9999999999),
		personKey("55555555", "usr_0000000000000005", "write", "terms-1", "usr_0000000000000005", 1, 300, 9999999999),
		personKey("66666666", "usr_0000000000000001", "read", "terms-1", "usr_0000000000000001", 1, 0, 500),
		personKey("77777777", "usr_0000000000000006", "write", "terms-1", "usr_0000000000000006", 1, 0, 9999999999),
		personKey("88888888", "usr_0000000000000007", "read", "terms-1", "usr_0000000000000007", 0, 0, 9999999999),
		personKey("99999999", "usr_0000000000000006", "write", "", "cli", 0, 0, 9999999999),
		`INSERT INTO api_key_accounts(key_prefix, account_id)
		 VALUES ('11111111', 'acc_t1'), ('22222222', 'acc_t2'), ('22222222', 'acc_0000000000000001'),
		        ('44444444', 'acc_0000000000000003'), ('55555555', 'acc_t2'), ('66666666', 'acc_0000000000000002'),
		        ('77777777', 'acc_t1'), ('77777777', 'acc_t2')`,
		// Sends by keys: an instance key's, and one the daily count of a key
		// will read.
		`INSERT INTO sends(account_id, idempotency_key, compose_hash, message_id_hdr, state, created_by, created_at,
		   updated_at, recipients, user_id)
		 VALUES ('acc_0000000000000005', 'k-ops', 'h3', 'm3@example.org', 'sent', 'key:aaaaaaaa', 960, 961, 1, '')`,
	)
	execAll(t, s, stmts...)
}

// twelveNewColumns are the columns 0012 adds to the tables that were there.
var twelveNewColumns = map[string][]string{
	"api_keys": {"origin", "workspace_id"},
}

// compareTwelve checks that every table of before is in after, row for row
// and value for value, but for the columns allowed to change and the rows
// allowed to go; and that the tables there before gained only 0012's columns.
func compareTwelve(t *testing.T, before, after tableDump, mayChange func(table string, row map[string]string, col string) bool, gone func(table string, row map[string]string) bool) {
	t.Helper()
	for table, rows := range before {
		got, ok := after[table]
		if !ok {
			t.Errorf("table %s is gone", table)
			continue
		}
		kept := rows[:0:0]
		for _, row := range rows {
			if gone == nil || !gone(table, row) {
				kept = append(kept, row)
			}
		}
		if len(got) != len(kept) {
			t.Errorf("%s should hold %d rows and holds %d", table, len(kept), len(got))
			continue
		}
		for i, row := range kept {
			for col, want := range row {
				if got[i][col] != want && (mayChange == nil || !mayChange(table, row, col)) {
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
			if want := twelveNewColumns[table]; !slices.Equal(extra, want) {
				t.Errorf("%s gained columns %v, want %v", table, extra, want)
			}
		}
	}
	if _, ok := after["key_access"]; !ok {
		t.Error("key_access was not created")
	}
}

// keysAfter lists every key as 0012 left it: prefix, workspace, origin, user,
// revoked (as the seed revoked it, or "revoked" since), expires ("a-year" when
// moved to a year from the migration), terms, created by.
func keysAfter(t *testing.T, s *Store, started int64) []string {
	t.Helper()
	return queryStrings(t, s, `SELECT prefix, coalesce(workspace_id, '-'), coalesce(nullif(origin, ''), '-'),
		       coalesce(user_id, '-'), CASE WHEN revoked_at IN (0, 70, 300) THEN revoked_at ELSE 'revoked' END,
		       CASE WHEN expires_at > ?1 + 31000000 AND expires_at < 9999999999 THEN 'a-year' ELSE expires_at END,
		       coalesce(nullif(terms_version, ''), '-'), created_by
		  FROM api_keys ORDER BY prefix`, started)
}

func TestMigrationTwelveMovesEachPersonKeyIntoTheWorkspaceItReaches(t *testing.T) {
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	before, fts := dumpTables(t, s), ftsAnswers(t, s)
	for table, rows := range before {
		if len(rows) == 0 {
			t.Fatalf("the seed left %s empty", table)
		}
	}
	started := time.Now().Unix()
	migrateTo(t, s, 12)

	carried := map[string]bool{"string:dddddddd": true, "string:22222222": true}
	compareTwelve(t, before, dumpTables(t, s), func(table string, row map[string]string, col string) bool {
		if table != "api_keys" {
			return false
		}
		switch col {
		case "user_id":
			return true
		case "revoked_at":
			return row["prefix"] == "string:44444444" || row["prefix"] == "string:99999999"
		case "expires_at":
			return carried[row["prefix"]]
		}
		return false
	}, func(table string, row map[string]string) bool {
		// A person's key's restriction rows are in key_access now.
		return table == "api_key_accounts" && row["key_prefix"] != "string:bbbbbbbb" &&
			row["key_prefix"] != "string:cccccccc"
	})
	checkIntact(t, s, fts)

	// Each key: its workspace, where it came from, nobody it acts as.
	if got := keysAfter(t, s, started); !slices.Equal(got, []string{
		"11111111 wsp_t1 person - 0 9999999999 terms-1 usr_0000000000000006",
		"22222222 - person - 0 a-year terms-1 usr_0000000000000001",
		"33333333 wsp_t2 person-all - 0 9999999999 terms-1 usr_0000000000000004",
		"44444444 " + personalOf(t, s, "usr_0000000000000002") + " person - revoked 9999999999 - key:aaaaaaaa",
		"55555555 wsp_t1 person - 300 9999999999 terms-1 usr_0000000000000005",
		"66666666 " + personalOf(t, s, "usr_0000000000000001") + " person - 0 500 terms-1 usr_0000000000000001",
		"77777777 wsp_t1 person - 0 9999999999 terms-1 usr_0000000000000006",
		"88888888 wsp_p_usr_0000000000000007 person-all - 0 9999999999 terms-1 usr_0000000000000007",
		"99999999 " + personalOf(t, s, "usr_0000000000000006") + " person-all - revoked 9999999999 - cli",
		"aaaaaaaa wsp_operator - - 0 9999999999 - cli",
		"bbbbbbbb wsp_operator - - 0 9999999999 - key:aaaaaaaa",
		"cccccccc wsp_operator - - 0 9999999999 - key:aaaaaaaa",
		"dddddddd - person-all - 0 a-year terms-1 usr_0000000000000002",
		"eeeeeeee " + personalOf(t, s, "usr_0000000000000001") + " person - 0 9999999999 terms-1 usr_0000000000000001",
		"ffffffff " + personalOf(t, s, "usr_0000000000000001") + " person-all - 70 9999999999 terms-1 usr_0000000000000001",
		"gggggggg " + personalOf(t, s, "usr_0000000000000001") + " person - revoked 9999999999 terms-1 usr_0000000000000001",
		"hhhhhhhh " + personalOf(t, s, "usr_0000000000000001") + " person - 0 9999999999 terms-1 usr_0000000000000001",
	}) {
		t.Errorf("keys after 0012:\n%s", strings.Join(got, "\n"))
	}
	// Revoked by the migration, the keys nobody agreed to the terms through,
	// each in its person's personal workspace whatever it reached: a team
	// never sees a key made for one of its members, and the key goes with
	// its person, as it did before.
	for _, prefix := range []string{"44444444", "99999999"} {
		if n := queryInt(t, s, `SELECT revoked_at FROM api_keys WHERE prefix = ?`, prefix); n < started {
			t.Errorf("the key %s nobody agreed to was revoked at %d, before the migration", prefix, n)
		}
	}

	// What each reaches, as it reached it yesterday: read, act only where
	// the key was write and its person held act, never send.
	if got := queryStrings(t, s, `SELECT x.key_prefix, x.account_id, x.workspace_id, x.read, x.act, x.send, x.granted_by
		  FROM key_access x ORDER BY x.key_prefix, x.account_id`); !slices.Equal(got, []string{
		"11111111 acc_t1 wsp_t1 1 0 0 migration",
		"22222222 acc_0000000000000001 " + personalOf(t, s, "usr_0000000000000001") + " 1 1 0 migration",
		"22222222 acc_t2 wsp_t1 1 0 0 migration",
		"33333333 acc_t3 wsp_t2 1 1 0 migration",
		"55555555 acc_t2 wsp_t1 1 0 0 migration",
		"66666666 acc_0000000000000002 " + personalOf(t, s, "usr_0000000000000001") + " 1 0 0 migration",
		"77777777 acc_t1 wsp_t1 1 0 0 migration",
		"dddddddd acc_0000000000000003 " + personalOf(t, s, "usr_0000000000000002") + " 1 1 0 migration",
		"dddddddd acc_t1 wsp_t1 1 1 0 migration",
		"dddddddd acc_t3 wsp_t2 1 1 0 migration",
		"eeeeeeee acc_0000000000000001 " + personalOf(t, s, "usr_0000000000000001") + " 1 0 0 migration",
		"hhhhhhhh acc_0000000000000001 " + personalOf(t, s, "usr_0000000000000001") + " 1 0 0 migration",
	}) {
		t.Errorf("what keys hold after 0012:\n%s", strings.Join(got, "\n"))
	}

	// Only the operator's keys are restricted to accounts now, as they were.
	if got := queryStrings(t, s, `SELECT key_prefix, account_id FROM api_key_accounts ORDER BY 1, 2`); !slices.Equal(got, []string{
		"bbbbbbbb acc_0000000000000005", "cccccccc acc_0000000000000001",
	}) {
		t.Errorf("restrictions after 0012: %v", got)
	}
}

// personalOf is a person's personal workspace, which 0008 named at random.
func personalOf(t *testing.T, s *Store, userID string) string {
	t.Helper()
	got := queryStrings(t, s, `SELECT id FROM workspaces WHERE person_id = ?`, userID)
	if len(got) != 1 {
		t.Fatalf("the personal workspace of %s: %v", userID, got)
	}
	return got[0]
}

func TestMigrationTwelveNeverWidensAKeysActions(t *testing.T) {
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	migrateTo(t, s, 12)
	// A row with act is a write key's, on a mailbox whose person held act on
	// it, as an active member: what the key could do yesterday.
	if n := queryInt(t, s, `SELECT count(*) FROM key_access x JOIN api_keys k ON k.prefix = x.key_prefix
		 WHERE x.act = 1 AND (k.scope = 'read'
		   OR NOT EXISTS (SELECT 1 FROM mailbox_access g WHERE g.account_id = x.account_id AND g.act = 1
		                    AND g.user_id = substr(k.created_by, 1)))`); n != 0 {
		t.Errorf("%d rows act where their key could not", n)
	}
	if n := queryInt(t, s, `SELECT count(*) FROM key_access WHERE send = 1`); n != 0 {
		t.Errorf("%d rows send: no person's key ever sent", n)
	}
	// And every row is one the schema would take now: act only on a key of
	// the write or send scope.
	if n := queryInt(t, s, `SELECT count(*) FROM key_access x JOIN api_keys k ON k.prefix = x.key_prefix
		 WHERE x.act = 1 AND k.scope NOT IN ('write', 'send')`); n != 0 {
		t.Errorf("%d rows act on a key whose scope does not", n)
	}
	// Fay's key for a mailbox she does not read reaches only the one she
	// does; the dead keys keep what they were made for, and reach nothing.
	if got := queryStrings(t, s, `SELECT account_id FROM key_access WHERE key_prefix = '77777777'`); !slices.Equal(got, []string{"acc_t1"}) {
		t.Errorf("Fay's key holds %v, want acc_t1 alone", got)
	}
}

func TestMigrationTwelveGivesAnUnrestrictedKeyWhatItReachedAndNoMore(t *testing.T) {
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	migrateTo(t, s, 12)
	for prefix, want := range map[string][]string{
		"33333333": {"acc_t3"}, // Dan reads the one mailbox of Ops
		"88888888": nil,        // Gil reads nothing: a key of his workspace that reaches nothing yet
		"ffffffff": nil,        // revoked: nothing
	} {
		if got := queryStrings(t, s, `SELECT account_id FROM key_access WHERE key_prefix = ? ORDER BY 1`, prefix); !slices.Equal(got, want) {
			t.Errorf("key %s holds %v, want %v", prefix, got, want)
		}
	}
	if n := queryInt(t, s, `SELECT count(*) FROM api_keys WHERE prefix = '88888888' AND revoked_at = 0`); n != 1 {
		t.Error("a key that reaches nothing was revoked; it waits for a mailbox")
	}
}

func TestMigrationTwelveCarriesOverAKeySpanningWorkspacesFrozen(t *testing.T) {
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	migrateTo(t, s, 12)
	ctx := context.Background()
	run := func(stmt string) error {
		return s.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, stmt)
			return err
		})
	}
	// It gains no mailbox, in any workspace it reaches.
	for _, ws := range []string{"wsp_t1", "wsp_t2"} {
		if err := run(`INSERT INTO key_access(key_prefix, account_id, workspace_id, read, created_at, updated_at)
			SELECT 'dddddddd', id, workspace_id, 1, 1, 1 FROM accounts WHERE workspace_id = '` + ws + `' AND id NOT IN
			  (SELECT account_id FROM key_access WHERE key_prefix = 'dddddddd') LIMIT 1`); err == nil &&
			queryInt(t, s, `SELECT count(*) FROM key_access WHERE key_prefix = 'dddddddd'`) > 3 {
			t.Errorf("a carried-over key gained a mailbox of %s", ws)
		}
	}
	// It works on what it holds until the last goes, and is revoked then:
	// a mailbox removed, then its workspace's rows taken out.
	if err := run(`DELETE FROM accounts WHERE id = 'acc_t3'`); err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, s, `SELECT revoked_at FROM api_keys WHERE prefix = 'dddddddd'`); n != 0 {
		t.Error("a carried-over key was revoked with mailboxes left")
	}
	if err := run(`DELETE FROM key_access WHERE key_prefix = 'dddddddd' AND account_id = 'acc_t1'`); err != nil {
		t.Fatal(err)
	}
	if err := run(`DELETE FROM key_access WHERE key_prefix = 'dddddddd'`); err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, s, `SELECT revoked_at FROM api_keys WHERE prefix = 'dddddddd'`); n == 0 {
		t.Error("a carried-over key left with no mailbox is still live")
	}
	// A key of a workspace left with no mailbox is not revoked: it reaches
	// nothing until it is given one.
	if err := run(`DELETE FROM key_access WHERE key_prefix = '11111111'`); err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, s, `SELECT revoked_at FROM api_keys WHERE prefix = '11111111'`); n != 0 {
		t.Error("a workspace key left with no mailbox was revoked")
	}
}

func TestMigrationTwelveLeavesOperatorKeysAndTheirRestrictionsAlone(t *testing.T) {
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	before := dumpTable(t, s, "api_keys", "rowid")
	migrateTo(t, s, 12)
	after := dumpTable(t, s, "api_keys", "rowid")
	for i, row := range before {
		if row["user_id"] != "<nil>:<nil>" {
			continue
		}
		for col, want := range row {
			if after[i][col] != want {
				t.Errorf("instance key %s: %s was %s and is %s", row["prefix"], col, want, after[i][col])
			}
		}
		if after[i]["workspace_id"] != "string:wsp_operator" || after[i]["origin"] != "string:" {
			t.Errorf("instance key %s is in %s, from %s", row["prefix"], after[i]["workspace_id"], after[i]["origin"])
		}
	}
	// A restriction row going still revokes an instance key left with none.
	if _, err := s.Writer().ExecContext(context.Background(), `DELETE FROM api_key_accounts WHERE key_prefix = 'bbbbbbbb'`); err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, s, `SELECT revoked_at FROM api_keys WHERE prefix = 'bbbbbbbb'`); n == 0 {
		t.Error("an instance key that lost its last restriction is live, as a key for every operator mailbox")
	}
}

func TestMigrationTwelveKeepsTheSmallestShapesKey(t *testing.T) {
	// The hosted service holds one person, their two mailboxes and one key
	// that nothing outside the database names: whichever it is, it is kept,
	// and works as it did.
	for _, tc := range []struct {
		name, key string
		rows      []string
		want      string
		holds     []string
	}{
		{
			name: "the person's key for both mailboxes",
			key:  personKey("abcdef01", "usr_0000000000000001", "read", "terms-1", "usr_0000000000000001", 1, 0, 9999999999),
			rows: []string{`INSERT INTO api_key_accounts(key_prefix, account_id)
				VALUES ('abcdef01', 'acc_0000000000000001'), ('abcdef01', 'acc_0000000000000002')`},
			want:  "abcdef01 personal person - 0",
			holds: []string{"acc_0000000000000001 1 0 0", "acc_0000000000000002 1 0 0"},
		},
		{
			name:  "the person's write key for every mailbox of theirs",
			key:   personKey("abcdef01", "usr_0000000000000001", "write", "terms-1", "usr_0000000000000001", 0, 0, 9999999999),
			want:  "abcdef01 personal person-all - 0",
			holds: []string{"acc_0000000000000001 1 1 0", "acc_0000000000000002 1 1 0"},
		},
		{
			name: "an instance key",
			key: `INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at, created_by)
				VALUES ('abcdef01', '$h', 'cli', 'admin', 1, 9999999999, 'cli')`,
			want: "abcdef01 operator - - 0",
		},
		{
			name:  "a revoked key",
			key:   personKey("abcdef01", "usr_0000000000000001", "write", "terms-1", "usr_0000000000000001", 0, 99, 9999999999),
			want:  "abcdef01 personal person-all - 99",
			holds: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openAtVersion(t, 7)
			seedSmallest(t, s)
			migrateTo(t, s, 11)
			execAll(t, s, append([]string{tc.key}, tc.rows...)...)
			before, fts := dumpTables(t, s), ftsAnswers(t, s)
			migrateTo(t, s, 12)
			compareTwelve(t, before, dumpTables(t, s), func(table string, _ map[string]string, col string) bool {
				return table == "api_keys" && col == "user_id"
			}, func(table string, _ map[string]string) bool { return table == "api_key_accounts" })
			checkIntact(t, s, fts)
			if got := queryStrings(t, s, `SELECT k.prefix, w.kind, coalesce(nullif(k.origin, ''), '-'),
				  coalesce(k.user_id, '-'), k.revoked_at
				  FROM api_keys k JOIN workspaces w ON w.id = k.workspace_id`); !slices.Equal(got, []string{tc.want}) {
				t.Errorf("the key after 0012: %v, want %s", got, tc.want)
			}
			if got := queryStrings(t, s, `SELECT account_id, read, act, send FROM key_access ORDER BY 1`); !slices.Equal(got, tc.holds) {
				t.Errorf("what it holds: %v, want %v", got, tc.holds)
			}
			if tc.name == "an instance key" {
				return
			}
			if n := queryInt(t, s, `SELECT count(*) FROM api_key_accounts`); n != 0 {
				t.Errorf("%d restriction rows left for a person's key", n)
			}
		})
	}
}

func TestMigrationTwelveRefusesWhatItsSchemaForbids(t *testing.T) {
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	migrateTo(t, s, 12)
	ctx := context.Background()
	run := func(stmt string) error {
		return s.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, stmt)
			return err
		})
	}
	refused := func(what, stmt string) {
		t.Helper()
		if err := run(stmt); err == nil {
			t.Errorf("%s was accepted", what)
		}
	}
	accepted := func(what, stmt string) {
		t.Helper()
		if err := run(stmt); err != nil {
			t.Errorf("%s was refused: %v", what, err)
		}
	}
	key := func(prefix, ws, scope string) string {
		w := "NULL"
		if ws != "" {
			w = "'" + ws + "'"
		}
		return fmt.Sprintf(`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at, workspace_id, created_by)
			VALUES ('%s', '$h', 'n', '%s', 1, 9999999999, %s, 'usr_0000000000000002')`, prefix, scope, w)
	}
	refused("a key acting as a person", `INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at,
		workspace_id, user_id) VALUES ('a0000001', '$h', 'n', 'read', 1, 2, 'wsp_t1', 'usr_0000000000000002')`)
	refused("a key given a person", `UPDATE api_keys SET user_id = 'usr_0000000000000001' WHERE prefix = 'aaaaaaaa'`)
	refused("a new key with no workspace", key("a0000002", "", "read"))
	refused("a workspace key with the admin scope", key("a0000003", "wsp_t1", "admin"))
	accepted("an operator key with the admin scope", key("a0000004", "wsp_operator", "admin"))
	accepted("a team's send key", key("a0000005", "wsp_t1", "send"))
	accepted("a team's read key", key("a0000006", "wsp_t1", "read"))
	refused("a key moved to another workspace", `UPDATE api_keys SET workspace_id = 'wsp_t2' WHERE prefix = 'a0000006'`)
	refused("a workspace key restricted to accounts", `INSERT INTO api_key_accounts(key_prefix, account_id)
		VALUES ('a0000006', 'acc_t1')`)

	hold := func(prefix, account, ws string, read, act, send int) string {
		return fmt.Sprintf(`INSERT INTO key_access(key_prefix, account_id, workspace_id, read, act, send, created_at, updated_at)
			VALUES ('%s', '%s', '%s', %d, %d, %d, 1, 1)`, prefix, account, ws, read, act, send)
	}
	refused("a mailbox of another workspace", hold("a0000006", "acc_t3", "wsp_t2", 1, 0, 0))
	refused("a mailbox named in the wrong workspace", hold("a0000006", "acc_t3", "wsp_t1", 1, 0, 0))
	refused("an operator key holding a mailbox", hold("a0000004", "acc_0000000000000005", "wsp_operator", 1, 0, 0))
	refused("act on a read key", hold("a0000006", "acc_t1", "wsp_t1", 1, 1, 0))
	refused("send on a read key", hold("a0000006", "acc_t1", "wsp_t1", 1, 0, 1))
	refused("act without read", hold("a0000005", "acc_t1", "wsp_t1", 0, 1, 0))
	refused("a hold of nothing", hold("a0000005", "acc_t1", "wsp_t1", 0, 0, 0))
	accepted("send without read on a send key", hold("a0000005", "acc_t2", "wsp_t1", 0, 0, 1))
	accepted("read, act and send on a send key", hold("a0000005", "acc_t1", "wsp_t1", 1, 1, 1))
	accepted("read on a read key", hold("a0000006", "acc_t1", "wsp_t1", 1, 0, 0))
	refused("act given to a read key", `UPDATE key_access SET act = 1 WHERE key_prefix = 'a0000006'`)
	refused("a hold moved to another mailbox", `UPDATE key_access SET account_id = 'acc_t2' WHERE key_prefix = 'a0000006'`)
	refused("a carried-over key given a mailbox", hold("22222222", "acc_t1", "wsp_t1", 1, 0, 0))

	// A workspace that goes takes its keys, and what they hold, with it.
	accepted("a team's mailbox removed", `DELETE FROM accounts WHERE workspace_id = 'wsp_t2'`)
	accepted("the team deleted", `DELETE FROM workspaces WHERE id = 'wsp_t2'`)
	if n := queryInt(t, s, `SELECT count(*) FROM api_keys WHERE prefix = '33333333'`); n != 0 {
		t.Error("a deleted team's key is still there")
	}
}
