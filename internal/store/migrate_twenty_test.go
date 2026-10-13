package store

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// Migration 0020: a removed mailbox, kept for 30 days, and the journal of
// what was done to mailboxes, with its notices (docs/workspaces.md,
// "Removed mailboxes").

// thirtyDays is how long a removed mailbox is kept, at least, in seconds.
const thirtyDays = 30 * 24 * 60 * 60

func TestMigrationTwentyAddsTheRemovalAndTheJournalAndChangesNothingElse(t *testing.T) {
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	migrateTo(t, s, 19)
	before, objects := dumpTables(t, s), queryStrings(t, s, schemaObjectsSQL)
	triggers := queryStrings(t, s, schemaTriggersSQL)
	fts := ftsAnswers(t, s)
	migrateTo(t, s, 20)
	after := dumpTables(t, s)

	newTables := []string{"mailbox_event_readers", "mailbox_events", "notices"}
	if len(after) != len(before)+len(newTables) {
		t.Errorf("0020 left %d tables, want the %d there were and %v", len(after), len(before), newTables)
	}
	for _, table := range newTables {
		if rows, ok := after[table]; !ok || len(rows) != 0 {
			t.Errorf("0020 made %s with %d rows (%t), want it empty", table, len(rows), ok)
		}
	}
	mailboxes := 0
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
			slices.Sort(added)
			want := []string(nil)
			if table == "accounts" {
				want = []string{"purge_after", "removed_at", "removed_by", "sync_held_at"}
				// Nothing was removed before, and nothing holds a sync:
				// every mailbox is live.
				for col, live := range map[string]string{
					"removed_at": "int64:0", "removed_by": "string:", "purge_after": "int64:0", "sync_held_at": "int64:0",
				} {
					if got[i][col] != live {
						t.Errorf("mailbox %s has %s %s", row["id"], col, got[i][col])
					}
				}
				mailboxes++
			}
			if !slices.Equal(added, want) {
				t.Errorf("%s row %d gained the columns %v, want %v", table, i, added, want)
			}
		}
	}
	if mailboxes == 0 {
		t.Error("the seed has no mailbox: the migration was not tried on one")
	}
	var added []string
	for _, o := range queryStrings(t, s, schemaObjectsSQL) {
		if !slices.Contains(objects, o) {
			added = append(added, o)
		}
	}
	want := []string{
		"index accounts_purge", "index mailbox_event_readers_user", "index mailbox_events_account",
		"index mailbox_events_created", "index notices_event", "index sqlite_autoindex_mailbox_event_readers_1",
		"index sqlite_autoindex_mailbox_events_1", "index sqlite_autoindex_notices_1",
		"table mailbox_event_readers", "table mailbox_events", "table notices",
		"trigger accounts_linked_live", "trigger accounts_removal_fixed", "trigger accounts_removal_whole",
		"trigger api_key_accounts_removed", "trigger key_access_removed", "trigger key_access_removed_flags",
		"trigger mailbox_access_removed", "trigger mailbox_access_removed_flags", "trigger mailbox_event_readers_fixed",
		"trigger mailbox_events_written_once", "trigger mailbox_grants_removed", "trigger mailbox_keys_removed",
		"trigger notices_fixed", "trigger notices_to_readers",
	}
	if !slices.Equal(added, want) {
		t.Errorf("0020 added\n%v\nwant\n%v", added, want)
	}
	// Every trigger that was there is as it was.
	now := queryStrings(t, s, schemaTriggersSQL)
	for _, trigger := range triggers {
		if !slices.Contains(now, trigger) {
			t.Errorf("0020 changed or dropped the trigger %s", trigger)
		}
	}
	if got := ftsAnswers(t, s); !mapsEqual(got, fts) {
		t.Errorf("the full-text index answers %v, and answered %v", got, fts)
	}
}

// mapsEqual compares two maps of string lists.
func mapsEqual(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !slices.Equal(v, b[k]) {
			return false
		}
	}
	return true
}

