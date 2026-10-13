package store

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// Migration 0019: closed teams (docs/workspaces.md, "Closed teams").

func TestMigrationNineteenAddsTheClosedTeamsAndChangesNothingElse(t *testing.T) {
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	migrateTo(t, s, 18)
	before, objects := dumpTables(t, s), queryStrings(t, s, schemaObjectsSQL)
	triggers := queryStrings(t, s, schemaTriggersSQL)
	migrateTo(t, s, 19)
	after := dumpTables(t, s)

	if len(after) != len(before) {
		t.Errorf("0019 left %d tables, want the %d there were", len(after), len(before))
	}
	teams := 0
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
			if table == "workspaces" {
				want = []string{"closed_at"}
				// Nothing was closed before: every team is open.
				if at := got[i]["closed_at"]; at != "int64:0" {
					t.Errorf("workspace %s is closed since %s", row["id"], at)
				}
				if row["kind"] == "string:team" {
					teams++
				}
			}
			if !slices.Equal(added, want) {
				t.Errorf("%s row %d gained the columns %v, want %v", table, i, added, want)
			}
		}
	}
	if teams == 0 {
		t.Error("the seed has no team: the migration was not tried on one")
	}
	var added []string
	for _, o := range queryStrings(t, s, schemaObjectsSQL) {
		if !slices.Contains(objects, o) {
			added = append(added, o)
		}
	}
	want := []string{
		"trigger accounts_closed_team", "trigger api_keys_closed_team", "trigger invites_closed_team",
		"trigger key_access_closed_team", "trigger key_access_closed_team_flags", "trigger mailbox_access_closed_team",
		"trigger mailbox_access_closed_team_flags", "trigger mailbox_grants_closed_team", "trigger mailbox_keys_closed_team",
	}
	if !slices.Equal(added, want) {
		t.Errorf("0019 added %v, want %v", added, want)
	}
	// Every trigger that was there is as it was.
	now := queryStrings(t, s, schemaTriggersSQL)
	for _, trigger := range triggers {
		if !slices.Contains(now, trigger) {
			t.Errorf("0019 changed or dropped the trigger %s", trigger)
		}
	}
}

