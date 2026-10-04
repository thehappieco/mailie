package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// This test lives inside the package because it has to stop between two
// migrations, which nothing outside it should ever be able to do.

func TestMigrationTwoKeepsExistingAccounts(t *testing.T) {
	// The console migration adds owners to accounts. Rebuilding that table
	// would cascade a DELETE into every credential, folder and message, so the
	// guarantee is checked the way a real upgrade meets it: a phase-1 database
	// with a mailbox in it, then the new schema on top.
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
	if len(migrations) < 2 || migrations[0].version != 1 || migrations[1].version != 2 {
		t.Fatalf("want migrations 1 and 2 in order, got %+v", migrations)
	}
	if err := s.applyMigration(ctx, migrations[0]); err != nil {
		t.Fatalf("apply 0001: %v", err)
	}

	err = s.Write(ctx, func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`INSERT INTO accounts(id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port,
			   smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at)
			 VALUES ('acc_1', 'person@example.com', 'gmail', 'oauth2', 'imap.gmail.com', 993,
			   'smtp.gmail.com', 465, 'implicit', 'person@example.com', 0, 'active', 1, 1, 1)`,
			`INSERT INTO credentials(account_id, field, keyid, ciphertext, updated_at)
			 VALUES ('acc_1', 'oauth_token', 1, x'0102', 1)`,
			`INSERT INTO folders(id, account_id, name, display_name) VALUES (7, 'acc_1', 'INBOX', 'Inbox')`,
			`INSERT INTO messages(account_id, folder_id, uidvalidity, uid, group_key, internal_date, first_seen_at, updated_at)
			 VALUES ('acc_1', 7, 1, 42, 'mid:x@y', 1, 1, 1)`,
			`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at)
			 VALUES ('deadbeef', '$argon2id$', 'cli', 'admin', 1, 2)`,
			`INSERT INTO api_key_accounts(key_prefix, account_id) VALUES ('deadbeef', 'acc_1')`,
			`INSERT INTO oauth_pending(state, account_id, flow, redirect_uri, expires_at)
			 VALUES ('st', 'acc_1', 'loopback', 'http://127.0.0.1:1/oauth/callback', 1)`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed a phase-1 database: %v", err)
	}

	if err := s.applyMigration(ctx, migrations[1]); err != nil {
		t.Fatalf("apply 0002: %v", err)
	}
	if v, err := s.SchemaVersion(ctx); err != nil || v != 2 {
		t.Fatalf("user_version = %d (%v), want 2", v, err)
	}

	for table, want := range map[string]int{
		"accounts": 1, "credentials": 1, "folders": 1, "messages": 1, "api_keys": 1, "api_key_accounts": 1,
	} {
		var n int
		if err := s.Reader().QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != want {
			t.Errorf("%s has %d rows after the migration, want %d", table, n, want)
		}
	}

	var owner sql.NullString
	var client string
	if err := s.Reader().QueryRowContext(ctx,
		`SELECT owner_user_id, oauth_client FROM accounts WHERE id = 'acc_1'`).Scan(&owner, &client); err != nil {
		t.Fatalf("read the new columns: %v", err)
	}
	// An account from before the console belongs to the instance, and its
	// token was issued to the client the CLI uses.
	if owner.Valid {
		t.Errorf("owner_user_id = %q, want NULL", owner.String)
	}
	if client != "installed" {
		t.Errorf("oauth_client = %q, want installed", client)
	}
	var keyUser sql.NullString
	if err := s.Reader().QueryRowContext(ctx,
		`SELECT user_id FROM api_keys WHERE prefix = 'deadbeef'`).Scan(&keyUser); err != nil {
		t.Fatalf("read api_keys.user_id: %v", err)
	}
	if keyUser.Valid {
		t.Errorf("a phase-1 key became bound to user %q", keyUser.String)
	}

	// The recreated table takes the new flow and still cascades from its
	// account.
	err = s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO oauth_pending(state, account_id, flow, redirect_uri, expires_at)
			VALUES ('web-state', 'acc_1', 'web', 'https://console.example/oauth/return', 1)`)
		return err
	})
	if err != nil {
		t.Fatalf("oauth_pending refuses the web flow: %v", err)
	}
	err = s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE accounts SET oauth_client = 'shared' WHERE id = 'acc_1'`)
		return err
	})
	if err == nil {
		t.Error("oauth_client accepted a value outside its CHECK")
	}
}

