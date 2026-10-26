package store

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// Migration 0025: one mailbox's sync paused, its index kept; a recovery's
// hold made the recovery's pause; and the journal rebuilt to record a pause
// and its end (docs/workspaces.md, "Pausing a mailbox's sync").

// pauseColumns are what 0025 adds to every account: not paused, unless the
// conversion made a recovery's hold a pause (paused, by id, at its moment).
func pauseColumns(paused map[string]string) map[string]map[string]addedColumn {
	return map[string]map[string]addedColumn{"accounts": {
		"sync_paused_at": func(row map[string]string) string {
			if at, ok := paused[strings.TrimPrefix(row["id"], "string:")]; ok {
				return "int64:" + at
			}
			return "int64:0"
		},
		"sync_paused_by": sameValue("string:"),
	}}
}

// seedJournalAt24 writes, at schema 24, a journal of every kind 0021 knew,
// with its readers and notices: removals (with their end), a sync turned off
// by a key and one stopped with the person bound to it, a recovery, a
// deletion for good, a stranding of each cause, entries of a team from
// another source (whose mark it moves), and a last entry swept by its
// retention, so the high-water mark is above the highest seq.
func seedJournalAt24(t *testing.T, f seededSchema) {
	t.Helper()
	const bea, dan, fay = "usr_0000000000000002", "usr_0000000000000004", "usr_0000000000000006"
	seedExternalTeam(t, f, "ext_p", bea, fay)
	externalMailbox(t, f, "acc_p1", "ext_p")
	execAll(t, f.s,
		`INSERT INTO mailbox_events(seq, id, kind, workspace_id, account_id, label, hint, actor, created_at, cause,
		   lost_user, purge_after) VALUES
		 (1, 'mbe_0000000000000001', 'removed', 'wsp_t1', 'acc_t1', 'Helpdesk', 's…@example.org', 'usr_0000000000000001', 900, '', '', 900 + 2592000),
		 (2, 'mbe_0000000000000002', 'sync_paused', 'wsp_t2', 'acc_t3', 'Operations', 'o…@team.example.org', 'key:abcd0001', 910, '', '', 0),
		 (3, 'mbe_0000000000000003', 'recovered', 'wsp_t1', 'acc_t1', 'Helpdesk', 's…@example.org', 'usr_0000000000000001', 920, '', '', 0),
		 (4, 'mbe_0000000000000004', 'purged', 'wsp_t1', 'acc_gone', '', '', 'sweep', 930, '', '', 0),
		 (5, 'mbe_0000000000000005', 'sync_paused', 'wsp_t1', 'acc_t2', '', 'b…@example.org', 'usr_0000000000000005', 935, 'bound_stopped', '', 0),
		 (6, 'mbe_0000000000000006', 'stranded', 'wsp_t1', 'acc_t2', '', 'b…@example.org', 'directory', 940, 'membership_ended', 'usr_0000000000000006', 0),
		 (7, 'mbe_0000000000000007', 'stranded', 'wsp_t1', 'acc_t2', '', 'b…@example.org', 'directory', 941, 'forgotten', '', 0),
		 (8, 'mbe_0000000000000008', 'stranded', 'wsp_t1', 'acc_t2', '', 'b…@example.org', 'usr_0000000000000001', 942, 'disabled', 'usr_0000000000000006', 0),
		 (9, 'mbe_0000000000000009', 'stranded', 'wsp_t1', 'acc_t2', '', 'b…@example.org', 'cli', 943, 'deleted', '', 0),
		 (10, 'mbe_000000000000000a', 'stranded', 'wsp_t1', 'acc_t2', '', 'b…@example.org', 'usr_0000000000000001', 944, 'reset', 'usr_0000000000000006', 0),
		 (11, 'mbe_000000000000000b', 'removed', 'ext_p', 'acc_p1', 'Box acc_p1', 'a…@ext.example.org', 'directory', 945, '', '', 945 + 2592000),
		 (12, 'mbe_000000000000000c', 'recovered', 'ext_p', 'acc_p1', 'Box acc_p1', 'a…@ext.example.org', 'directory', 946, '', '', 0),
		 (13, 'mbe_000000000000000d', 'sync_paused', 'wsp_t1', 'acc_t1', 'Helpdesk', 's…@example.org', 'operator', 947, '', '', 0)`,
		`INSERT INTO mailbox_event_readers(event_seq, user_id) VALUES
		 (1, 'usr_0000000000000001'), (1, '`+bea+`'), (1, '`+fay+`'), (2, '`+dan+`'), (3, '`+bea+`'),
		 (5, '`+fay+`'), (6, '`+fay+`'), (11, '`+bea+`'), (11, '`+fay+`'), (13, '`+bea+`')`,
		`INSERT INTO notices(user_id, event_seq, created_at) VALUES
		 ('`+bea+`', 1, 900), ('`+fay+`', 1, 900), ('`+dan+`', 2, 910), ('`+fay+`', 5, 935), ('`+fay+`', 11, 945),
		 ('`+bea+`', 13, 947)`,
		`INSERT INTO mailbox_events(seq, id, kind, workspace_id, account_id, actor, created_at)
		 VALUES (14, 'mbe_000000000000000e', 'recovered', 'wsp_t1', 'acc_t2', 'cli', 950)`,
		`DELETE FROM mailbox_events WHERE seq = 14`,
	)
}

