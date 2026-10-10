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

// Migration 0011: a team mailbox belongs to its workspace.

// teamAccountRow is an INSERT of a schema-10 account in workspace ws, linked
// by linker (NULL for the operator's).
func teamAccountRow(id, email, ws, linker string, created int) string {
	return fmt.Sprintf(`INSERT INTO accounts(id, workspace_id, email, display_name, provider, auth_kind, imap_host,
	  imap_port, smtp_host, smtp_port, smtp_tls, login_user, save_sent_copy, state, state_reason, state_changed_at,
	  last_ok_at, created_at, updated_at, owner_user_id, oauth_client)
	  VALUES ('%[1]s', '%[3]s', '%[2]s', 'Name of %[1]s', 'imap', 'password', 'imap.example.org', 993,
	  'smtp.example.org', 587, 'starttls', '%[2]s', 1, 'active', '', %[5]d, %[5]d, %[5]d, %[5]d, '%[4]s', 'installed')`,
		id, email, ws, linker, created)
}

// person is an INSERT of a schema-10 person with their personal workspace and
// its membership.
func person(id, email, role, status string, consentAt int, consentVersion string) []string {
	return []string{
		fmt.Sprintf(`INSERT INTO users(id, email, name, password_hash, role, status, password_changed_at, created_at,
		   updated_at, sync_consent_at, sync_consent_version)
		 VALUES ('%[1]s', '%[2]s', 'Name of %[1]s', '$argon2id$p', '%[3]s', '%[4]s', 140, 140, 141, %[5]d, '%[6]s')`,
			id, email, role, status, consentAt, consentVersion),
		fmt.Sprintf(`INSERT INTO workspaces(id, kind, person_id, created_at, updated_at)
		 VALUES ('wsp_p_%[1]s', 'personal', '%[1]s', 140, 140)`, id),
		fmt.Sprintf(`INSERT INTO workspace_members(workspace_id, user_id, role, status, created_at, updated_at)
		 VALUES ('wsp_p_%[1]s', '%[1]s', 'owner', 'active', 140, 140)`, id),
	}
}

