package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
)

// Migration 0022: the directories an extension follows, the people they
// say things of, held memberships, a workspace's source version, and an
// extension's own state (docs/workspaces.md, "Following an external
// directory").

const issuer = "https://id.example.org"

func TestMigrationTwentyTwoAddsTheDirectoryAndChangesNothingElse(t *testing.T) {
	f := newSchemaAt(t, 21)
	seedJournal21(t, f)
	// A team from another source, which a binary that followed nothing
	// never wrote but a test may: its columns start at 0 too.
	f.accepted("a team from another source", f.exec(`INSERT INTO workspaces(id, kind, source, name, created_at, updated_at)
		VALUES ('ext_team', 'team', 'platform', 'Elsewhere', 800, 800)`))
	f.accepted("its member", f.exec(`INSERT INTO workspace_members(workspace_id, user_id, role, status, created_at,
		updated_at) VALUES ('ext_team', 'usr_0000000000000002', 'owner', 'disabled', 800, 800)`))
	before, schema, fts := dumpTables(t, f.s), schemaSQL(t, f.s), ftsAnswers(t, f.s)
	migrateTo(t, f.s, 22)

	tried := compareMigrated(t, before, dumpTables(t, f.s), map[string]map[string]addedColumn{
		"workspaces":        {"source_version": sameValue("int64:0"), "source_reset": sameValue("int64:0")},
		"workspace_members": {"held_at": sameValue("int64:0")},
	}, []string{"directories", "directory_people", "extension_state"})
	if tried["workspaces"] == 0 || tried["workspace_members"] == 0 {
		t.Errorf("the seed tried the migration on %v", tried)
	}
	added, changed, dropped := schemaChanges(schema, schemaSQL(t, f.s))
	if want := []string{
		"index directory_people_subject", "index directory_people_user", "index sqlite_autoindex_directories_1",
		"index sqlite_autoindex_directory_people_1", "index sqlite_autoindex_extension_state_1",
		"table directories", "table directory_people", "table extension_state",
		"trigger directories_kept", "trigger directories_reset", "trigger directory_people_rises",
		"trigger directory_people_written_at_the_reset", "trigger workspace_members_held",
		"trigger workspace_members_held_insert", "trigger workspaces_source_reset_insert",
		"trigger workspaces_source_version_rises",
	}; !slices.Equal(added, want) {
		t.Errorf("0022 added\n%v\nwant\n%v", added, want)
	}
	// Two tables gained columns, which ALTER TABLE writes into their text;
	// nothing else changed, nothing went.
	if !slices.Equal(changed, []string{"table workspace_members", "table workspaces"}) || len(dropped) != 0 {
		t.Errorf("0022 changed %v and dropped %v", changed, dropped)
	}
	if got := ftsAnswers(t, f.s); !mapsEqual(got, fts) {
		t.Errorf("the full-text index answers %v, and answered %v", got, fts)
	}
}

// seedJournal21 writes, at schema 21 or later, an entry of each kind.
func seedJournal21(t *testing.T, f seededSchema) {
	t.Helper()
	f.accepted("a journal", f.exec(`INSERT INTO mailbox_events(id, kind, workspace_id, account_id, actor, created_at,
		  cause, lost_user, purge_after) VALUES
		('mbe_0000000000000001', 'removed', 'wsp_t1', 'acc_t1', 'usr_0000000000000001', 900, '', '', 900 + 2592000),
		('mbe_0000000000000002', 'stranded', 'wsp_t1', 'acc_t2', 'cli', 910, 'disabled', 'usr_0000000000000006', 0),
		('mbe_0000000000000003', 'sync_paused', 'wsp_t2', 'acc_t3', 'usr_0000000000000004', 920, 'bound_stopped', '', 0)`))
}