// schemaTwenty is the seed at schema 20, with a way to run one statement and
// to say what the schema did with it.
type schemaTwenty struct {
	t *testing.T
	s *Store
}

func newSchemaTwenty(t *testing.T) schemaTwenty {
	t.Helper()
	s := openAtVersion(t, 7)
	seedKeys(t, s)
	migrateTo(t, s, 20)
	return schemaTwenty{t: t, s: s}
}

func (f schemaTwenty) exec(query string, args ...any) error {
	ctx := context.Background()
	return f.s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, query, args...)
		return err
	})
}

func (f schemaTwenty) refused(what, why string, err error) {
	f.t.Helper()
	if err == nil || !strings.Contains(err.Error(), why) {
		f.t.Errorf("%s: %v, want the refusal %q", what, err, why)
	}
}

func (f schemaTwenty) accepted(what string, err error) {
	f.t.Helper()
	if err != nil {
		f.t.Errorf("%s: %v", what, err)
	}
}

// remove removes a mailbox at at, as the repository does, for 30 days.
func (f schemaTwenty) remove(accountID, by string, at int64) {
	f.t.Helper()
	if err := f.exec(`UPDATE accounts SET removed_at = ?, removed_by = ?, purge_after = ? WHERE id = ? AND removed_at = 0`,
		at, by, at+thirtyDays, accountID); err != nil {
		f.t.Fatalf("remove %s: %v", accountID, err)
	}
}

func TestTheSchemaKeepsARemovalThirtyDaysRecordedWhole(t *testing.T) {
	// The seed at schema 20: acc_t1 is a mailbox of the team wsp_t1, which
	// Owner (usr_1) owns.
	f := newSchemaTwenty(t)
	const whole = "a removed mailbox is kept for 30 days, recorded whole"
	const fixed = "a removal is recorded once"
	const owner = "usr_0000000000000001"

	// A mailbox is linked live, whatever the insert says.
	for col, value := range map[string]string{
		"removed_at": "900", "removed_by": "'usr_0000000000000001'", "purge_after": "9999999", "sync_held_at": "900",
	} {
		f.refused("a mailbox linked with "+col, "a mailbox is linked live", f.exec(`INSERT INTO accounts(id, workspace_id,
			email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port, smtp_tls, login_user, save_sent_copy,
			state, state_changed_at, created_at, updated_at, `+col+`)
			VALUES ('acc_new', 'wsp_t1', 'new@example.org', 'imap', 'password', 'imap.example.org', 993,
			'smtp.example.org', 587, 'starttls', 'new@example.org', 1, 'active', 900, 900, 900, `+value+`)`))
	}

	// A removal is whole: its moment, who removed it and its end, 30 days
	// or more after.
	f.refused("a moment alone", whole, f.exec(`UPDATE accounts SET removed_at = 1000 WHERE id = 'acc_t1'`))
	f.refused("an end alone", whole, f.exec(`UPDATE accounts SET purge_after = 1000 + ? WHERE id = 'acc_t1'`, thirtyDays))
	f.refused("who removed it, alone", whole, f.exec(`UPDATE accounts SET removed_by = ? WHERE id = 'acc_t1'`, owner))
	f.refused("29 days", whole, f.exec(`UPDATE accounts SET removed_at = 1000, removed_by = ?, purge_after = 1000 + ?
		WHERE id = 'acc_t1'`, owner, thirtyDays-24*60*60))
	f.refused("an end before its moment", whole, f.exec(`UPDATE accounts SET removed_at = 1000, removed_by = ?,
		purge_after = 999 WHERE id = 'acc_t1'`, owner))
	f.accepted("30 days", f.exec(`UPDATE accounts SET removed_at = 1000, removed_by = ?, purge_after = 1000 + ?
		WHERE id = 'acc_t1'`, owner, thirtyDays))

	// And it never moves while the mailbox is removed: not its moment, not
	// its end, not who removed it, but for forgetting a person deleted.
	f.refused("its moment moved", fixed, f.exec(`UPDATE accounts SET removed_at = 2000, purge_after = 2000 + ?
		WHERE id = 'acc_t1'`, thirtyDays))
	f.refused("its end put off", fixed, f.exec(`UPDATE accounts SET purge_after = purge_after + 1 WHERE id = 'acc_t1'`))
	if err := f.exec(`UPDATE accounts SET purge_after = 1001 WHERE id = 'acc_t1'`); err == nil ||
		(!strings.Contains(err.Error(), whole) && !strings.Contains(err.Error(), fixed)) {
		t.Errorf("its end brought forward: %v, want a refusal", err)
	}
	f.refused("another remover", fixed, f.exec(`UPDATE accounts SET removed_by = 'cli' WHERE id = 'acc_t1'`))
	f.accepted("who removed it, forgotten", f.exec(`UPDATE accounts SET removed_by = '' WHERE id = 'acc_t1'`))
	f.refused("who removed it, written again", fixed, f.exec(`UPDATE accounts SET removed_by = ? WHERE id = 'acc_t1'`, owner))
	if got := queryStrings(t, f.s, `SELECT removed_at || ' ' || removed_by || ' ' || (purge_after - removed_at)
		FROM accounts WHERE id = 'acc_t1'`); !slices.Equal(got, []string{"1000  2592000"}) {
		t.Errorf("after the refused writes, the removal is %v", got)
	}

	// Recovered, every part goes at once, and its sync is held.
	f.refused("recovered but for who removed it", whole, f.exec(`UPDATE accounts SET removed_at = 0, purge_after = 0,
		removed_by = 'cli' WHERE id = 'acc_t1'`))
	f.accepted("recovered", f.exec(`UPDATE accounts SET removed_at = 0, removed_by = '', purge_after = 0,
		sync_held_at = 3000 WHERE id = 'acc_t1'`))
	// Removed again later, from scratch.
	f.accepted("removed again", f.exec(`UPDATE accounts SET removed_at = 4000, removed_by = 'key:aaaaaaaa',
		purge_after = 4000 + ? WHERE id = 'acc_t1'`, thirtyDays))
}

