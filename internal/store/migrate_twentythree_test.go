package store

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// Migration 0023: the marks of the workspaces from another source whose
// snapshot changed (docs/workspaces.md, "Following an external directory").

func TestMigrationTwentyThreeAddsTheChangeMarksAndChangesNothingElse(t *testing.T) {
	f := newSchemaAt(t, 22)
	seedJournal21(t, f)
	seedExternalTeam(t, f, "ext_team", "usr_0000000000000002")
	externalMailbox(t, f, "acc_e1", "ext_team")
	before, schema, fts := dumpTables(t, f.s), schemaSQL(t, f.s), ftsAnswers(t, f.s)
	migrateTo(t, f.s, 23)

	// Nothing is marked by the migration: an extension builds every
	// snapshot at its first start anyway.
	compareMigrated(t, before, dumpTables(t, f.s), nil, []string{"workspace_changes"})
	added, changed, dropped := schemaChanges(schema, schemaSQL(t, f.s))
	if want := []string{
		"index sqlite_autoindex_workspace_changes_1", "table workspace_changes",
		"trigger accounts_change_delete", "trigger accounts_change_insert", "trigger accounts_change_update",
		"trigger mailbox_access_change_delete", "trigger mailbox_access_change_insert", "trigger mailbox_access_change_update",
		"trigger mailbox_events_change", "trigger mailbox_grants_change_delete", "trigger mailbox_grants_change_insert",
		"trigger mailbox_keys_change_delete", "trigger mailbox_keys_change_insert", "trigger users_change",
		"trigger workspace_members_change_delete", "trigger workspace_members_change_insert",
		"trigger workspace_members_change_update", "trigger workspaces_change_delete", "trigger workspaces_change_insert",
		"trigger workspaces_change_update",
	}; !slices.Equal(added, want) {
		t.Errorf("0023 added\n%v\nwant\n%v", added, want)
	}
	if len(changed) != 0 || len(dropped) != 0 {
		t.Errorf("0023 changed %v and dropped %v", changed, dropped)
	}
	if got := ftsAnswers(t, f.s); !mapsEqual(got, fts) {
		t.Errorf("the full-text index answers %v, and answered %v", got, fts)
	}
}

// changesFixture is the seed at the latest schema with two teams from
// another source: ext_a, whose members are Bea (usr_2, its owner), Dan
// (usr_4) and Fay (usr_6), with the mailboxes acc_a1, which Bea and Fay
// read, and acc_a2; and ext_b, whose member is Dan, with acc_b1.
func changesFixture(t *testing.T) seededSchema {
	t.Helper()
	f := newSchemaLatest(t)
	const bea, dan, fay = "usr_0000000000000002", "usr_0000000000000004", "usr_0000000000000006"
	seedExternalTeam(t, f, "ext_a", bea, dan, fay)
	f.accepted("Bea its owner", f.exec(`UPDATE workspace_members SET role = 'owner' WHERE workspace_id = 'ext_a'
		AND user_id = ?`, bea))
	seedExternalTeam(t, f, "ext_b", dan)
	for account, team := range map[string]string{"acc_a1": "ext_a", "acc_a2": "ext_a", "acc_b1": "ext_b"} {
		externalMailbox(t, f, account, team)
	}
	for _, user := range []string{bea, fay} {
		f.accepted("read", f.exec(`INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, granted_by,
			created_at, updated_at) VALUES ('acc_a1', 'ext_a', ?, 1, 'directory', 800, 800)`, user))
	}
	return f
}

// mark is a workspace's mark, 0 when it has none.
func (f seededSchema) mark(workspaceID string) int64 {
	f.t.Helper()
	return queryInt(f.t, f.s, `SELECT coalesce((SELECT seq FROM workspace_changes WHERE workspace_id = ?), 0)`, workspaceID)
}