func TestMigrationTwentyFiveRebuildsTheJournalAndPausesOnlyWhatARecoveryHeld(t *testing.T) {
	f := newSchemaAt(t, 24)
	seedJournalAt24(t, f)
	// One hold: a recovery's, on a live team mailbox whose consent stands.
	execAll(t, f.s, `UPDATE accounts SET sync_held_at = 920 WHERE id = 'acc_t1'`)
	if got := queryStrings(t, f.s, `SELECT seq FROM sqlite_sequence WHERE name = 'mailbox_events'`); !slices.Equal(got, []string{"14"}) {
		t.Fatalf("the seed's journal has the high-water mark %v, want 14", got)
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM workspace_changes WHERE workspace_id = 'ext_p'`); n != 1 {
		t.Fatalf("the seed's team from another source has %d marks, want 1", n)
	}
	before, schema, fts := dumpTables(t, f.s), schemaSQL(t, f.s), ftsAnswers(t, f.s)
	migrateTo(t, f.s, 25)
	after := dumpTables(t, f.s)

	// The one value that changes besides the new columns: the converted
	// hold.
	for _, row := range before["accounts"] {
		if row["id"] == "string:acc_t1" {
			row["sync_held_at"] = "int64:0"
		}
	}
	tried := compareMigrated(t, before, after, pauseColumns(map[string]string{"acc_t1": "920"}), nil)
	if tried["accounts"] != len(before["accounts"]) || tried["accounts"] == 0 {
		t.Errorf("the migration was tried on %d accounts, want the seed's %d", tried["accounts"], len(before["accounts"]))
	}
	if got := queryStrings(t, f.s, `SELECT sync_paused_at || ' ' || sync_paused_by || ' ' || sync_held_at FROM accounts
		WHERE id = 'acc_t1'`); !slices.Equal(got, []string{"920  0"}) {
		t.Errorf("the recovery's hold became %v, want its pause at 920, naming nobody", got)
	}

	// The schema is as it was but for the journal's table, the accounts'
	// columns, the triggers that hold a pause and a hold, the change marks'
	// trigger and the notices' one; the journal's own triggers are written
	// again byte for byte.
	now := schemaSQL(t, f.s)
	added, changed, dropped := schemaChanges(schema, now)
	if want := []string{"trigger accounts_held_personal", "trigger accounts_linked_unpaused", "trigger accounts_pause_consent",
		"trigger accounts_pause_fixed", "trigger accounts_pause_live", "trigger accounts_pause_whole"}; !slices.Equal(added, want) ||
		len(dropped) != 0 || !slices.Equal(changed, []string{"table accounts", "table mailbox_events",
		"trigger accounts_change_update", "trigger notices_to_readers"}) {
		t.Errorf("0025 added %v, changed %v and dropped %v", added, changed, dropped)
	}
	for _, name := range []string{"trigger mailbox_events_written_once", "trigger mailbox_events_change",
		"index mailbox_events_account", "index mailbox_events_created", "index mailbox_events_workspace"} {
		if schema[name] != now[name] || now[name] == "" {
			t.Errorf("%s is not as it was", name)
		}
	}
	if !strings.Contains(now["trigger accounts_change_update"], "OR NEW.sync_paused_at IS NOT OLD.sync_paused_at") {
		t.Error("the change marks do not watch a pause")
	}
	if strings.ReplaceAll(now["trigger notices_to_readers"], "('removed', 'sync_paused', 'paused')", "('removed', 'sync_paused')") !=
		schema["trigger notices_to_readers"] {
		t.Error("notices_to_readers differs from 0021's by more than the pause")
	}
	// Its readers and notices refer to the rebuilt journal, and go with an
	// entry deleted.
	for _, table := range []string{"mailbox_event_readers", "notices"} {
		if got := queryStrings(t, f.s, `SELECT "table" FROM pragma_foreign_key_list(?) WHERE "from" = 'event_seq'`, table); !slices.Equal(got, []string{"mailbox_events"}) {
			t.Errorf("%s refers to %v", table, got)
		}
	}
	execAll(t, f.s, `DELETE FROM mailbox_events WHERE seq = 1`)
	if n := queryInt(t, f.s, `SELECT (SELECT count(*) FROM mailbox_event_readers WHERE event_seq = 1)
		+ (SELECT count(*) FROM notices WHERE event_seq = 1)`); n != 0 {
		t.Errorf("an entry deleted left %d readers and notices", n)
	}
	if got := queryStrings(t, f.s, `PRAGMA foreign_key_check`); len(got) != 0 {
		t.Errorf("foreign_key_check: %v", got)
	}
	if got := queryStrings(t, f.s, `PRAGMA integrity_check`); !slices.Equal(got, []string{"ok"}) {
		t.Errorf("integrity_check: %v", got)
	}
	if got := ftsAnswers(t, f.s); !mapsEqual(got, fts) {
		t.Errorf("the full-text index answers %v, and answered %v", got, fts)
	}
}

// at24Mailbox links, at schema 24, a mailbox of person's personal workspace,
// live, with nothing indexed.
func at24Mailbox(t *testing.T, f seededSchema, id, person string) {
	t.Helper()
	f.accepted("the mailbox "+id, f.exec(`INSERT INTO accounts(id, workspace_id, email, display_name, provider, auth_kind,
		imap_host, imap_port, smtp_host, smtp_port, smtp_tls, login_user, save_sent_copy, state, state_changed_at,
		created_at, updated_at, owner_user_id) VALUES (?1, (SELECT id FROM workspaces WHERE person_id = ?2), ?1 || '@home.example.org',
		'Box ' || ?1, 'imap', 'password', 'imap.example.org', 993, 'smtp.example.org', 587, 'starttls', ?1, 1, 'active',
		800, 800, 800, ?2)`, id, person))
}

// at24Entry journals, at schema 24, kind of a mailbox by actor at at, with
// no reader: what 0025 reads to tell a recovery's hold from the operator's.
func at24Entry(t *testing.T, f seededSchema, kind, account, actor string, at int64) {
	t.Helper()
	end := int64(0)
	if kind == "removed" {
		end = at + thirtyDays
	}
	f.accepted(kind+" of "+account, f.exec(`INSERT INTO mailbox_events(id, kind, workspace_id, account_id, actor,
		created_at, purge_after) VALUES ('mbe_' || printf('%016x', (SELECT coalesce(max(seq), 0) + 1 FROM mailbox_events)),
		?, (SELECT workspace_id FROM accounts WHERE id = ?), ?, ?, ?, ?)`, kind, account, account, actor, at, end))
}

func TestMigrationTwentyFiveTurnsARecoverysHoldIntoItsPauseAndKeepsTheOperators(t *testing.T) {
	// The seed at schema 24: acc_t1 is a team mailbox under its consent
	// (bound to Bea by 0011); acc_t3 one never agreed to; acc_t2 one whose
	// consent stopped; acc_5 an operator mailbox switched on; acc_1 and
	// acc_2 Owner's own (usr_1, who consents), acc_3 Bea's own (usr_2,
	// whose consent is taken back below), each indexed. Fay (usr_6) never
	// consented.
	f := newSchemaAt(t, 24)
	const owner, bea, fay = "usr_0000000000000001", "usr_0000000000000002", "usr_0000000000000006"
	f.accepted("acc_2 keeps a folder alone", f.exec(`DELETE FROM messages WHERE account_id = 'acc_0000000000000002'`))
	f.accepted("Bea's consent taken back", f.exec(`UPDATE users SET sync_consent_at = 0 WHERE id = ?`, bea))
	for _, m := range []struct{ id, person string }{
		{"acc_e", owner}, {"acc_e1", owner}, {"acc_e2", fay}, {"acc_e3", owner}, {"acc_e4", owner},
		{"acc_g", owner}, {"acc_h", owner}, {"acc_h2", owner}, {"acc_i1", owner}, {"acc_i2", owner},
	} {
		at24Mailbox(t, f, m.id, m.person)
	}
	f.accepted("acc_g's folder", f.exec(`INSERT INTO folders(account_id, name, display_name, role, role_source, uidvalidity)
		VALUES ('acc_g', 'INBOX', 'Inbox', 'inbox', 'inbox', 1)`))
	// The journal: recoveries at the hold's moment, one the operator turned
	// off after, one at another moment; removals, and the operator's
	// turn-offs before and after them.
	at24Entry(t, f, "recovered", "acc_e1", owner, 950)
	at24Entry(t, f, "recovered", "acc_e2", fay, 950)
	at24Entry(t, f, "recovered", "acc_e3", owner, 950)
	at24Entry(t, f, "sync_paused", "acc_e3", "operator", 951)
	at24Entry(t, f, "recovered", "acc_e4", owner, 900)
	at24Entry(t, f, "recovered", "acc_h2", owner, 950)
	at24Entry(t, f, "removed", "acc_h2", owner, 960)
	at24Entry(t, f, "removed", "acc_i1", owner, 960)
	at24Entry(t, f, "sync_paused", "acc_i1", "operator", 970)
	at24Entry(t, f, "sync_paused", "acc_i2", "operator", 955)
	at24Entry(t, f, "removed", "acc_i2", owner, 960)
	for _, id := range []string{"acc_t2", "acc_0000000000000004", "acc_g", "acc_h", "acc_h2", "acc_i1", "acc_i2"} {
		f.remove(id, owner, 960)
	}
	for _, id := range []string{"acc_t1", "acc_t2", "acc_t3", "acc_0000000000000005", "acc_0000000000000001",
		"acc_0000000000000002", "acc_0000000000000003", "acc_e", "acc_e1", "acc_e2", "acc_e3", "acc_e4", "acc_g",
		"acc_h", "acc_h2"} {
		f.accepted("held "+id, f.exec(`UPDATE accounts SET sync_held_at = 950 WHERE id = ?`, id))
	}
	// A team from another source, whose mark the conversion leaves alone.
	seedExternalTeam(t, f, "ext_q", bea)
	externalMailbox(t, f, "acc_q1", "ext_q")
	f.accepted("its consent and hold", f.exec(`UPDATE accounts SET sync_enabled_at = 900, sync_enabled_by = ?,
		sync_held_at = 950 WHERE id = 'acc_q1'`, bea))
	mark := f.mark("ext_q")
	updated := queryStrings(t, f.s, `SELECT id || ' ' || updated_at FROM accounts ORDER BY id`)

	migrateTo(t, f.s, 25)

	type state struct {
		paused, held int64
		by           string
	}
	want := map[string]state{
		"acc_t1":               {paused: 950}, // (a) a team's, consent standing
		"acc_t3":               {},            // (b) a team's, consent off
		"acc_0000000000000005": {paused: 950}, // (c) an operator mailbox switched on
		"acc_0000000000000001": {paused: 950}, // (d) a person's, indexed, consent standing
		"acc_0000000000000002": {paused: 950}, // (d) the same with a folder alone
		"acc_0000000000000003": {},            // (d') a person's, indexed, consent off
		"acc_e":                {held: 950},   // (e) no index, no entry: kept
		"acc_e1":               {paused: 950}, // (e') a recovery's entry, consent standing
		"acc_e2":               {},            // (e') the same, consent off
		"acc_e3":               {held: 950},   // (e'') the operator turned it off after
		"acc_e4":               {held: 950},   // (e''') a recovery at another moment
		"acc_t2":               {},            // (f) a removed team's
		"acc_g":                {},            // (g) a removed person's, indexed
		"acc_h":                {held: 950},   // (h) removed, no index, no entry: kept
		"acc_h2":               {},            // (h) the same with a recovery's entry
		"acc_i1":               {held: 970},   // (i) the operator turned it off while removed
		"acc_i2":               {},            // (i) the operator turned it off before
		"acc_q1":               {paused: 950},
		"acc_0000000000000004": {},
		"acc_0000000000000006": {},
	}
	rows := queryStrings(t, f.s, `SELECT id, sync_paused_at, sync_held_at, sync_paused_by FROM accounts ORDER BY id`)
	if len(rows) != len(want) {
		t.Errorf("%d accounts after the migration, want %d", len(rows), len(want))
	}
	for _, row := range rows {
		var (
			id string
			st state
		)
		if _, err := fmt.Sscan(row, &id, &st.paused, &st.held); err != nil {
			t.Fatalf("%q: %v", row, err)
		}
		st.by = strings.TrimSpace(strings.TrimPrefix(row, fmt.Sprintf("%s %d %d", id, st.paused, st.held)))
		if w, ok := want[id]; !ok || st != w {
			t.Errorf("%s is paused at %d by %q and held at %d, want %+v", id, st.paused, st.by, st.held, w)
		}
	}
	if got := queryStrings(t, f.s, `SELECT id || ' ' || updated_at FROM accounts ORDER BY id`); !slices.Equal(got, updated) {
		t.Errorf("the conversion moved updated_at: %v, was %v", got, updated)
	}
	if got := f.mark("ext_q"); got != mark {
		t.Errorf("the conversion moved ext_q's mark to %d (was %d)", got, mark)
	}
}

func TestMigrationTwentyFiveOnAJournalNeverWrittenChangesNothingElse(t *testing.T) {
	f := newSchemaAt(t, 24)
	mark := `SELECT count(*) FROM sqlite_sequence WHERE name IN ('mailbox_events', 'mailbox_events_new')`
	if n := queryInt(t, f.s, mark); n != 0 {
		t.Fatalf("a journal never written has %d high-water marks", n)
	}
	before := dumpTables(t, f.s)
	migrateTo(t, f.s, 25)
	compareMigrated(t, before, dumpTables(t, f.s), pauseColumns(nil), nil)
	if n := queryInt(t, f.s, mark); n != 0 {
		t.Errorf("the rebuild left %d high-water marks for a journal never written", n)
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM sqlite_schema WHERE name LIKE 'mailbox_events_new%'`); n != 0 {
		t.Error("the rebuild left its new table behind")
	}
}

