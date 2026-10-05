package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The rebuild procedure and migration 0008, which is its first use, and 0009,
// which sets right what 0008 could not change. These live inside the package
// because they stop between migrations and hand the runner migrations of
// their own.

// openAtVersion opens a database and applies the migrations up to and
// including version.
func openAtVersion(t *testing.T, version int) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "mail.db"), Options{SkipMigrate: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.version > version {
			break
		}
		if err := s.applyMigration(ctx, m); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}
	if v, err := s.SchemaVersion(ctx); err != nil || v != version {
		t.Fatalf("user_version = %d (%v), want %d", v, err, version)
	}
	return s
}

// migrateTo applies the pending migrations up to and including version.
func migrateTo(t *testing.T, s *Store, version int) {
	t.Helper()
	ctx := context.Background()
	current, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.version <= current || m.version > version {
			continue
		}
		if err := s.applyMigration(ctx, m); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}
	if v, err := s.SchemaVersion(ctx); err != nil || v != version {
		t.Fatalf("user_version = %d (%v), want %d", v, err, version)
	}
}

// latestVersion is the last migration this binary embeds.
func latestVersion(t *testing.T) int {
	t.Helper()
	migrations, err := loadMigrations()
	if err != nil || len(migrations) == 0 {
		t.Fatalf("no migrations: %v", err)
	}
	return migrations[len(migrations)-1].version
}