func TestTheSchemaGivesNothingOnARemovedMailbox(t *testing.T) {
	// The seed at schema 20: wsp_t1 is a local team (Owner usr_1 its owner,
	// usr_2 an admin, Fay usr_6 and usr_3 members) with the mailboxes acc_t1
	// and acc_t2; Fay holds read and manage on acc_t1. The instance key
	// aaaaaaaa reaches every operator mailbox; acc_0000000000000005 is one.
	f := newSchemaTwenty(t)
	const why = "nothing is granted, keyed or given on a removed mailbox"

	// While acc_t1 is live: a key of its team holding read and act on it,
	// its first mailbox key, and Bea's grant sealed with it.
	f.accepted("a key of the team", f.exec(`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at,
		workspace_id, terms_version, created_by) VALUES ('abcd0001', '$argon2id$x', 'Bot', 'write', 500, 9999999999,
		'wsp_t1', 'terms-1', 'usr_0000000000000001')`))
	f.accepted("what the key holds", f.exec(`INSERT INTO key_access(key_prefix, account_id, workspace_id, read, act, send,
		granted_by, created_at, updated_at) VALUES ('abcd0001', 'acc_t1', 'wsp_t1', 1, 1, 0, 'usr_0000000000000001', 500, 500)`))
	f.accepted("another key of the team", f.exec(`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at,
		workspace_id, terms_version, created_by) VALUES ('abcd0002', '$argon2id$x', 'Bot', 'read', 500, 9999999999,
		'wsp_t1', 'terms-1', 'usr_0000000000000001')`))
	f.accepted("acc_t1's first key", f.exec(`INSERT INTO mailbox_keys(account_id, epoch, public_key, namespace, created_by,
		created_at) VALUES ('acc_t1', 1, randomblob(32), '11111111-2222-4333-8444-555555555555', 'usr_0000000000000002', 500)`))
	sealed := "x'4d4c" + strings.Repeat("ab", 86) + "'"
	f.accepted("Bea's grant", f.exec(`INSERT INTO mailbox_grants(account_id, workspace_id, user_id, epoch, grant,
		granted_by, created_at) VALUES ('acc_t1', 'wsp_t1', 'usr_0000000000000002', 1, `+sealed+`,
		'usr_0000000000000002', 500)`))

	f.remove("acc_t1", "usr_0000000000000001", 900)
	f.remove("acc_0000000000000005", "key:aaaaaaaa", 900)

	// Nothing granted: no grant, not even one written by an upsert that
	// would only lower flags, and no flag going from 0 to 1.
	f.refused("a grant to a member", why, f.exec(`INSERT INTO mailbox_access(account_id, workspace_id, user_id, read,
		act, send, manage, granted_by, created_at, updated_at)
		VALUES ('acc_t1', 'wsp_t1', 'usr_0000000000000003', 1, 0, 0, 0, 'usr_0000000000000001', 950, 950)`))
	f.refused("a grant written by an upsert", why, f.exec(`INSERT INTO mailbox_access(account_id, workspace_id, user_id,
		read, act, send, manage, granted_by, created_at, updated_at)
		VALUES ('acc_t1', 'wsp_t1', 'usr_0000000000000006', 1, 0, 0, 0, 'usr_0000000000000001', 950, 950)
		ON CONFLICT(account_id, user_id) DO UPDATE SET manage = excluded.manage`))
	f.refused("a flag given", why, f.exec(`UPDATE mailbox_access SET act = 1
		WHERE account_id = 'acc_t1' AND user_id = 'usr_0000000000000006'`))
	// Nothing keyed: no next mailbox key, no grant sealed with the one it
	// has.
	f.refused("its next key", why, f.exec(`INSERT INTO mailbox_keys(account_id, epoch, public_key, namespace, created_by,
		created_at) VALUES ('acc_t1', 2, randomblob(32), '11111111-2222-4333-8444-555555555555', 'usr_0000000000000002', 950)`))
	f.refused("a grant sealed with its key", why, f.exec(`INSERT INTO mailbox_grants(account_id, workspace_id, user_id,
		epoch, grant, granted_by, created_at) VALUES ('acc_t1', 'wsp_t1', 'usr_0000000000000006', 1,
		`+sealed+`, 'usr_0000000000000002', 950)`))
	// Nothing given to a key: no team key given it, no flag of one rising,
	// no instance key restricted to it.
	f.refused("given to a key", why, f.exec(`INSERT INTO key_access(key_prefix, account_id, workspace_id, read, act,
		send, granted_by, created_at, updated_at)
		VALUES ('abcd0002', 'acc_t1', 'wsp_t1', 1, 0, 0, 'usr_0000000000000001', 950, 950)`))
	f.accepted("a key's act taken", f.exec(`UPDATE key_access SET act = 0 WHERE key_prefix = 'abcd0001' AND account_id = 'acc_t1'`))
	f.refused("a key's flag given back", why, f.exec(`UPDATE key_access SET act = 1
		WHERE key_prefix = 'abcd0001' AND account_id = 'acc_t1'`))
	f.refused("an instance key restricted to it", why, f.exec(`INSERT INTO api_key_accounts(key_prefix, account_id)
		VALUES ('aaaaaaaa', 'acc_0000000000000005')`))

	// What takes something away is allowed.
	f.accepted("a flag taken", f.exec(`UPDATE mailbox_access SET manage = 0
		WHERE account_id = 'acc_t1' AND user_id = 'usr_0000000000000006'`))
	f.accepted("a grant deleted", f.exec(`DELETE FROM mailbox_access
		WHERE account_id = 'acc_t1' AND user_id = 'usr_0000000000000006'`))
	f.accepted("a key's mailbox taken out of it", f.exec(`DELETE FROM key_access WHERE key_prefix = 'abcd0001'`))
	f.accepted("an instance key's restriction to it taken", f.exec(`DELETE FROM api_key_accounts
		WHERE account_id = 'acc_0000000000000005'`))
	// And what it kept is still there: its key, Bea's grant sealed with it,
	// her flags.
	if n := queryInt(t, f.s, `SELECT (SELECT count(*) FROM mailbox_keys WHERE account_id = 'acc_t1')
		+ (SELECT count(*) FROM mailbox_grants WHERE account_id = 'acc_t1')
		+ (SELECT count(*) FROM mailbox_access WHERE account_id = 'acc_t1' AND user_id = 'usr_0000000000000002')`); n != 3 {
		t.Errorf("the removed mailbox keeps %d of its key, Bea's grant and her flags, want all 3", n)
	}

	// Another mailbox, live, is given things as before; and so is acc_t1
	// once recovered.
	f.accepted("a grant on another mailbox", f.exec(`INSERT INTO mailbox_access(account_id, workspace_id, user_id, read,
		act, send, manage, granted_by, created_at, updated_at)
		VALUES ('acc_t2', 'wsp_t1', 'usr_0000000000000006', 1, 0, 0, 0, 'usr_0000000000000001', 950, 950)`))
	f.accepted("acc_t1 recovered", f.exec(`UPDATE accounts SET removed_at = 0, removed_by = '', purge_after = 0,
		sync_held_at = 990 WHERE id = 'acc_t1'`))
	f.accepted("a grant once it is recovered", f.exec(`INSERT INTO mailbox_access(account_id, workspace_id, user_id, read,
		act, send, manage, granted_by, created_at, updated_at)
		VALUES ('acc_t1', 'wsp_t1', 'usr_0000000000000006', 1, 0, 0, 0, 'usr_0000000000000001', 990, 990)`))
}