func TestTheJournalRebuiltTwiceNeverReusesASeq(t *testing.T) {
	// Entries the retention swept before the upgrade: the journal is empty,
	// and its high-water mark is the last seq it handed out.
	f := newSchemaAt(t, 24)
	seedJournalAt24(t, f)
	execAll(t, f.s, `DELETE FROM mailbox_events`)
	migrateTo(t, f.s, latestVersion(t))
	id := f.journal(MailboxSyncPaused, "acc_t3", "usr_0000000000000004", 1000, false)
	if seq := queryInt(t, f.s, `SELECT seq FROM mailbox_events WHERE id = ?`, id); seq != 15 {
		t.Errorf("the first entry after the rebuild is %d, want 15, above every seq handed out", seq)
	}
}

func TestTheSchemaRecordsAPauseWholeAndOnce(t *testing.T) {
	// The seed: acc_t1 is a team mailbox under its consent.
	f := newSchemaLatest(t)
	const whole = "a pause is recorded whole"
	const once = "a pause is recorded once"
	f.refused("who paused it, not paused", whole, f.exec(`UPDATE accounts SET sync_paused_by = 'cli' WHERE id = 'acc_t1'`))
	f.accepted("paused", f.exec(`UPDATE accounts SET sync_paused_at = 900, sync_paused_by = 'usr_0000000000000002'
		WHERE id = 'acc_t1'`))
	f.refused("its moment moved", once, f.exec(`UPDATE accounts SET sync_paused_at = 901 WHERE id = 'acc_t1'`))
	f.refused("another who paused it", once, f.exec(`UPDATE accounts SET sync_paused_by = 'cli' WHERE id = 'acc_t1'`))
	f.refused("ending it but naming who", whole, f.exec(`UPDATE accounts SET sync_paused_at = 0 WHERE id = 'acc_t1'`))
	f.accepted("who paused it forgotten", f.exec(`UPDATE accounts SET sync_paused_by = '' WHERE id = 'acc_t1'`))
	f.accepted("its end", f.exec(`UPDATE accounts SET sync_paused_at = 0, sync_paused_by = '' WHERE id = 'acc_t1'`))
	f.accepted("paused again", f.exec(`UPDATE accounts SET sync_paused_at = 910, sync_paused_by = 'cli' WHERE id = 'acc_t1'`))
	if got := queryStrings(t, f.s, `SELECT sync_paused_at || ' ' || sync_paused_by FROM accounts WHERE id = 'acc_t1'`); !slices.Equal(got, []string{"910 cli"}) {
		t.Errorf("the pause is %v", got)
	}
}