func execAll(t *testing.T, s *Store, stmts ...string) {
	t.Helper()
	ctx := context.Background()
	err := s.Write(ctx, func(tx *sql.Tx) error {
		for _, stmt := range stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// accountRow is an INSERT of a schema-7 account.
func accountRow(id, email, provider, owner, client, state string, created int) string {
	ownerSQL := "NULL"
	if owner != "" {
		ownerSQL = "'" + owner + "'"
	}
	authKind, imap, smtp, tls, port := "oauth2", "imap.gmail.com", "smtp.gmail.com", "implicit", 465
	switch provider {
	case "imap":
		authKind, imap, smtp, tls, port = "password", "imap.example.org", "smtp.example.org", "starttls", 587
	case "microsoft":
		imap, smtp, tls, port = "outlook.office365.com", "smtp.office365.com", "starttls", 587
	}
	return fmt.Sprintf(`INSERT INTO accounts(id, email, display_name, provider, auth_kind, imap_host, imap_port,
	  smtp_host, smtp_port, smtp_tls, login_user, oauth_tenant, sync_tier, sync_tier_resolved, save_sent_copy,
	  initial_days, folder_overrides, state, state_reason, state_changed_at, last_ok_at, last_error,
	  consecutive_failures, next_retry_at, last_idle_event_at, created_at, updated_at, owner_user_id, oauth_client,
	  sync_enabled_at, sync_enabled_by)
	  VALUES ('%[1]s', '%[2]s', 'Name of %[1]s', '%[3]s', '%[4]s', '%[5]s', 993, '%[6]s', %[7]d, '%[8]s', '%[2]s',
	  'common', 'auto', 'condstore', %[9]d, 90, '{"sent":"Sent Items"}', '%[10]s', 'a reason', %[11]d, %[11]d, 'an error',
	  2, %[11]d, %[11]d, %[11]d, %[12]d, %[13]s, '%[14]s', %[15]d, '%[16]s')`,
		id, email, provider, authKind, imap, smtp, port, tls, map[bool]int{true: 1}[provider == "imap"],
		state, created+5, created+9, ownerSQL, client,
		map[bool]int{true: created + 7}[owner == ""], map[bool]string{true: "cli"}[owner == ""])
}

// seedSmallest is the smallest real shape of a schema-7 database: one owner,
// consented, with the two mailboxes they connected, their index, events and
// sends, the invite they arrived with, and no keys.
func seedSmallest(t *testing.T, s *Store) {
	t.Helper()
	execAll(t, s,
		`INSERT INTO meta(key, value) VALUES ('instance_id', 'abc'), ('created_at', '1')`,
		`INSERT INTO users(id, email, name, password_hash, role, status, password_changed_at, created_at, updated_at,
		   sync_consent_at, sync_consent_version, actions_consent_at, actions_consent_version, send_consent_at, send_consent_version)
		 VALUES ('usr_0000000000000001', 'owner@example.org', 'Owner', '$argon2id$x', 'owner', 'active', 100, 100, 101,
		   102, 'sync-1', 103, 'actions-1', 104, 'send-1')`,
		`INSERT INTO sessions(id, user_id, token_hash, user_agent, created_at, last_seen_at, expires_at)
		 VALUES ('ses_0000000000000001', 'usr_0000000000000001', zeroblob(32), 'Firefox', 100, 200, 9999999999)`,
		`INSERT INTO invites(code_hash, email, role, created_by, created_at, expires_at, used_at, used_by)
		 VALUES (randomblob(32), 'owner@example.org', 'member', 'cli', 90, 9999999999, 100, 'usr_0000000000000001')`,
		accountRow("acc_0000000000000001", "owner@gmail.com", "gmail", "usr_0000000000000001", "web", "active", 200),
		accountRow("acc_0000000000000002", "owner@example.org", "imap", "usr_0000000000000001", "installed", "active", 300),
		`INSERT INTO credentials(account_id, field, keyid, ciphertext, updated_at)
		 VALUES ('acc_0000000000000001', 'oauth_token', 1, x'01020304', 201),
		        ('acc_0000000000000002', 'password', 1, x'05060708', 301)`,
		`INSERT INTO folders(id, account_id, name, display_name, role, role_source, uidvalidity, max_seen_uid)
		 VALUES (1, 'acc_0000000000000001', 'INBOX', 'Inbox', 'inbox', 'inbox', 7, 42),
		        (2, 'acc_0000000000000002', 'INBOX', 'Inbox', 'inbox', 'inbox', 9, 3)`,
		`INSERT INTO messages(account_id, folder_id, uidvalidity, uid, message_id, group_key, subject, from_text,
		   to_text, internal_date, size, first_seen_at, updated_at, body_text)
		 VALUES ('acc_0000000000000001', 1, 7, 41, 'a@x', 'mid:a@x', 'Quarterly report', 'Ana', 'Owner', 500, 1000, 500, 500, 'figures attached'),
		        ('acc_0000000000000001', 1, 7, 42, 'b@x', 'mid:b@x', 'Invoice 17', 'Bob', 'Owner', 600, 2000, 600, 600, 'please pay'),
		        ('acc_0000000000000002', 2, 9, 3, 'c@x', 'mid:c@x', 'Dinner', 'Cid', 'Owner', 700, 300, 700, 700, 'friday report')`,
		`INSERT INTO parts(msg_id, path, mime_type, size, is_body, is_attachment, sha256)
		 VALUES (1, '1', 'text/plain', 10, 1, 0, NULL), (1, '2', 'application/pdf', 900, 0, 1, 'aa11')`,
		`INSERT INTO bodies(msg_id, headers_raw, text_html, size, fetched_at, last_access_at)
		 VALUES (1, x'48656164', '<p>figures</p>', 50, 510, 520)`,
		`INSERT INTO attachment_blobs(sha256, size, last_access_at) VALUES ('aa11', 900, 530)`,
		`INSERT INTO events(type, account_id, payload_json, created_at)
		 VALUES ('account.state', 'acc_0000000000000001', '{"state":"active"}', 210),
		        ('message.new', 'acc_0000000000000001', '{"subject":"Invoice 17"}', 610)`,
		`INSERT INTO sends(account_id, idempotency_key, compose_hash, message_id_hdr, state, attempts, created_by,
		   created_at, updated_at, sent_at, recipients, user_id)
		 VALUES ('acc_0000000000000001', 'k1', 'h1', 'm1@gmail.com', 'sent', 1, 'usr_0000000000000001',
		   800, 801, 801, 2, 'usr_0000000000000001')`,
	)
}

// seedSelfHosted is a larger self-hosted shape, with a row in every table:
// several people, one of them disabled; mailboxes they own and mailboxes the
// command line made; instance keys unrestricted, restricted to an unowned
// mailbox and to an owned one; person keys restricted and not, two of them to
// mailboxes nobody owned, which an owner could choose then; invites used
// and unused; sessions; consent attempts on owned and unowned mailboxes;
// sends with and without a person; events; webhooks and their deliveries;
// drafts.
func seedSelfHosted(t *testing.T, s *Store) {
	t.Helper()
	seedSmallest(t, s)
	execAll(t, s,
		`INSERT INTO users(id, email, name, password_hash, role, status, password_changed_at, created_at, updated_at,
		   sync_consent_at, sync_consent_version)
		 VALUES ('usr_0000000000000002', 'bea@example.org', 'Bea', '$argon2id$y', 'member', 'active', 110, 110, 111, 112, 'sync-1'),
		        ('usr_0000000000000003', 'cid@example.org', 'Cid', '$argon2id$z', 'member', 'disabled', 120, 120, 121, 0, ''),
		        ('usr_0000000000000004', 'dan@example.org', 'Dan', '$argon2id$w', 'owner', 'active', 130, 130, 131, 0, '')`,
		`INSERT INTO sessions(id, user_id, token_hash, user_agent, created_at, last_seen_at, expires_at, revoked_at)
		 VALUES ('ses_0000000000000002', 'usr_0000000000000002', randomblob(32), 'Safari', 110, 210, 9999999999, 0),
		        ('ses_0000000000000003', 'usr_0000000000000003', randomblob(32), '', 120, 220, 9999999999, 230)`,
		`INSERT INTO invites(code_hash, email, role, created_by, created_at, expires_at, used_at, used_by)
		 VALUES (randomblob(32), 'bea@example.org', 'member', 'usr_0000000000000001', 105, 9999999999, 110, 'usr_0000000000000002'),
		        (randomblob(32), 'eve@example.org', 'member', 'usr_0000000000000001', 106, 9999999999, 0, ''),
		        (randomblob(32), 'old@example.org', 'owner', 'key:aaaaaaaa', 10, 20, 0, '')`,
		accountRow("acc_0000000000000003", "bea@outlook.com", "microsoft", "usr_0000000000000002", "web", "active", 400),
		accountRow("acc_0000000000000004", "cid@gmail.com", "gmail", "usr_0000000000000003", "web", "needs_reauth", 410),
		accountRow("acc_0000000000000005", "ops@example.org", "imap", "", "installed", "active", 420),
		accountRow("acc_0000000000000006", "alerts@gmail.com", "gmail", "", "installed", "pending_auth", 430),
		`INSERT INTO credentials(account_id, field, keyid, ciphertext, updated_at)
		 VALUES ('acc_0000000000000003', 'oauth_token', 2, x'0a0b', 401),
		        ('acc_0000000000000005', 'password', 1, x'0c0d', 421)`,
		`INSERT INTO oauth_pending(state, account_id, owner_user_id, flow, pkce_verifier, redirect_uri, device_code, expires_at)
		 VALUES ('st-owned', 'acc_0000000000000003', 'usr_0000000000000002', 'web', 'v1', 'https://console.example/oauth/return', '', 9999999999),
		        ('st-cli', 'acc_0000000000000006', NULL, 'device', '', 'urn:ietf:wg:oauth:2.0:oob', 'dc', 9999999999),
		        ('st-owner-on-unowned', 'acc_0000000000000006', 'usr_0000000000000001', 'loopback', 'v2', 'http://127.0.0.1:1/cb', '', 9999999999)`,
		`INSERT INTO folders(id, account_id, name, display_name, role, role_source, uidvalidity)
		 VALUES (3, 'acc_0000000000000003', 'INBOX', 'Inbox', 'inbox', 'inbox', 11),
		        (4, 'acc_0000000000000005', 'INBOX', 'Inbox', 'inbox', 'inbox', 13),
		        (5, 'acc_0000000000000005', 'Sent', 'Sent', 'sent', 'special-use', 13)`,
		`INSERT INTO messages(account_id, folder_id, uidvalidity, uid, message_id, group_key, subject, from_text,
		   to_text, internal_date, size, first_seen_at, updated_at, body_text, seen)
		 VALUES ('acc_0000000000000003', 3, 11, 1, 'd@x', 'mid:d@x', 'Report for Bea', 'Ana', 'Bea', 900, 10, 900, 900, 'the annual report', 1),
		        ('acc_0000000000000005', 4, 13, 1, 'e@x', 'mid:e@x', 'Disk alert', 'monitor', 'ops', 910, 20, 910, 910, 'disk full', 0),
		        ('acc_0000000000000005', 5, 13, 2, 'f@x', 'mid:f@x', 'Re: Disk alert', 'ops', 'monitor', 920, 30, 920, 920, 'fixed', 1)`,
		`INSERT INTO drafts(id, account_id, compose_json, compose_hash, message_id_hdr, state, created_by, created_at, updated_at, expires_at)
		 VALUES ('drf_0000000000000001', 'acc_0000000000000005', '{"to":["x@y"]}', 'dh', 'd1@example.org', 'open', 'key:aaaaaaaa', 930, 930, 9999999999)`,
		`INSERT INTO sends(account_id, idempotency_key, compose_hash, message_id_hdr, state, created_by, created_at, updated_at, recipients, user_id)
		 VALUES ('acc_0000000000000005', 'draft:drf_0000000000000001', 'dh', 'd1@example.org', 'unknown', 'key:aaaaaaaa', 940, 941, 1, ''),
		        ('acc_0000000000000003', 'k2', 'h2', 'm2@outlook.com', 'failed', 'usr_0000000000000002', 950, 951, 3, 'usr_0000000000000002')`,
		`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at, revoked_at, last_used_at, user_id,
		   terms_version, created_by, restricted)
		 VALUES ('aaaaaaaa', '$argon2id$a', 'cli', 'admin', 50, 9999999999, 0, 60, NULL, '', 'cli', 0),
		        ('bbbbbbbb', '$argon2id$b', 'ops only', 'read', 51, 9999999999, 0, 0, NULL, '', 'key:aaaaaaaa', 1),
		        ('cccccccc', '$argon2id$c', 'for a person''s mailbox', 'read', 52, 9999999999, 0, 0, NULL, '', 'key:aaaaaaaa', 1),
		        ('dddddddd', '$argon2id$d', 'Bea''s assistant', 'write', 53, 9999999999, 0, 61, 'usr_0000000000000002', 'terms-1', 'usr_0000000000000002', 0),
		        ('eeeeeeee', '$argon2id$e', 'Owner''s gmail only', 'read', 54, 9999999999, 0, 0, 'usr_0000000000000001', 'terms-1', 'usr_0000000000000001', 1),
		        ('ffffffff', '$argon2id$f', 'revoked', 'write', 55, 9999999999, 70, 0, 'usr_0000000000000001', 'terms-1', 'usr_0000000000000001', 0),
		        ('gggggggg', '$argon2id$g', 'Owner''s ops box', 'read', 56, 9999999999, 0, 0, 'usr_0000000000000001', 'terms-1', 'usr_0000000000000001', 1),
		        ('hhhhhhhh', '$argon2id$h', 'Owner''s gmail and alerts', 'read', 57, 9999999999, 0, 0, 'usr_0000000000000001', 'terms-1', 'usr_0000000000000001', 1)`,
		// An owner saw the mailboxes nobody owned under schema 7, and
		// could restrict a key of theirs to one: gggggggg to one alone,
		// hhhhhhhh to one beside a mailbox of their own.
		`INSERT INTO api_key_accounts(key_prefix, account_id)
		 VALUES ('bbbbbbbb', 'acc_0000000000000005'), ('cccccccc', 'acc_0000000000000001'),
		        ('eeeeeeee', 'acc_0000000000000001'), ('gggggggg', 'acc_0000000000000005'),
		        ('hhhhhhhh', 'acc_0000000000000001'), ('hhhhhhhh', 'acc_0000000000000006')`,
		`INSERT INTO events(type, account_id, payload_json, created_at)
		 VALUES ('message.new', 'acc_0000000000000005', '{"subject":"Disk alert"}', 911),
		        ('account.state', 'acc_0000000000000003', '{"state":"active"}', 402)`,
		`INSERT INTO webhooks(id, url, secret_ciphertext, keyid, events_json, accounts_json, created_at, updated_at)
		 VALUES ('whk_0000000000000001', 'https://hooks.example/a', x'00', 1, '["message.new"]', '["acc_0000000000000005"]', 60, 60),
		        ('whk_0000000000000002', 'https://hooks.example/b', x'01', 1, '["message.new"]', '[]', 61, 61)`,
		`INSERT INTO webhook_deliveries(webhook_id, event_seq, attempt, next_attempt_at, status, created_at, updated_at)
		 VALUES ('whk_0000000000000001', 3, 1, 999, 'pending', 912, 912),
		        ('whk_0000000000000002', 2, 2, 999, 'delivered', 611, 612)`,
	)
}

// tableDump is every row of every table, as text that keeps each value's
// type, keyed by table and then by column.
type tableDump map[string][]map[string]string

// dumpTables reads every table but the virtual ones, ordered by rowid, or by
// every column for a table without one.
func dumpTables(t *testing.T, s *Store) tableDump {
	t.Helper()
	ctx := context.Background()
	out := tableDump{}
	for _, tb := range queryStrings(t, s, `SELECT name || ' ' || (sql LIKE '%WITHOUT ROWID%') FROM sqlite_schema
		WHERE type = 'table' AND sql NOT LIKE 'CREATE VIRTUAL TABLE%' ORDER BY name`) {
		name, withoutRowid, _ := strings.Cut(tb, " ")
		order := "rowid"
		if withoutRowid == "1" {
			var cols int
			if err := s.Writer().QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info(?)`, name).Scan(&cols); err != nil {
				t.Fatal(err)
			}
			var by []string
			for i := 1; i <= cols; i++ {
				by = append(by, fmt.Sprint(i))
			}
			order = strings.Join(by, ", ")
		}
		out[name] = dumpTable(t, s, name, order)
	}
	return out
}

func dumpTable(t *testing.T, s *Store, name, order string) []map[string]string {
	t.Helper()
	r, err := s.Writer().QueryContext(context.Background(), `SELECT * FROM "`+name+`" ORDER BY `+order)
	if err != nil {
		t.Fatalf("dump %s: %v", name, err)
	}
	defer func() { _ = r.Close() }()
	cols, err := r.Columns()
	if err != nil {
		t.Fatal(err)
	}
	out := []map[string]string{}
	for r.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := r.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		row := map[string]string{}
		for i, c := range cols {
			row[c] = fmt.Sprintf("%T:%v", vals[i], vals[i])
		}
		out = append(out, row)
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// schemaSQL is every object of the schema but the named tables and what
// belongs to them, as sqlite_schema holds it.
func schemaSQL(t *testing.T, s *Store) map[string]string {
	t.Helper()
	rows, err := s.Writer().QueryContext(context.Background(),
		`SELECT type || ' ' || name, coalesce(sql, '') FROM sqlite_schema ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var name, text string
		if err := rows.Scan(&name, &text); err != nil {
			t.Fatal(err)
		}
		out[name] = text
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func ftsAnswers(t *testing.T, s *Store) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, q := range []string{"report", "invoice", "disk", "figures", "pay"} {
		out[q] = queryStrings(t, s, `SELECT rowid FROM messages_fts WHERE messages_fts MATCH ? ORDER BY rowid`, q)
	}
	return out
}

func queryStrings(t *testing.T, s *Store, query string, args ...any) []string {
	t.Helper()
	rows, err := s.Writer().QueryContext(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			parts[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(parts, " "))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func queryInt(t *testing.T, s *Store, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := s.Writer().QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// newColumns are what 0008 adds to the tables that were there.
var newColumns = map[string][]string{
	"accounts": {"workspace_id"},
	"invites":  {"id", "workspace_id", "workspace_role"},
}

// compareDumps checks that every table of before is in after with the same
// rows, value for value, and only the new columns besides.
func compareDumps(t *testing.T, before, after tableDump) {
	t.Helper()
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
			var extra []string
			for col := range got[i] {
				if _, ok := row[col]; !ok {
					extra = append(extra, col)
				}
			}
			sort.Strings(extra)
			want := slices.Clone(newColumns[table])
			sort.Strings(want)
			if !slices.Equal(extra, want) {
				t.Errorf("%s gained columns %v, want %v", table, extra, want)
			}
		}
	}
}

// migrateEight applies 0008 to a seeded schema-7 database and checks every
// guarantee that does not depend on the shape.
func migrateEight(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	before := dumpTables(t, s)
	schemaBefore := schemaSQL(t, s)
	ftsBefore := ftsAnswers(t, s)
	if len(ftsBefore["report"]) == 0 || len(ftsBefore["invoice"]) == 0 {
		t.Fatalf("the seed's full-text answers are empty, so comparing them proves nothing: %v", ftsBefore)
	}
	columnsBefore := queryStrings(t, s, `SELECT name, type, "notnull", coalesce(dflt_value, 'NULL'), pk
		FROM pragma_table_info('accounts') ORDER BY cid`)
	fksBefore := queryStrings(t, s, `SELECT "table", "from", coalesce("to", ''), on_delete
		FROM pragma_foreign_key_list('accounts') ORDER BY "from"`)

	migrateTo(t, s, 8)

	compareDumps(t, before, dumpTables(t, s))

	// Every object but accounts' own is as it was; the new ones are new.
	schemaAfter := schemaSQL(t, s)
	for name, text := range schemaBefore {
		if name == "table accounts" || name == "table invites" ||
			strings.HasPrefix(name, "index sqlite_autoindex_accounts") {
			continue
		}
		if schemaAfter[name] != text {
			t.Errorf("%s changed:\n%s\nis now:\n%s", name, text, schemaAfter[name])
		}
	}
	if !strings.Contains(schemaAfter["index accounts_owner"], "owner_user_id") {
		t.Errorf("accounts_owner was not recreated: %q", schemaAfter["index accounts_owner"])
	}

	// accounts keeps every column as it was, with workspace_id after id.
	columnsAfter := queryStrings(t, s, `SELECT name, type, "notnull", coalesce(dflt_value, 'NULL'), pk
		FROM pragma_table_info('accounts') WHERE name <> 'workspace_id' ORDER BY cid`)
	if !slices.Equal(columnsAfter, columnsBefore) {
		t.Errorf("accounts' columns were:\n%s\nare now:\n%s", strings.Join(columnsBefore, "\n"), strings.Join(columnsAfter, "\n"))
	}
	if got := queryStrings(t, s, `SELECT name, type, "notnull" FROM pragma_table_info('accounts') WHERE cid = 1`); !slices.Equal(got, []string{"workspace_id TEXT 1"}) {
		t.Errorf("accounts' second column is %v", got)
	}
	fksAfter := queryStrings(t, s, `SELECT "table", "from", coalesce("to", ''), on_delete
		FROM pragma_foreign_key_list('accounts') WHERE "from" <> 'workspace_id' ORDER BY "from"`)
	if !slices.Equal(fksAfter, fksBefore) {
		t.Errorf("accounts' foreign keys were %v, are %v", fksBefore, fksAfter)
	}
	// The children still name accounts, and nothing names a table that is
	// gone.
	for _, child := range []string{"credentials", "oauth_pending", "folders", "messages", "drafts", "sends", "api_key_accounts"} {
		if got := queryStrings(t, s, `SELECT "table" FROM pragma_foreign_key_list(?) WHERE "from" = 'account_id'`, child); !slices.Equal(got, []string{"accounts"}) {
			t.Errorf("%s.account_id refers to %v", child, got)
		}
	}
	if n := queryInt(t, s, `SELECT count(*) FROM sqlite_schema WHERE name LIKE '%accounts_new%' OR sql LIKE '%accounts_new%'`); n != 0 {
		t.Errorf("the schema still names accounts_new %d times", n)
	}
	// The address is unique per workspace, without regard to case.
	if got := queryStrings(t, s, `SELECT group_concat(x.name || ':' || x.coll, ',')
		FROM pragma_index_list('accounts') l, pragma_index_xinfo(l.name) x
		WHERE l."unique" = 1 AND x.key = 1 GROUP BY l.name ORDER BY 1`); !slices.Equal(got,
		[]string{"id:BINARY", "id:BINARY,workspace_id:BINARY", "workspace_id:BINARY,email:NOCASE"}) {
		t.Errorf("accounts' unique indexes are %v", got)
	}

	// The full-text index answers as it did, and is intact.
	if got := ftsAnswers(t, s); fmt.Sprint(got) != fmt.Sprint(ftsBefore) {
		t.Errorf("full-text answers were %v, are %v", ftsBefore, got)
	}
	if _, err := s.Writer().ExecContext(ctx, `INSERT INTO messages_fts(messages_fts) VALUES ('integrity-check')`); err != nil {
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

	// One operator workspace; one personal workspace per person, as old as
	// the person, and they its only member, an active owner.
	if got := queryStrings(t, s, `SELECT id, kind, source, name, coalesce(person_id, 'NULL') FROM workspaces WHERE kind = 'operator'`); !slices.Equal(got, []string{"wsp_operator operator local  NULL"}) {
		t.Errorf("operator workspace: %v", got)
	}
	if n := queryInt(t, s, `SELECT count(*) FROM users u WHERE NOT EXISTS (
		SELECT 1 FROM workspaces w JOIN workspace_members m ON m.workspace_id = w.id
		 WHERE w.person_id = u.id AND w.kind = 'personal' AND w.source = 'local' AND w.name = ''
		   AND w.created_at = u.created_at AND m.user_id = u.id AND m.role = 'owner' AND m.status = 'active')`); n != 0 {
		t.Errorf("%d people have no personal workspace of their own", n)
	}
	if n := queryInt(t, s, `SELECT count(*) FROM workspaces WHERE kind = 'personal' AND id NOT GLOB 'wsp_[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]'`); n != 0 {
		t.Errorf("%d personal workspaces have an id not shaped wsp_ + 16 hex", n)
	}
	if n, people := queryInt(t, s, `SELECT count(*) FROM workspaces`), queryInt(t, s, `SELECT count(*) FROM users`); n != people+1 {
		t.Errorf("%d workspaces for %d people, want one each and the operator's", n, people)
	}
	if n, people := queryInt(t, s, `SELECT count(*) FROM workspace_members`), queryInt(t, s, `SELECT count(*) FROM users`); n != people {
		t.Errorf("%d memberships for %d people", n, people)
	}

	// Each mailbox is in its owner's personal workspace, with a grant of
	// every flag to them; one nobody owns is the operator's, without grants.
	if n := queryInt(t, s, `SELECT count(*) FROM accounts a WHERE a.workspace_id IS NOT
		coalesce((SELECT id FROM workspaces WHERE person_id = a.owner_user_id), 'wsp_operator')`); n != 0 {
		t.Errorf("%d mailboxes are in the wrong workspace", n)
	}
	if n := queryInt(t, s, `SELECT count(*) FROM accounts a WHERE a.owner_user_id IS NOT NULL AND NOT EXISTS (
		SELECT 1 FROM mailbox_access g WHERE g.account_id = a.id AND g.user_id = a.owner_user_id
		   AND g.workspace_id = a.workspace_id AND g.read AND g.act AND g.send AND g.manage
		   AND g.granted_by = 'migration' AND g.created_at = a.created_at)`); n != 0 {
		t.Errorf("%d owned mailboxes have no full grant to their owner", n)
	}
	if n, owned := queryInt(t, s, `SELECT count(*) FROM mailbox_access`),
		queryInt(t, s, `SELECT count(*) FROM accounts WHERE owner_user_id IS NOT NULL`); n != owned {
		t.Errorf("%d grants for %d owned mailboxes", n, owned)
	}

	// A person's key reaches the mailboxes of theirs it reached: its person
	// holds a full grant on every mailbox they own in its restriction. One
	// nobody owned is the operator's now, which 0009 takes out of the key.
	if n := queryInt(t, s, `SELECT count(*) FROM api_key_accounts r JOIN api_keys k ON k.prefix = r.key_prefix
		JOIN accounts a ON a.id = r.account_id
		WHERE k.user_id IS NOT NULL AND a.owner_user_id = k.user_id AND NOT EXISTS (SELECT 1 FROM mailbox_access g
		  WHERE g.account_id = r.account_id AND g.user_id = k.user_id AND g.read AND g.act AND g.send AND g.manage)`); n != 0 {
		t.Errorf("%d restrictions of person keys name a mailbox of theirs they cannot reach", n)
	}

	// Invites: an id each, no workspace, no workspace role.
	if n := queryInt(t, s, `SELECT count(*) FROM invites WHERE id NOT GLOB 'inv_[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]'
		OR workspace_id IS NOT NULL OR workspace_role <> ''`); n != 0 {
		t.Errorf("%d invites came out of the migration misshapen", n)
	}

	// The rebuilt table's foreign keys hold: removing a mailbox still takes
	// its credentials, folders, messages and the rest with it.
	var gone string
	if err := s.Writer().QueryRowContext(ctx, `SELECT id FROM accounts WHERE owner_user_id IS NOT NULL ORDER BY rowid LIMIT 1`).Scan(&gone); err != nil {
		t.Fatal(err)
	}
	execAll(t, s, `DELETE FROM accounts WHERE id = '`+gone+`'`)
	for _, child := range []string{"credentials", "oauth_pending", "folders", "messages", "drafts", "sends", "api_key_accounts", "mailbox_access"} {
		if n := queryInt(t, s, `SELECT count(*) FROM "`+child+`" WHERE account_id = ?`, gone); n != 0 {
			t.Errorf("removing %s left %d rows in %s", gone, n, child)
		}
	}
}

func TestMigrationEightKeepsEveryRowAndGivesEachPersonTheirMailboxes(t *testing.T) {
	t.Run("the smallest shape", func(t *testing.T) {
		s := openAtVersion(t, 7)
		seedSmallest(t, s)
		migrateEight(t, s)
		// One owner with the two mailboxes they connected: one personal
		// workspace, both mailboxes in it, a full grant on each.
		if got := queryStrings(t, s, `SELECT a.id, w.kind, g.user_id FROM accounts a
			JOIN workspaces w ON w.id = a.workspace_id JOIN mailbox_access g ON g.account_id = a.id
			ORDER BY a.rowid`); !slices.Equal(got, []string{
			"acc_0000000000000002 personal usr_0000000000000001", // the first was removed above
		}) {
			t.Errorf("mailboxes after the migration: %v", got)
		}
		if n := queryInt(t, s, `SELECT count(*) FROM workspaces WHERE kind = 'personal'`); n != 1 {
			t.Errorf("%d personal workspaces, want 1", n)
		}
	})

	t.Run("a self-hosted shape", func(t *testing.T) {
		s := openAtVersion(t, 7)
		seedSelfHosted(t, s)
		// Rows in every table, so that every one is compared.
		for table, rows := range dumpTables(t, s) {
			if len(rows) == 0 {
				t.Fatalf("the seed left %s empty", table)
			}
		}
		migrateEight(t, s)
		if got := queryStrings(t, s, `SELECT a.id, w.kind, coalesce(a.owner_user_id, '-'),
			  coalesce((SELECT group_concat(g.user_id) FROM mailbox_access g WHERE g.account_id = a.id), '-')
			  FROM accounts a JOIN workspaces w ON w.id = a.workspace_id ORDER BY a.rowid`); !slices.Equal(got, []string{
			"acc_0000000000000002 personal usr_0000000000000001 usr_0000000000000001",
			"acc_0000000000000003 personal usr_0000000000000002 usr_0000000000000002",
			"acc_0000000000000004 personal usr_0000000000000003 usr_0000000000000003",
			"acc_0000000000000005 operator - -",
			"acc_0000000000000006 operator - -",
		}) {
			t.Errorf("mailboxes after the migration:\n%s", strings.Join(got, "\n"))
		}
		// The disabled person keeps their workspace and grant: being
		// disabled on the instance is what keeps them out.
		if n := queryInt(t, s, `SELECT count(*) FROM workspaces WHERE person_id = 'usr_0000000000000003'`); n != 1 {
			t.Errorf("the disabled person has %d personal workspaces", n)
		}
		// No key changed (compared above). Removing the mailbox at the end
		// revoked the keys made for it alone, as 0005's trigger does: the
		// key restrictions still follow the rebuilt table.
		if got := queryStrings(t, s, `SELECT prefix FROM api_keys WHERE revoked_at <> 0 ORDER BY prefix`); !slices.Equal(got,
			[]string{"cccccccc", "eeeeeeee", "ffffffff"}) {
			t.Errorf("revoked keys after removing the first mailbox: %v", got)
		}
	})
}

func TestMigrationEightRefusesWhatTheNewSchemaForbids(t *testing.T) {
	s := openAtVersion(t, 7)
	seedSelfHosted(t, s)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	personal := func(user string) string {
		var id string
		if err := s.Writer().QueryRow(`SELECT id FROM workspaces WHERE person_id = ?`, user).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	owner, bea := personal("usr_0000000000000001"), personal("usr_0000000000000002")
	execAll(t, s, `INSERT INTO workspaces(id, kind, name, created_at, updated_at) VALUES ('wsp_team', 'team', 'Support', 1, 1)`,
		`INSERT INTO workspace_members(workspace_id, user_id, role, created_at, updated_at)
		 VALUES ('wsp_team', 'usr_0000000000000001', 'owner', 1, 1), ('wsp_team', 'usr_0000000000000002', 'member', 1, 1)`)

	ctx := context.Background()
	refused := func(what, stmt string) {
		t.Helper()
		err := s.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, stmt)
			return err
		})
		if err == nil {
			t.Errorf("%s was accepted", what)
		}
	}
	accepted := func(what, stmt string) {
		t.Helper()
		err := s.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, stmt)
			return err
		})
		if err != nil {
			t.Errorf("%s was refused: %v", what, err)
		}
	}
	link := func(id, ws, email, owner string) string {
		return fmt.Sprintf(`INSERT INTO accounts(id, workspace_id, email, provider, auth_kind, imap_host, imap_port,
		  smtp_host, smtp_port, smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at, owner_user_id)
		  VALUES ('%s', '%s', '%s', 'gmail', 'oauth2', 'h', 993, 'h', 465, 'implicit', 'x', 0, 'active', 1, 1, 1, %s)`,
			id, ws, email, owner)
	}

	accepted("the same address in another workspace", link("acc_dup", "wsp_team", "OWNER@example.org", "'usr_0000000000000001'"))
	refused("the same address twice in one workspace, in another case", link("acc_dup2", owner, "OWNER@EXAMPLE.ORG", "'usr_0000000000000001'"))
	refused("a mailbox nobody linked outside the operator workspace", link("acc_x", "wsp_team", "x@example.org", "NULL"))
	refused("a linked mailbox in the operator workspace", link("acc_y", "wsp_operator", "y@example.org", "'usr_0000000000000001'"))
	refused("a mailbox moved to another workspace", `UPDATE accounts SET workspace_id = '`+bea+`' WHERE id = 'acc_dup'`)
	refused("a linker taken off a team mailbox", `UPDATE accounts SET owner_user_id = NULL WHERE id = 'acc_dup'`)
	accepted("a take-over", `UPDATE accounts SET owner_user_id = 'usr_0000000000000002' WHERE id = 'acc_dup'`)

	refused("a member of the operator workspace", `INSERT INTO workspace_members(workspace_id, user_id, role, created_at, updated_at)
		VALUES ('wsp_operator', 'usr_0000000000000001', 'owner', 1, 1)`)
	refused("a second member of a personal workspace", `INSERT INTO workspace_members(workspace_id, user_id, role, created_at, updated_at)
		VALUES ('`+owner+`', 'usr_0000000000000002', 'member', 1, 1)`)
	refused("a personal membership demoted", `UPDATE workspace_members SET role = 'member' WHERE workspace_id = '`+owner+`'`)
	refused("a personal membership disabled", `UPDATE workspace_members SET status = 'disabled' WHERE workspace_id = '`+owner+`'`)
	refused("a membership moved", `UPDATE workspace_members SET user_id = 'usr_0000000000000004' WHERE workspace_id = 'wsp_team' AND user_id = 'usr_0000000000000002'`)
	refused("a second operator workspace", `INSERT INTO workspaces(id, kind, created_at, updated_at) VALUES ('wsp_operator2', 'operator', 1, 1)`)
	refused("a team without a name", `INSERT INTO workspaces(id, kind, created_at, updated_at) VALUES ('wsp_t2', 'team', 1, 1)`)
	refused("a personal workspace without a person", `INSERT INTO workspaces(id, kind, created_at, updated_at) VALUES ('wsp_p2', 'personal', 1, 1)`)
	refused("a second personal workspace for a person", `INSERT INTO workspaces(id, kind, person_id, created_at, updated_at)
		VALUES ('wsp_p3', 'personal', 'usr_0000000000000001', 1, 1)`)

	grant := func(account, ws, user string, read, act, send, manage int) string {
		return fmt.Sprintf(`INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act, send, manage, created_at, updated_at)
			VALUES ('%s', '%s', '%s', %d, %d, %d, %d, 1, 1)`, account, ws, user, read, act, send, manage)
	}
	refused("a grant to someone who is not a member", grant("acc_dup", "wsp_team", "usr_0000000000000004", 1, 0, 0, 0))
	refused("a grant naming another workspace than the mailbox's", grant("acc_0000000000000003", "wsp_team", "usr_0000000000000001", 1, 0, 0, 0))
	refused("act without read", grant("acc_dup", "wsp_team", "usr_0000000000000001", 0, 1, 0, 0))
	refused("a grant with no flag", grant("acc_dup", "wsp_team", "usr_0000000000000001", 0, 0, 0, 0))
	accepted("a grant to a member", grant("acc_dup", "wsp_team", "usr_0000000000000001", 1, 1, 0, 0))

	// Removing a member takes their grants; removing a mailbox takes its
	// grants.
	accepted("a member removed", `DELETE FROM workspace_members WHERE workspace_id = 'wsp_team' AND user_id = 'usr_0000000000000001'`)
	if n := queryInt(t, s, `SELECT count(*) FROM mailbox_access WHERE account_id = 'acc_dup' AND user_id = 'usr_0000000000000001'`); n != 0 {
		t.Errorf("a removed member kept %d grants", n)
	}

	refused("a team invite without a role", `INSERT INTO invites(code_hash, email, role, created_at, expires_at, id, workspace_id)
		VALUES (randomblob(32), 'z@example.org', 'member', 1, 2, 'inv_a', 'wsp_team')`)
	refused("an instance invite with a team role", `INSERT INTO invites(code_hash, email, role, created_at, expires_at, id, workspace_role)
		VALUES (randomblob(32), 'z@example.org', 'member', 1, 2, 'inv_b', 'admin')`)
	refused("an invite into a personal workspace", `INSERT INTO invites(code_hash, email, role, created_at, expires_at, id, workspace_id, workspace_role)
		VALUES (randomblob(32), 'z@example.org', 'member', 1, 2, 'inv_c', '`+owner+`', 'member')`)
	refused("an invite into the operator workspace", `INSERT INTO invites(code_hash, email, role, created_at, expires_at, id, workspace_id, workspace_role)
		VALUES (randomblob(32), 'z@example.org', 'member', 1, 2, 'inv_d', 'wsp_operator', 'member')`)
	accepted("a team invite", `INSERT INTO invites(code_hash, email, role, created_at, expires_at, id, workspace_id, workspace_role)
		VALUES (randomblob(32), 'z@example.org', 'member', 1, 2, 'inv_e', 'wsp_team', 'admin')`)
}

// brokenRebuild is a migration the runner is handed in place of a real one.
func brokenRebuild(sql string) migration {
	return migration{version: 100, name: "0100_broken.sql", sql: rebuildMarker + "\n" + sql, rebuild: true}
}

// refusedRebuild applies a broken rebuild to a migrated, populated database
// and checks that it changed nothing.
func refusedRebuild(t *testing.T, m migration) error {
	t.Helper()
	ctx := context.Background()
	s := openAtVersion(t, 7)
	seedSelfHosted(t, s)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	before, schemaBefore := dumpTables(t, s), schemaSQL(t, s)

	err := s.applyMigration(ctx, m)
	if !errors.Is(err, ErrRebuildRefused) {
		t.Fatalf("the broken rebuild: %v, want ErrRebuildRefused", err)
	}

	after := dumpTables(t, s)
	if fmt.Sprint(after) != fmt.Sprint(before) {
		t.Error("the refused rebuild changed rows")
	}
	if fmt.Sprint(schemaSQL(t, s)) != fmt.Sprint(schemaBefore) {
		t.Error("the refused rebuild changed the schema")
	}
	if v, err := s.SchemaVersion(ctx); err != nil || v != latestVersion(t) {
		t.Errorf("user_version = %d (%v), want %d", v, err, latestVersion(t))
	}
	if fk := queryInt(t, s, `PRAGMA foreign_keys`); fk != 1 {
		t.Errorf("foreign_keys is %d after the refused rebuild, want 1", fk)
	}
	// And they hold: removing a mailbox still cascades.
	execAll(t, s, `DELETE FROM accounts WHERE id = 'acc_0000000000000005'`)
	if n := queryInt(t, s, `SELECT count(*) FROM folders WHERE account_id = 'acc_0000000000000005'`); n != 0 {
		t.Errorf("removing a mailbox left %d folders: foreign keys are off", n)
	}
	return err
}

func TestARebuildThatBreaksAForeignKeyChangesNothing(t *testing.T) {
	// Every draft copied, under the id of a mailbox that does not exist: no
	// row lost, every one orphaned.
	err := refusedRebuild(t, brokenRebuild(`
		CREATE TABLE drafts_new (id TEXT PRIMARY KEY, account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
		  compose_json TEXT NOT NULL, compose_hash TEXT NOT NULL, message_id_hdr TEXT NOT NULL,
		  state TEXT NOT NULL, error TEXT NOT NULL DEFAULT '', created_by TEXT NOT NULL DEFAULT '',
		  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, sent_at INTEGER NOT NULL DEFAULT 0,
		  expires_at INTEGER NOT NULL);
		INSERT INTO drafts_new(rowid, id, account_id, compose_json, compose_hash, message_id_hdr, state, error,
		  created_by, created_at, updated_at, sent_at, expires_at)
		SELECT rowid, id, account_id || '-gone', compose_json, compose_hash, message_id_hdr, state, error,
		  created_by, created_at, updated_at, sent_at, expires_at FROM drafts;
		DROP TABLE drafts;
		ALTER TABLE drafts_new RENAME TO drafts;
		CREATE INDEX drafts_account ON drafts(account_id, state);`))
	if !strings.Contains(err.Error(), "foreign keys broken") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

func TestARebuildThatLosesARowChangesNothing(t *testing.T) {
	err := refusedRebuild(t, brokenRebuild(`
		CREATE TABLE attachment_blobs_new (sha256 TEXT PRIMARY KEY, size INTEGER NOT NULL, last_access_at INTEGER NOT NULL);
		INSERT INTO attachment_blobs_new SELECT sha256, size, last_access_at FROM attachment_blobs WHERE 0;
		DROP TABLE attachment_blobs;
		ALTER TABLE attachment_blobs_new RENAME TO attachment_blobs;
		CREATE INDEX attachment_blobs_lru ON attachment_blobs(last_access_at);`))
	if !strings.Contains(err.Error(), "attachment_blobs held 1 rows and holds 0") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

func TestARebuildThatAddsARowToATableThatWasThereChangesNothing(t *testing.T) {
	refusedRebuild(t, brokenRebuild(`INSERT INTO meta(key, value) VALUES ('surprise', '1');`))
}

func TestARebuildIsRefusedWithLegacyRenameRules(t *testing.T) {
	// With legacy_alter_table on, a rename would leave triggers and views
	// naming the old table; the runner refuses before it begins.
	ctx := context.Background()
	s := openAtVersion(t, 7)
	seedSmallest(t, s)
	if _, err := s.Writer().ExecContext(ctx, `PRAGMA legacy_alter_table = ON`); err != nil {
		t.Fatal(err)
	}
	err := s.Migrate(ctx)
	if !errors.Is(err, ErrRebuildRefused) {
		t.Fatalf("Migrate with legacy_alter_table on: %v, want ErrRebuildRefused", err)
	}
	if v, err := s.SchemaVersion(ctx); err != nil || v != 7 {
		t.Errorf("user_version = %d (%v), want 7", v, err)
	}
	if fk := queryInt(t, s, `PRAGMA foreign_keys`); fk != 1 {
		t.Errorf("foreign_keys is %d after the refusal, want 1", fk)
	}
}

// dropTable finds the tables a migration drops.
var dropTable = regexp.MustCompile(`(?i)\bDROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?["]?([A-Za-z_][A-Za-z0-9_]*)`)

func TestOnlyARebuildMayDropAReferencedTable(t *testing.T) {
	// A DROP TABLE in an ordinary migration runs with foreign keys on, where
	// it is a DELETE cascading into every table that refers to it. Only the
	// rebuild procedure may drop such a table. oauth_pending, dropped and
	// recreated by 0002, is the one ordinary drop: nothing refers to it, which
	// is checked here as well.
	allowed := map[string]bool{"0002_console.sql oauth_pending": true}
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		drops := dropTable.FindAllStringSubmatch(m.sql, -1)
		if m.rebuild || len(drops) == 0 {
			continue
		}
		s := openAtVersion(t, m.version-1)
		for _, d := range drops {
			table := d[1]
			if !allowed[m.name+" "+table] {
				t.Errorf("%s drops %s without %q on its first line", m.name, table, rebuildMarker)
				continue
			}
			referrers := queryStrings(t, s, `SELECT m.name FROM sqlite_schema m, pragma_foreign_key_list(m.name) f
				WHERE m.type = 'table' AND f."table" = ? COLLATE NOCASE AND m.name <> ?`, table, table)
			if len(referrers) != 0 {
				t.Errorf("%s drops %s, which %v refer to", m.name, table, referrers)
			}
		}
	}
	// And the one rebuild so far is marked as one.
	for _, m := range migrations {
		if m.version == 8 && !m.rebuild {
			t.Error("0008 rebuilds accounts and is not marked as a rebuild")
		}
	}
}

func TestARebuildIgnoresTheStatisticsOfAnAnalyzedDatabase(t *testing.T) {
	// ANALYZE, which an operator may have run by hand, keeps a line per
	// index in sqlite_stat1, and dropping a table drops its indexes' lines.
	// They describe the data; they are not data, and lose nothing.
	ctx := context.Background()
	s := openAtVersion(t, 7)
	seedSelfHosted(t, s)
	if _, err := s.Writer().ExecContext(ctx, `ANALYZE`); err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, s, `SELECT count(*) FROM sqlite_stat1 WHERE tbl = 'accounts'`); n == 0 {
		t.Fatal("ANALYZE kept no statistics for accounts")
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate on an analyzed database: %v", err)
	}
	if v, err := s.SchemaVersion(ctx); err != nil || v != latestVersion(t) {
		t.Fatalf("user_version = %d (%v), want %d", v, err, latestVersion(t))
	}
}

func TestMigrationNineTakesTheMailboxesTheirPersonCannotReadOutOfPersonKeys(t *testing.T) {
	// Under schema 7 the owner restricted gggggggg to a mailbox nobody owned,
	// and hhhhhhhh to one beside their own gmail. Those mailboxes are the
	// operator's now, which no person reaches: the rows go, gggggggg, left
	// with none, is revoked rather than widened, and hhhhhhhh keeps the
	// mailbox that is still theirs. Nothing else changes.
	s := openAtVersion(t, 7)
	seedSelfHosted(t, s)
	migrateTo(t, s, 8)
	before := dumpTables(t, s)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if v, err := s.SchemaVersion(context.Background()); err != nil || v != 9 {
		t.Fatalf("user_version = %d (%v), want 9", v, err)
	}
	after := dumpTables(t, s)
	for table, rows := range before {
		if table == "api_key_accounts" || table == "api_keys" {
			continue
		}
		if fmt.Sprint(after[table]) != fmt.Sprint(rows) {
			t.Errorf("0009 changed %s", table)
		}
	}

	if got := queryStrings(t, s, `SELECT key_prefix, account_id FROM api_key_accounts ORDER BY 1, 2`); !slices.Equal(got, []string{
		"bbbbbbbb acc_0000000000000005", // an instance key's: the operator's mailbox
		"cccccccc acc_0000000000000001", // an instance key's on a person's mailbox: kept, reaching nothing, for the operator to revoke
		"eeeeeeee acc_0000000000000001",
		"hhhhhhhh acc_0000000000000001",
	}) {
		t.Errorf("restrictions after 0009:\n%s", strings.Join(got, "\n"))
	}
	if got := queryStrings(t, s, `SELECT prefix, revoked_at <> 0, restricted FROM api_keys ORDER BY prefix`); !slices.Equal(got, []string{
		"aaaaaaaa 0 0", "bbbbbbbb 0 1", "cccccccc 0 1", "dddddddd 0 0", "eeeeeeee 0 1", "ffffffff 1 0",
		"gggggggg 1 1", "hhhhhhhh 0 1",
	}) {
		t.Errorf("keys after 0009:\n%s", strings.Join(got, "\n"))
	}
	if n := queryInt(t, s, `SELECT count(*) FROM api_key_accounts r JOIN api_keys k ON k.prefix = r.key_prefix
		WHERE k.user_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM mailbox_access g
		  WHERE g.account_id = r.account_id AND g.user_id = k.user_id AND g.read)`); n != 0 {
		t.Errorf("%d restrictions of person keys name a mailbox their person cannot read", n)
	}
	if got := queryStrings(t, s, `PRAGMA foreign_key_check`); len(got) != 0 {
		t.Errorf("foreign_key_check: %v", got)
	}
}

func TestMigrationNineGivesAServerNobodySignedUpToItsFirstOwner(t *testing.T) {
	// Under schema 7 whoever signed up first became an owner, and the quick
	// start's `user invite --bootstrap` made a member invite counting on it.
	// A server upgraded before anyone signed up gets that owner from its
	// oldest invite still waiting, and from nothing else.
	invite := func(email, role string, created, expires int) string {
		return fmt.Sprintf(`INSERT INTO invites(code_hash, email, role, created_by, created_at, expires_at)
			VALUES (randomblob(32), '%s', '%s', 'cli', %d, %d)`, email, role, created, expires)
	}
	const later = 9999999999
	for _, c := range []struct {
		name  string
		seed  []string
		owner []string
	}{
		{"nobody yet", []string{
			invite("expired@example.org", "member", 1, 2),
			invite("first@example.org", "member", 10, later),
			invite("second@example.org", "member", 11, later),
		}, []string{"first@example.org"}},
		{"an owner invite already waiting", []string{
			invite("first@example.org", "member", 10, later),
			invite("owner@example.org", "owner", 11, later),
		}, []string{"owner@example.org"}},
		{"somebody signed up", []string{
			`INSERT INTO users(id, email, name, password_hash, role, status, password_changed_at, created_at, updated_at)
			 VALUES ('usr_0000000000000001', 'member@example.org', '', '$argon2id$x', 'member', 'active', 1, 1, 1)`,
			invite("first@example.org", "member", 10, later),
		}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := openAtVersion(t, 7)
			execAll(t, s, c.seed...)
			if err := s.Migrate(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := queryStrings(t, s, `SELECT email FROM invites WHERE role = 'owner' ORDER BY email`); !slices.Equal(got, c.owner) {
				t.Errorf("owner invites after the migration: %v, want %v", got, c.owner)
			}
		})
	}
}

func TestADatabaseANewerBinaryMigratedIsRefused(t *testing.T) {
	// A binary rolled back alone would read and write a schema it does not
	// know: one from before 0008 made people without a personal workspace.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mail.db")
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	newer := latestVersion(t) + 1
	if _, err := s.Writer().ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", newer)); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("Migrate on a newer schema: %v, want ErrSchemaTooNew", err)
	}
	if _, err := s.PendingMigrations(ctx); !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("PendingMigrations on a newer schema: %v, want ErrSchemaTooNew", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if again, err := Open(ctx, path, Options{}); !errors.Is(err, ErrSchemaTooNew) {
		if again != nil {
			_ = again.Close()
		}
		t.Fatalf("Open on a newer schema: %v, want ErrSchemaTooNew", err)
	}
	// Opening without migrating still works: `migrate --dry-run` opens so,
	// and reports the refusal above, with both versions.
	plain, err := Open(ctx, path, Options{SkipMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plain.Close() }()
	if v, err := plain.SchemaVersion(ctx); err != nil || v != newer {
		t.Errorf("SchemaVersion = %d (%v), want %d", v, err, newer)
	}
}