func TestEveryChangeToWhatASnapshotShowsMarksItsWorkspace(t *testing.T) {
	f := changesFixture(t)
	const bea, dan, fay = "usr_0000000000000002", "usr_0000000000000004", "usr_0000000000000006"
	sealed := "x'4d4c" + strings.Repeat("ab", 86) + "'"
	for _, c := range []struct{ what, query string }{
		{"a grant", `INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, granted_by, created_at, updated_at)
			VALUES ('acc_a2', 'ext_a', 'usr_0000000000000004', 1, 'directory', 900, 900)`},
		{"a flag raised", `UPDATE mailbox_access SET send = 1 WHERE account_id = 'acc_a2' AND user_id = 'usr_0000000000000004'`},
		{"a flag lowered", `UPDATE mailbox_access SET send = 0 WHERE account_id = 'acc_a2' AND user_id = 'usr_0000000000000004'`},
		{"a revocation", `DELETE FROM mailbox_access WHERE account_id = 'acc_a2' AND user_id = 'usr_0000000000000004'`},
		{"a key written", `INSERT INTO mailbox_keys(account_id, epoch, public_key, namespace, created_by, created_at)
			VALUES ('acc_a1', 1, randomblob(32), '0000000a-2222-4333-8444-555555555555', '', 900)`},
		{"a key supplied", `INSERT INTO mailbox_grants(account_id, workspace_id, user_id, epoch, grant, granted_by,
			created_at) VALUES ('acc_a1', 'ext_a', 'usr_0000000000000006', 1, ` + sealed + `, '', 900)`},
		{"a sealed grant deleted", `DELETE FROM mailbox_grants WHERE account_id = 'acc_a1' AND user_id = 'usr_0000000000000006'`},
		{"a key deleted", `DELETE FROM mailbox_keys WHERE account_id = 'acc_a1'`},
		{"a role", `UPDATE workspace_members SET role = 'admin' WHERE workspace_id = 'ext_a' AND user_id = 'usr_0000000000000006'`},
		{"a hold", `UPDATE workspace_members SET status = 'disabled', held_at = 900 WHERE workspace_id = 'ext_a'
			AND user_id = 'usr_0000000000000006'`},
		{"a hold released", `UPDATE workspace_members SET status = 'active', held_at = 0 WHERE workspace_id = 'ext_a'
			AND user_id = 'usr_0000000000000006'`},
		{"a suspension", `UPDATE users SET status = 'disabled', suspended_at = 900 WHERE id = 'usr_0000000000000006'`},
		{"a resumption", `UPDATE users SET status = 'active', suspended_at = 0 WHERE id = 'usr_0000000000000006'`},
		{"an enrolment", `UPDATE users SET public_key = randomblob(32) WHERE id = 'usr_0000000000000006'`},
		{"a person's sync consent", `UPDATE users SET sync_consent_at = 950 WHERE id = 'usr_0000000000000006'`},
		{"a removal", `UPDATE accounts SET removed_at = 900, removed_by = 'directory', purge_after = 900 + 2592000
			WHERE id = 'acc_a2'`},
		{"a recovery", `UPDATE accounts SET removed_at = 0, removed_by = '', purge_after = 0, sync_held_at = 950
			WHERE id = 'acc_a2'`},
		{"a hold on its sync lifted", `UPDATE accounts SET sync_held_at = 0 WHERE id = 'acc_a2'`},
		{"an account's state", `UPDATE accounts SET state = 'needs_reauth' WHERE id = 'acc_a1'`},
		{"an account's name", `UPDATE accounts SET display_name = 'Orders' WHERE id = 'acc_a1'`},
		{"a team's consent", `UPDATE accounts SET sync_enabled_at = 960, sync_enabled_by = 'usr_0000000000000002'
			WHERE id = 'acc_a1'`},
		{"a consent bound to a person", `UPDATE accounts SET sync_enabled_via = 'migration' WHERE id = 'acc_a1'`},
		{"a closure", `UPDATE workspaces SET closed_at = 970 WHERE id = 'ext_a'`},
		{"a reopening", `UPDATE workspaces SET closed_at = 0 WHERE id = 'ext_a'`},
		{"a rename", `UPDATE workspaces SET name = 'Renamed' WHERE id = 'ext_a'`},
		{"a journal entry", `INSERT INTO mailbox_events(id, kind, workspace_id, account_id, actor, created_at)
			VALUES ('mbe_0000000000000001', 'recovered', 'ext_a', 'acc_a2', 'directory', 980)`},
		{"a member added", `INSERT INTO workspace_members(workspace_id, user_id, role, status, created_at, updated_at)
			VALUES ('ext_a', 'usr_0000000000000007', 'member', 'active', 990, 990)`},
		{"a member gone", `DELETE FROM workspace_members WHERE workspace_id = 'ext_a' AND user_id = 'usr_0000000000000007'`},
		{"a mailbox linked", `INSERT INTO accounts(id, workspace_id, email, provider, auth_kind, imap_host, imap_port,
			smtp_host, smtp_port, smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at)
			VALUES ('acc_a3', 'ext_a', 'a3@example.org', 'imap', 'password', 'h', 993, 'h', 587, 'starttls', 'a3', 1,
			'pending_auth', 990, 990, 990)`},
		{"a mailbox deleted", `DELETE FROM accounts WHERE id = 'acc_a3'`},
	} {
		before := f.mark("ext_a")
		other := f.mark("ext_b")
		f.accepted(c.what, f.exec(c.query))
		if after := f.mark("ext_a"); after <= before {
			t.Errorf("%s left ext_a's mark at %d (was %d)", c.what, after, before)
		}
		if f.mark("ext_b") != other {
			t.Errorf("%s moved ext_b's mark", c.what)
		}
	}
	// Dan is a member of both: his own state marks both.
	a, b := f.mark("ext_a"), f.mark("ext_b")
	f.accepted("Dan enrolled", f.exec(`UPDATE users SET public_key = randomblob(32) WHERE id = ?`, dan))
	if f.mark("ext_a") <= a || f.mark("ext_b") <= b {
		t.Errorf("Dan's enrolment marked ext_a %d (was %d) and ext_b %d (was %d)", f.mark("ext_a"), a, f.mark("ext_b"), b)
	}
	// What a snapshot does not show moves no mark: a mailbox's sync
	// bookkeeping, a state written again as it was, a member's role written
	// again, a person's name.
	a, b = f.mark("ext_a"), f.mark("ext_b")
	for what, query := range map[string]string{
		"a mailbox's bookkeeping": `UPDATE accounts SET last_ok_at = 999, consecutive_failures = 3, next_retry_at = 999,
			state_reason = 'x', last_idle_event_at = 999, last_error = 'y', sync_tier_resolved = 'uidpoll' WHERE id = 'acc_a1'`,
		"a state written again":  `UPDATE accounts SET state = state, display_name = display_name WHERE id = 'acc_a1'`,
		"a role written again":   `UPDATE workspace_members SET role = role, status = status WHERE workspace_id = 'ext_a'`,
		"flags written again":    `UPDATE mailbox_access SET read = read WHERE account_id = 'acc_a1'`,
		"a person's name":        `UPDATE users SET name = 'Bea B', status = status WHERE id = '` + bea + `'`,
		"a workspace's version":  `UPDATE workspaces SET source_version = 4, name = name WHERE id = 'ext_a'`,
		"a reader of an old one": `INSERT INTO mailbox_event_readers(event_seq, user_id) SELECT seq, '` + fay + `' FROM mailbox_events`,
	} {
		f.accepted(what, f.exec(query))
	}
	if f.mark("ext_a") != a || f.mark("ext_b") != b {
		t.Errorf("what no snapshot shows moved the marks: ext_a %d (was %d), ext_b %d (was %d)",
			f.mark("ext_a"), a, f.mark("ext_b"), b)
	}
}