// seedTeams is a self-hosted schema-10 database with teams, on top of
// seedSelfHosted: two teams; a team mailbox whose linker is active and
// consented, one whose linker consented and was disabled since, one whose
// linker never consented; owners and admins holding stored manage, and a
// member holding it; invites from creators who could, and could not, make
// them now.
//
//	Support (wsp_t1): Owner (usr_1) owner, Bea (usr_2) admin, Eve (usr_5,
//	  disabled on the instance) admin, Fay (usr_6) member, Cid (usr_3,
//	  disabled on the instance) member
//	Ops (wsp_t2): Dan (usr_4) owner, Bea member
//
//	acc_t1 in Support, linked by Bea: Bea every flag, Fay read+manage, Owner
//	  manage alone
//	acc_t2 in Support, linked by Eve: Eve every flag, Owner read+manage
//	acc_t3 in Ops, linked by Dan (no consent): Dan every flag, Bea every flag
//
// Two invites were sent by people deleted since, which schema 10 left
// pending with no creator.
func seedTeams(t *testing.T, s *Store) {
	t.Helper()
	seedSelfHosted(t, s)
	migrateTo(t, s, 10)
	stmts := append(person("usr_0000000000000005", "eve@example.org", "member", "disabled", 150, "sync-1"),
		person("usr_0000000000000006", "fay@example.org", "member", "active", 0, "")...)
	stmts = append(stmts,
		`INSERT INTO workspaces(id, kind, name, created_at, updated_at)
		 VALUES ('wsp_t1', 'team', 'Support', 160, 160), ('wsp_t2', 'team', 'Ops', 161, 161)`,
		`INSERT INTO workspace_members(workspace_id, user_id, role, status, created_at, updated_at)
		 VALUES ('wsp_t1', 'usr_0000000000000001', 'owner', 'active', 160, 160),
		        ('wsp_t1', 'usr_0000000000000002', 'admin', 'active', 162, 162),
		        ('wsp_t1', 'usr_0000000000000005', 'admin', 'active', 163, 163),
		        ('wsp_t1', 'usr_0000000000000006', 'member', 'active', 164, 164),
		        ('wsp_t1', 'usr_0000000000000003', 'member', 'active', 165, 165),
		        ('wsp_t2', 'usr_0000000000000004', 'owner', 'active', 161, 161),
		        ('wsp_t2', 'usr_0000000000000002', 'member', 'active', 166, 166)`,
		teamAccountRow("acc_t1", "support@example.org", "wsp_t1", "usr_0000000000000002", 170),
		teamAccountRow("acc_t2", "billing@example.org", "wsp_t1", "usr_0000000000000005", 171),
		teamAccountRow("acc_t3", "ops@team.example.org", "wsp_t2", "usr_0000000000000004", 172),
		`INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act, send, manage, granted_by, created_at, updated_at)
		 VALUES ('acc_t1', 'wsp_t1', 'usr_0000000000000002', 1, 1, 1, 1, 'usr_0000000000000002', 170, 170),
		        ('acc_t1', 'wsp_t1', 'usr_0000000000000006', 1, 0, 0, 1, 'usr_0000000000000002', 173, 173),
		        ('acc_t1', 'wsp_t1', 'usr_0000000000000001', 0, 0, 0, 1, 'usr_0000000000000002', 174, 174),
		        ('acc_t2', 'wsp_t1', 'usr_0000000000000005', 1, 1, 1, 1, 'usr_0000000000000005', 171, 171),
		        ('acc_t2', 'wsp_t1', 'usr_0000000000000001', 1, 0, 0, 1, 'usr_0000000000000005', 175, 175),
		        ('acc_t3', 'wsp_t2', 'usr_0000000000000004', 1, 1, 1, 1, 'usr_0000000000000004', 172, 172),
		        ('acc_t3', 'wsp_t2', 'usr_0000000000000002', 1, 1, 1, 1, 'usr_0000000000000004', 176, 176)`,
		`INSERT INTO folders(id, account_id, name, display_name, role, role_source, uidvalidity)
		 VALUES (6, 'acc_t1', 'INBOX', 'Inbox', 'inbox', 'inbox', 21), (7, 'acc_t2', 'INBOX', 'Inbox', 'inbox', 'inbox', 23)`,
		`INSERT INTO messages(account_id, folder_id, uidvalidity, uid, message_id, group_key, subject, from_text,
		   to_text, internal_date, size, first_seen_at, updated_at, body_text)
		 VALUES ('acc_t1', 6, 21, 1, 'g@x', 'mid:g@x', 'Ticket report', 'customer', 'support', 980, 40, 980, 980, 'printer broken'),
		        ('acc_t2', 7, 23, 1, 'h@x', 'mid:h@x', 'Invoice 99', 'vendor', 'billing', 990, 50, 990, 990, 'please pay soon')`,
		`INSERT INTO events(type, account_id, payload_json, created_at)
		 VALUES ('message.new', 'acc_t2', '{"subject":"Invoice 99"}', 991)`,
		`INSERT INTO user_identities(issuer, subject, user_id, created_at)
		 VALUES ('https://accounts.example.com', 'subject-of-fay', 'usr_0000000000000006', 150)`,
		`INSERT INTO identity_key_pins(issuer, subject, key_id, key, pinned_at)
		 VALUES ('https://accounts.example.com', 'subject-of-fay', 'k1', x'0102', 150)`,
		`INSERT INTO oauth_pending(state, account_id, owner_user_id, flow, pkce_verifier, redirect_uri, device_code, expires_at)
		 VALUES ('st-team', 'acc_t3', 'usr_0000000000000004', 'web', 'v3', 'https://console.example/oauth/return', '', 9999999999)`,
		// Invites: whether their creator could make each of them now.
		`INSERT INTO invites(id, code_hash, email, role, created_by, created_at, expires_at, used_at, used_by, workspace_id, workspace_role)
		 VALUES ('inv_disabled_inst', randomblob(32), 'a1@example.org', 'member', 'usr_0000000000000005', 180, 9999999999, 0, '', NULL, ''),
		        ('inv_member_inst', randomblob(32), 'a2@example.org', 'member', 'usr_0000000000000002', 181, 9999999999, 0, '', NULL, ''),
		        ('inv_admin_member', randomblob(32), 'a3@example.org', 'member', 'usr_0000000000000002', 182, 9999999999, 0, '', 'wsp_t1', 'member'),
		        ('inv_admin_admin', randomblob(32), 'a4@example.org', 'member', 'usr_0000000000000002', 183, 9999999999, 0, '', 'wsp_t1', 'admin'),
		        ('inv_member_team', randomblob(32), 'a5@example.org', 'member', 'usr_0000000000000006', 184, 9999999999, 0, '', 'wsp_t1', 'member'),
		        ('inv_owner_team', randomblob(32), 'a6@example.org', 'member', 'usr_0000000000000001', 185, 9999999999, 0, '', 'wsp_t1', 'owner'),
		        ('inv_cli_team', randomblob(32), 'a7@example.org', 'member', 'cli', 186, 9999999999, 0, '', 'wsp_t2', 'admin'),
		        ('inv_key_inst', randomblob(32), 'a8@example.org', 'owner', 'key:aaaaaaaa', 187, 9999999999, 0, '', NULL, ''),
		        ('inv_used', randomblob(32), 'cid@example.org', 'member', 'usr_0000000000000003', 188, 9999999999, 189, 'usr_0000000000000003', 'wsp_t1', 'member'),
		        ('inv_lapsed', randomblob(32), 'a9@example.org', 'member', 'usr_0000000000000005', 190, 191, 0, '', NULL, ''),
		        ('inv_member_ops', randomblob(32), 'b1@example.org', 'member', 'usr_0000000000000002', 192, 9999999999, 0, '', 'wsp_t2', 'member'),
		        -- From people deleted before the upgrade: schema 10 blanked who sent them and left them pending.
		        ('inv_deleted_team', randomblob(32), 'b2@example.org', 'member', '', 193, 9999999999, 0, '', 'wsp_t1', 'owner'),
		        ('inv_deleted_inst', randomblob(32), 'b3@example.org', 'owner', '', 194, 9999999999, 0, '', NULL, '')`,
	)
	execAll(t, s, stmts...)
}