func TestTheSchemaGivesNothingInAClosedTeam(t *testing.T) {
	// The seed at schema 19: wsp_t1 is a local team (Owner usr_1 its owner,
	// usr_2 an admin, Fay usr_6 and usr_3 members) with the mailboxes acc_t1
	// and acc_t2; Fay holds read and manage on acc_t1. wsp_t2 is another
	// team, with acc_t3.
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	migrateTo(t, s, 19)
	ctx := context.Background()
	exec := func(query string, args ...any) error {
		return s.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, query, args...)
			return err
		})
	}
	const why = "nothing is linked, granted, keyed or invited in a closed team"
	refused := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: %v, want the closed team's refusal", what, err)
		}
	}
	accepted := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
	checkRefused := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "CHECK") {
			t.Errorf("%s: %v, want the CHECK's refusal", what, err)
		}
	}

	// Only a team closes.
	checkRefused("the operator workspace closed", exec(`UPDATE workspaces SET closed_at = 900 WHERE id = 'wsp_operator'`))
	checkRefused("a personal workspace closed",
		exec(`UPDATE workspaces SET closed_at = 900 WHERE person_id = 'usr_0000000000000001'`))

	// While wsp_t1 is open: a key of it holding read and act on acc_t1, and
	// acc_t1's first mailbox key.
	accepted("a key of the team", exec(`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at,
		workspace_id, terms_version, created_by) VALUES ('abcd0001', '$argon2id$x', 'Bot', 'write', 500, 9999999999,
		'wsp_t1', 'terms-1', 'usr_0000000000000001')`))
	accepted("what the key holds", exec(`INSERT INTO key_access(key_prefix, account_id, workspace_id, read, act, send,
		granted_by, created_at, updated_at) VALUES ('abcd0001', 'acc_t1', 'wsp_t1', 1, 1, 0, 'usr_0000000000000001', 500, 500)`))
	accepted("acc_t1's first key", exec(`INSERT INTO mailbox_keys(account_id, epoch, public_key, namespace, created_by,
		created_at) VALUES ('acc_t1', 1, randomblob(32), '11111111-2222-4333-8444-555555555555', 'usr_0000000000000001', 500)`))

	accepted("wsp_t1 closed", exec(`UPDATE workspaces SET closed_at = 900 WHERE id = 'wsp_t1'`))

	// Nothing linked.
	refused("a mailbox linked into it", exec(teamAccountRow("acc_t9", "new@example.org", "wsp_t1", "usr_0000000000000001", 950)))
	// Nothing granted: no grant, not even one written by an upsert that
	// would only lower flags, and no flag going from 0 to 1.
	refused("a grant to a member", exec(`INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act, send,
		manage, granted_by, created_at, updated_at)
		VALUES ('acc_t1', 'wsp_t1', 'usr_0000000000000003', 1, 0, 0, 0, 'usr_0000000000000001', 950, 950)`))
	refused("a grant written by an upsert", exec(`INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act,
		send, manage, granted_by, created_at, updated_at)
		VALUES ('acc_t1', 'wsp_t1', 'usr_0000000000000006', 1, 0, 0, 0, 'usr_0000000000000001', 950, 950)
		ON CONFLICT(account_id, user_id) DO UPDATE SET manage = excluded.manage`))
	refused("a flag given", exec(`UPDATE mailbox_access SET act = 1
		WHERE account_id = 'acc_t1' AND user_id = 'usr_0000000000000006'`))
	refused("a grant sealed with the mailbox key", exec(`INSERT INTO mailbox_grants(account_id, workspace_id, user_id,
		epoch, grant, granted_by, created_at)
		VALUES ('acc_t1', 'wsp_t1', 'usr_0000000000000002', 1, x'4d4c' || randomblob(86), 'usr_0000000000000001', 950)`))
	// Nothing keyed: no mailbox key, no API key, nothing given to one.
	refused("a mailbox's next key", exec(`INSERT INTO mailbox_keys(account_id, epoch, public_key, namespace, created_by,
		created_at) VALUES ('acc_t1', 2, randomblob(32), '11111111-2222-4333-8444-555555555555', 'usr_0000000000000001', 950)`))
	refused("a key of the team", exec(`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at,
		workspace_id, terms_version, created_by) VALUES ('abcd0002', '$argon2id$x', 'Bot', 'read', 950, 9999999999,
		'wsp_t1', 'terms-1', 'usr_0000000000000001')`))
	refused("a mailbox given to a key", exec(`INSERT INTO key_access(key_prefix, account_id, workspace_id, read, act,
		send, granted_by, created_at, updated_at)
		VALUES ('abcd0001', 'acc_t2', 'wsp_t1', 1, 0, 0, 'usr_0000000000000001', 950, 950)`))
	accepted("a key's act taken", exec(`UPDATE key_access SET act = 0 WHERE key_prefix = 'abcd0001' AND account_id = 'acc_t1'`))
	refused("a key's flag given", exec(`UPDATE key_access SET act = 1 WHERE key_prefix = 'abcd0001' AND account_id = 'acc_t1'`))
	// Nobody invited.
	refused("an invite into it", exec(`INSERT INTO invites(id, code_hash, email, role, created_by, created_at, expires_at,
		workspace_id, workspace_role)
		VALUES ('inv_closed', randomblob(32), 'c1@example.org', 'member', 'usr_0000000000000001', 950, 9999999999, 'wsp_t1', 'member')`))

	// What takes something away is allowed, and so is every change to the
	// memberships.
	accepted("a flag taken", exec(`UPDATE mailbox_access SET manage = 0
		WHERE account_id = 'acc_t1' AND user_id = 'usr_0000000000000006'`))
	accepted("a key's mailbox taken out of it", exec(`DELETE FROM key_access WHERE key_prefix = 'abcd0001'`))
	accepted("a key revoked", exec(`UPDATE api_keys SET revoked_at = 950, stop_reason = 'manual' WHERE prefix = 'abcd0001'`))
	accepted("a member added", exec(`INSERT INTO workspace_members(workspace_id, user_id, role, status, created_at, updated_at)
		VALUES ('wsp_t1', 'usr_0000000000000004', 'member', 'active', 950, 950)`))
	accepted("a member made an admin", exec(`UPDATE workspace_members SET role = 'admin'
		WHERE workspace_id = 'wsp_t1' AND user_id = 'usr_0000000000000004'`))
	accepted("a member removed", exec(`DELETE FROM workspace_members
		WHERE workspace_id = 'wsp_t1' AND user_id = 'usr_0000000000000003'`))
	accepted("a grant deleted", exec(`DELETE FROM mailbox_access WHERE account_id = 'acc_t1' AND user_id = 'usr_0000000000000006'`))
	accepted("a mailbox removed", exec(`DELETE FROM accounts WHERE id = 'acc_t2'`))
	// Another team, and the instance, are not closed.
	accepted("an invite into another team", exec(`INSERT INTO invites(id, code_hash, email, role, created_by, created_at,
		expires_at, workspace_id, workspace_role)
		VALUES ('inv_open', randomblob(32), 'c2@example.org', 'member', 'usr_0000000000000004', 950, 9999999999, 'wsp_t2', 'member')`))
	accepted("an invite to the instance", exec(`INSERT INTO invites(id, code_hash, email, role, created_by, created_at,
		expires_at) VALUES ('inv_instance', randomblob(32), 'c3@example.org', 'member', 'cli', 950, 9999999999)`))

	// Reopened, it is given things again.
	accepted("wsp_t1 reopened", exec(`UPDATE workspaces SET closed_at = 0 WHERE id = 'wsp_t1'`))
	accepted("a grant once it reopened", exec(`INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act,
		send, manage, granted_by, created_at, updated_at)
		VALUES ('acc_t1', 'wsp_t1', 'usr_0000000000000004', 1, 0, 0, 0, 'usr_0000000000000001', 990, 990)`))
	accepted("an invite once it reopened", exec(`INSERT INTO invites(id, code_hash, email, role, created_by, created_at,
		expires_at, workspace_id, workspace_role)
		VALUES ('inv_reopened', randomblob(32), 'c4@example.org', 'member', 'usr_0000000000000001', 990, 9999999999, 'wsp_t1', 'member')`))
}