func TestDeletingAPersonMarksEveryWorkspaceTheyWereIn(t *testing.T) {
	// Hal (usr_8) is a member of ext_a and ext_b, from another source, and
	// of the local team wsp_t1; deleting him deletes his memberships by the
	// schema's cascade, which marks each of the first two.
	f := changesFixture(t)
	const hal = "usr_0000000000000008"
	execAll(t, f.s, person(hal, "hal@example.org", "member", "active", 0, "")...)
	for _, ws := range []string{"ext_a", "ext_b", "wsp_t1"} {
		f.accepted("a membership", f.exec(`INSERT INTO workspace_members(workspace_id, user_id, role, status, created_at,
			updated_at) VALUES (?, ?, 'member', 'active', 900, 900)`, ws, hal))
	}
	f.accepted("a grant", f.exec(`INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, granted_by,
		created_at, updated_at) VALUES ('acc_b1', 'ext_b', ?, 1, 'directory', 900, 900)`, hal))
	a, b := f.mark("ext_a"), f.mark("ext_b")
	execAll(t, f.s, `DELETE FROM workspaces WHERE person_id = '`+hal+`'`, `DELETE FROM users WHERE id = '`+hal+`'`)
	if f.mark("ext_a") <= a || f.mark("ext_b") <= b {
		t.Errorf("deleting Hal marked ext_a %d (was %d) and ext_b %d (was %d)", f.mark("ext_a"), a, f.mark("ext_b"), b)
	}
	if f.mark("wsp_t1") != 0 {
		t.Error("deleting Hal marked a local team")
	}
	// A workspace deleted is marked, and its mark stays, so an extension
	// learns it is gone.
	execAll(t, f.s, `DELETE FROM workspace_members WHERE workspace_id = 'ext_b'`, `DELETE FROM accounts WHERE id = 'acc_b1'`)
	b = f.mark("ext_b")
	execAll(t, f.s, `DELETE FROM workspaces WHERE id = 'ext_b'`)
	if f.mark("ext_b") <= b {
		t.Errorf("deleting ext_b left its mark at %d (was %d)", f.mark("ext_b"), b)
	}
}

