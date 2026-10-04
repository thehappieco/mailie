package sync_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/store"
	syncengine "github.com/thehappieco/mailie/internal/sync"
)

func TestAFolderGoneFromTheServerIsDroppedOnlyAfterTwoListings(t *testing.T) {
	// A LIST that raced a rename must not throw a folder's index away: the
	// first absence only marks it missing, the second consecutive one deletes
	// it, and its messages are announced as deleted.
	h := newHarness(t, setup{caps: providertest.GmailCaps()})
	h.box.CreateFolder("Receipts")
	for i := range 2 {
		h.box.Deliver("Receipts", mail(fmt.Sprintf("r%d", i), time.Now().Add(-time.Hour)))
	}
	sub := h.live()
	receipts := h.folderID("Receipts")

	h.box.DeleteFolder("Receipts")
	if err := h.engine.Trigger(context.Background(), h.account); err != nil {
		t.Fatal(err)
	}
	h.waitFor("the folder to be marked missing", 3*time.Second, func() bool {
		return h.count(`SELECT count(*) FROM folders WHERE id = ? AND missing_since <> 0`, receipts) == 1
	})
	if n := h.count(`SELECT count(*) FROM messages WHERE folder_id = ?`, receipts); n != 2 {
		t.Errorf("%d rows kept after one absence, want both", n)
	}
	if evs := h.journaled(events.TypeMessageDeleted); len(evs) != 0 {
		t.Errorf("%d deletions announced after one absence", len(evs))
	}

	if err := h.engine.Trigger(context.Background(), h.account); err != nil {
		t.Fatal(err)
	}
	for {
		ev := next(t, sub, events.TypeFolderChanged, 3*time.Second)
		if p := payload[store.FolderChanged](t, ev); p.FolderID == receipts && p.Change == store.FolderRemoved {
			break
		}
	}
	if n := h.count(`SELECT count(*) FROM folders WHERE id = ?`, receipts) +
		h.count(`SELECT count(*) FROM messages WHERE folder_id = ?`, receipts); n != 0 {
		t.Errorf("%d folder and message rows left after the second absence", n)
	}
	if evs := h.journaled(events.TypeMessageDeleted); len(evs) != 2 {
		t.Errorf("%d message.deleted for the folder's two messages", len(evs))
	}
}

func TestAFolderThatStopsBeingSyncedLosesItsRows(t *testing.T) {
	// What is stored is the synced folders' metadata. A folder an override
	// now rules out keeps its listing row and loses what was indexed from it,
	// announced message by message: a copy removed where the message is
	// still in another folder, a deletion where it was the only copy.
	h := newHarness(t, setup{caps: providertest.GmailCaps(), sharedFlags: true})
	h.box.CreateFolder("Receipts")
	both := h.box.Deliver("INBOX", mail("both", time.Now().Add(-time.Hour)))
	h.box.CopyTo("INBOX", both, "Receipts")
	h.box.Deliver("Receipts", mail("only", time.Now().Add(-time.Hour)))
	h.live()
	receipts := h.folderID("Receipts")
	inboxRow := h.rowID("INBOX", both)

	h.exec(`UPDATE accounts SET folder_overrides = '{"important":"Receipts"}' WHERE id = ?`, h.account)
	if err := h.engine.Trigger(context.Background(), h.account); err != nil {
		t.Fatal(err)
	}
	h.waitFor("the folder's rows to go", 3*time.Second, func() bool {
		return h.count(`SELECT count(*) FROM messages WHERE folder_id = ?`, receipts) == 0
	})
	if n := h.count(`SELECT count(*) FROM folders WHERE id = ? AND synced = 0 AND uidvalidity = 0
		AND max_seen_uid = 0 AND sync_state = 'new'`, receipts); n != 1 {
		t.Error("the folder's row is gone, still synced, or keeps its old sync position")
	}
	deleted := h.journaled(events.TypeMessageDeleted)
	if len(deleted) != 1 {
		t.Fatalf("%d message.deleted, want the one only the folder held", len(deleted))
	}
	var removed []store.MessageMoved
	for _, ev := range h.journaled(events.TypeMessageMoved) {
		if p := payload[store.MessageMoved](t, ev); !p.NewCopy {
			removed = append(removed, p)
		}
	}
	if len(removed) != 1 || removed[0].To != nil || removed[0].PrimaryID != inboxRow {
		t.Errorf("copies removed: %s, want the label copy with the inbox row %d standing", describe(removed), inboxRow)
	}
	if n := h.count(`SELECT count(*) FROM messages WHERE id = ? AND vanished_at = 0 AND dup_of IS NULL`, inboxRow); n != 1 {
		t.Error("the inbox copy did not survive as the primary")
	}

	h.box.ResetCalls()
	if err := h.engine.Trigger(context.Background(), h.account); err != nil {
		t.Fatal(err)
	}
	h.waitFor("the triggered round", 3*time.Second, func() bool {
		return h.box.CallCount(providertest.MethodSelect) > 0
	})
	time.Sleep(50 * time.Millisecond)
	for _, c := range h.box.Calls() {
		if c.Folder == "Receipts" {
			t.Errorf("%s on a folder that is no longer synced", c.Method)
		}
	}
}

func TestDeletingThePrimaryCopyPromotesTheLabelCopy(t *testing.T) {
	// Archiving in Gmail takes the message out of the inbox and leaves it
	// under its labels. The label's row becomes the primary, and the inbox
	// row going away is a copy removed, not a deletion.
	h := newHarness(t, setup{caps: providertest.GmailCaps(), sharedFlags: true,
		opts: syncengine.Options{ConfirmDiffDelay: 50 * time.Millisecond}})
	h.box.CreateFolder("Receipts")
	uid := h.box.Deliver("INBOX", mail("invoice", time.Now().Add(-time.Hour)))
	copied := h.box.CopyTo("INBOX", uid, "Receipts")
	sub := h.live()
	primary := h.rowID("INBOX", uid)
	label := h.rowID("Receipts", copied)
	if n := h.count(`SELECT count(*) FROM messages WHERE id = ? AND dup_of = ?`, label, primary); n != 1 {
		t.Fatalf("the label row %d does not point at the inbox row %d", label, primary)
	}

	h.box.Expunge("INBOX", uid)
	got := payload[store.MessageMoved](t, next(t, sub, events.TypeMessageMoved, 3*time.Second))
	if got.NewCopy || got.To != nil || got.MessageID != primary || got.PrimaryID != label {
		t.Errorf("message.moved = %s, want row %d gone and row %d the primary", describe(got), primary, label)
	}
	if n := h.count(`SELECT count(*) FROM messages WHERE id = ? AND dup_of IS NULL AND vanished_at = 0`, label); n != 1 {
		t.Error("the label copy was not promoted")
	}
	if evs := h.journaled(events.TypeMessageDeleted); len(evs) != 0 {
		t.Errorf("archiving announced %d deletions", len(evs))
	}
	if n := h.messages(); n != 1 {
		t.Errorf("%d live rows, want the label copy", n)
	}
}
