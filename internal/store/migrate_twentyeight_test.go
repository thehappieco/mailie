package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Migration 0028: keys an issuer makes elsewhere, mirrored here
// (docs/workspaces.md, "Keys an issuer makes").

const (
	testIssuer = "https://issuer.example.org"
	// issuedHash is an issued key's hash, as its issuer computes one.
	issuedHash = "a0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf"
)

func TestMigration28ChangesNoRowOfASchema27Database(t *testing.T) {
	f := newSchemaAt(t, 27)
	seedJournalAt24(t, f)
	// A directory followed, a person of it and a team from it whose mailbox
	// an own key holds: what 0028's triggers read, untouched by it.
	f.accepted("a directory", f.exec(`INSERT INTO directories(issuer, generation, reset, changed_at)
		VALUES (?, 'g1', 2, 900)`, testIssuer))
	f.accepted("a person of it", f.exec(`INSERT INTO directory_people(issuer, user_id, subject, generation, version, reset,
		status, applied_at) VALUES (?, 'usr_0000000000000002', 'sub-bea', 'g1', 3, 2, 'active', 900)`, testIssuer))
	seedExternalTeam(t, f, "ext_team", "usr_0000000000000002")
	externalMailbox(t, f, "acc_e1", "ext_team")
	f.accepted("a key of the team", f.exec(`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at,
		workspace_id, terms_version, created_by) VALUES ('e1e1e1e1', '$argon2id$k', 'Team key', 'write', 900, 99999999,
		'ext_team', 'terms-1', 'usr_0000000000000002')`))
	f.accepted("what it holds", f.exec(`INSERT INTO key_access(key_prefix, account_id, workspace_id, read, act,
		granted_by, created_at, updated_at) VALUES ('e1e1e1e1', 'acc_e1', 'ext_team', 1, 1, 'usr_0000000000000002',
		900, 900)`))
	keys := queryInt(t, f.s, `SELECT count(*) FROM api_keys`)
	if keys < 5 {
		t.Fatalf("the seed holds %d keys; the test would prove nothing of them", keys)
	}
	marks := queryStrings(t, f.s, `SELECT workspace_id || ' ' || seq FROM workspace_changes ORDER BY seq`)
	before, schema, fts := dumpTables(t, f.s), schemaSQL(t, f.s), ftsAnswers(t, f.s)
	migrateTo(t, f.s, 28)

	tried := compareMigrated(t, before, dumpTables(t, f.s), map[string]map[string]addedColumn{
		"api_keys": {"issuer": sameValue("string:")},
	}, []string{
		"directory_keys", "issued_key_ends", "issued_key_grants", "issued_key_stops", "issued_key_tell",
		"issued_key_uses", "issued_keys",
	})
	if tried["api_keys"] != int(keys) {
		t.Errorf("the seed tried the migration on %d keys of %d", tried["api_keys"], keys)
	}
	// The migration marks nothing.
	if got := queryStrings(t, f.s, `SELECT workspace_id || ' ' || seq FROM workspace_changes ORDER BY seq`); !slices.Equal(got, marks) {
		t.Errorf("the workspace marks are %v, and were %v", got, marks)
	}

	added, changed, dropped := schemaChanges(schema, schemaSQL(t, f.s))
	if want := []string{
		"index api_keys_issuer", "index issued_key_grants_account", "index issued_key_grants_waiting",
		"index issued_key_tell_held", "index issued_key_tell_issuer", "index issued_key_tell_prefix",
		"index sqlite_autoindex_directory_keys_1", "index sqlite_autoindex_issued_key_ends_1",
		"index sqlite_autoindex_issued_key_grants_1", "index sqlite_autoindex_issued_key_stops_1",
		"index sqlite_autoindex_issued_key_uses_1", "index sqlite_autoindex_issued_keys_1",
		"index sqlite_autoindex_issued_keys_2",
		"table directory_keys", "table issued_key_ends", "table issued_key_grants", "table issued_key_stops",
		"table issued_key_tell", "table issued_key_uses", "table issued_keys",
		"trigger accounts_removed_issued_grants", "trigger api_keys_issued_change", "trigger api_keys_issued_expiry",
		"trigger api_keys_issued_fixed", "trigger api_keys_issued_forgotten", "trigger api_keys_issued_insert",
		"trigger api_keys_issued_revocation_kept", "trigger api_keys_issued_shape_insert",
		"trigger api_keys_issued_shape_update", "trigger api_keys_issuer_fixed",
		"trigger directory_keys_kept", "trigger directory_keys_rises", "trigger directory_keys_written_at_the_reset",
		"trigger issued_key_ends_fixed", "trigger issued_key_stops_change_delete", "trigger issued_key_stops_change_insert",
		"trigger issued_keys_change", "trigger issued_keys_end_kept", "trigger issued_keys_fixed",
		"trigger issued_keys_leave_their_end", "trigger issued_keys_of_their_key", "trigger issued_keys_version",
		"trigger key_access_change_delete", "trigger key_access_change_insert", "trigger key_access_change_update",
	}; !slices.Equal(added, want) {
		t.Errorf("0028 added\n%v\nwant\n%v", added, want)
	}
	if want := []string{"table api_keys"}; !slices.Equal(changed, want) || len(dropped) != 0 {
		t.Errorf("0028 changed %v and dropped %v", changed, dropped)
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

// issuedSchema is the seed at the latest schema with Ivy (usr_i), a person
// whose personal workspace (wsp_i) holds her mailbox acc_i1, and a directory
// followed: what an issued key is written for.
func issuedSchema(t *testing.T) seededSchema {
	t.Helper()
	f := newSchemaLatest(t)
	for _, q := range []string{
		`INSERT INTO users(id, email, password_hash, role, password_changed_at, created_at, updated_at)
		 VALUES ('usr_i', 'ivy@example.com', '', 'member', 1, 800, 800)`,
		`INSERT INTO workspaces(id, kind, person_id, created_at, updated_at) VALUES ('wsp_i', 'personal', 'usr_i', 800, 800)`,
		`INSERT INTO workspace_members(workspace_id, user_id, role, status, created_at, updated_at)
		 VALUES ('wsp_i', 'usr_i', 'owner', 'active', 800, 800)`,
		`INSERT INTO accounts(id, workspace_id, owner_user_id, email, provider, auth_kind, imap_host, imap_port, smtp_host,
		 smtp_port, smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at)
		 VALUES ('acc_i1', 'wsp_i', 'usr_i', 'ivy@mail.example.org', 'imap', 'password', 'imap.example.org', 993,
		 'smtp.example.org', 587, 'starttls', 'ivy', 1, 'active', 800, 800, 800)`,
		`INSERT INTO directories(issuer, generation, reset, changed_at) VALUES ('` + testIssuer + `', 'g1', 0, 800)`,
	} {
		f.accepted("the scene", f.exec(q))
	}
	return f
}

// issue writes an issued key of Ivy's, live until expires, as an extension
// that mirrors an issuer's keys would.
func (f seededSchema) issue(prefix string, expires int64) {
	f.t.Helper()
	f.accepted("the key "+prefix, f.exec(`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at,
		workspace_id, terms_version, created_by, issuer) VALUES (?, ?, '', 'write', 900, ?, 'wsp_i', 'terms-3', 'usr_i', ?)`,
		prefix, issuedHash, expires, testIssuer))
	f.accepted("its record "+prefix, f.exec(`INSERT INTO issued_keys(prefix, issuer, external_id, version, created_at_us,
		terms_ok) VALUES (?, ?, 'id-' || ?, 1, 900000000, 1)`, prefix, testIssuer, prefix))
}

func TestAnIssuedKeyIsNeverAnOperatorKey(t *testing.T) {
	f := issuedSchema(t)
	for _, ws := range []string{"'wsp_operator'", "NULL"} {
		f.refused("an issued key in "+ws, "an issued key belongs to a workspace",
			f.exec(`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at, workspace_id, created_by, issuer)
			VALUES ('0123456789abcdef', ?, '', 'write', 900, 99999999, `+ws+`, 'usr_i', ?)`, issuedHash, testIssuer))
	}
	f.accepted("an operator key", f.exec(`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at,
		workspace_id, created_by) VALUES ('0a0a0a0a', '$argon2id$k', 'cli', 'admin', 900, 99999999, 'wsp_operator', 'cli')`))
}

func TestAnIssuedKeyHasNoNameNoOriginAndTheWriteScope(t *testing.T) {
	f := issuedSchema(t)
	insert := func(set map[string]string) error {
		cols := map[string]string{
			"prefix": "'0123456789abcdef'", "hash": "'" + issuedHash + "'", "name": "''", "scope": "'write'",
			"created_at": "900", "expires_at": "99999999", "workspace_id": "'wsp_i'", "created_by": "'usr_i'",
			"issuer": "'" + testIssuer + "'", "origin": "''", "last_used_at": "0", "restricted": "0",
		}
		for k, v := range set {
			cols[k] = v
		}
		var names, values []string
		for k, v := range cols {
			names, values = append(names, k), append(values, v)
		}
		return f.exec(`INSERT INTO api_keys(` + strings.Join(names, ", ") + `) VALUES (` + strings.Join(values, ", ") + `)`)
	}
	const why = "an issued key has no name, origin or last use, the write scope"
	for what, set := range map[string]map[string]string{
		"a name":             {"name": "'Claude'"},
		"an origin":          {"origin": "'person'"},
		"the read scope":     {"scope": "'read'"},
		"the send scope":     {"scope": "'send'"},
		"a last use":         {"last_used_at": "950"},
		"a restriction":      {"restricted": "1"},
		"no person":          {"created_by": "'cli'"},
		"a key's creator":    {"created_by": "'key:aaaaaaaa'"},
		"a selector too big": {"prefix": "'" + strings.Repeat("a", 65) + "'"},
	} {
		f.refused("an issued key with "+what, why, insert(set))
	}
	f.issue("0123456789abcdef", 99999999)
	for what, set := range map[string]string{
		"named": "name = 'Claude'", "given an origin": "origin = 'person'", "of the send scope": "scope = 'send'",
		"used": "last_used_at = 950", "restricted": "restricted = 1",
	} {
		f.refused("an issued key "+what, why, f.exec(`UPDATE api_keys SET `+set+` WHERE prefix = '0123456789abcdef'`))
	}
	// An own key is as it was.
	f.accepted("an own key renamed", f.exec(`UPDATE api_keys SET name = 'renamed' WHERE prefix = '88888888'`))
}

func TestAnIssuedKeysIssuerNeverChanges(t *testing.T) {
	f := issuedSchema(t)
	f.issue("0123456789abcdef", 99999999)
	f.refused("an issued key's issuer changed", "a key's issuer never changes",
		f.exec(`UPDATE api_keys SET issuer = 'https://other.example.org' WHERE prefix = '0123456789abcdef'`))
	f.refused("an issued key made an own key", "a key's issuer never changes",
		f.exec(`UPDATE api_keys SET issuer = '' WHERE prefix = '0123456789abcdef'`))
	f.refused("an own key made an issued key", "a key's issuer never changes",
		f.exec(`UPDATE api_keys SET issuer = ? WHERE prefix = '88888888'`, testIssuer))
	f.refused("a record of another issuer", "an issued key's record names a key of its issuer",
		f.exec(`INSERT INTO issued_keys(prefix, issuer, external_id, created_at_us, terms_ok)
		VALUES ('88888888', ?, 'id-own', 900000000, 1)`, testIssuer))
	f.refused("its record's issuer changed", "an issued key's id, creation and judged terms never change",
		f.exec(`UPDATE issued_keys SET issuer = 'https://other.example.org' WHERE prefix = '0123456789abcdef'`))
}

func TestAnIssuedKeysHashCreatorCreationAndWorkspaceNeverChange(t *testing.T) {
	f := issuedSchema(t)
	f.issue("0123456789abcdef", 99999999)
	const why = "an issued key's hash, creator, creation and workspace never change"
	for what, set := range map[string]string{
		"its hash": "hash = '" + strings.Repeat("0", 64) + "'", "its creator": "created_by = 'usr_0000000000000001'",
		"its creator forgotten": "created_by = ''", "its creation": "created_at = 901",
	} {
		f.refused(what, why, f.exec(`UPDATE api_keys SET `+set+` WHERE prefix = '0123456789abcdef'`))
	}
	f.refused("its workspace", "", f.exec(`UPDATE api_keys SET workspace_id = (SELECT id FROM workspaces
		WHERE person_id = 'usr_0000000000000001') WHERE prefix = '0123456789abcdef'`))
	for what, set := range map[string]string{
		"its id": "external_id = 'other'", "its creation at the issuer": "created_at_us = 1",
		"its judged terms": "terms_ok = 0",
	} {
		f.refused("the record's "+what, "an issued key's id, creation and judged terms never change",
			f.exec(`UPDATE issued_keys SET `+set+` WHERE prefix = '0123456789abcdef'`))
	}
	f.accepted("a version rising", f.exec(`UPDATE issued_keys SET version = 5 WHERE prefix = '0123456789abcdef'`))
	f.refused("a version falling", "an issued key's version falls only to 0",
		f.exec(`UPDATE issued_keys SET version = 4 WHERE prefix = '0123456789abcdef'`))
	f.accepted("a version zeroed at a reset", f.exec(`UPDATE issued_keys SET version = 0 WHERE prefix = '0123456789abcdef'`))
}

func TestARevokedIssuedKeyIsNeverLiveAgain(t *testing.T) {
	f := issuedSchema(t)
	f.issue("0123456789abcdef", 99999999)
	f.accepted("its revocation", f.exec(`UPDATE api_keys SET revoked_at = 950, stop_reason = 'manual'
		WHERE prefix = '0123456789abcdef'`))
	f.accepted("its end", f.exec(`UPDATE issued_keys SET ended_at = 950, end_kind = 'revoked'
		WHERE prefix = '0123456789abcdef'`))
	f.refused("its revocation undone", "a revoked issued key is never live again",
		f.exec(`UPDATE api_keys SET revoked_at = 0 WHERE prefix = '0123456789abcdef'`))
	f.refused("its end undone", "an issued key that ended never lives again",
		f.exec(`UPDATE issued_keys SET ended_at = 0, end_kind = '' WHERE prefix = '0123456789abcdef'`))
	f.refused("an end without its kind", "CHECK constraint failed",
		f.exec(`UPDATE issued_keys SET end_kind = '' WHERE prefix = '0123456789abcdef'`))
}

func TestAnIssuedKeysExpiryNeverMovesLater(t *testing.T) {
	f := issuedSchema(t)
	f.issue("0123456789abcdef", 99999999)
	f.refused("a later expiry", "an issued key's expiry never moves later",
		f.exec(`UPDATE api_keys SET expires_at = 99999999 + 1 WHERE prefix = '0123456789abcdef'`))
	f.accepted("an earlier expiry", f.exec(`UPDATE api_keys SET expires_at = 5000 WHERE prefix = '0123456789abcdef'`))
	f.refused("the earlier one moved later", "an issued key's expiry never moves later",
		f.exec(`UPDATE api_keys SET expires_at = 5001 WHERE prefix = '0123456789abcdef'`))
	// An own key's is the repository's to decide.
	f.accepted("an own key's expiry moved", f.exec(`UPDATE api_keys SET expires_at = expires_at + 10 WHERE prefix = '88888888'`))
}

func TestAnIssuedKeyNeverHasANeverExpiringExpiry(t *testing.T) {
	f := issuedSchema(t)
	f.refused("an issued key that never expires", "CHECK constraint failed",
		f.exec(`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at, workspace_id, created_by, issuer)
		VALUES ('0123456789abcdef', ?, '', 'write', 900, 0, 'wsp_i', 'usr_i', ?)`, issuedHash, testIssuer))
	f.issue("0123456789abcdef", 99999999)
	f.refused("its expiry written as none", "an issued key always expires",
		f.exec(`UPDATE api_keys SET expires_at = 0 WHERE prefix = '0123456789abcdef'`))
	f.accepted("an own key that never expires", f.exec(`INSERT INTO api_keys(prefix, hash, name, scope, created_at,
		expires_at, workspace_id, created_by) VALUES ('0b0b0b0b', '$argon2id$k', 'old', 'admin', 900, 0, 'wsp_operator', 'cli')`))
}

func TestAnIssuedKeyThatGoesWithItsWorkspaceLeavesOnlyItsSelectorHowAndEnd(t *testing.T) {
	f := issuedSchema(t)
	// Ivy's mailbox goes first, as a person's closure deletes it, so that
	// her workspace can go.
	f.accepted("Ivy's mailbox deleted", f.exec(`DELETE FROM accounts WHERE id = 'acc_i1'`))
	keys := map[string]struct {
		endKind string
		endedAt int64
		stops   []string
	}{
		"00000000000000a1": {}, // live
		"00000000000000a2": {"revoked", 940, nil},
		"00000000000000a3": {"unlisted", 941, []string{"operator"}},
		"00000000000000a4": {"expired", 942, []string{"creator"}},
		"00000000000000a5": {"expired", 943, []string{"operator", "creator"}},
		"00000000000000a6": {"expired", 944, nil},
	}
	for prefix, k := range keys {
		f.issue(prefix, 99999999)
		if k.endKind != "" {
			f.accepted("the end of "+prefix, f.exec(`UPDATE issued_keys SET ended_at = ?, end_kind = ? WHERE prefix = ?`,
				k.endedAt, k.endKind, prefix))
		}
		for _, cause := range k.stops {
			f.accepted("a stop of "+prefix, f.exec(`INSERT INTO issued_key_stops(prefix, cause, at) VALUES (?, ?, 930)`,
				prefix, cause))
		}
		f.accepted("a refusal owed of "+prefix, f.exec(`INSERT INTO issued_key_tell(issuer, kind, external_id, prefix,
			version, account_id, use, cause, received_at) VALUES (?, 'refusal', 'id-' || ?, ?, 1, 'acc_x', 'read',
			'giver_not_reader', 930)`, testIssuer, prefix, prefix))
	}
	f.accepted("a refusal owed of a record never stored", f.exec(`INSERT INTO issued_key_tell(issuer, kind, external_id,
		version, account_id, use, cause, received_at) VALUES (?, 'refusal', 'id-never', 1, 'acc_x', 'read',
		'mailbox_unknown', 930)`, testIssuer))

	before := time.Now().Unix()
	// The workspace goes, as it goes with its person: the keys cascade from
	// it, and their records from them.
	f.accepted("Ivy's workspace deleted", f.exec(`DELETE FROM workspaces WHERE id = 'wsp_i'`))
	after := time.Now().Unix()

	for _, table := range []string{"issued_keys", "issued_key_stops", "issued_key_grants", "issued_key_uses"} {
		if n := queryInt(t, f.s, `SELECT count(*) FROM `+table); n != 0 {
			t.Errorf("%s holds %d rows once the keys went", table, n)
		}
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM api_keys WHERE issuer <> ''`); n != 0 {
		t.Errorf("%d issued keys outlived their workspace", n)
	}
	if got := queryStrings(t, f.s, `SELECT external_id FROM issued_key_tell`); !slices.Equal(got, []string{"id-never"}) {
		t.Errorf("what is owed once the keys went: %v, want only the record never stored", got)
	}
	want := map[string]string{
		"00000000000000a2": "revoked 940", "00000000000000a3": "revoked 941", "00000000000000a4": "held 942",
		"00000000000000a5": "held 943", "00000000000000a6": "expired 944",
	}
	got := map[string]string{}
	for _, row := range queryStrings(t, f.s, `SELECT prefix || ' ' || how || ' ' || ended_at || ' ' || forget_at
		FROM issued_key_ends`) {
		parts := strings.Fields(row)
		if parts[3] != "31536900" {
			t.Errorf("%s is forgotten at %s, want its creation and a year", parts[0], parts[3])
		}
		got[parts[0]] = parts[1] + " " + parts[2]
	}
	live := strings.Fields(got["00000000000000a1"])
	delete(got, "00000000000000a1")
	if len(live) != 2 || live[0] != "revoked" || atoi(t, live[1]) < before || atoi(t, live[1]) > after {
		t.Errorf("a key still live when its workspace went left %v, want revoked at the deletion's moment", live)
	}
	if len(got) != len(want) {
		t.Errorf("what is left: %v, want %v", got, want)
	}
	for prefix, w := range want {
		if got[prefix] != w {
			t.Errorf("%s left %q, want %q", prefix, got[prefix], w)
		}
	}
	if cols := queryStrings(t, f.s, `SELECT name FROM pragma_table_info('issued_key_ends') ORDER BY cid`); !slices.Equal(cols,
		[]string{"prefix", "how", "ended_at", "forget_at"}) {
		t.Errorf("what stays of an issued key is %v", cols)
	}
}

func atoi(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTheSchemaNeverStoresAForgottenSelectorAgain(t *testing.T) {
	f := issuedSchema(t)
	f.issue("0123456789abcdef", 99999999)
	f.accepted("its row deleted", f.exec(`DELETE FROM api_keys WHERE prefix = '0123456789abcdef'`))
	if n := queryInt(t, f.s, `SELECT count(*) FROM issued_key_ends WHERE prefix = '0123456789abcdef'`); n != 1 {
		t.Fatalf("the key left %d ends", n)
	}
	f.refused("the selector stored again", "an issued key this server forgot is never stored again",
		f.exec(`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at, workspace_id, created_by, issuer)
		VALUES ('0123456789abcdef', ?, '', 'write', 900, 99999999, 'wsp_i', 'usr_i', ?)`, issuedHash, testIssuer))
	f.refused("what is left changed", "what is left of an issued key never changes",
		f.exec(`UPDATE issued_key_ends SET forget_at = 1 WHERE prefix = '0123456789abcdef'`))
	f.accepted("what is left forgotten", f.exec(`DELETE FROM issued_key_ends WHERE prefix = '0123456789abcdef'`))
}

func TestTheKeysCursorGoesBackOnlyWithAReset(t *testing.T) {
	f := issuedSchema(t)
	f.refused("the keys' row at another reset", "an issuer's keys are written at its directory's reset",
		f.exec(`INSERT INTO directory_keys(issuer, reset) VALUES (?, 1)`, testIssuer))
	f.refused("the keys' row of a directory not followed", "an issuer's keys are written at its directory's reset",
		f.exec(`INSERT INTO directory_keys(issuer, reset) VALUES ('https://other.example.org', 0)`))
	f.accepted("the keys' row", f.exec(`INSERT INTO directory_keys(issuer, reset) VALUES (?, 0)`, testIssuer))
	f.accepted("the cursor moving", f.exec(`UPDATE directory_keys SET cursor = 40, published_through_us = 900
		WHERE issuer = ?`, testIssuer))
	const why = "an issuer's cursor goes back only with a reset"
	f.refused("the cursor going back", why, f.exec(`UPDATE directory_keys SET cursor = 39 WHERE issuer = ?`, testIssuer))
	f.refused("what was published going back", why,
		f.exec(`UPDATE directory_keys SET published_through_us = 899 WHERE issuer = ?`, testIssuer))
	f.refused("a reset its directory did not record", why,
		f.exec(`UPDATE directory_keys SET reset = 1, cursor = 0 WHERE issuer = ?`, testIssuer))
	f.accepted("the directory's reset", f.exec(`UPDATE directories SET reset = 1, generation = 'g2', cursor = 0
		WHERE issuer = ?`, testIssuer))
	f.accepted("the keys' reset", f.exec(`UPDATE directory_keys SET reset = 1, cursor = 0, published_through_us = 0,
		trusted_through_us = 900, pass_open = 1 WHERE issuer = ?`, testIssuer))
	f.refused("what was trusted falling", why,
		f.exec(`UPDATE directory_keys SET trusted_through_us = 899 WHERE issuer = ?`, testIssuer))
	f.refused("the keys' row deleted", "an issuer followed is never forgotten",
		f.exec(`DELETE FROM directory_keys WHERE issuer = ?`, testIssuer))
}

func TestTheSchemaRecordsAnIssuedGrantWithItsCause(t *testing.T) {
	f := issuedSchema(t)
	f.issue("0123456789abcdef", 99999999)
	grant := func(use, state, cause string) error {
		return f.exec(`INSERT INTO issued_key_grants(prefix, account_id, use, given_by, given_at_us, state, cause, version)
			VALUES ('0123456789abcdef', 'acc_i1', ?, 'sub-ivy', 900000000, ?, ?, 1)`, use, state, cause)
	}
	f.refused("a held grant with a cause", "CHECK constraint failed", grant("read", "held", "reset"))
	f.refused("a refusal without one", "CHECK constraint failed", grant("read", "refused", ""))
	f.refused("a refusal for a removal's cause", "CHECK constraint failed", grant("read", "refused", "mailbox_removed"))
	f.refused("a tombstone for a refusal's cause", "CHECK constraint failed", grant("read", "removed", "not_offered"))
	f.refused("a grant of manage", "CHECK constraint failed", grant("manage", "held", ""))
	f.refused("a grant on a mailbox nobody has", "FOREIGN KEY constraint failed",
		f.exec(`INSERT INTO issued_key_grants(prefix, account_id, use, given_by, given_at_us, state)
		VALUES ('0123456789abcdef', 'acc_nobody', 'read', 'sub-ivy', 900000000, 'held')`))
	f.accepted("read held", grant("read", "held", ""))
	f.accepted("act waiting", grant("act", "waiting", ""))
	f.accepted("send refused", grant("send", "refused", "not_offered"))
	f.refused("a refusal owed at version 0", "CHECK constraint failed", f.exec(`INSERT INTO issued_key_tell(issuer, kind,
		external_id, version, account_id, use, cause, received_at) VALUES (?, 'refusal', 'id-x', 0, 'acc_i1', 'read',
		'not_offered', 930)`, testIssuer))
	f.accepted("a narrowing owed", f.exec(`INSERT INTO issued_key_tell(issuer, kind, external_id, revoked, removed,
		received_at) VALUES (?, 'held', 'id-x', 1, '[{"item":"acc_i1","perm":"read"}]', 930)`, testIssuer))
	f.refused("a second narrowing owed of one key", "UNIQUE constraint failed", f.exec(`INSERT INTO issued_key_tell(issuer,
		kind, external_id, received_at) VALUES (?, 'held', 'id-x', 931)`, testIssuer))

	// The mailbox removed: what was held or waited for is a tombstone, and a
	// recovery gives it back to nobody.
	f.remove("acc_i1", "usr_i", 950)
	if got := queryStrings(t, f.s, `SELECT use || ' ' || state || ' ' || cause FROM issued_key_grants ORDER BY use`); !slices.Equal(got,
		[]string{"act removed mailbox_removed", "read removed mailbox_removed", "send refused not_offered"}) {
		t.Errorf("the grants once the mailbox was removed: %v", got)
	}
	f.accepted("the mailbox recovered", f.exec(`UPDATE accounts SET removed_at = 0, removed_by = '', purge_after = 0
		WHERE id = 'acc_i1'`))
	if n := queryInt(t, f.s, `SELECT count(*) FROM issued_key_grants WHERE state IN ('held', 'waiting')`); n != 0 {
		t.Errorf("a recovery gave %d grants back", n)
	}
}

func TestTheSchemaCountsAnIssuedKeysRefusedUsesByKind(t *testing.T) {
	f := issuedSchema(t)
	f.issue("0123456789abcdef", 99999999)
	use := func(account, use, refused string) error {
		return f.exec(`INSERT INTO issued_key_uses(prefix, day, account_id, use, refused, count)
			VALUES ('0123456789abcdef', '2026-11-19', ?, ?, ?, 1)`, account, use, refused)
	}
	f.accepted("a key refused", use("", "", "revoked"))
	f.accepted("a use refused", use("acc_i1", "act", "creator_consent"))
	f.refused("a key refused naming a mailbox", "CHECK constraint failed", use("acc_i1", "read", "expired"))
	f.refused("a use refused naming none", "CHECK constraint failed", use("", "", "no_flag"))
	f.refused("a refusal the core does not make", "CHECK constraint failed", use("acc_i1", "read", "stale_feed"))
}

func TestAnIssuedKeysChangeMarksItsWorkspace(t *testing.T) {
	// changesFixture: ext_a, a team from another source, with acc_a1; an
	// issued key of Bea's there is what the schema allows, whatever the
	// service stores.
	f := changesFixture(t)
	const prefix, bea = "0123456789abcdef", "usr_0000000000000002"
	f.accepted("an issued key", f.exec(`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at,
		workspace_id, created_by, issuer) VALUES (?, ?, '', 'write', 900, 99999999, 'ext_a', ?, ?)`,
		prefix, issuedHash, bea, testIssuer))
	f.accepted("its record", f.exec(`INSERT INTO issued_keys(prefix, issuer, external_id, created_at_us, terms_ok)
		VALUES (?, ?, 'id-1', 900000000, 1)`, prefix, testIssuer))
	for _, c := range []struct{ what, query string }{
		{"read given", `INSERT INTO key_access(key_prefix, account_id, workspace_id, read, granted_by, created_at,
			updated_at) VALUES ('` + prefix + `', 'acc_a1', 'ext_a', 1, '` + bea + `', 900, 900)`},
		{"act given", `UPDATE key_access SET act = 1 WHERE key_prefix = '` + prefix + `'`},
		{"an operator's hold", `INSERT INTO issued_key_stops(prefix, cause, at) VALUES ('` + prefix + `', 'operator', 900)`},
		{"the hold lifted", `DELETE FROM issued_key_stops WHERE prefix = '` + prefix + `'`},
		{"an earlier expiry", `UPDATE api_keys SET expires_at = 5000 WHERE prefix = '` + prefix + `'`},
		{"its end", `UPDATE issued_keys SET ended_at = 5000, end_kind = 'expired' WHERE prefix = '` + prefix + `'`},
		{"its revocation", `UPDATE api_keys SET revoked_at = 950, stop_reason = 'manual' WHERE prefix = '` + prefix + `'`},
		{"the mailbox taken", `DELETE FROM key_access WHERE key_prefix = '` + prefix + `'`},
	} {
		a, b := f.mark("ext_a"), f.mark("ext_b")
		f.accepted(c.what, f.exec(c.query))
		if f.mark("ext_a") <= a || f.mark("ext_b") != b {
			t.Errorf("%s marked ext_a %d (was %d) and ext_b %d (was %d)", c.what, f.mark("ext_a"), a, f.mark("ext_b"), b)
		}
	}
	// A local workspace's keys mark nothing.
	n := queryInt(t, f.s, `SELECT count(*) FROM workspace_changes`)
	f.accepted("an own key's flag in a local workspace", f.exec(`UPDATE key_access SET act = 1 - act
		WHERE key_prefix = '11111111'`))
	if got := queryInt(t, f.s, `SELECT count(*) FROM workspace_changes`); got != n {
		t.Errorf("a local workspace's key marked it")
	}
}

func TestAnIssuersKeyFeedIsMadeOnceForADirectoryAlreadyFollowed(t *testing.T) {
	// A schema-27 database following a directory reset twice: its key feed
	// is made at that reset, once, and nothing is made for a directory not
	// followed.
	f := newSchemaAt(t, 27)
	f.accepted("a directory reset twice", f.exec(`INSERT INTO directories(issuer, generation, reset, changed_at)
		VALUES (?, 'g3', 2, 900)`, testIssuer))
	migrateTo(t, f.s, 28)
	for range 2 {
		var got DirectoryKeys
		if err := f.write(func(ctx context.Context, tx *sql.Tx) error {
			var err error
			got, err = EnsureDirectoryKeysTx(ctx, tx, testIssuer)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if got != (DirectoryKeys{Issuer: testIssuer, Reset: 2}) {
			t.Errorf("the key feed = %+v", got)
		}
	}
	f.accepted("the feed moving", f.exec(`UPDATE directory_keys SET cursor = 7 WHERE issuer = ?`, testIssuer))
	if err := f.write(func(ctx context.Context, tx *sql.Tx) error {
		got, err := EnsureDirectoryKeysTx(ctx, tx, testIssuer)
		if err == nil && got.Cursor != 7 {
			t.Errorf("made again over a feed that moved: %+v", got)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err := f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := EnsureDirectoryKeysTx(ctx, tx, "https://other.example.org")
		return err
	})
	if !errors.Is(err, ErrNoDirectory) {
		t.Errorf("a directory not followed: %v", err)
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM directory_keys`); n != 1 {
		t.Errorf("%d key feeds", n)
	}
}