func TestMigrationThreeKeepsAConsoleDatabaseAndStartsWithNothingConsented(t *testing.T) {
	// Production runs a phase-1 database with the console on top: people,
	// their sessions, mailboxes they own and mailboxes the CLI created. The
	// sync migration must keep all of it and must not quietly turn sync on for
	// anyone: the policy promises nothing is stored until a person agrees.
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
	if len(migrations) < 3 || migrations[2].version != 3 {
		t.Fatalf("want migration 3 third, got %+v", migrations)
	}
	for _, m := range migrations[:2] {
		if err := s.applyMigration(ctx, m); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}

	err = s.Write(ctx, func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`INSERT INTO users(id, email, name, password_hash, role, password_changed_at, created_at, updated_at)
			 VALUES ('usr_1', 'owner@example.com', 'Owner', '$argon2id$', 'owner', 1, 1, 1)`,
			`INSERT INTO sessions(id, user_id, token_hash, created_at, last_seen_at, expires_at)
			 VALUES ('ses_1', 'usr_1', zeroblob(32), 1, 1, 2)`,
			`INSERT INTO accounts(id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port,
			   smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at, owner_user_id, oauth_client)
			 VALUES ('acc_owned', 'person@gmail.com', 'gmail', 'oauth2', 'imap.gmail.com', 993,
			   'smtp.gmail.com', 465, 'implicit', 'person@gmail.com', 0, 'active', 1, 1, 1, 'usr_1', 'web')`,
			`INSERT INTO accounts(id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port,
			   smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at)
			 VALUES ('acc_cli', 'ops@example.com', 'imap', 'password', 'imap.example.com', 993,
			   'smtp.example.com', 587, 'starttls', 'ops@example.com', 1, 'active', 1, 1, 1)`,
			`INSERT INTO credentials(account_id, field, keyid, ciphertext, updated_at)
			 VALUES ('acc_owned', 'oauth_token', 1, x'0102', 1)`,
			`INSERT INTO folders(id, account_id, name, display_name) VALUES (7, 'acc_owned', 'INBOX', 'Inbox')`,
			`INSERT INTO messages(account_id, folder_id, uidvalidity, uid, group_key, internal_date, first_seen_at, updated_at)
			 VALUES ('acc_owned', 7, 1, 42, 'mid:x@y', 1, 1, 1)`,
			`INSERT INTO events(type, account_id, payload_json, created_at) VALUES ('account.state', 'acc_owned', '{}', 1)`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed a console database: %v", err)
	}

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if v, err := s.SchemaVersion(ctx); err != nil || v < 3 {
		t.Fatalf("user_version = %d (%v), want at least 3", v, err)
	}

	for table, want := range map[string]int{
		"users": 1, "sessions": 1, "accounts": 2, "credentials": 1, "folders": 1, "messages": 1, "events": 1,
	} {
		var n int
		if err := s.Reader().QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != want {
			t.Errorf("%s has %d rows after the migration, want %d", table, n, want)
		}
	}

	var consentAt int64
	var consentVersion string
	if err := s.Reader().QueryRowContext(ctx,
		`SELECT sync_consent_at, sync_consent_version FROM users WHERE id = 'usr_1'`).Scan(&consentAt, &consentVersion); err != nil {
		t.Fatalf("read the consent columns: %v", err)
	}
	if consentAt != 0 || consentVersion != "" {
		t.Errorf("an existing person starts consented (%d, %q); the policy says they must be asked", consentAt, consentVersion)
	}
	rows, err := s.Reader().QueryContext(ctx, `SELECT id, sync_enabled_at, sync_enabled_by FROM accounts`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, by string
		var at int64
		if err := rows.Scan(&id, &at, &by); err != nil {
			t.Fatal(err)
		}
		if at != 0 || by != "" {
			t.Errorf("%s starts with sync switched on (%d by %q)", id, at, by)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	eligible, err := s.SyncEligibleAccounts(ctx)
	if err != nil {
		t.Fatalf("SyncEligibleAccounts: %v", err)
	}
	if len(eligible) != 0 {
		t.Errorf("accounts eligible to sync right after the upgrade: %v, want none", eligible)
	}

	var secureDelete int
	if err := s.Reader().QueryRowContext(ctx,
		`SELECT v FROM messages_fts_config WHERE k = 'secure-delete'`).Scan(&secureDelete); err != nil || secureDelete != 1 {
		t.Errorf("the full-text index's secure-delete is %d (%v), want 1: deleted terms would stay in old segments",
			secureDelete, err)
	}

	var initialTotal, initialFetched int
	if err := s.Reader().QueryRowContext(ctx,
		`SELECT initial_total, initial_fetched FROM folders WHERE id = 7`).Scan(&initialTotal, &initialFetched); err != nil {
		t.Fatalf("read the progress columns: %v", err)
	}
	if initialTotal != 0 || initialFetched != 0 {
		t.Errorf("progress columns start at %d/%d, want 0/0", initialFetched, initialTotal)
	}
}

func TestMigrationFourKeepsASyncingDatabaseAndStartsWithActionsOff(t *testing.T) {
	// Production runs phase 3a: people who agreed to sync, their mailboxes
	// and what is indexed for them. Actions are a new use of the mailbox, so
	// the migration must keep all of that and allow actions for nobody, not
	// even for those who agreed to sync.
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
	if len(migrations) < 4 || migrations[3].version != 4 {
		t.Fatalf("want migration 4 fourth, got %+v", migrations)
	}
	for _, m := range migrations[:3] {
		if err := s.applyMigration(ctx, m); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}
	err = s.Write(ctx, func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`INSERT INTO users(id, email, name, password_hash, role, password_changed_at, created_at, updated_at,
			   sync_consent_at, sync_consent_version)
			 VALUES ('usr_1', 'owner@example.com', 'Owner', '$argon2id$', 'owner', 1, 1, 1, 5, '2026-01-test-sync')`,
			`INSERT INTO sessions(id, user_id, token_hash, created_at, last_seen_at, expires_at)
			 VALUES ('ses_1', 'usr_1', zeroblob(32), 1, 1, 2)`,
			`INSERT INTO accounts(id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port,
			   smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at, owner_user_id)
			 VALUES ('acc_owned', 'person@gmail.com', 'gmail', 'oauth2', 'imap.gmail.com', 993,
			   'smtp.gmail.com', 465, 'implicit', 'person@gmail.com', 0, 'active', 1, 1, 1, 'usr_1')`,
			`INSERT INTO folders(id, account_id, name, display_name) VALUES (7, 'acc_owned', 'INBOX', 'Inbox')`,
			`INSERT INTO messages(account_id, folder_id, uidvalidity, uid, group_key, internal_date, first_seen_at, updated_at)
			 VALUES ('acc_owned', 7, 1, 42, 'mid:x@y', 1, 1, 1)`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed a syncing database: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if v, err := s.SchemaVersion(ctx); err != nil || v < 4 {
		t.Fatalf("user_version = %d (%v), want at least 4", v, err)
	}
	for table, want := range map[string]int{"users": 1, "sessions": 1, "accounts": 1, "folders": 1, "messages": 1} {
		var n int
		if err := s.Reader().QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != want {
			t.Errorf("%s has %d rows after the migration, want %d", table, n, want)
		}
	}
	c, err := s.ActionsConsentOf(ctx, "usr_1")
	if err != nil || c.At != 0 || c.Version != "" {
		t.Errorf("a person who agreed to sync starts with actions %+v (%v); the policy says they must be asked", c, err)
	}
	if sync, err := s.SyncConsentOf(ctx, "usr_1"); err != nil || sync.At != 5 {
		t.Errorf("the sync consent became %+v (%v)", sync, err)
	}
}

func TestMigrationFiveKeepsEveryKeyAndRecordsNoAgreementForThem(t *testing.T) {
	// Production runs phase 3b: keys an administrator issued, some acting
	// for a person, some restricted to a mailbox. The key terms are new, so
	// no key made before them may look agreed to, and none may change.
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
	if len(migrations) < 5 || migrations[4].version != 5 {
		t.Fatalf("want migration 5 fifth, got %+v", migrations)
	}
	for _, m := range migrations[:4] {
		if err := s.applyMigration(ctx, m); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}
	err = s.Write(ctx, func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`INSERT INTO users(id, email, name, password_hash, role, password_changed_at, created_at, updated_at)
			 VALUES ('usr_1', 'owner@example.com', 'Owner', '$argon2id$', 'owner', 1, 1, 1)`,
			`INSERT INTO accounts(id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port,
			   smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at, owner_user_id)
			 VALUES ('acc_owned', 'person@gmail.com', 'gmail', 'oauth2', 'imap.gmail.com', 993,
			   'smtp.gmail.com', 465, 'implicit', 'person@gmail.com', 0, 'active', 1, 1, 1, 'usr_1')`,
			`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at) VALUES ('aaaaaaaa', '$h', 'cli', 'admin', 1, 9999999999)`,
			`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at, user_id)
			 VALUES ('bbbbbbbb', '$h', 'for a person', 'read', 1, 9999999999, 'usr_1')`,
			`INSERT INTO api_key_accounts(key_prefix, account_id) VALUES ('bbbbbbbb', 'acc_owned')`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed a phase 3b database: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	rows, err := s.Reader().QueryContext(ctx,
		`SELECT prefix, scope, coalesce(user_id, ''), revoked_at, terms_version, created_by FROM api_keys ORDER BY prefix`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var prefix, scope, user, terms, by string
		var revoked int64
		if err := rows.Scan(&prefix, &scope, &user, &revoked, &terms, &by); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s %s %q %d %q %q", prefix, scope, user, revoked, terms, by))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{`aaaaaaaa admin "" 0 "" ""`, `bbbbbbbb read "usr_1" 0 "" ""`}
	if !slices.Equal(got, want) {
		t.Errorf("keys after the migration:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	var restricted int
	if err := s.Reader().QueryRowContext(ctx, `SELECT count(*) FROM api_key_accounts`).Scan(&restricted); err != nil || restricted != 1 {
		t.Errorf("restrictions after the migration: %d (%v)", restricted, err)
	}
}

func TestMigrationSixRecordsWhichKeysWereMadeForChosenAccounts(t *testing.T) {
	// Keys issued before the flag existed are restricted if they have
	// restriction rows now.
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
	if len(migrations) < 6 || migrations[5].version != 6 {
		t.Fatalf("want migration 6 sixth, got %+v", migrations)
	}
	for _, m := range migrations[:5] {
		if err := s.applyMigration(ctx, m); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}
	err = s.Write(ctx, func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`INSERT INTO accounts(id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port,
			   smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at)
			 VALUES ('acc_a', 'a@example.com', 'imap', 'password', 'imap.example.com', 993,
			   'smtp.example.com', 465, 'implicit', 'a@example.com', 0, 'active', 1, 1, 1)`,
			`INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at)
			 VALUES ('aaaaaaaa', '$h', 'every account', 'read', 1, 9999999999),
			        ('bbbbbbbb', '$h', 'one account', 'read', 1, 9999999999)`,
			`INSERT INTO api_key_accounts(key_prefix, account_id) VALUES ('bbbbbbbb', 'acc_a')`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed a phase 4 database: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for prefix, want := range map[string]bool{"aaaaaaaa": false, "bbbbbbbb": true} {
		var got bool
		if err := s.Reader().QueryRowContext(ctx,
			`SELECT restricted FROM api_keys WHERE prefix = ?`, prefix).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("key %s restricted = %t, want %t", prefix, got, want)
		}
	}
}

func TestMigrationSevenStartsEveryoneWithSendingOff(t *testing.T) {
	// Production runs phase 4: people who agreed to sync and to actions, and
	// send rows no code wrote yet. Sending is a new use of the mailbox: the
	// migration keeps everything and allows sending for nobody.
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "mail.db"), Options{SkipMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) < 7 || migrations[6].version != 7 {
		t.Fatalf("want migration 7 seventh, got %+v", migrations)
	}
	for _, m := range migrations[:6] {
		if err := s.applyMigration(ctx, m); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}
	err = s.Write(ctx, func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`INSERT INTO users(id, email, name, password_hash, role, password_changed_at, created_at, updated_at,
			   sync_consent_at, sync_consent_version, actions_consent_at, actions_consent_version)
			 VALUES ('usr_1', 'owner@example.com', 'Owner', '$argon2id$', 'owner', 1, 1, 1,
			   5, '2026-01-test-sync', 6, '2026-01-test-actions')`,
			`INSERT INTO accounts(id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port,
			   smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at, owner_user_id)
			 VALUES ('acc_owned', 'person@gmail.com', 'gmail', 'oauth2', 'imap.gmail.com', 993,
			   'smtp.gmail.com', 465, 'implicit', 'person@gmail.com', 0, 'active', 1, 1, 1, 'usr_1')`,
			`INSERT INTO sends(account_id, idempotency_key, compose_hash, message_id_hdr, state, created_at, updated_at)
			 VALUES ('acc_owned', 'k', 'h', 'm@x', 'sent', 1, 1)`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed a phase 4 database: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	c, err := s.SendConsentOf(ctx, "usr_1")
	if err != nil || c.At != 0 || c.Version != "" {
		t.Errorf("a person who agreed to sync and actions starts with sending %+v (%v)", c, err)
	}
	if a, err := s.ActionsConsentOf(ctx, "usr_1"); err != nil || a.At != 6 {
		t.Errorf("the actions consent became %+v (%v)", a, err)
	}
	row, err := s.SendOf(ctx, "acc_owned", "k")
	if err != nil || row.State != SendSent || row.Recipients != 0 || row.UserID != "" {
		t.Errorf("the send row after the migration = %+v, %v", row, err)
	}
}