// elevenNewColumns are the columns 0011 adds to the tables that were there.
var elevenNewColumns = map[string][]string{
	"accounts": {"linked_by", "sync_consent_version", "sync_enabled_via"},
}

// compareEleven checks that every table of before is in after, row for row
// and value for value, but for the columns allowed to change: a function of
// the table, the row as it was, and the column.
func compareEleven(t *testing.T, before, after tableDump, mayChange func(table string, row map[string]string, col string) bool, gone func(table string, row map[string]string) bool) {
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
			want := slices.Clone(elevenNewColumns[table])
			slices.Sort(want)
			if !slices.Equal(extra, want) {
				t.Errorf("%s gained columns %v, want %v", table, extra, want)
			}
		}
	}
}

// checkIntact runs the checks every migration must pass: the full-text index
// answers as before and is intact, and SQLite finds nothing broken.
func checkIntact(t *testing.T, s *Store, ftsBefore map[string][]string) {
	t.Helper()
	if got := ftsAnswers(t, s); fmt.Sprint(got) != fmt.Sprint(ftsBefore) {
		t.Errorf("full-text answers were %v, are %v", ftsBefore, got)
	}
	if _, err := s.Writer().ExecContext(context.Background(), `INSERT INTO messages_fts(messages_fts) VALUES ('integrity-check')`); err != nil {
		t.Errorf("full-text integrity-check: %v", err)
	}
	if got := queryStrings(t, s, `PRAGMA integrity_check`); !slices.Equal(got, []string{"ok"}) {
		t.Errorf("integrity_check: %v", got)
	}
	if got := queryStrings(t, s, `PRAGMA foreign_key_check`); len(got) != 0 {
		t.Errorf("foreign_key_check: %v", got)
	}
	if fk := queryInt(t, s, `PRAGMA foreign_keys`); fk != 1 {
		t.Errorf("foreign_keys is %d after the migration, want 1", fk)
	}
}

