package store_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
)

// A paused mailbox's index, as the engine and an action see it (migration
// 0025): the engine stores nothing more, an action's result still is.

func (f *indexFixture) pause() {
	f.t.Helper()
	switched, err := f.db.PauseSync(context.Background(), f.account, "usr_1", nil)
	if err != nil || !switched.Changed {
		f.t.Fatalf("pausing: %+v (%v)", switched, err)
	}
}

func TestAPausedMailboxIsNotSyncedButItsIndexStillFollowsActions(t *testing.T) {
	f := newIndex(t)
	ctx := context.Background()
	f.pause()
	if ok, err := f.db.SyncEligible(ctx, f.account); err != nil || ok {
		t.Errorf("a paused mailbox is eligible to sync: %v (%v)", ok, err)
	}
	if all, err := f.db.SyncEligibleAccounts(ctx); err != nil || slices.Contains(all, f.account) {
		t.Errorf("the accounts eligible to sync are %v (%v)", all, err)
	}
	if ok, err := f.db.IndexFollows(ctx, f.account); err != nil || !ok {
		t.Errorf("a paused mailbox's index does not follow actions: %v (%v)", ok, err)
	}
	if paused, err := f.db.PausedAccounts(ctx); err != nil || !slices.Equal(paused, []string{f.account}) {
		t.Errorf("the paused accounts are %v (%v)", paused, err)
	}
	engine := f.db.Write(ctx, func(tx *sql.Tx) error { return f.db.RequireSyncEligibleTx(ctx, tx, f.account) })
	if !errors.Is(engine, store.ErrNotEligible) {
		t.Errorf("the engine's rule in a transaction: %v, want ErrNotEligible", engine)
	}
	if err := f.db.Write(ctx, func(tx *sql.Tx) error { return f.db.RequireIndexFollowsTx(ctx, tx, f.account) }); err != nil {
		t.Errorf("the index rule in a transaction: %v", err)
	}
}

func TestASyncBatchInFlightWhenThePauseCommitsStoresNothing(t *testing.T) {
	f := newIndex(t)
	ctx := context.Background()
	inbox := f.gmailFolders()["INBOX"]
	f.live(inbox, mail(5, "a"))
	index := func() []int64 {
		return []int64{
			f.int(`SELECT count(*) FROM messages WHERE account_id = ?`, f.account),
			f.int(`SELECT count(*) FROM folders WHERE account_id = ?`, f.account),
			f.int(`SELECT max(max_seen_uid) FROM folders WHERE account_id = ?`, f.account),
			f.int(`SELECT seen FROM messages WHERE account_id = ?`, f.account),
			f.journaled(),
		}
	}
	before := index()
	f.pause()
	for what, batch := range map[string]func(tx *sql.Tx) error{
		"summaries": func(tx *sql.Tx) error {
			_, err := f.db.ApplySummaries(ctx, tx, store.SummaryBatch{AccountID: f.account, FolderID: inbox,
				UIDValidity: 7, Mode: store.ApplyLive, Summaries: []provider.Summary{mail(6, "b")}, Now: f.now})
			return err
		},
		"flags": func(tx *sql.Tx) error {
			_, err := f.db.ApplyFlags(ctx, tx, store.FlagBatch{AccountID: f.account, FolderID: inbox, UIDValidity: 7,
				Updates: []provider.FlagUpdate{{UID: 5, Flags: []imap.Flag{imap.FlagSeen}}}, Now: f.now})
			return err
		},
		"folders": func(tx *sql.Tx) error {
			_, err := f.db.SyncFolders(ctx, tx, store.FolderDiscovery{AccountID: f.account,
				Folders: []provider.Folder{folder("INBOX"), folder("New")}, Profile: provider.ProfileFor(provider.KindGmail),
				Now: f.now})
			return err
		},
	} {
		if err := f.db.Write(ctx, batch); !errors.Is(err, store.ErrNotEligible) {
			t.Errorf("a batch of %s after the pause: %v, want ErrNotEligible", what, err)
		}
	}
	if after := index(); !slices.Equal(after, before) {
		t.Errorf("the index was %v and is %v", before, after)
	}
}

func TestAnActionOnAPausedMailboxIsRecordedInItsIndex(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox, receipts := ids["INBOX"], ids["Receipts"]
	f.live(inbox, mail(5, "a"), mail(6, "b"))
	f.live(receipts, mail(3, "old"))
	f.pause()
	evs := f.actionFlags(store.ActionFlags{
		FolderID: inbox, Updates: []provider.FlagUpdate{{UID: 5, Flags: []imap.Flag{imap.FlagSeen}}},
	})
	wantTypes(t, "marking read while paused", evs, "message.flags")
	if seen := f.int(`SELECT seen FROM messages WHERE folder_id = ? AND uid = 5`, inbox); seen != 1 {
		t.Errorf("the message marked read while paused is seen %d", seen)
	}
	id := f.msgID(inbox, 6)
	res := f.move(store.ActionMove{
		FromFolderID: inbox, ToFolderID: receipts, DestUIDValidity: 7,
		Moves: []store.MovedRow{{ID: id, UID: 6, DestUID: 4}},
	})
	if len(res.Kept) != 1 || f.msgID(receipts, 4) != id {
		t.Errorf("the move while paused: %+v, the row at Receipts 4 is %d, want %d", res, f.msgID(receipts, 4), id)
	}
}