func TestOnlyALiveMailboxThatNothingHoldsIsPaused(t *testing.T) {
	// The seed: acc_0000000000000003 is Bea's own, under her consent.
	f := newSchemaLatest(t)
	const own = "acc_0000000000000003"
	const live = "only a live mailbox whose sync nothing holds is paused"
	f.remove(own, "usr_0000000000000002", 900)
	f.refused("a removed one paused", live, f.exec(`UPDATE accounts SET sync_paused_at = 910 WHERE id = ?`, own))
	f.accepted("the recovery's one statement", f.exec(`UPDATE accounts SET removed_at = 0, removed_by = '',
		purge_after = 0, sync_paused_at = 920, sync_paused_by = 'usr_0000000000000002' WHERE id = ?`, own))
	f.refused("a paused one removed", live, f.exec(`UPDATE accounts SET removed_at = 930, removed_by = 'cli',
		purge_after = 930 + ? WHERE id = ?`, thirtyDays, own))
	f.refused("a paused one held", live, f.exec(`UPDATE accounts SET sync_held_at = 930 WHERE id = ?`, own))
	f.accepted("its pause ended", f.exec(`UPDATE accounts SET sync_paused_at = 0, sync_paused_by = '' WHERE id = ?`, own))
	f.accepted("then held", f.exec(`UPDATE accounts SET sync_held_at = 940 WHERE id = ?`, own))
	f.refused("a held one paused", live, f.exec(`UPDATE accounts SET sync_paused_at = 950 WHERE id = ?`, own))
}

