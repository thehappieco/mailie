package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
)

func TestAnIndexBatchPreparesEachOfItsWritesOnceWhateverItsSize(t *testing.T) {
	// SQLite compiles every trigger a statement fires each time it prepares
	// the statement, the usage totals' whole-row expressions among them
	// (migration 0030): a batch that prepared its UPDATE or DELETE again for
	// every row held the single writer for seconds where it took a tenth of
	// that, a mass flag change in another client or a mass delete. Every
	// batch below changes several rows the same way, so a statement prepared
	// per row is one prepared more than once in its transaction.
	f := newIndex(t)
	prepares := store.WatchWritePrepares(t, f.db)
	step := func(what string) {
		t.Helper()
		if repeated := prepares.Repeated(); len(repeated) != 0 {
			t.Fatalf("%s prepared, in one transaction, %v", what, repeated)
		}
	}
	gmail := provider.ProfileFor(provider.KindGmail)
	listed := []provider.Folder{
		folder("INBOX"), folder("[Gmail]/Sent Mail", imap.MailboxAttrSent), folder("[Gmail]/Trash", imap.MailboxAttrTrash),
		folder("[Gmail]/All Mail", imap.MailboxAttrAll), folder("Receipts"),
	}
	ids := f.gmailFolders()
	inbox, sent, all, receipts := ids["INBOX"], ids["[Gmail]/Sent Mail"], ids["[Gmail]/All Mail"], ids["Receipts"]
	step("folders listed")

	uids := []imap.UID{1, 2, 3, 4, 5, 6}
	batch := func(flags ...imap.Flag) []provider.Summary {
		var sums []provider.Summary
		for _, uid := range uids {
			sums = append(sums, partsOf(mail(uid, fmt.Sprintf("m%d", uid), flags...)))
		}
		return sums
	}
	updates := func(flags ...imap.Flag) []provider.FlagUpdate {
		var ups []provider.FlagUpdate
		for _, uid := range uids {
			ups = append(ups, provider.FlagUpdate{UID: uid, Flags: flags})
		}
		return ups
	}

	f.live(inbox, batch()...)
	step("new mail with its parts")
	f.live(all, batch()...)
	if n := f.int(`SELECT count(*) FROM messages WHERE dup_of IS NOT NULL`); n != int64(len(uids)) {
		t.Fatalf("%d copies point at a primary, want %d", n, len(uids))
	}
	step("copies pointed at their primaries")
	f.live(inbox, batch(imap.FlagSeen)...)
	step("a pass finding new flags on every row")
	f.flags(inbox, 0, updates(imap.FlagSeen, imap.FlagFlagged)...)
	step("a flag pass changing every row")
	f.actionFlags(store.ActionFlags{FolderID: inbox, Shared: true, Updates: updates(imap.FlagAnswered)})
	step("an action's flags on every row and its copies")

	f.diff(inbox, 1)
	if n := f.int(`SELECT count(*) FROM messages WHERE folder_id = ? AND vanished_at <> 0`, inbox); n != int64(len(uids)) {
		t.Fatalf("%d rows tombstoned, want %d", n, len(uids))
	}
	step("a diff tombstoning every row, their copies made primaries")
	f.diff(inbox, 1, uids...)
	step("a diff reviving every row")
	f.diff(inbox, 1)
	f.diff(inbox, 1)
	if n := f.int(`SELECT count(*) FROM messages WHERE folder_id = ?`, inbox); n != 0 {
		t.Fatalf("%d rows left in the folder after two misses", n)
	}
	step("a diff deleting every row")

	// Moves: half of All Mail's rows into a synced folder, the other half
	// into one not synced, out of the index.
	var placed, out []store.MovedRow
	for i, uid := range uids {
		row := store.MovedRow{ID: f.msgID(all, uid), UID: uid, DestUID: 100 + uid}
		if i%2 == 0 {
			placed = append(placed, row)
		} else {
			out = append(out, row)
		}
	}
	if res := f.move(store.ActionMove{FromFolderID: all, ToFolderID: receipts, DestUIDValidity: 7, Moves: placed}); len(res.Kept) != len(placed) {
		t.Fatalf("the move into a synced folder: %+v", res)
	}
	step("a move of several rows")
	if res := f.move(store.ActionMove{FromFolderID: all, ToFolderID: sent, DestUIDValidity: 41, Moves: out}); len(res.Removed) != len(out) {
		t.Fatalf("the move out of the index: %+v", res)
	}
	step("a move of several rows out of the index")

	// A UIDVALIDITY change: the rows a resync claims are refreshed, and the
	// ones nothing claimed deleted at its end.
	f.live(sent, batch()...)
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := f.db.BeginResync(ctx, tx, f.account, sent, provider.FolderStatus{UIDValidity: 8, UIDNext: 100},
			store.ResyncFromLive)
		return err
	})
	step("a resync begun")
	var claimed []provider.Summary
	for _, sum := range batch()[:3] {
		sum.UID += 50
		claimed = append(claimed, sum)
	}
	f.apply(store.SummaryBatch{FolderID: sent, UIDValidity: 8, Mode: store.ApplyResync, Summaries: claimed})
	step("a resync batch claiming several rows")
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := f.db.FinishResync(ctx, tx, f.account, sent, f.now)
		return err
	})
	step("a resync finished deleting several rows")

	// A listing where several folders stop being selectable, and then are
	// gone twice: their rows go, folder by folder.
	extra := []provider.Folder{folder("Projects"), folder("Travel")}
	f.discover(gmail, nil, append(slices.Clone(listed), extra...)...)
	step("folders added")
	f.discover(gmail, map[string]string{"sent": "Projects", "trash": "Travel"}, append(slices.Clone(listed), extra...)...)
	if n := f.int(`SELECT count(*) FROM folders WHERE account_id = ? AND name IN ('Projects', 'Travel') AND role <> ''`,
		f.account); n != 2 {
		t.Fatalf("%d of the folders took the roles moved to them, want 2", n)
	}
	step("roles moved off several folders")
	for _, name := range []string{"Projects", "Travel"} {
		id := f.int(`SELECT id FROM folders WHERE account_id = ? AND name = ?`, f.account, name)
		f.setUIDValidity(id, 7)
		f.live(id, batch()...)
	}
	var unselectable []provider.Folder
	for _, fo := range extra {
		unselectable = append(unselectable, provider.Folder{Name: fo.Name, Delim: '/', Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}})
	}
	if res := f.discover(gmail, nil, append(slices.Clone(listed), unselectable...)...); len(res.Unsynced) != len(extra) {
		t.Fatalf("the folders no longer selectable: %+v", res)
	}
	step("folders no longer synced")
	f.discover(gmail, nil, listed...)
	if res := f.discover(gmail, nil, listed...); len(res.Removed) != len(extra) {
		t.Fatalf("the folders gone twice: %+v", res)
	}
	step("folders gone from the list twice")

	if prepares.Total() == 0 {
		t.Fatal("the watched writer prepared nothing: the store wrote through another")
	}
	totalsHold(t, f.db, "the batches")
}