func TestALocalWorkspaceIsNeverMarked(t *testing.T) {
	// The seed's every workspace is local: its teams, its people's own,
	// the operator's. Bea (usr_2) is in wsp_t1 and wsp_t2.
	f := newSchemaLatest(t)
	f.journal(MailboxRemoved, "acc_t1", "usr_0000000000000001", 900, true)
	f.journal(MailboxRecovered, "acc_t1", "usr_0000000000000001", 910, false)
	for _, c := range []struct{ what, query string }{
		{"a grant", `UPDATE mailbox_access SET act = 0 WHERE account_id = 'acc_t1'`},
		{"a role", `UPDATE workspace_members SET role = 'member' WHERE workspace_id = 'wsp_t1' AND user_id = 'usr_0000000000000002'`},
		{"a person's state", `UPDATE users SET status = 'disabled', suspended_at = 920,
			public_key = coalesce(public_key, randomblob(32)), sync_consent_at = 0`},
		{"a mailbox's state", `UPDATE accounts SET state = 'error', display_name = 'x'`},
		{"a closure", `UPDATE workspaces SET closed_at = 930, name = 'Closed' WHERE kind = 'team'`},
		{"a team", `INSERT INTO workspaces(id, kind, name, created_at, updated_at) VALUES ('wsp_new', 'team', 'New', 940, 940)`},
		{"a team deleted", `DELETE FROM workspaces WHERE id = 'wsp_new'`},
	} {
		f.accepted(c.what, f.exec(c.query))
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM workspace_changes`); n != 0 {
		t.Errorf("%d marks on a server with no workspace from another source", n)
	}
}

func TestAWorkspaceMarkOnlyRises(t *testing.T) {
	f := changesFixture(t)
	ctx := context.Background()
	var seen []int64
	for i, query := range []string{
		`UPDATE accounts SET display_name = 'One' WHERE id = 'acc_a1'`,
		`UPDATE accounts SET display_name = 'Two' WHERE id = 'acc_b1'`,
		`UPDATE accounts SET display_name = 'Three' WHERE id = 'acc_a1'`,
		`UPDATE workspaces SET name = 'Four' WHERE id = 'ext_a'`,
	} {
		f.accepted("a change", f.exec(query))
		seen = append(seen, max(f.mark("ext_a"), f.mark("ext_b")))
		if i > 0 && seen[i] <= seen[i-1] {
			t.Errorf("change %d took seq %d, after %d", i, seen[i], seen[i-1])
		}
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM workspace_changes`); n != 2 {
		t.Errorf("%d marks for two workspaces", n)
	}
	// Read from a cursor: in order, at most the limit, with the head.
	changes, head, err := f.s.WorkspaceChanges(ctx, 0, 1)
	if err != nil || len(changes) != 1 || changes[0].WorkspaceID != "ext_b" || head != f.mark("ext_a") {
		t.Errorf("the first mark: %+v, head %d (%v)", changes, head, err)
	}
	changes, _, err = f.s.WorkspaceChanges(ctx, changes[0].Seq, 0)
	if err != nil || len(changes) != 1 || changes[0].WorkspaceID != "ext_a" || changes[0].Seq != head {
		t.Errorf("the marks after it: %+v (%v)", changes, err)
	}
	if changes, h, err := f.s.WorkspaceChanges(ctx, head, 10); err != nil || len(changes) != 0 || h != head {
		t.Errorf("after the head: %+v, head %d (%v)", changes, h, err)
	}
	// A seq is never handed out again, even once its mark moved on.
	f.accepted("ext_b again", f.exec(`UPDATE accounts SET display_name = 'Five' WHERE id = 'acc_b1'`))
	if f.mark("ext_b") <= head {
		t.Errorf("ext_b's mark %d is not above %d", f.mark("ext_b"), head)
	}
}