func TestMigrationElevenMakesTheLinkersConsentTheWorkspacesAndLeavesPersonalMailboxesAlone(t *testing.T) {
	t.Run("the smallest shape", func(t *testing.T) {
		// One owner and the two mailboxes they connected, as the hosted
		// service holds them: nothing changes but the new columns and the
		// owner's stored manage, which their role in their personal
		// workspace gives them now.
		s := openAtVersion(t, 7)
		seedSmallest(t, s)
		migrateTo(t, s, 10)
		before, fts := dumpTables(t, s), ftsAnswers(t, s)
		migrateTo(t, s, 11)
		compareEleven(t, before, dumpTables(t, s), func(table string, _ map[string]string, col string) bool {
			return table == "mailbox_access" && col == "manage"
		}, nil)
		checkIntact(t, s, fts)
		if got := queryStrings(t, s, `SELECT a.id, w.kind, a.owner_user_id, a.linked_by, a.sync_enabled_at,
			  a.sync_enabled_by, a.sync_consent_version, a.sync_enabled_via, g.read, g.act, g.send, g.manage
			  FROM accounts a JOIN workspaces w ON w.id = a.workspace_id JOIN mailbox_access g ON g.account_id = a.id
			 ORDER BY a.rowid`); !slices.Equal(got, []string{
			"acc_0000000000000001 personal usr_0000000000000001 usr_0000000000000001 0    1 1 1 0",
			"acc_0000000000000002 personal usr_0000000000000001 usr_0000000000000001 0    1 1 1 0",
		}) {
			t.Errorf("mailboxes after 0011:\n%s", strings.Join(got, "\n"))
		}
	})

	t.Run("a self-hosted shape with teams", func(t *testing.T) {
		s := openAtVersion(t, 7)
		seedTeams(t, s)
		before, fts := dumpTables(t, s), ftsAnswers(t, s)
		for table, rows := range before {
			if len(rows) == 0 {
				t.Fatalf("the seed left %s empty", table)
			}
		}
		started := time.Now().Unix()
		migrateTo(t, s, 11)

		teamRow := func(row map[string]string) bool {
			return strings.Contains(row["workspace_id"], "wsp_t")
		}
		expired := map[string]bool{
			"string:inv_disabled_inst": true, "string:inv_member_inst": true, "string:inv_admin_admin": true,
			"string:inv_member_team": true, "string:inv_member_ops": true,
			"string:inv_deleted_team": true, "string:inv_deleted_inst": true,
		}
		compareEleven(t, before, dumpTables(t, s), func(table string, row map[string]string, col string) bool {
			switch table {
			case "accounts":
				return teamRow(row) && (col == "owner_user_id" || col == "sync_enabled_at" || col == "sync_enabled_by")
			case "mailbox_access":
				return col == "manage"
			case "invites":
				return col == "expires_at" && expired[row["id"]]
			}
			return false
		}, func(table string, row map[string]string) bool {
			// A grant of manage alone to an owner or an admin: the role
			// gives it now.
			return table == "mailbox_access" && row["read"] == "int64:0" && row["act"] == "int64:0" &&
				row["send"] == "int64:0" && row["account_id"] == "string:acc_t1" && row["user_id"] == "string:usr_0000000000000001"
		})
		checkIntact(t, s, fts)

		// Every mailbox: personal ones name their person, team ones name
		// nobody; the linker is attribution; the consent is the
		// workspace's where the linker was active and consented, bound to
		// them; kept bound and off where they are disabled.
		if got := queryStrings(t, s, `SELECT a.id, w.kind, coalesce(a.owner_user_id, '-'), a.linked_by,
			  a.sync_enabled_at, a.sync_enabled_by, a.sync_consent_version, a.sync_enabled_via
			  FROM accounts a JOIN workspaces w ON w.id = a.workspace_id ORDER BY a.rowid`); !slices.Equal(got, []string{
			"acc_0000000000000001 personal usr_0000000000000001 usr_0000000000000001 0   ",
			"acc_0000000000000002 personal usr_0000000000000001 usr_0000000000000001 0   ",
			"acc_0000000000000003 personal usr_0000000000000002 usr_0000000000000002 0   ",
			"acc_0000000000000004 personal usr_0000000000000003 usr_0000000000000003 0   ",
			"acc_0000000000000005 operator -  427 cli  ",
			"acc_0000000000000006 operator -  437 cli  ",
			"acc_t1 team - usr_0000000000000002 112 usr_0000000000000002 sync-1 migration",
			"acc_t2 team - usr_0000000000000005 0 usr_0000000000000005  migration",
			"acc_t3 team - usr_0000000000000004 0   ",
		}) {
			t.Errorf("mailboxes after 0011:\n%s", strings.Join(got, "\n"))
		}

		// Stored manage stays for members only; a grant of manage alone to
		// an owner went.
		if got := queryStrings(t, s, `SELECT g.account_id, g.user_id, m.role, g.read, g.act, g.send, g.manage, g.granted_by
			  FROM mailbox_access g JOIN workspace_members m ON m.workspace_id = g.workspace_id AND m.user_id = g.user_id
			 WHERE g.account_id LIKE 'acc_t%' ORDER BY g.rowid`); !slices.Equal(got, []string{
			"acc_t1 usr_0000000000000002 admin 1 1 1 0 usr_0000000000000002",
			"acc_t1 usr_0000000000000006 member 1 0 0 1 usr_0000000000000002",
			"acc_t2 usr_0000000000000005 admin 1 1 1 0 usr_0000000000000005",
			"acc_t2 usr_0000000000000001 owner 1 0 0 0 usr_0000000000000005",
			"acc_t3 usr_0000000000000004 owner 1 1 1 0 usr_0000000000000004",
			"acc_t3 usr_0000000000000002 member 1 1 1 1 usr_0000000000000004",
		}) {
			t.Errorf("team grants after 0011:\n%s", strings.Join(got, "\n"))
		}

		// The invites their creator could not make now are expired, at
		// the migration; the others are as they were.
		if got := queryStrings(t, s, `SELECT id FROM invites
			 WHERE used_at = 0 AND expires_at BETWEEN ? AND ? ORDER BY id`, started, time.Now().Unix()); !slices.Equal(got, []string{
			"inv_admin_admin", "inv_deleted_inst", "inv_deleted_team", "inv_disabled_inst", "inv_member_inst",
			"inv_member_ops", "inv_member_team",
		}) {
			t.Errorf("invites expired by 0011: %v", got)
		}
		if n := queryInt(t, s, `SELECT count(*) FROM invites WHERE id IN ('inv_admin_member', 'inv_owner_team',
			'inv_cli_team', 'inv_key_inst', 'inv_used') AND expires_at = 9999999999`); n != 5 {
			t.Errorf("%d of the five invites their creator could still make kept their expiry", n)
		}

		// Nothing of the index went: the kept index of the disabled linker's
		// mailbox is there, and so are its events.
		if n := queryInt(t, s, `SELECT count(*) FROM messages WHERE account_id IN ('acc_t1', 'acc_t2')`); n != 2 {
			t.Errorf("%d team messages after 0011, want 2", n)
		}
		// The consent attempt Dan started on Ops's mailbox is his still: he
		// manages it as Ops's owner.
		if n := queryInt(t, s, `SELECT count(*) FROM oauth_pending WHERE state = 'st-team' AND owner_user_id = 'usr_0000000000000004'`); n != 1 {
			t.Error("the team's consent attempt went")
		}
	})
}