func TestATeamsOrOperatorMailboxIsPausedOnlyWhileItsConsentStands(t *testing.T) {
	// The seed: acc_t1 is a team mailbox under its consent, acc_t3 one
	// never agreed to; acc_5 an operator mailbox switched on, acc_6 one
	// switched on too; Fay (usr_6) never consented, and owns no mailbox.
	f := newSchemaLatest(t)
	const consent = "a team's or an operator mailbox is paused only while its consent stands"
	f.refused("a team's never agreed to, paused", consent,
		f.exec(`UPDATE accounts SET sync_paused_at = 900 WHERE id = 'acc_t3'`))
	f.accepted("the operator's switched off", f.exec(`UPDATE accounts SET sync_enabled_at = 0, sync_enabled_by = ''
		WHERE id = 'acc_0000000000000006'`))
	f.refused("an operator mailbox switched off, paused", consent,
		f.exec(`UPDATE accounts SET sync_paused_at = 900 WHERE id = 'acc_0000000000000006'`))
	for _, id := range []string{"acc_t1", "acc_0000000000000005"} {
		f.accepted("paused", f.exec(`UPDATE accounts SET sync_paused_at = 900, sync_paused_by = 'cli' WHERE id = ?`, id))
		f.refused("its consent zeroed while paused", consent,
			f.exec(`UPDATE accounts SET sync_enabled_at = 0, sync_enabled_by = '' WHERE id = ?`, id))
		f.accepted("its pause ended", f.exec(`UPDATE accounts SET sync_paused_at = 0, sync_paused_by = '' WHERE id = ?`, id))
		f.accepted("then its consent", f.exec(`UPDATE accounts SET sync_enabled_at = 0, sync_enabled_by = '' WHERE id = ?`, id))
	}
	// A personal mailbox's consent is its person's, which the code holds.
	f.accepted("Owner's consent off", f.exec(`UPDATE users SET sync_consent_at = 0 WHERE id = 'usr_0000000000000001'`))
	f.accepted("a person's paused all the same", f.exec(`UPDATE accounts SET sync_paused_at = 900
		WHERE id = 'acc_0000000000000001'`))
}

