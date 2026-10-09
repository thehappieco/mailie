// Package storetest opens throwaway databases for tests.
//
// It lives in its own package, the way net/http/httptest does, so that nothing
// in the daemon can import the testing machinery by accident.
package storetest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
)

// New opens a migrated database in a temporary directory.
//
// A file, never ":memory:". An in-memory database belongs to one connection,
// so the writer and the reader pool would each see their own empty database
// and every read would fail with "no such table" — which looks exactly like a
// schema bug and is not one.
func New(t *testing.T) *store.Store {
	t.Helper()
	return NewAt(t, filepath.Join(t.TempDir(), "mail.db"), nil)
}

// NewAt opens a database at a given path with an optional clock, so a test can
// close it, reopen the same file and assert on what survived.
//
// Where no file is there yet, it starts from a copy of the migrated template
// (template, below) instead of migrating; a file that is there is opened as
// it is, and migrated as store.Open migrates any.
func NewAt(t *testing.T, path string, now func() time.Time) *store.Store {
	t.Helper()
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		if err := copyTemplate(path); err != nil {
			t.Fatalf("storetest: %v", err)
		}
	}
	s, err := store.Open(context.Background(), path, store.Options{Now: now})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return s
}

// template is a database every migration has run on, made once per test
// binary and kept in memory: New and NewAt write a copy of it where they open
// one, rather than run every migration again for every test, which was most
// of what a test that opens a database spent (the FTS5 tables, triggers and
// rebuilt tables of every migration, many times over under -race).
var template struct {
	once  sync.Once
	bytes []byte
	err   error
}

// migrated is the template's bytes: store.Open's own migrations on a fresh
// file, closed, so that the WAL is checkpointed into the file and removed and
// the file alone is the whole database.
func migrated() ([]byte, error) {
	template.once.Do(func() {
		template.bytes, template.err = migrateTemplate()
	})
	return template.bytes, template.err
}

func migrateTemplate() ([]byte, error) {
	dir, err := os.MkdirTemp("", "storetest-template-")
	if err != nil {
		return nil, fmt.Errorf("the template's directory: %w", err)
	}
	//nolint:errcheck // a temporary directory; the bytes are read by then
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "mail.db")
	s, err := store.Open(context.Background(), path, store.Options{})
	if err != nil {
		return nil, fmt.Errorf("migrating the template: %w", err)
	}
	if err := s.Close(); err != nil {
		return nil, fmt.Errorf("closing the template: %w", err)
	}
	if info, err := os.Stat(path + "-wal"); err == nil && info.Size() > 0 {
		return nil, errors.New("the template's WAL outlived its last connection; the file alone is not the database")
	}
	return os.ReadFile(path) //nolint:gosec // the file this function just made
}

// copyTemplate writes the template at path, as store.Open would create the
// file: its directory 0700 and the file 0600.
func copyTemplate(path string) error {
	b, err := migrated()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("the database's directory: %w", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("copying the template: %w", err)
	}
	return nil
}

// IndexMailbox stores what the sync engine would for everything mb holds
// now: its folder list, and the metadata of every message in the folders it
// syncs, announcing nothing. It opens one sync connection and logs it out.
//
// It is the engine without its schedule, windows or events, for tests of
// what reads the index; the account must be one that may sync (consented,
// or switched on by the operator), as the engine's own writes require.
func IndexMailbox(t *testing.T, db *store.Store, accountID string, mb provider.Mailbox) []store.Folder {
	t.Helper()
	ctx := context.Background()
	sess, err := mb.Open(ctx, provider.RoleSync)
	if err != nil {
		t.Fatalf("IndexMailbox: open: %v", err)
	}
	//nolint:errcheck // logging out a connection this is done with
	defer func() { _ = sess.Close() }()
	listed, err := sess.ListFolders(ctx, false)
	if err != nil {
		t.Fatalf("IndexMailbox: list: %v", err)
	}
	var discovered store.DiscoveryResult
	if err := db.Write(ctx, func(tx *sql.Tx) error {
		var err error
		discovered, err = db.SyncFolders(ctx, tx, store.FolderDiscovery{
			AccountID: accountID, Folders: listed, Profile: mb.Profile().ForServer(sess.Caps(), ""),
		})
		return err
	}); err != nil {
		t.Fatalf("IndexMailbox: folders: %v", err)
	}
	for _, f := range discovered.Folders {
		if !f.Synced || !f.Selectable {
			continue
		}
		st, err := sess.Select(ctx, f.Name, true, 0)
		if err != nil {
			t.Fatalf("IndexMailbox: select: %v", err)
		}
		var sums []provider.Summary
		all := imap.UIDSet{imap.UIDRange{Start: 1, Stop: 0}}
		mark := db.ActionMark()
		if st.NumMessages > 0 {
			if err := sess.FetchSummaries(ctx, all, 0, func(s provider.Summary) error {
				sums = append(sums, s)
				return nil
			}); err != nil {
				t.Fatalf("IndexMailbox: fetch: %v", err)
			}
		}
		uidvalidity, state := st.UIDValidity, store.FolderStateLive
		if err := db.Write(ctx, func(tx *sql.Tx) error {
			if err := store.UpdateFolderSync(ctx, tx, f.ID, store.FolderSync{
				UIDValidity: &uidvalidity, SyncState: &state,
			}); err != nil {
				return err
			}
			if len(sums) == 0 {
				return nil
			}
			_, err := db.ApplySummaries(ctx, tx, store.SummaryBatch{
				AccountID: accountID, FolderID: f.ID, UIDValidity: uidvalidity, Mode: store.ApplyQuiet, Summaries: sums,
				ActionMark: mark,
			})
			return err
		}); err != nil {
			t.Fatalf("IndexMailbox: store: %v", err)
		}
	}
	folders, err := db.Folders(ctx, accountID)
	if err != nil {
		t.Fatalf("IndexMailbox: %v", err)
	}
	return folders
}
