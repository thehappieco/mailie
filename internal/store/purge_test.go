package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
)

// onDisk reports each needle still present, live or not, in the database file
// or its write-ahead log.
func (f *indexFixture) onDisk(needles ...string) []string {
	f.t.Helper()
	var found []string
	for _, file := range []string{f.db.Path(), f.db.Path() + "-wal"} {
		raw, err := os.ReadFile(file)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			f.t.Fatal(err)
		}
		for _, needle := range needles {
			if n := bytes.Count(raw, []byte(needle)); n > 0 {
				found = append(found, fmt.Sprintf("%s holds %q %d times", filepath.Base(file), needle, n))
			}
		}
	}
	return found
}

func TestPurgingTheIndexLeavesNoTraceInTheFiles(t *testing.T) {
	// Turning sync off deletes what was stored: from the tables, from the
	// full-text index's segments, and from the files on disk.
	f := newIndex(t)
	ctx := context.Background()
	ids := f.gmailFolders()

	var sums []provider.Summary
	for i := range 40 {
		s := mail(imap.UID(i+1), fmt.Sprintf("k%02d", i))
		s.Envelope.Subject = fmt.Sprintf("Zanzibarquarterly%02d invoice", i)
		s.Envelope.From[0].Mailbox = "xylophonist"
		sums = append(sums, s)
	}
	f.live(ids["INBOX"], sums...)
	f.live(ids["Receipts"], sums[:10]...)
	f.flags(ids["INBOX"], 0, provider.FlagUpdate{UID: 1, Flags: []imap.Flag{imap.FlagSeen}})

	// The other account's index is not touched.
	f.addAccount("acc_2", "usr_1")
	other := f.account
	f.account = "acc_2"
	otherIDs := f.discover(provider.ProfileFor(provider.KindGmail), nil, folder("INBOX"))
	f.setUIDValidity(otherIDs.Folders[0].ID, 7)
	f.live(otherIDs.Folders[0].ID, mail(1, "keepme"))
	f.account = other

	needles := []string{"zanzibarquarterly", "Zanzibarquarterly", "xylophonist"}
	if found := f.onDisk(needles...); len(found) == 0 {
		t.Fatal("the needles are not on disk before the purge; the test would prove nothing")
	}

	err := f.db.Write(ctx, func(tx *sql.Tx) error { return store.ForgetIndexTx(ctx, tx, []string{f.account}) })
	if err != nil {
		t.Fatalf("ForgetIndexTx: %v", err)
	}
	if err := f.db.CompactFullText(ctx); err != nil {
		t.Fatalf("CompactFullText: %v", err)
	}
	if err := f.db.Scrub(ctx); err != nil {
		t.Fatalf("Scrub: %v", err)
	}

	for table, query := range map[string]string{
		"messages": `SELECT count(*) FROM messages WHERE account_id = ?`,
		"folders":  `SELECT count(*) FROM folders WHERE account_id = ?`,
		"events":   `SELECT count(*) FROM events WHERE account_id = ?`,
	} {
		if n := f.int(query, f.account); n != 0 {
			t.Errorf("%d %s rows survived the purge", n, table)
		}
	}
	if n := f.int(`SELECT count(*) FROM parts`); n != 1 {
		t.Errorf("%d parts left, want only the other account's one", n)
	}
	if n := f.int(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'zanzibarquarterly00'`); n != 0 {
		t.Error("the full-text index still finds a purged subject")
	}
	if n := f.int(`SELECT count(*) FROM messages WHERE account_id = 'acc_2'`); n != 1 {
		t.Error("the other account's index was purged too")
	}
	if n := f.int(`SELECT count(*) FROM accounts WHERE id = ?`, f.account); n != 1 {
		t.Error("the purge deleted the account itself")
	}
	if found := f.onDisk(needles...); len(found) > 0 {
		t.Fatalf("purged data is still on disk: %v", found)
	}
}

func TestTheFullTextIndexForgetsAPurgedSubjectAndAddressWithoutACompaction(t *testing.T) {
	// secure-delete on the full-text index: a deletion takes the terms out of
	// the index's segments at once. Without it they stay in an older segment
	// — a page SQLite considers live, so neither secure_delete nor the WAL
	// checkpoint reaches it — until some later merge, and turning sync off
	// would leave the subjects and addresses of other people's mail in the
	// file. Measured: eight copies of each survive without it. Two accounts
	// are interleaved so the segments hold terms that must stay beside the
	// ones that must go, which is the case a whole-table delete hides.
	f := newIndex(t)
	ctx := context.Background()
	mine := f.gmailFolders()["INBOX"]
	f.addAccount("acc_2", "usr_1")
	me := f.account
	f.account = "acc_2"
	theirs := f.discover(provider.ProfileFor(provider.KindGmail), nil, folder("INBOX")).Folders[0].ID
	f.setUIDValidity(theirs, 7)
	f.account = me

	for i := range 60 {
		f.account = "acc_2"
		keep := mail(imap.UID(i+1), fmt.Sprintf("o%02d", i))
		keep.Envelope.Subject = fmt.Sprintf("Marmalade%02d", i)
		f.apply(store.SummaryBatch{FolderID: theirs, Mode: store.ApplyQuiet, Summaries: []provider.Summary{keep}})
		f.account = me
		gone := mail(imap.UID(i+1), fmt.Sprintf("k%02d", i))
		gone.Envelope.Subject = fmt.Sprintf("Zanzibarquarterly%02d statement", i)
		gone.Envelope.From[0].Mailbox, gone.Envelope.From[0].Host = "xylophonist", "quietmail.example"
		gone.Envelope.To[0].Mailbox = "okapiwrangler"
		f.apply(store.SummaryBatch{FolderID: mine, Mode: store.ApplyQuiet, Summaries: []provider.Summary{gone}})
	}
	needles := []string{
		"zanzibarquarterly", "Zanzibarquarterly", // a subject word, as indexed and as written
		"xylophonist@quietmail.example", "xylophonist", // an address, whole and as the tokenizer splits it
		"okapiwrangler", // a recipient
	}
	if found := f.onDisk(needles...); len(found) < len(needles) {
		t.Fatalf("only %v on disk before the purge; the test would prove nothing", found)
	}
	if n := f.int(`SELECT v FROM messages_fts_config WHERE k = 'secure-delete'`); n != 1 {
		t.Fatalf("secure-delete is %d on the full-text index, want 1", n)
	}

	err := f.db.Write(ctx, func(tx *sql.Tx) error { return store.ForgetIndexTx(ctx, tx, []string{me}) })
	if err != nil {
		t.Fatalf("ForgetIndexTx: %v", err)
	}
	// No CompactFullText: the deletion alone has to be enough.
	if err := f.db.Scrub(ctx); err != nil {
		t.Fatalf("Scrub: %v", err)
	}

	if found := f.onDisk(needles...); len(found) > 0 {
		t.Fatalf("purged, but still in the files: %v", found)
	}
	if n := f.int(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'marmalade07'`); n != 1 {
		t.Errorf("the other account's subject is found %d times, want 1", n)
	}
	if n := f.int(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'xylophonist'`); n != 0 {
		t.Errorf("the purged address is still found %d times", n)
	}
	var integrity sql.NullString
	if err := f.db.Writer().QueryRowContext(ctx,
		`INSERT INTO messages_fts(messages_fts, rank) VALUES ('integrity-check', 1)`).Scan(&integrity); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("the full-text index fails its integrity check: %v", err)
	}
}
