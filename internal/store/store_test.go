package store_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

func TestAFreshDatabaseIsMigratedAndReportsItsVersion(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)

	v, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v < 1 {
		t.Fatalf("user_version = %d, want at least 1", v)
	}
	pending, err := s.PendingMigrations(ctx)
	if err != nil {
		t.Fatalf("PendingMigrations: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("still pending after Open: %v", pending)
	}
}

func TestReopeningDoesNotReapplyMigrations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mail.db")

	first := storetest.NewAt(t, path, nil)
	if err := first.SetMeta(ctx, "instance_id", "abc123"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A second migration pass would run CREATE TABLE again and fail; that it
	// does not is the whole point of user_version.
	second := storetest.NewAt(t, path, nil)
	got, err := second.Meta(ctx, "instance_id")
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if got != "abc123" {
		t.Fatalf("meta did not survive the reopen: %q", got)
	}
}

func TestTheDatabaseFileIsNotWorldReadable(t *testing.T) {
	// It holds sealed refresh tokens and every subject line in the index; the
	// process umask is not something to depend on.
	path := filepath.Join(t.TempDir(), "mail.db")
	storetest.NewAt(t, path, nil)

	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("%s has mode %o, want no group or other access", filepath.Base(p), mode)
		}
	}
}

func TestTheReaderPoolCannotWrite(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)

	_, err := s.Reader().ExecContext(ctx, `INSERT INTO meta(key, value) VALUES ('x', 'y')`)
	if err == nil {
		t.Fatal("the read-only pool accepted a write")
	}
	if !store.IsReadOnly(err) {
		t.Fatalf("want SQLITE_READONLY so the mistake is unambiguous, got %v", err)
	}
}

func TestConcurrentWritersNeverSeeABusySnapshot(t *testing.T) {
	// A deferred transaction that reads and then writes takes
	// SQLITE_BUSY_SNAPSHOT under contention, and busy_timeout cannot rescue
	// it. Store.Write opens every transaction immediate, which is what makes
	// this loop boring.
	ctx := context.Background()
	s := storetest.New(t)

	const writers, each = 4, 50
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				err := s.Write(ctx, func(tx *sql.Tx) error {
					var n int
					// Read first, then write: the exact shape that fails
					// with a deferred transaction.
					if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM meta`).Scan(&n); err != nil {
						return err
					}
					_, err := tx.ExecContext(ctx,
						`INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
						"k", "v")
					return err
				})
				if err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if store.IsBusy(err) {
			t.Fatalf("SQLITE_BUSY on the writer pool: %v", err)
		}
		t.Fatalf("write failed: %v", err)
	}
}

func TestReadersRunWhileTheWriterHoldsATransaction(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)

	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.Write(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES ('held', '1')`); err != nil {
				return err
			}
			<-release
			return nil
		})
	}()

	// WAL readers do not block behind a writer; if they did, every list and
	// search request would queue behind whatever the sync engine is doing.
	deadline := time.After(5 * time.Second)
	for {
		var n int
		err := s.Reader().QueryRowContext(ctx, `SELECT count(*) FROM accounts`).Scan(&n)
		if err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("a read could not complete while the writer held a transaction: %v", err)
		default:
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestTheUniqueViolationOfAnIdempotencyKeyIsRecognisable(t *testing.T) {
	// The send path reserves its idempotency key with an INSERT and reads the
	// existing row when this fires. Misclassifying it would mean sending twice.
	ctx := context.Background()
	s := storetest.New(t)
	seedAccount(t, s, "acc_1")

	insert := func() error {
		return s.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO sends(account_id, idempotency_key, compose_hash, message_id_hdr, state, created_at, updated_at)
				 VALUES (?, ?, ?, ?, 'sending', 0, 0)`,
				"acc_1", "key-1", "hash", "id@example.com")
			return err
		})
	}
	if err := insert(); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	err := insert()
	if !store.IsUnique(err) {
		t.Fatalf("want a unique violation, got %v", err)
	}
}