func TestTheSchemaHoldsOnlyAMembershipOfATeamFromAnotherSource(t *testing.T) {
	// The seed: Fay (usr_6) is a member of the local team wsp_t1; ext_team
	// comes from another source, Fay and Dan (usr_4) its members.
	f := newSchemaLatest(t)
	const fay, dan = "usr_0000000000000006", "usr_0000000000000004"
	seedExternalTeam(t, f, "ext_team", fay)
	const onlyThere = "only a membership of a team from another source is held"

	f.refused("a membership held when written", "a membership is never held when written",
		f.exec(`INSERT INTO workspace_members(workspace_id, user_id, role, status, held_at, created_at, updated_at)
		VALUES ('ext_team', ?, 'member', 'disabled', 900, 900, 900)`, dan))
	f.refused("a local team's membership held", onlyThere, f.exec(`UPDATE workspace_members
		SET status = 'disabled', held_at = 900 WHERE workspace_id = 'wsp_t1' AND user_id = ?`, fay))
	f.refused("a personal workspace's membership held", "", f.exec(`UPDATE workspace_members
		SET status = 'disabled', held_at = 900 WHERE workspace_id = 'wsp_p_usr_0000000000000006'`))
	f.refused("a membership held and active", "CHECK", f.exec(`UPDATE workspace_members SET held_at = 900
		WHERE workspace_id = 'ext_team' AND user_id = ?`, fay))
	f.accepted("a membership of the team from elsewhere held", f.exec(`UPDATE workspace_members
		SET status = 'disabled', held_at = 900 WHERE workspace_id = 'ext_team' AND user_id = ?`, fay))
	f.refused("active again while held", "CHECK", f.exec(`UPDATE workspace_members SET status = 'active'
		WHERE workspace_id = 'ext_team' AND user_id = ?`, fay))
	f.accepted("its hold released, active again", f.exec(`UPDATE workspace_members SET status = 'active', held_at = 0
		WHERE workspace_id = 'ext_team' AND user_id = ?`, fay))
	if n := queryInt(t, f.s, `SELECT count(*) FROM workspace_members WHERE held_at <> 0`); n != 0 {
		t.Errorf("%d memberships held after the refused writes", n)
	}
}

