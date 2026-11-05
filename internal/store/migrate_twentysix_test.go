package store

import (
	"slices"
	"testing"
)

// Migration 0026: a link at the provider, kept until its return
// (internal/account, links.go).

func TestMigrationTwentySixAddsTheLinksAndChangesNothingElse(t *testing.T) {
	f := newSchemaAt(t, 25)
	seedJournalAt24(t, f)
	before, schema, fts := dumpTables(t, f.s), schemaSQL(t, f.s), ftsAnswers(t, f.s)
	migrateTo(t, f.s, 26)

	// Nothing is under way when the table appears.
	compareMigrated(t, before, dumpTables(t, f.s), nil, []string{"oauth_links"})
	added, changed, dropped := schemaChanges(schema, schemaSQL(t, f.s))
	if want := []string{
		"index oauth_links_expiry", "index oauth_links_owner", "index sqlite_autoindex_oauth_links_1",
		"index sqlite_autoindex_oauth_links_2", "table oauth_links",
	}; !slices.Equal(added, want) {
		t.Errorf("0026 added\n%v\nwant\n%v", added, want)
	}
	if len(changed) != 0 || len(dropped) != 0 {
		t.Errorf("0026 changed %v and dropped %v", changed, dropped)
	}
	if got := ftsAnswers(t, f.s); !mapsEqual(got, fts) {
		t.Errorf("the full-text index answers %v, and answered %v", got, fts)
	}
	if got := queryStrings(t, f.s, `PRAGMA foreign_key_check`); len(got) != 0 {
		t.Errorf("foreign_key_check: %v", got)
	}
}

// link inserts a link of Owner (usr_1) into the team wsp_t1, waiting for its
// return, and then sets what set says of it, if anything.
func (f seededSchema) link(id, set string) error {
	if err := f.exec(`INSERT INTO oauth_links(id, state, owner_user_id, workspace_id, provider, oauth_client,
		pkce_verifier, redirect_uri, created_at, expires_at) VALUES (?, 'state-' || ?, 'usr_0000000000000001',
		'wsp_t1', 'gmail', 'web', 'verifier', 'https://mail.example.test/oauth/return', 900, 1500)`, id, id); err != nil {
		return err
	}
	if set == "" {
		return nil
	}
	return f.exec(`UPDATE oauth_links SET `+set+` WHERE id = ?`, id)
}

func TestTheSchemaKeepsALinksVerifierAndAddressWhereTheyBelong(t *testing.T) {
	// The seed at the latest schema: Owner (usr_1) owns the team wsp_t1,
	// whose mailbox acc_t1 is.
	f := newSchemaLatest(t)
	f.accepted("a link waiting", f.link("lnk_waiting", ""))
	f.accepted("a link into the personal workspace", f.link("lnk_own", "workspace_id = NULL, provider = 'microsoft'"))
	f.accepted("a link failed with the address it named", f.link("lnk_failed", `pkce_verifier = '', taken_at = 950,
		finished_at = 960, outcome = 'failed', email = 'ana@gmail.com', reason = 'a reason'`))
	f.accepted("a link failed before it was taken", f.link("lnk_expired", `pkce_verifier = '', finished_at = 960,
		outcome = 'failed', reason = 'a reason'`))
	f.accepted("a link connected", f.link("lnk_connected", `pkce_verifier = '', taken_at = 950, finished_at = 960,
		outcome = 'connected', account_id = 'acc_t1'`))

	const finished = "pkce_verifier = '', taken_at = 950, finished_at = 960, "
	for _, c := range []struct{ what, set string }{
		{"a link of another provider", "provider = 'imap'"},
		{"a link under the installed client", "oauth_client = 'installed'"},
		{"an id of another kind", "id = 'acc_try'"},
		{"a waiting link with an address", "email = 'ana@gmail.com'"},
		{"a waiting link with a mailbox", "account_id = 'acc_t1'"},
		{"an outcome with no moment", "pkce_verifier = '', outcome = 'failed', reason = 'a reason'"},
		{"a moment with no outcome", "pkce_verifier = '', finished_at = 960"},
		{"a link connected untaken", "pkce_verifier = '', finished_at = 960, outcome = 'connected'"},
		{"a link connected with an address", finished + "outcome = 'connected', email = 'ana@gmail.com'"},
		{"a link failed without a reason", finished + "outcome = 'failed'"},
		{"a link failed with a mailbox", finished + "outcome = 'failed', reason = 'a reason', account_id = 'acc_t1'"},
		{"a finished link keeping its verifier", "taken_at = 950, finished_at = 960, outcome = 'failed', reason = 'a reason'"},
		{"a finished link keeping its name", finished + "outcome = 'failed', reason = 'a reason', display_name = 'Work'"},
		{"a finished link keeping its sync text", finished + "outcome = 'failed', reason = 'a reason', " +
			"sync_consent_version = 'sync-1'"},
	} {
		f.refused(c.what, "CHECK constraint failed", f.link("lnk_try", c.set))
		f.accepted("the link tried", f.exec(`DELETE FROM oauth_links WHERE id = 'lnk_try'`))
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM oauth_links`); n != 5 {
		t.Errorf("the table holds %d links, want the 5 accepted", n)
	}

	// A mailbox deleted for good leaves its link naming none; a person's
	// links go with them, and a team's with it.
	f.accepted("the mailbox deleted for good", f.exec(`DELETE FROM accounts WHERE id = 'acc_t1'`))
	if got := queryStrings(t, f.s, `SELECT coalesce(account_id, '-') || ' ' || outcome FROM oauth_links
		WHERE id = 'lnk_connected'`); !slices.Equal(got, []string{"- connected"}) {
		t.Errorf("the connected link once its mailbox is gone: %v", got)
	}
	if got := queryStrings(t, f.s, `SELECT "from" || ' ' || "table" || ' ' || on_delete
		FROM pragma_foreign_key_list('oauth_links') ORDER BY "from"`); !slices.Equal(got, []string{
		"account_id accounts SET NULL", "owner_user_id users CASCADE", "workspace_id workspaces CASCADE",
	}) {
		t.Errorf("the links refer to %v", got)
	}
}