func TestOnlyAPersonalMailboxIsHeld(t *testing.T) {
	f := newSchemaLatest(t)
	const personal = "only a personal mailbox is held"
	for _, id := range []string{"acc_t1", "acc_t3", "acc_0000000000000005"} {
		f.refused(id+" held", personal, f.exec(`UPDATE accounts SET sync_held_at = 900 WHERE id = ?`, id))
	}
	f.accepted("a person's held, live", f.exec(`UPDATE accounts SET sync_held_at = 900 WHERE id = 'acc_0000000000000001'`))
	f.remove("acc_0000000000000002", "usr_0000000000000001", 900)
	f.accepted("a person's held, removed", f.exec(`UPDATE accounts SET sync_held_at = 910 WHERE id = 'acc_0000000000000002'`))
}

func TestAMailboxIsLinkedUnpaused(t *testing.T) {
	f := newSchemaLatest(t)
	for col, value := range map[string]string{"sync_paused_at": "900", "sync_paused_by": "'cli'"} {
		f.refused("a mailbox linked with "+col, "a mailbox is linked with its sync not paused", f.exec(`INSERT INTO
			accounts(id, workspace_id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port, smtp_tls,
			login_user, save_sent_copy, state, state_changed_at, created_at, updated_at, `+col+`)
			VALUES ('acc_new', 'wsp_t1', 'new@example.org', 'imap', 'password', 'imap.example.org', 993,
			'smtp.example.org', 587, 'starttls', 'new@example.org', 1, 'active', 900, 900, 900, `+value+`)`))
	}
}

