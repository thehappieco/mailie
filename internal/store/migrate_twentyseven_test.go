package store

import (
	"slices"
	"strings"
	"testing"
)

// Migration 0027: a person's sign-out everywhere, a mailbox's first
// connection and what its server is, the marks of a person whose own
// settings changed, and the workspace marks widened to every column a
// workspace's settings show (docs/workspaces.md, "Migration 0027").

func TestMigrationTwentySevenAddsThePersonMarksTheSignOutRecordAndTheFirstConnectionAndChangesNothingElse(t *testing.T) {
	f := newSchemaAt(t, 26)
	seedJournalAt24(t, f)
	// An OAuth mailbox whose consent came long after its row: when it
	// connected is not known, and stays so. A password mailbox with nothing
	// stored: never connected.
	for _, c := range []struct{ id, provider, auth string }{
		{"acc_late", "gmail", "oauth2"}, {"acc_nopw", "imap", "password"},
	} {
		f.accepted("the mailbox "+c.id, f.exec(`INSERT INTO accounts(id, workspace_id, email, provider, auth_kind,
			imap_host, imap_port, smtp_host, smtp_port, smtp_tls, login_user, save_sent_copy, state, state_changed_at,
			created_at, updated_at, owner_user_id) VALUES (?1, (SELECT id FROM workspaces WHERE person_id = ?4),
			?1 || '@example.org', ?2, ?3, 'imap.example.org', 993, 'smtp.example.org', 587, 'starttls', ?1, 0, 'active',
			100, 100, 100, ?4)`, c.id, c.provider, c.auth, "usr_0000000000000001"))
	}
	execAll(t, f.s, `INSERT INTO credentials(account_id, field, keyid, ciphertext, updated_at)
		 VALUES ('acc_late', 'oauth_token', 1, x'0e0f', 5000)`)
	withCredentials := map[string]bool{}
	for _, id := range queryStrings(t, f.s, `SELECT DISTINCT account_id FROM credentials`) {
		withCredentials["string:"+id] = true
	}
	marks := queryStrings(t, f.s, `SELECT workspace_id || ' ' || seq FROM workspace_changes ORDER BY seq`)
	if len(marks) == 0 {
		t.Fatal("the seed marks no workspace; the test would prove nothing of the marks")
	}
	before, schema, fts := dumpTables(t, f.s), schemaSQL(t, f.s), ftsAnswers(t, f.s)
	migrateTo(t, f.s, 27)

	// Nobody signed out everywhere that the schema knows of; a password
	// mailbox with its password connected when it was written; nothing
	// else connected at a known moment; no login recorded a server yet.
	tried := compareMigrated(t, before, dumpTables(t, f.s), map[string]map[string]addedColumn{
		"users": {"signed_out_everywhere_at": sameValue("int64:0")},
		"accounts": {
			"connected_at": func(row map[string]string) string {
				if row["auth_kind"] == "string:password" && withCredentials[row["id"]] {
					return row["created_at"]
				}
				return "int64:0"
			},
			"server_kind": sameValue("string:"),
		},
	}, []string{"person_changes"})
	if tried["users"] < 6 || tried["accounts"] < 10 {
		t.Errorf("the seed tried the migration on %v", tried)
	}
	if got := queryStrings(t, f.s, `SELECT id || ' ' || connected_at FROM accounts WHERE connected_at <> 0 ORDER BY id`); !slices.Equal(got,
		[]string{"acc_0000000000000002 305", "acc_0000000000000005 425"}) {
		t.Errorf("the mailboxes connected at a known moment: %v", got)
	}
	// The migration marks nothing: no person, and no workspace again.
	if got := queryStrings(t, f.s, `SELECT workspace_id || ' ' || seq FROM workspace_changes ORDER BY seq`); !slices.Equal(got, marks) {
		t.Errorf("the workspace marks are %v, and were %v", got, marks)
	}

	now := schemaSQL(t, f.s)
	added, changed, dropped := schemaChanges(schema, now)
	if want := []string{
		"index sqlite_autoindex_person_changes_1", "table person_changes",
		"trigger accounts_person_change", "trigger accounts_person_delete", "trigger credentials_connected",
		"trigger mailbox_events_person_change", "trigger notices_person_delete", "trigger notices_person_insert",
		"trigger user_identities_person_change", "trigger user_identities_person_delete", "trigger users_person_change",
		"trigger workspace_members_person_delete", "trigger workspace_members_person_insert",
		"trigger workspaces_person_delete",
	}; !slices.Equal(added, want) {
		t.Errorf("0027 added\n%v\nwant\n%v", added, want)
	}
	if want := []string{"table accounts", "table users", "trigger accounts_change_update"}; !slices.Equal(changed, want) ||
		len(dropped) != 0 {
		t.Errorf("0027 changed %v and dropped %v", changed, dropped)
	}
	for _, column := range []string{"state", "display_name", "removed_at", "purge_after", "sync_enabled_at", "sync_held_at",
		"sync_enabled_via", "sync_paused_at", "state_reason", "linked_by", "sync_enabled_by", "sync_consent_version",
		"sync_paused_by", "save_sent_copy", "imap_host", "connected_at", "server_kind"} {
		if !strings.Contains(now["trigger accounts_change_update"], "NEW."+column+" IS NOT OLD."+column) {
			t.Errorf("the workspace marks do not watch %s", column)
		}
	}
	if strings.Contains(now["trigger accounts_change_update"], "last_ok_at") {
		t.Error("the workspace marks watch when a mailbox last synced, which is measured")
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

func TestTheSchemaKeepsWhatALoginFoundAndTheNewMomentsToTheirValues(t *testing.T) {
	f := newSchemaLatest(t)
	const box, owner = "acc_0000000000000002", "usr_0000000000000001"
	for _, kind := range []string{"gmail", "other", ""} {
		f.accepted("the server kind "+kind, f.exec(`UPDATE accounts SET server_kind = ? WHERE id = ?`, kind, box))
	}
	f.refused("a server kind outside the list", "CHECK constraint failed",
		f.exec(`UPDATE accounts SET server_kind = 'exchange' WHERE id = ?`, box))
	f.refused("a negative first connection", "CHECK constraint failed",
		f.exec(`UPDATE accounts SET connected_at = -1 WHERE id = ?`, box))
	f.refused("a negative sign-out everywhere", "CHECK constraint failed",
		f.exec(`UPDATE users SET signed_out_everywhere_at = -1 WHERE id = ?`, owner))

	// A credential written once more, by the upsert every writer uses,
	// keeps the moment the first one was written.
	f.accepted("a never-connected mailbox", f.exec(`INSERT INTO accounts(id, workspace_id, email, provider, auth_kind,
		imap_host, imap_port, smtp_host, smtp_port, smtp_tls, login_user, save_sent_copy, state, state_changed_at,
		created_at, updated_at, owner_user_id) VALUES ('acc_new', (SELECT id FROM workspaces WHERE person_id = ?1),
		'new@example.org', 'gmail', 'oauth2', 'imap.gmail.com', 993, 'smtp.gmail.com', 465, 'implicit', 'new', 0,
		'pending_auth', 900, 900, 900, ?1)`, owner))
	upsert := `INSERT INTO credentials(account_id, field, keyid, ciphertext, updated_at) VALUES ('acc_new', 'oauth_token', 1,
		x'01', ?) ON CONFLICT(account_id, field) DO UPDATE SET ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`
	f.accepted("its first credential", f.exec(upsert, 950))
	f.accepted("its token refreshed", f.exec(upsert, 990))
	if got := queryInt(t, f.s, `SELECT connected_at FROM accounts WHERE id = 'acc_new'`); got != 950 {
		t.Errorf("the mailbox connected at %d, want its first credential's 950", got)
	}
}

func TestAWorkspaceIsMarkedWhenAMailboxsReasonAttributionAgreementPauseSentCopyOrServerKindChanges(t *testing.T) {
	// changesFixture: acc_a1 is a mailbox of ext_a, a team from another
	// source, whose owner is Bea; ext_b is another.
	f := changesFixture(t)
	f.accepted("a team's consent", f.exec(`UPDATE accounts SET sync_enabled_at = 900, sync_enabled_by = 'cli',
		sync_consent_version = 'sync-1' WHERE id = 'acc_a1'`))
	f.accepted("a pause", f.exec(`UPDATE accounts SET sync_paused_at = 910, sync_paused_by = 'usr_0000000000000002'
		WHERE id = 'acc_a1'`))
	for _, c := range []struct{ what, set string }{
		{"its state's reason", `state_reason = 'the mail server refused this authorization for the mailbox'`},
		{"who linked it", `linked_by = 'usr_0000000000000002'`},
		{"who linked it forgotten", `linked_by = ''`},
		{"who agreed", `sync_enabled_by = 'usr_0000000000000002'`},
		{"the text agreed to", `sync_consent_version = 'sync-2'`},
		{"who paused it forgotten", `sync_paused_by = ''`},
		{"its sent copy", `save_sent_copy = 0`},
		{"its host", `imap_host = 'imap.mail.example.org'`},
		{"its first connection", `connected_at = 905`},
		{"what its server is", `server_kind = 'other'`},
		{"what its server is now", `server_kind = 'gmail'`},
	} {
		a, b := f.mark("ext_a"), f.mark("ext_b")
		f.accepted(c.what, f.exec(`UPDATE accounts SET `+c.set+` WHERE id = 'acc_a1'`))
		if f.mark("ext_a") <= a || f.mark("ext_b") != b {
			t.Errorf("%s marked ext_a %d (was %d) and ext_b %d (was %d)", c.what, f.mark("ext_a"), a, f.mark("ext_b"), b)
		}
		// Written again as it is, it marks nothing.
		a = f.mark("ext_a")
		f.accepted(c.what+" again", f.exec(`UPDATE accounts SET `+c.set+` WHERE id = 'acc_a1'`))
		if f.mark("ext_a") != a {
			t.Errorf("%s written again as it was moved ext_a's mark", c.what)
		}
	}
}