func TestAnOuterInsertOrIgnoreStillMarks(t *testing.T) {
	// A trigger's statements take the conflict clause of the statement
	// that fired them: INSERT OR IGNORE outside would leave a mark that an
	// INSERT OR REPLACE inside tried to move where it was.
	f := changesFixture(t)
	for what, query := range map[string]string{
		"INSERT OR IGNORE": `INSERT OR IGNORE INTO mailbox_access(account_id, workspace_id, user_id, read, granted_by,
			created_at, updated_at) VALUES ('acc_a2', 'ext_a', 'usr_0000000000000002', 1, 'directory', 900, 900)`,
		"INSERT OR REPLACE": `INSERT OR REPLACE INTO mailbox_access(account_id, workspace_id, user_id, read, granted_by,
			created_at, updated_at) VALUES ('acc_a2', 'ext_a', 'usr_0000000000000006', 1, 'directory', 900, 900)`,
		"INSERT OR IGNORE of a member": `INSERT OR IGNORE INTO workspace_members(workspace_id, user_id, role, status,
			created_at, updated_at) VALUES ('ext_a', 'usr_0000000000000007', 'member', 'active', 900, 900)`,
		"an upsert": `INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, granted_by, created_at, updated_at)
			VALUES ('acc_a1', 'ext_a', 'usr_0000000000000006', 1, 'directory', 900, 900)
			ON CONFLICT(account_id, user_id) DO UPDATE SET act = 1`,
	} {
		before := f.mark("ext_a")
		f.accepted(what, f.exec(query))
		if f.mark("ext_a") <= before {
			t.Errorf("%s left the mark at %d", what, before)
		}
	}
	// One that inserts nothing changes nothing, and marks nothing.
	before := f.mark("ext_a")
	f.accepted("an ignored insert", f.exec(`INSERT OR IGNORE INTO workspace_members(workspace_id, user_id, role, status,
		created_at, updated_at) VALUES ('ext_a', 'usr_0000000000000007', 'member', 'active', 900, 900)`))
	if f.mark("ext_a") != before {
		t.Error("an insert that inserted nothing moved the mark")
	}
}
