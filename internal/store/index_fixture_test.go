package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

// indexFixture is a database with one consenting person and their account,
// ready for the index to write into.
type indexFixture struct {
	t       *testing.T
	db      *store.Store
	account string
	now     time.Time
}

var t0 = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func newIndex(t *testing.T) *indexFixture {
	t.Helper()
	f := &indexFixture{t: t, db: storetest.New(t), account: "acc_1", now: t0}
	f.exec(`INSERT INTO users(id, email, password_hash, role, password_changed_at, created_at, updated_at, sync_consent_at, sync_consent_version)
		VALUES ('usr_1', 'person@example.com', '$argon2id$', 'owner', 1, 1, 1, 1, 'test')`)
	f.addAccount(f.account, "usr_1")
	return f
}

func (f *indexFixture) addAccount(id, owner string) {
	f.t.Helper()
	var ownerArg any
	if owner != "" {
		ownerArg = owner
	}
	f.exec(`INSERT INTO accounts(id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port,
		smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at, owner_user_id)
		VALUES (?, ?, 'gmail', 'oauth2', 'imap.gmail.com', 993, 'smtp.gmail.com', 465, 'implicit', ?, 0, 'active', 1, 1, 1, ?)`,
		id, id+"@example.com", id+"@example.com", ownerArg)
}

func (f *indexFixture) exec(query string, args ...any) {
	f.t.Helper()
	err := f.db.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), query, args...)
		return err
	})
	if err != nil {
		f.t.Fatalf("exec %q: %v", query, err)
	}
}

func (f *indexFixture) int(query string, args ...any) int64 {
	f.t.Helper()
	var n sql.NullInt64
	if err := f.db.Reader().QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		f.t.Fatalf("query %q: %v", query, err)
	}
	return n.Int64
}

func (f *indexFixture) str(query string, args ...any) string {
	f.t.Helper()
	var s sql.NullString
	if err := f.db.Reader().QueryRowContext(context.Background(), query, args...).Scan(&s); err != nil {
		f.t.Fatalf("query %q: %v", query, err)
	}
	return s.String
}

// write runs fn in a writer transaction and fails the test on error.
func (f *indexFixture) write(fn func(ctx context.Context, tx *sql.Tx) error) {
	f.t.Helper()
	ctx := context.Background()
	if err := f.db.Write(ctx, func(tx *sql.Tx) error { return fn(ctx, tx) }); err != nil {
		f.t.Fatal(err)
	}
}

// discover records a LIST for the account with the Gmail profile unless
// another is given.
func (f *indexFixture) discover(profile provider.Profile, overrides map[string]string, folders ...provider.Folder) store.DiscoveryResult {
	f.t.Helper()
	var res store.DiscoveryResult
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		var err error
		res, err = f.db.SyncFolders(ctx, tx, store.FolderDiscovery{
			AccountID: f.account, Folders: folders, Profile: profile, Overrides: overrides, Now: f.now,
		})
		return err
	})
	return res
}

// folder makes a LIST row.
func folder(name string, attrs ...imap.MailboxAttr) provider.Folder {
	return provider.Folder{Name: name, Delim: '/', Attrs: attrs, Selectable: true}
}

// gmailFolders sets up INBOX, Sent, Trash, All Mail and one label, and gives
// each synced folder UIDVALIDITY 7. It returns folder ids by name.
func (f *indexFixture) gmailFolders() map[string]int64 {
	f.t.Helper()
	res := f.discover(provider.ProfileFor(provider.KindGmail), nil,
		folder("INBOX"),
		folder("[Gmail]/Sent Mail", imap.MailboxAttrSent),
		folder("[Gmail]/Trash", imap.MailboxAttrTrash),
		folder("[Gmail]/All Mail", imap.MailboxAttrAll),
		folder("Receipts"),
	)
	ids := map[string]int64{}
	for _, fo := range res.Folders {
		ids[fo.Name] = fo.ID
		f.setUIDValidity(fo.ID, 7)
	}
	return ids
}