func TestMigrationElevenLeavesATeamMailboxOfADisabledLinkerStoppedWithItsIndex(t *testing.T) {
	s := openAtVersion(t, 7)
	seedTeams(t, s)
	migrateTo(t, s, 11)
	// The daemon reads it at the latest schema, whose later migrations change
	// nothing of this.
	migrateTo(t, s, latestVersion(t))
	// Its consent was its linker's, who is disabled: nothing is copied, so
	// it syncs nothing, and its index stays as it was, bound to them.
	if ok, err := s.SyncEligible(context.Background(), "acc_t2"); err != nil || ok {
		t.Errorf("the disabled linker's team mailbox is eligible to sync: %v, %v", ok, err)
	}
	if n := queryInt(t, s, `SELECT count(*) FROM messages WHERE account_id = 'acc_t2'`); n != 1 {
		t.Errorf("its index holds %d messages, want the 1 it had", n)
	}
	kept, unread, err := s.TeamSyncNotices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kept, []string{"acc_t2"}) {
		t.Errorf("the daemon would list %v as kept stopped, want acc_t2", kept)
	}
	if len(unread) != 0 {
		t.Errorf("the daemon would list %v as read by nobody", unread)
	}
	// The active, consented linker's keeps syncing, under the team's
	// consent now; the one whose linker never consented does not.
	if ok, err := s.SyncEligible(context.Background(), "acc_t1"); err != nil || !ok {
		t.Errorf("the consented linker's team mailbox stopped: %v, %v", ok, err)
	}
	if ok, err := s.SyncEligible(context.Background(), "acc_t3"); err != nil || ok {
		t.Errorf("a team mailbox nobody consented to syncs: %v, %v", ok, err)
	}
}