// directoryFixture is the seed at the latest schema, following issuer at
// generation "g1", which said things of Bea (usr_2) and Fay (usr_6).
func directoryFixture(t *testing.T) seededSchema {
	t.Helper()
	f := newSchemaLatest(t)
	ctx := context.Background()
	err := f.s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := EnsureDirectoryTx(ctx, tx, issuer, "g1", 900); err != nil {
			return err
		}
		for _, p := range []DirectoryPerson{
			{Issuer: issuer, UserID: "usr_0000000000000002", Subject: "sub-bea", Generation: "g1", Version: 5,
				Status: "active", DisabledAtUS: 1_000_000, SessionsRevokedAtUS: 2_000_000, CredentialsRevokedAtUS: 3_000_000,
				AppliedAt: 900},
			{Issuer: issuer, UserID: "usr_0000000000000006", Subject: "sub-fay", Generation: "g1", Version: 2,
				Status: "active", AppliedAt: 900},
		} {
			if err := PutDirectoryPersonTx(ctx, tx, p); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f seededSchema) write(fn func(ctx context.Context, tx *sql.Tx) error) error {
	ctx := context.Background()
	return f.s.Write(ctx, func(tx *sql.Tx) error { return fn(ctx, tx) })
}

func TestTheSchemaNeverLowersWhatADirectoryRevoked(t *testing.T) {
	f := directoryFixture(t)
	const rises = "what a directory revoked, and its version, only rise"
	const bea = "usr_0000000000000002"
	for what, set := range map[string]string{
		"its disable":              "disabled_at_us = 999999",
		"its sessions' revocation": "sessions_revoked_at_us = 0",
		"its keys' revocation":     "credentials_revoked_at_us = 2999999",
		"another person":           "user_id = 'usr_0000000000000003'",
		"another subject":          "subject = 'sub-other'",
		"another directory":        "issuer = 'https://other.example.org'",
		"its version, lower":       "version = 4",
		"its reset, forged":        "reset = 1",
	} {
		f.refused(what, rises, f.exec(`UPDATE directory_people SET `+set+` WHERE user_id = ?`, bea))
	}
	f.accepted("every mark higher, and the version", f.exec(`UPDATE directory_people SET disabled_at_us = 1000001,
		sessions_revoked_at_us = 2000001, credentials_revoked_at_us = 3000001, version = 6 WHERE user_id = ?`, bea))
	// Recording a document whose marks are lower keeps the higher ones.
	if err := f.write(func(ctx context.Context, tx *sql.Tx) error {
		return PutDirectoryPersonTx(ctx, tx, DirectoryPerson{Issuer: issuer, UserID: bea, Subject: "sub-bea",
			Generation: "g1", Version: 7, Status: "disabled", DisabledAtUS: 5, AppliedAt: 950})
	}); err != nil {
		t.Fatal(err)
	}
	if got := queryStrings(t, f.s, `SELECT version || ' ' || status || ' ' || disabled_at_us || ' ' || sessions_revoked_at_us
		|| ' ' || credentials_revoked_at_us || ' ' || applied_at FROM directory_people WHERE user_id = ?`, bea); !slices.Equal(got, []string{
		"7 disabled 1000001 2000001 3000001 950",
	}) {
		t.Errorf("after a document with lower marks: %v", got)
	}
	// Gone once, for good.
	f.accepted("gone", f.write(func(ctx context.Context, tx *sql.Tx) error { return MarkDirectoryPersonGoneTx(ctx, tx, bea, 960) }))
	f.accepted("gone again changes nothing", f.write(func(ctx context.Context, tx *sql.Tx) error {
		return MarkDirectoryPersonGoneTx(ctx, tx, bea, 970)
	}))
	f.refused("back", rises, f.exec(`UPDATE directory_people SET gone_at = 0 WHERE user_id = ?`, bea))
	f.refused("gone at another moment", rises, f.exec(`UPDATE directory_people SET gone_at = 980 WHERE user_id = ?`, bea))
	if got := queryInt(t, f.s, `SELECT gone_at FROM directory_people WHERE user_id = ?`, bea); got != 960 {
		t.Errorf("gone at %d, want 960", got)
	}
}

func TestTheSchemaLetsAVersionFallOnlyToZeroWithARecordedReset(t *testing.T) {
	// Following issuer, whose document of Bea is at version 5; ext_team,
	// from another source, at version 3.
	f := directoryFixture(t)
	const bea = "usr_0000000000000002"
	const rises = "what a directory revoked, and its version, only rise"
	const wsFalls = "a workspace's source version falls only to 0, with a reset"
	seedExternalTeam(t, f, "ext_team")
	f.accepted("its version", f.exec(`UPDATE workspaces SET source_version = 3 WHERE id = 'ext_team'`))

	f.refused("a stray zeroing of a person's version", rises, f.exec(`UPDATE directory_people SET version = 0
		WHERE user_id = ?`, bea))
	f.refused("a zeroing under a forged reset", rises, f.exec(`UPDATE directory_people SET version = 0, reset = 1
		WHERE user_id = ?`, bea))
	f.refused("a stray zeroing of a workspace's version", wsFalls, f.exec(`UPDATE workspaces SET source_version = 0
		WHERE id = 'ext_team'`))
	f.refused("a zeroing under a forged sum", wsFalls, f.exec(`UPDATE workspaces SET source_version = 0, source_reset = 1
		WHERE id = 'ext_team'`))
	f.refused("a workspace's version lower", wsFalls, f.exec(`UPDATE workspaces SET source_version = 2
		WHERE id = 'ext_team'`))
	f.refused("a workspace from another source written at another sum", "a workspace from another source is written "+
		"at the directories' reset", f.exec(`INSERT INTO workspaces(id, kind, source, name, created_at, updated_at,
		source_reset) VALUES ('ext_other', 'team', 'platform', 'Other', 900, 900, 1)`))

	// The reset, recorded: every version falls to 0 under it, the marks
	// stay, and the directory starts again.
	var d Directory
	if err := f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		d, err = ResetDirectoryTx(ctx, tx, issuer, "g2", 1000)
		return err
	}); err != nil {
		t.Fatalf("the reset's writes were refused: %v", err)
	}
	if d.Generation != "g2" || d.Reset != 1 || d.Cursor != 0 || d.Confirmed || d.PublishedThroughUS != 0 {
		t.Errorf("reset, the directory is %+v", d)
	}
	if got := queryStrings(t, f.s, `SELECT user_id || ' ' || version || ' ' || reset || ' ' || disabled_at_us
		FROM directory_people ORDER BY user_id`); !slices.Equal(got, []string{
		bea + " 0 1 1000000", "usr_0000000000000006 0 1 0",
	}) {
		t.Errorf("reset, its people: %v", got)
	}
	if got := queryStrings(t, f.s, `SELECT id || ' ' || source_version || ' ' || source_reset FROM workspaces
		WHERE source <> 'local' OR source_version <> 0 OR source_reset <> 0`); !slices.Equal(got, []string{"ext_team 0 1"}) {
		t.Errorf("reset, the workspaces: %v", got)
	}
	// Versions rise again from there, and a new workspace is written at
	// the new sum.
	f.accepted("a person's version rising again", f.exec(`UPDATE directory_people SET version = 1 WHERE user_id = ?`, bea))
	f.accepted("a workspace's version rising again", f.exec(`UPDATE workspaces SET source_version = 1 WHERE id = 'ext_team'`))
	f.refused("a new workspace at the old sum", "a workspace from another source is written at the directories' reset",
		f.exec(`INSERT INTO workspaces(id, kind, source, name, created_at, updated_at) VALUES ('ext_other', 'team',
		'platform', 'Other', 900, 900)`))
	f.accepted("a new workspace at the new sum", f.exec(`INSERT INTO workspaces(id, kind, source, name, created_at,
		updated_at, source_reset) VALUES ('ext_other', 'team', 'platform', 'Other', 900, 900, 1)`))
	f.refused("a person written at an old reset", "a directory's person is written at its directory's reset",
		f.exec(`INSERT INTO directory_people(issuer, user_id, subject, generation, reset, status, applied_at)
		VALUES (?, 'usr_0000000000000004', 'sub-dan', 'g2', 0, 'active', 1000)`, issuer))
	// The writer writes a new person at the directory's reset, whatever it
	// is told.
	if err := f.write(func(ctx context.Context, tx *sql.Tx) error {
		return PutDirectoryPersonTx(ctx, tx, DirectoryPerson{Issuer: issuer, UserID: "usr_0000000000000004",
			Subject: "sub-dan", Generation: "g2", Version: 1, Reset: 0, Status: "active", AppliedAt: 1000})
	}); err != nil {
		t.Fatal(err)
	}
	if got := queryInt(t, f.s, `SELECT reset FROM directory_people WHERE user_id = 'usr_0000000000000004'`); got != 1 {
		t.Errorf("a new person written at reset %d, want 1", got)
	}
}