func TestAPauseAndAResumeHaveNoCauseAndOnlyAPauseGivesANotice(t *testing.T) {
	// The seed: Bea (usr_2) and Fay (usr_6) read acc_t1.
	f := newSchemaLatest(t)
	const owner, bea, fay = "usr_0000000000000001", "usr_0000000000000002", "usr_0000000000000006"
	const notice = "a notice goes to a reader of the mailbox, never to who acted"
	entry := func(id, kind, cause string) error {
		return f.exec(`INSERT INTO mailbox_events(id, kind, workspace_id, account_id, actor, created_at, cause)
			VALUES (?, ?, 'wsp_t1', 'acc_t1', ?, 900, ?)`, id, kind, owner, cause)
	}
	for _, kind := range []string{"paused", "resumed"} {
		for _, cause := range []string{CauseBoundStopped, CauseDisabled} {
			f.refused(kind+" for a cause", "CHECK", entry("mbe_00000000000000f1", kind, cause))
		}
		f.refused(kind+" with an end", "CHECK", f.exec(`INSERT INTO mailbox_events(id, kind, workspace_id, account_id,
			actor, created_at, purge_after) VALUES ('mbe_00000000000000f2', ?, 'wsp_t1', 'acc_t1', ?, 900, 900 + ?)`,
			kind, owner, thirtyDays))
	}
	f.accepted("a pause", entry("mbe_0000000000000001", "paused", ""))
	f.accepted("a resume", entry("mbe_0000000000000002", "resumed", ""))
	for _, id := range []string{"mbe_0000000000000001", "mbe_0000000000000002"} {
		f.accepted("its readers", f.exec(`INSERT INTO mailbox_event_readers(event_seq, user_id)
			SELECT seq, ? FROM mailbox_events WHERE id = ? UNION ALL SELECT seq, ? FROM mailbox_events WHERE id = ?
			UNION ALL SELECT seq, ? FROM mailbox_events WHERE id = ?`, bea, id, fay, id, owner, id))
	}
	tell := func(user, id string) error {
		return f.exec(`INSERT INTO notices(user_id, event_seq, created_at)
			SELECT ?, seq, 900 FROM mailbox_events WHERE id = ?`, user, id)
	}
	f.accepted("Bea told of the pause", tell(bea, "mbe_0000000000000001"))
	f.refused("the actor told of the pause", notice, tell(owner, "mbe_0000000000000001"))
	f.refused("Fay told of the resume", notice, tell(fay, "mbe_0000000000000002"))
}