func TestMigrationElevenKeepsADisabledLinkersOwnMailboxBoundAndListsItAsReadByNobody(t *testing.T) {
	// Billing2 in Support was linked by Eve, who alone read it, and who is
	// disabled: schema 10 let her go without force, since nobody else could
	// read it. The upgrade keeps it stopped with its index, bound to her, so
	// that deleting her still deletes the index only she read; at start it
	// is listed as read by nobody, which can only be removed, never as one
	// to turn on again.
	s := openAtVersion(t, 7)
	seedTeams(t, s)
	execAll(t, s,
		teamAccountRow("acc_t4", "billing2@example.org", "wsp_t1", "usr_0000000000000005", 177),
		`INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act, send, manage, granted_by, created_at, updated_at)
		 VALUES ('acc_t4', 'wsp_t1', 'usr_0000000000000005', 1, 1, 1, 1, 'usr_0000000000000005', 177, 177)`,
		`INSERT INTO folders(id, account_id, name, display_name, role, role_source, uidvalidity)
		 VALUES (8, 'acc_t4', 'INBOX', 'Inbox', 'inbox', 'inbox', 24)`,
		`INSERT INTO messages(account_id, folder_id, uidvalidity, uid, message_id, group_key, subject, from_text,
		   to_text, internal_date, size, first_seen_at, updated_at, body_text)
		 VALUES ('acc_t4', 8, 24, 1, 'i@x', 'mid:i@x', 'Invoice 100', 'vendor', 'billing2', 995, 50, 995, 995, 'pay')`)
	migrateTo(t, s, 11)
	// The daemon reads it at the latest schema, whose later migrations change
	// nothing of this.
	migrateTo(t, s, latestVersion(t))

	if got := queryStrings(t, s, `SELECT coalesce(owner_user_id, '-'), linked_by, sync_enabled_at, sync_enabled_by,
		  sync_enabled_via FROM accounts WHERE id = 'acc_t4'`); !slices.Equal(got, []string{
		"- usr_0000000000000005 0 usr_0000000000000005 migration",
	}) {
		t.Errorf("the mailbox only Eve read, after 0011: %v", got)
	}
	if ok, err := s.SyncEligible(context.Background(), "acc_t4"); err != nil || ok {
		t.Errorf("it is eligible to sync: %v, %v", ok, err)
	}
	if n := queryInt(t, s, `SELECT count(*) FROM messages WHERE account_id = 'acc_t4'`); n != 1 {
		t.Errorf("its index holds %d messages, want the 1 it had", n)
	}
	kept, unread, err := s.TeamSyncNotices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kept, []string{"acc_t2"}) || !slices.Equal(unread, []string{"acc_t4"}) {
		t.Errorf("at start: kept stopped %v (want acc_t2, which the owner reads), read by nobody %v (want acc_t4)", kept, unread)
	}
}