func TestTheSchemaGivesALocalWorkspaceNoSourceVersion(t *testing.T) {
	f := newSchemaLatest(t)
	personal := queryStrings(t, f.s, `SELECT id FROM workspaces WHERE person_id = 'usr_0000000000000002'`)
	if len(personal) != 1 {
		t.Fatalf("Bea's personal workspaces: %v", personal)
	}
	for _, id := range []string{"wsp_t1", personal[0], "wsp_operator"} {
		f.refused(id+" given a version", "CHECK", f.exec(`UPDATE workspaces SET source_version = 1 WHERE id = ?`, id))
		// Refused twice over: no directory was reset, and the workspace is
		// local.
		f.refused(id+" given a reset", "", f.exec(`UPDATE workspaces SET source_reset = 1 WHERE id = ?`, id))
	}
	if err := f.write(func(ctx context.Context, tx *sql.Tx) error {
		if _, err := EnsureDirectoryTx(ctx, tx, issuer, "g1", 900); err != nil {
			return err
		}
		_, err := ResetDirectoryTx(ctx, tx, issuer, "g2", 1000)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.refused("a local team given the sum of the resets", "CHECK", f.exec(`UPDATE workspaces SET source_reset = 1
		WHERE id = 'wsp_t1'`))
	f.refused("a local team written with a version", "CHECK", f.exec(`INSERT INTO workspaces(id, kind, name, created_at,
		updated_at, source_version) VALUES ('wsp_new', 'team', 'New', 900, 900, 1)`))
	if n := queryInt(t, f.s, `SELECT count(*) FROM workspaces WHERE source_version <> 0 OR source_reset <> 0`); n != 0 {
		t.Errorf("%d workspaces have a source version", n)
	}
}

func TestTheSchemaMovesADirectorysGenerationAndCursorBackOnlyWithAReset(t *testing.T) {
	f := directoryFixture(t)
	const back = "a directory's generation, cursor and confirmation go back only with a reset"
	f.accepted("read up to 10, published through, confirmed", f.exec(`UPDATE directories SET cursor = 10,
		published_through_us = 5000, confirmed = 1 WHERE issuer = ?`, issuer))
	for what, set := range map[string]string{
		"another generation":      "generation = 'g9'",
		"its cursor back":         "cursor = 9",
		"unconfirmed":             "confirmed = 0",
		"published through, back": "published_through_us = 4999",
		"its reset back":          "reset = -1",
		"another issuer":          "issuer = 'https://other.example.org'",
	} {
		if err := f.exec(`UPDATE directories SET `+set+` WHERE issuer = ?`, issuer); err == nil ||
			(!strings.Contains(err.Error(), back) && !strings.Contains(err.Error(), "CHECK")) {
			t.Errorf("%s: %v, want a refusal", what, err)
		}
	}
	f.refused("forgotten", "a directory followed is never forgotten", f.exec(`DELETE FROM directories`))
	f.accepted("forward", f.exec(`UPDATE directories SET cursor = 11, published_through_us = 6000 WHERE issuer = ?`, issuer))
	f.accepted("with a reset, everything back", f.exec(`UPDATE directories SET reset = 1, generation = 'g2', cursor = 0,
		published_through_us = 0, confirmed = 0 WHERE issuer = ?`, issuer))

	// The cursor's writer is a compare and set: never across a reset, nor
	// back, nor for a directory it did not find; published through only
	// rises.
	cursor := func(gen string, reset, at, published int64) error {
		return f.write(func(ctx context.Context, tx *sql.Tx) error {
			return SetDirectoryCursorTx(ctx, tx, issuer, gen, reset, at, published, 1000)
		})
	}
	for what, err := range map[string]error{
		"from before the reset":    cursor("g2", 0, 5, 0),
		"of the generation before": cursor("g1", 1, 5, 0),
		"for a directory never followed": f.write(func(ctx context.Context, tx *sql.Tx) error {
			return SetDirectoryCursorTx(ctx, tx, "https://other.example.org", "g2", 1, 5, 0, 1000)
		}),
	} {
		if !errors.Is(err, ErrDirectoryMoved) {
			t.Errorf("a cursor %s: %v, want ErrDirectoryMoved", what, err)
		}
	}
	f.accepted("a cursor", cursor("g2", 1, 5, 7000))
	if err := cursor("g2", 1, 4, 0); !errors.Is(err, ErrDirectoryMoved) {
		t.Errorf("a cursor behind: %v, want ErrDirectoryMoved", err)
	}
	f.accepted("the same cursor, published through earlier", cursor("g2", 1, 5, 6500))
	f.accepted("further, published through unknown", cursor("g2", 1, 8, 0))
	if got := queryStrings(t, f.s, `SELECT cursor || ' ' || published_through_us FROM directories`); !slices.Equal(got, []string{"8 7000"}) {
		t.Errorf("the cursor and published through are %v, want 8 7000", got)
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM directories`); n != 1 {
		t.Errorf("the cursor's writer made a directory: %d", n)
	}

	// Confirming: only the generation held, once.
	confirm := func(gen string) (changed bool, err error) {
		err = f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
			changed, err = ConfirmDirectoryTx(ctx, tx, issuer, gen, 1000)
			return err
		})
		return changed, err
	}
	if _, err := confirm("g1"); !errors.Is(err, ErrDirectoryMoved) {
		t.Errorf("confirming another generation: %v, want ErrDirectoryMoved", err)
	}
	if changed, err := confirm("g2"); err != nil || !changed {
		t.Errorf("confirming: %v, %v", changed, err)
	}
	if changed, err := confirm("g2"); err != nil || changed {
		t.Errorf("confirming twice: %v, %v, want nothing changed", changed, err)
	}
	// A reset of a directory never followed creates it, at reset 0.
	var d Directory
	if err := f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		d, err = ResetDirectoryTx(ctx, tx, "https://other.example.org", "h1", 1100)
		return err
	}); err != nil || d.Generation != "h1" || d.Reset != 0 || d.Cursor != 0 || d.Confirmed {
		t.Errorf("a first reset: %+v (%v)", d, err)
	}
}

func TestAGoneRowAndALiveRowMayShareASubject(t *testing.T) {
	// Hal (usr_8) signed in as sub-hal and is deleted here; the same
	// subject signs in again as a new person, Gil (usr_7). Fay (usr_6) is
	// followed, and here.
	f := directoryFixture(t)
	const hal, gil, fay = "usr_0000000000000008", "usr_0000000000000007", "usr_0000000000000006"
	execAll(t, f.s, person(hal, "hal@example.org", "member", "active", 0, "")...)
	put := func(user, subject string) error {
		return f.write(func(ctx context.Context, tx *sql.Tx) error {
			return PutDirectoryPersonTx(ctx, tx, DirectoryPerson{Issuer: issuer, UserID: user, Subject: subject,
				Generation: "g1", Version: 1, Status: "active", AppliedAt: 1000})
		})
	}
	f.accepted("Hal followed", put(hal, "sub-hal"))
	f.refused("two people here with one subject", "UNIQUE", put(gil, "sub-hal"))
	f.accepted("Hal gone", f.write(func(ctx context.Context, tx *sql.Tx) error {
		return MarkDirectoryPersonGoneTx(ctx, tx, hal, 950)
	}))
	f.accepted("the subject again, for Gil", put(gil, "sub-hal"))
	if got := queryStrings(t, f.s, `SELECT user_id || ' ' || gone_at FROM directory_people WHERE subject = 'sub-hal'
		ORDER BY user_id`); !slices.Equal(got, []string{gil + " 0", hal + " 950"}) {
		t.Errorf("the rows of sub-hal: %v", got)
	}

	// Released only once he is gone from here too, which forgets him in
	// the journal as his deletion would have; Fay, still followed and
	// still here, stays named.
	f.accepted("Hal's and Fay's names in the journal", f.exec(`INSERT INTO mailbox_events(id, kind, workspace_id,
		account_id, actor, created_at, cause, lost_user) VALUES
		('mbe_0000000000000001', 'stranded', 'wsp_t1', 'acc_t1', ?1, 900, 'disabled', ?2),
		('mbe_0000000000000002', 'stranded', 'wsp_t1', 'acc_t2', ?2, 900, 'deleted', ?1)`, hal, fay))
	release := func(user string) error {
		return f.write(func(ctx context.Context, tx *sql.Tx) error { return ReleaseDirectoryPersonTx(ctx, tx, issuer, user) })
	}
	if err := release(hal); !errors.Is(err, ErrPersonStillHere) {
		t.Fatalf("releasing Hal while he is here: %v, want ErrPersonStillHere", err)
	}
	execAll(t, f.s, `DELETE FROM workspaces WHERE person_id = '`+hal+`'`, `DELETE FROM users WHERE id = '`+hal+`'`)
	// Deleted, but followed: the journal keeps his name until the release.
	f.accepted("forgotten as a person nobody follows would be", f.write(func(ctx context.Context, tx *sql.Tx) error {
		return ForgetPersonInJournalTx(ctx, tx, hal)
	}))
	if n := queryInt(t, f.s, `SELECT count(*) FROM mailbox_events WHERE actor = ?1 OR lost_user = ?1`, hal); n != 2 {
		t.Errorf("before his release, %d entries name Hal, want 2", n)
	}
	f.accepted("Hal released", release(hal))
	if got := queryStrings(t, f.s, `SELECT id || '|' || actor || '|' || lost_user FROM mailbox_events ORDER BY seq`); !slices.Equal(got, []string{
		"mbe_0000000000000001||" + fay, "mbe_0000000000000002|" + fay + "|",
	}) {
		t.Errorf("the journal after Hal's release: %v", got)
	}
	if got := queryStrings(t, f.s, `SELECT user_id FROM directory_people WHERE subject = 'sub-hal'`); !slices.Equal(got, []string{gil}) {
		t.Errorf("after the release, sub-hal's rows: %v", got)
	}
	f.accepted("releasing him again changes nothing", release(hal))
}

func TestAnExtensionsStateTakesAMebibyteAndRefusesMore(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	full := bytes.Repeat([]byte{0xa5}, MaxExtensionStateValue)
	if err := f.s.WriteExtensionState(ctx, "follower", map[string][]byte{
		"cursor/ws1": []byte("41"), "big": full, "empty": {}, "none": nil, strings.Repeat("k", 256): []byte("x"),
	}, nil); err != nil {
		t.Fatalf("a mebibyte, an empty value and a 256-byte key: %v", err)
	}
	for key, want := range map[string][]byte{"big": full, "cursor/ws1": []byte("41"), "empty": {}, "none": {}} {
		got, ok, err := f.s.ExtensionState(ctx, "follower", key)
		if err != nil || !ok || !bytes.Equal(got, want) || got == nil {
			t.Errorf("%s reads back %d bytes (%t, %v)", key, len(got), ok, err)
		}
	}
	// One byte more is refused, and so is everything written with it.
	for what, put := range map[string]map[string][]byte{
		"a value over a mebibyte": {"cursor/ws1": []byte("42"), "big": append(full, 0)},
		"a key over 256 bytes":    {"cursor/ws1": []byte("42"), strings.Repeat("k", 257): nil},
		"an empty key":            {"cursor/ws1": []byte("42"), "": nil},
	} {
		if err := f.s.WriteExtensionState(ctx, "follower", put, nil); !errors.Is(err, ErrExtensionState) {
			t.Errorf("%s: %v, want ErrExtensionState", what, err)
		}
	}
	if err := f.s.WriteExtensionState(ctx, strings.Repeat("e", 65), map[string][]byte{"k": nil}, nil); !errors.Is(err, ErrExtensionState) {
		t.Errorf("an extension's name over 64 bytes: %v, want ErrExtensionState", err)
	}
	if got, _, _ := f.s.ExtensionState(ctx, "follower", "cursor/ws1"); string(got) != "41" {
		t.Errorf("a refused write changed the cursor to %q", got)
	}
	// The schema holds the bound whatever writes.
	f.refused("over a mebibyte, by hand", "CHECK", f.exec(`INSERT INTO extension_state(extension, key, value, updated_at)
		VALUES ('follower', 'raw', zeroblob(1048577), 900)`))
	f.refused("text, by hand", "CHECK", f.exec(`INSERT INTO extension_state(extension, key, value, updated_at)
		VALUES ('follower', 'raw', 'text', 900)`))

	// Keys by prefix, sorted, the extension's own only; deletions and puts
	// in one write.
	if err := f.s.WriteExtensionState(ctx, "other", map[string][]byte{"cursor/ws0": nil}, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.s.WriteExtensionState(ctx, "follower", map[string][]byte{"cursor/ws0": []byte("1"), "cursor/ws2": nil},
		[]string{"big", "none", "absent"}); err != nil {
		t.Fatal(err)
	}
	keys, err := f.s.ExtensionStateKeys(ctx, "follower", "cursor/")
	if err != nil || !slices.Equal(keys, []string{"cursor/ws0", "cursor/ws1", "cursor/ws2"}) {
		t.Errorf("the follower's cursors: %v (%v)", keys, err)
	}
	keys, err = f.s.ExtensionStateKeys(ctx, "follower", "")
	if err != nil || !slices.Equal(keys, []string{"cursor/ws0", "cursor/ws1", "cursor/ws2", "empty", strings.Repeat("k", 256)}) {
		t.Errorf("every key of the follower: %v (%v)", keys, err)
	}
	if _, ok, err := f.s.ExtensionState(ctx, "follower", "big"); ok || err != nil {
		t.Errorf("a deleted key still reads (%t, %v)", ok, err)
	}
}