func TestAPauseMarksItsWorkspaceFromAnotherSourceChanged(t *testing.T) {
	// changesFixture: acc_a1 is a mailbox of ext_a, a team from another
	// source; acc_t1 one of the local team wsp_t1.
	f := changesFixture(t)
	f.accepted("a team's consent", f.exec(`UPDATE accounts SET sync_enabled_at = 900, sync_enabled_by = 'cli'
		WHERE id = 'acc_a1'`))
	for _, c := range []struct {
		what, query string
		moves       bool
	}{
		{"a pause", `UPDATE accounts SET sync_paused_at = 910, sync_paused_by = 'usr_0000000000000002' WHERE id = 'acc_a1'`, true},
		{"who paused it forgotten", `UPDATE accounts SET sync_paused_by = '' WHERE id = 'acc_a1'`, false},
		{"its end", `UPDATE accounts SET sync_paused_at = 0 WHERE id = 'acc_a1'`, true},
		{"a local team's pause", `UPDATE accounts SET sync_paused_at = 920 WHERE id = 'acc_t1'`, false},
	} {
		before := f.mark("ext_a")
		f.accepted(c.what, f.exec(c.query))
		if moved := f.mark("ext_a") > before; moved != c.moves {
			t.Errorf("%s moved ext_a's mark: %v, want %v", c.what, moved, c.moves)
		}
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM workspace_changes WHERE workspace_id = 'wsp_t1'`); n != 0 {
		t.Error("a local team was marked")
	}
}

func TestTheJournalsTriggersSurviveItsSecondRebuild(t *testing.T) {
	f := changesFixture(t)
	f.accepted("an entry", f.exec(`INSERT INTO mailbox_events(id, kind, workspace_id, account_id, actor, created_at)
		VALUES ('mbe_0000000000000001', 'paused', 'wsp_t1', 'acc_t1', 'usr_0000000000000001', 900)`))
	f.refused("it changed", "a mailbox event is written once",
		f.exec(`UPDATE mailbox_events SET kind = 'resumed' WHERE id = 'mbe_0000000000000001'`))
	before := f.mark("ext_a")
	f.accepted("an entry of ext_a", f.exec(`INSERT INTO mailbox_events(id, kind, workspace_id, account_id, actor, created_at)
		VALUES ('mbe_0000000000000002', 'resumed', 'ext_a', 'acc_a1', 'usr_0000000000000002', 910)`))
	if f.mark("ext_a") <= before {
		t.Error("an entry of a team from another source did not mark it")
	}
}