func TestMigrationElevenRefusesAPersonalMailboxThatNamesAnotherPerson(t *testing.T) {
	s := openAtVersion(t, 7)
	seedTeams(t, s)
	// Schema 10 allowed it in a personal workspace (it only forbade a
	// linker in the operator's); no code wrote one, but if a row says so,
	// the migration stops rather than guess whose mailbox it is.
	execAll(t, s, `UPDATE accounts SET owner_user_id = 'usr_0000000000000002' WHERE id = 'acc_0000000000000002'`)
	before := dumpTables(t, s)
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var eleven migration
	for _, m := range migrations {
		if m.version == 11 {
			eleven = m
		}
	}
	if err := s.applyMigration(context.Background(), eleven); err == nil ||
		!strings.Contains(err.Error(), "names a person exactly when") {
		t.Fatalf("0011 over a personal mailbox naming another person: %v", err)
	}
	if v, err := s.SchemaVersion(context.Background()); err != nil || v != 10 {
		t.Errorf("user_version = %d (%v) after the refused migration, want 10", v, err)
	}
	if after := dumpTables(t, s); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Error("the refused migration changed something")
	}
}

func TestMigrationElevenTakesStoredManageFromOwnersAndAdminsOnly(t *testing.T) {
	s := openAtVersion(t, 7)
	seedTeams(t, s)
	migrateTo(t, s, 11)
	if n := queryInt(t, s, `SELECT count(*) FROM mailbox_access g JOIN workspace_members m
		ON m.workspace_id = g.workspace_id AND m.user_id = g.user_id WHERE g.manage = 1 AND m.role IN ('owner', 'admin')`); n != 0 {
		t.Errorf("%d owners and admins still hold a stored manage", n)
	}
	if got := queryStrings(t, s, `SELECT g.account_id, g.user_id FROM mailbox_access g WHERE g.manage = 1 ORDER BY 1, 2`); !slices.Equal(got, []string{
		"acc_t1 usr_0000000000000006", "acc_t3 usr_0000000000000002",
	}) {
		t.Errorf("stored manage after 0011: %v, want the two members'", got)
	}
}