func TestTheSchemaNeverGivesANoticeToWhoActed(t *testing.T) {
	// The seed at schema 20: Bea (usr_2) and Fay (usr_6) read acc_t1, which
	// Owner (usr_1) removes; Dan (usr_4) reads acc_t3.
	f := newSchemaTwenty(t)
	const owner, bea, fay, dan = "usr_0000000000000001", "usr_0000000000000002", "usr_0000000000000006", "usr_0000000000000004"
	const why = "a notice goes to a reader of the mailbox, never to who acted"
	event := func(id, kind, account, actor string) int64 {
		t.Helper()
		if err := f.exec(`INSERT INTO mailbox_events(id, kind, workspace_id, account_id, label, hint, actor, created_at)
			VALUES (?, ?, (SELECT workspace_id FROM accounts WHERE id = ?), ?, 'Support', 's…@example.org', ?, 900)`,
			id, kind, account, account, actor); err != nil {
			t.Fatalf("journal %s: %v", id, err)
		}
		return queryInt(t, f.s, `SELECT seq FROM mailbox_events WHERE id = ?`, id)
	}
	reader := func(seq int64, user string) {
		t.Helper()
		f.accepted("a reader recorded", f.exec(`INSERT INTO mailbox_event_readers(event_seq, user_id) VALUES (?, ?)`, seq, user))
	}
	notice := func(seq int64, user string) error {
		return f.exec(`INSERT INTO notices(user_id, event_seq, created_at) VALUES (?, ?, 900)`, user, seq)
	}

	// Owner removed acc_t1, which she read with Bea and Fay.
	removed := event("mbe_0000000000000001", "removed", "acc_t1", owner)
	for _, user := range []string{owner, bea, fay} {
		reader(removed, user)
	}
	f.accepted("Bea told", notice(removed, bea))
	f.accepted("Fay told", notice(removed, fay))
	f.refused("Owner told what she did", why, notice(removed, owner))
	f.refused("Dan, who did not read it, told", why, notice(removed, dan))
	f.refused("Bea told twice", "UNIQUE", notice(removed, bea))
	f.refused("a notice moved to Owner", "a notice is written once", f.exec(`UPDATE notices SET user_id = ?
		WHERE user_id = ? AND event_seq = ?`, owner, fay, removed))
	f.refused("Dan made a reader he was not", "written once", f.exec(`UPDATE mailbox_event_readers SET user_id = ?
		WHERE user_id = ? AND event_seq = ?`, dan, fay, removed))

	// A sync turned off by a key tells every reader; a recovery and a
	// deletion for good tell nobody.
	paused := event("mbe_0000000000000002", "sync_paused", "acc_t3", "key:abcd0001")
	reader(paused, dan)
	f.accepted("Dan told of a key's change", notice(paused, dan))
	for _, kind := range []string{"recovered", "purged"} {
		seq := event("mbe_00000000000000"+map[string]string{"recovered": "03", "purged": "04"}[kind], kind, "acc_t1", owner)
		reader(seq, bea)
		f.refused("Bea told of a mailbox "+kind, why, notice(seq, bea))
	}

	// An event is written once: only its label, its hint and its actor go,
	// and only to nothing.
	f.refused("another kind", "a mailbox event is written once", f.exec(`UPDATE mailbox_events SET kind = 'recovered'
		WHERE seq = ?`, removed))
	f.refused("another mailbox", "a mailbox event is written once", f.exec(`UPDATE mailbox_events SET account_id = 'acc_t2'
		WHERE seq = ?`, removed))
	f.refused("another moment", "a mailbox event is written once", f.exec(`UPDATE mailbox_events SET created_at = 901
		WHERE seq = ?`, removed))
	f.refused("another actor", "a mailbox event is written once", f.exec(`UPDATE mailbox_events SET actor = ?
		WHERE seq = ?`, bea, removed))
	f.refused("another label", "a mailbox event is written once", f.exec(`UPDATE mailbox_events SET label = 'Sales'
		WHERE seq = ?`, removed))
	f.accepted("its names and its actor forgotten", f.exec(`UPDATE mailbox_events SET label = '', hint = '', actor = ''
		WHERE seq = ?`, removed))

	// And it never holds an address.
	f.refused("an address as its label", "CHECK", f.exec(`INSERT INTO mailbox_events(id, kind, workspace_id, account_id,
		label, created_at) VALUES ('mbe_0000000000000005', 'removed', 'wsp_t1', 'acc_t1', 'support@example.org', 900)`))
	f.refused("an address as its hint", "CHECK", f.exec(`INSERT INTO mailbox_events(id, kind, workspace_id, account_id,
		hint, created_at) VALUES ('mbe_0000000000000005', 'removed', 'wsp_t1', 'acc_t1', 'support@example.org', 900)`))
	f.refused("a hint holding two addresses", "CHECK", f.exec(`INSERT INTO mailbox_events(id, kind, workspace_id,
		account_id, hint, created_at) VALUES ('mbe_0000000000000005', 'removed', 'wsp_t1', 'acc_t1',
		's…@x@example.org', 900)`))
	f.refused("an id of another shape", "CHECK", f.exec(`INSERT INTO mailbox_events(id, kind, workspace_id, account_id,
		created_at) VALUES ('mbe_x', 'removed', 'wsp_t1', 'acc_t1', 900)`))

	// Deleting a person takes their notices and their place among the
	// readers; deleting an event, everything that hangs off it.
	f.accepted("the readers of a sync turned off deleted with it", f.exec(`DELETE FROM mailbox_events WHERE seq = ?`, paused))
	if n := queryInt(t, f.s, `SELECT (SELECT count(*) FROM notices WHERE event_seq = ?1)
		+ (SELECT count(*) FROM mailbox_event_readers WHERE event_seq = ?1)`, paused); n != 0 {
		t.Errorf("a deleted event left %d notices and readers", n)
	}
}