func (f *indexFixture) setUIDValidity(folderID int64, v uint32) {
	f.t.Helper()
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return store.UpdateFolderSync(ctx, tx, folderID, store.FolderSync{UIDValidity: &v})
	})
}

func (f *indexFixture) apply(b store.SummaryBatch) store.ApplyResult {
	f.t.Helper()
	if b.AccountID == "" {
		b.AccountID = f.account
	}
	if b.UIDValidity == 0 {
		b.UIDValidity = 7
	}
	if b.Now.IsZero() {
		b.Now = f.now
	}
	var res store.ApplyResult
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		var err error
		res, err = f.db.ApplySummaries(ctx, tx, b)
		return err
	})
	return res
}

func (f *indexFixture) live(folderID int64, sums ...provider.Summary) store.ApplyResult {
	f.t.Helper()
	return f.apply(store.SummaryBatch{FolderID: folderID, Mode: store.ApplyLive, Summaries: sums})
}

func (f *indexFixture) diff(folderID int64, floor imap.UID, server ...imap.UID) store.DiffResult {
	f.t.Helper()
	var res store.DiffResult
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		var err error
		res, err = f.db.ApplyUIDDiff(ctx, tx, store.UIDDiff{
			AccountID: f.account, FolderID: folderID, UIDValidity: 7, Floor: floor, ServerUIDs: server, Now: f.now,
		})
		return err
	})
	return res
}

func (f *indexFixture) flags(folderID int64, modseq uint64, updates ...provider.FlagUpdate) store.FlagResult {
	f.t.Helper()
	var res store.FlagResult
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		var err error
		res, err = f.db.ApplyFlags(ctx, tx, store.FlagBatch{
			AccountID: f.account, FolderID: folderID, UIDValidity: 7, Updates: updates, HighestModSeq: modseq, Now: f.now,
		})
		return err
	})
	return res
}

// msgID returns the local id of the row at (folder, uid), or 0.
func (f *indexFixture) msgID(folderID int64, uid imap.UID) int64 {
	f.t.Helper()
	return f.int(`SELECT id FROM messages WHERE folder_id = ? AND uid = ?`, folderID, uint32(uid))
}

// mail builds a summary. key makes the Message-ID, subject and sender unique.
func mail(uid imap.UID, key string, flags ...imap.Flag) provider.Summary {
	return provider.Summary{
		UID:   uid,
		Flags: flags,
		Envelope: &imap.Envelope{
			Subject:   "Subject " + key,
			From:      []imap.Address{{Name: "Sender " + key, Mailbox: "sender-" + key, Host: "example.com"}},
			To:        []imap.Address{{Name: "Person", Mailbox: "person", Host: "example.com"}},
			MessageID: key + "@mail.example.com",
			Date:      t0.Add(-time.Hour),
		},
		InternalDate: t0.Add(-time.Hour),
		Size:         1234,
		Parts: []provider.PartInfo{
			{Path: []int{1}, MIMEType: "text/plain", Params: map[string]string{"charset": "utf-8"}, Encoding: "7bit", Size: 100, IsBody: true},
		},
	}
}

// types lists the event types, in order.
func types(evs []events.Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, string(ev.Type))
	}
	return out
}

func wantTypes(t *testing.T, what string, evs []events.Event, want ...string) {
	t.Helper()
	got := types(evs)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s announced %v, want %v", what, got, want)
	}
	for _, ev := range evs {
		if ev.Seq == 0 {
			t.Errorf("%s returned an event that was not journaled: %+v", what, ev)
		}
		if ev.AccountID == "" {
			t.Errorf("%s returned an event without an account", what)
		}
	}
}

func payload[T any](t *testing.T, ev events.Event) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(ev.Payload, &out); err != nil {
		t.Fatalf("decode %s payload %s: %v", ev.Type, ev.Payload, err)
	}
	return out
}

func (f *indexFixture) journaled() int64 {
	return f.int(`SELECT count(*) FROM events WHERE account_id = ?`, f.account)
}

func describe(v any) string { return fmt.Sprintf("%+v", v) }