func TestAForeignKeyViolationIsRecognisable(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)

	err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO credentials(account_id, field, keyid, ciphertext, updated_at) VALUES ('nope', 'password', 1, x'00', 0)`)
		return err
	})
	if !store.IsForeignKey(err) {
		t.Fatalf("want a foreign-key violation (foreign_keys must be on), got %v", err)
	}
}

func TestTheFullTextIndexFollowsTheMessagesTable(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	seedAccount(t, s, "acc_1")
	folderID := seedFolder(t, s, "acc_1", "INBOX")

	id := seedMessage(t, s, "acc_1", folderID, 1, "Fatura de setembro", "contas@example.com")

	var hits int
	q := `SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?`
	if err := s.Reader().QueryRowContext(ctx, q, `fatura`).Scan(&hits); err != nil {
		t.Fatalf("match: %v", err)
	}
	if hits != 1 {
		t.Fatalf("want 1 hit after insert, got %d", hits)
	}

	// remove_diacritics 2 is why an accented subject is findable unaccented.
	if err := s.Reader().QueryRowContext(ctx, q, `setembro`).Scan(&hits); err != nil {
		t.Fatalf("match: %v", err)
	}
	if hits != 1 {
		t.Fatalf("want 1 hit for an unaccented query, got %d", hits)
	}

	if err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, id)
		return err
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.Reader().QueryRowContext(ctx, q, `fatura`).Scan(&hits); err != nil {
		t.Fatalf("match: %v", err)
	}
	if hits != 0 {
		t.Fatalf("the full-text row outlived its message: %d hits", hits)
	}
}

func TestMarkingAMessageReadDoesNotRewriteTheFullTextRow(t *testing.T) {
	// The AFTER UPDATE trigger is scoped to the indexed columns precisely so
	// the hot path — a flag change — does not touch FTS.
	ctx := context.Background()
	s := storetest.New(t)
	seedAccount(t, s, "acc_1")
	folderID := seedFolder(t, s, "acc_1", "INBOX")
	id := seedMessage(t, s, "acc_1", folderID, 1, "Reunião", "chefe@example.com")

	before := ftsRowCount(t, s)
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE messages SET seen = 1 WHERE id = ?`, id)
		return err
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if after := ftsRowCount(t, s); after != before {
		t.Fatalf("full-text rows changed from %d to %d on a flag update", before, after)
	}
}

func TestFolderCountersTrackTheLocalIndexNotTheServer(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	seedAccount(t, s, "acc_1")
	folderID := seedFolder(t, s, "acc_1", "INBOX")

	first := seedMessage(t, s, "acc_1", folderID, 1, "one", "a@example.com")
	seedMessage(t, s, "acc_1", folderID, 2, "two", "b@example.com")
	assertCounts(t, s, folderID, 2, 2)

	if err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE messages SET seen = 1 WHERE id = ?`, first)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assertCounts(t, s, folderID, 2, 1)

	// A tombstone is still a row, but it is not a message any more.
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE messages SET vanished_at = 100 WHERE id = ?`, first)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assertCounts(t, s, folderID, 1, 1)

	if err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE folder_id = ?`, folderID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assertCounts(t, s, folderID, 0, 0)
}

func TestOneFolderPerRolePerAccount(t *testing.T) {
	// A localised-name table that matches two folders would otherwise file
	// sent mail in whichever one the last pass happened to see.
	ctx := context.Background()
	s := storetest.New(t)
	seedAccount(t, s, "acc_1")
	seedFolderWithRole(t, s, "acc_1", "Sent Items", "sent")

	err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO folders(account_id, name, display_name, role, sync_state) VALUES ('acc_1', 'Itens Enviados', 'Itens Enviados', 'sent', 'new')`)
		return err
	})
	if !store.IsUnique(err) {
		t.Fatalf("want a unique violation for a second sent folder, got %v", err)
	}
}

func TestDeletingAnAccountLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	seedAccount(t, s, "acc_1")
	folderID := seedFolder(t, s, "acc_1", "INBOX")
	seedMessage(t, s, "acc_1", folderID, 1, "one", "a@example.com")

	if err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE id = 'acc_1'`)
		return err
	}); err != nil {
		t.Fatalf("delete account: %v", err)
	}

	for _, table := range []string{"folders", "messages", "credentials"} {
		var n int
		if err := s.Reader().QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s still holds %d rows after the account was removed", table, n)
		}
	}
}

func TestTheSchemaRejectsAnUnknownAccountState(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)

	err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO accounts(id, workspace_id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port,
			 smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at)
			 VALUES ('acc_x', 'wsp_operator', 'x@example.com', 'gmail', 'oauth2', 'imap.gmail.com', 993, 'smtp.gmail.com', 465,
			 'implicit', 'x@example.com', 0, 'confused', 0, 0, 0)`)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Fatalf("want a CHECK constraint failure, got %v", err)
	}
}