func TestMigrationElevenExpiresTheInvitesTheirCreatorCouldNotMakeNow(t *testing.T) {
	s := openAtVersion(t, 7)
	seedTeams(t, s)
	migrateTo(t, s, 11)
	got := queryStrings(t, s, `SELECT id, expires_at <= ? FROM invites WHERE length(id) <> 20 ORDER BY id`,
		time.Now().Unix())
	want := []string{
		"inv_admin_admin 1",   // an admin inviting an admin
		"inv_admin_member 0",  // an admin inviting a member, still one
		"inv_cli_team 0",      // the operator's
		"inv_deleted_inst 1",  // an instance invite from someone deleted since
		"inv_deleted_team 1",  // a team invite from someone deleted since
		"inv_disabled_inst 1", // from someone disabled on the instance
		"inv_key_inst 0",      // an instance key's
		"inv_lapsed 1",        // expired before, and untouched
		"inv_member_inst 1",   // an instance invite from someone who is no instance owner
		"inv_member_ops 1",    // a team invite from a plain member of the team
		"inv_member_team 1",   // the same, in another team
		"inv_owner_team 0",    // the team's owner's
		"inv_used 0",          // used: the record of how someone arrived
	}
	if !slices.Equal(got, want) {
		t.Errorf("invites after 0011:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if n := queryInt(t, s, `SELECT count(*) FROM invites WHERE id = 'inv_lapsed' AND expires_at = 191`); n != 1 {
		t.Error("an invite already expired had its expiry moved")
	}
}

func TestMigrationElevenRefusesWhatItsSchemaForbids(t *testing.T) {
	s := openAtVersion(t, 7)
	seedTeams(t, s)
	migrateTo(t, s, 11)
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
	link := func(id, ws, owner string) string {
		return fmt.Sprintf(`INSERT INTO accounts(id, workspace_id, email, provider, auth_kind, imap_host, imap_port,
		  smtp_host, smtp_port, smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at, owner_user_id)
		  VALUES ('%s', '%s', '%s@example.org', 'gmail', 'oauth2', 'h', 993, 'h', 465, 'implicit', 'x', 0, 'active', 1, 1, 1, %s)`,
			id, ws, id, owner)
	}
	refused("a team mailbox naming a person", link("acc_a", "wsp_t1", "'usr_0000000000000001'"))
	accepted("a team mailbox naming nobody", link("acc_b", "wsp_t1", "NULL"))
	refused("a personal mailbox naming nobody", link("acc_c", "wsp_p_usr_0000000000000006", "NULL"))
	refused("a personal mailbox naming another person", link("acc_d", "wsp_p_usr_0000000000000006", "'usr_0000000000000002'"))
	accepted("a personal mailbox naming its person", link("acc_e", "wsp_p_usr_0000000000000006", "'usr_0000000000000006'"))
	refused("an operator mailbox naming a person", link("acc_f", "wsp_operator", "'usr_0000000000000001'"))
	refused("a team mailbox given a person", `UPDATE accounts SET owner_user_id = 'usr_0000000000000002' WHERE id = 'acc_t1'`)
	refused("a personal mailbox given another person", `UPDATE accounts SET owner_user_id = 'usr_0000000000000002' WHERE id = 'acc_e'`)
	refused("a sync source the schema does not know", `UPDATE accounts SET sync_enabled_via = 'console' WHERE id = 'acc_t1'`)

	grant := func(account, ws, user string, read, manage int) string {
		return fmt.Sprintf(`INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, manage, created_at, updated_at)
			VALUES ('%s', '%s', '%s', %d, %d, 1, 1)`, account, ws, user, read, manage)
	}
	refused("manage stored for an owner", grant("acc_b", "wsp_t1", "usr_0000000000000001", 1, 1))
	refused("manage stored for an admin", grant("acc_b", "wsp_t1", "usr_0000000000000002", 0, 1))
	accepted("manage stored for a member", grant("acc_b", "wsp_t1", "usr_0000000000000006", 0, 1))
	accepted("read for an owner", grant("acc_b", "wsp_t1", "usr_0000000000000001", 1, 0))
	refused("manage given to an owner's grant", `UPDATE mailbox_access SET manage = 1
		WHERE account_id = 'acc_b' AND user_id = 'usr_0000000000000001'`)

	// A member made an admin manages by the role: their stored manage goes,
	// and a grant of manage alone goes with it.
	accepted("a member promoted", `UPDATE workspace_members SET role = 'admin'
		WHERE workspace_id = 'wsp_t1' AND user_id = 'usr_0000000000000006'`)
	if got := queryStrings(t, s, `SELECT account_id, read, manage FROM mailbox_access
		WHERE user_id = 'usr_0000000000000006' ORDER BY account_id`); !slices.Equal(got, []string{"acc_t1 1 0"}) {
		t.Errorf("the promoted member's grants: %v, want read kept on acc_t1 and nothing on acc_b", got)
	}
}