func TestEveryRefusalOfTheSchemaIsAConstraint(t *testing.T) {
	f := issuedSchema(t)
	f.issue("0123456789abcdef", 99999999)
	for what, err := range map[string]error{
		"a trigger's":   f.exec(`UPDATE api_keys SET issuer = '' WHERE prefix = '0123456789abcdef'`),
		"a CHECK":       f.exec(`UPDATE issued_keys SET listed = 2 WHERE prefix = '0123456789abcdef'`),
		"a UNIQUE":      f.exec(`INSERT INTO issued_key_stops(prefix, cause, at) VALUES ('0123456789abcdef', 'operator', 1), ('0123456789abcdef', 'operator', 2)`),
		"a FOREIGN KEY": f.exec(`INSERT INTO issued_key_stops(prefix, cause, at) VALUES ('ffffffffffffffff', 'operator', 1)`),
		"a NOT NULL":    f.exec(`INSERT INTO issued_key_ends(prefix, how, ended_at, forget_at) VALUES (NULL, 'held', 1, 1)`),
	} {
		if !IsConstraint(err) {
			t.Errorf("%s refusal is not a constraint: %v", what, err)
		}
	}
	if IsConstraint(f.exec(`SELECT * FROM no_such_table`)) || IsConstraint(nil) {
		t.Error("another failure is a constraint")
	}
}