// --- helpers ---------------------------------------------------------------

func seedAccount(t *testing.T, s *store.Store, id string) {
	t.Helper()
	ctx := context.Background()
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO accounts(id, workspace_id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port,
			 smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at)
			 VALUES (?, 'wsp_operator', ?, 'gmail', 'oauth2', 'imap.gmail.com', 993, 'smtp.gmail.com', 465,
			 'implicit', ?, 0, 'active', 0, 0, 0)`,
			id, id+"@example.com", id+"@example.com")
		return err
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
}

func seedFolder(t *testing.T, s *store.Store, account, name string) int64 {
	t.Helper()
	return seedFolderWithRole(t, s, account, name, "")
}

func seedFolderWithRole(t *testing.T, s *store.Store, account, name, role string) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`INSERT INTO folders(account_id, name, display_name, role, uidvalidity, sync_state)
			 VALUES (?, ?, ?, ?, 1, 'live') RETURNING id`,
			account, name, name, role).Scan(&id)
	}); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	return id
}

func seedMessage(t *testing.T, s *store.Store, account string, folderID int64, uid int, subject, from string) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`INSERT INTO messages(account_id, folder_id, uidvalidity, uid, message_id, group_key, subject,
			 from_addr, from_text, internal_date, first_seen_at, updated_at)
			 VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, 1000, 0, 0) RETURNING id`,
			account, folderID, uid, subject+"@example.com", "mid:"+subject+"@example.com",
			subject, from, from).Scan(&id)
	}); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	return id
}

func ftsRowCount(t *testing.T, s *store.Store) int {
	t.Helper()
	var n int
	if err := s.Reader().QueryRowContext(context.Background(), `SELECT count(*) FROM messages_fts`).Scan(&n); err != nil {
		t.Fatalf("count fts: %v", err)
	}
	return n
}

func assertCounts(t *testing.T, s *store.Store, folderID int64, wantLocal, wantUnseen int) {
	t.Helper()
	var local, unseen int
	if err := s.Reader().QueryRowContext(context.Background(),
		`SELECT local_count, unseen_count FROM folders WHERE id = ?`, folderID).Scan(&local, &unseen); err != nil {
		t.Fatalf("read counters: %v", err)
	}
	if local != wantLocal || unseen != wantUnseen {
		t.Fatalf("counters = (%d local, %d unseen), want (%d, %d)", local, unseen, wantLocal, wantUnseen)
	}
}

func TestADeletedRowIsGoneFromTheFilesOnceScrubbed(t *testing.T) {
	// A DELETE only unlinks a row: without secure_delete its bytes stay in
	// the page, and in WAL mode the page as it was stays in the log as well.
	// The privacy policy says a closed account is deleted, which has to mean
	// from the disk.
	ctx := context.Background()
	s := storetest.New(t)
	const secret = "zelda.quintero@example.org"

	write := func(query string, args ...any) {
		t.Helper()
		if err := s.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, query, args...)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Enough other rows that the page survives the delete and is rewritten
	// rather than freed whole.
	for i := range 50 {
		write(`INSERT INTO meta(key, value) VALUES (?, ?)`, "filler-"+strings.Repeat("x", i), "kept")
	}
	write(`INSERT INTO meta(key, value) VALUES ('person', ?)`, secret)
	if err := s.Scrub(ctx); err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if !filesHold(t, s, secret) {
		t.Fatal("the row never reached the file, so this test would prove nothing")
	}

	write(`DELETE FROM meta WHERE key = 'person'`)
	if err := s.Scrub(ctx); err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if filesHold(t, s, secret) {
		t.Error("the deleted value is still in the database file or its log")
	}
	var kept int
	if err := s.Reader().QueryRowContext(ctx, `SELECT count(*) FROM meta WHERE value = 'kept'`).Scan(&kept); err != nil || kept != 50 {
		t.Errorf("the other rows: %d, %v", kept, err)
	}
	if info, err := os.Stat(s.Path() + "-wal"); err == nil && info.Size() != 0 {
		t.Errorf("the log is %d bytes after a scrub, want it truncated", info.Size())
	}
}

// filesHold reports whether the database file or its write-ahead log contains
// needle anywhere, live or not.
func filesHold(t *testing.T, s *store.Store, needle string) bool {
	t.Helper()
	for _, file := range []string{s.Path(), s.Path() + "-wal"} {
		raw, err := os.ReadFile(file)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), needle) {
			return true
		}
	}
	return false
}
