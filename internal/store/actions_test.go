package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
)

func (f *indexFixture) move(b store.ActionMove) store.ActionMoveResult {
	f.t.Helper()
	b.AccountID = f.account
	if b.FromUIDValidity == 0 {
		b.FromUIDValidity = 7
	}
	var res store.ActionMoveResult
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		var err error
		res, err = f.db.ApplyActionMove(ctx, tx, b)
		return err
	})
	return res
}

func (f *indexFixture) actionFlags(b store.ActionFlags) []events.Event {
	f.t.Helper()
	b.AccountID = f.account
	b.UIDValidity = 7
	var evs []events.Event
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		var err error
		evs, err = f.db.ApplyActionFlags(ctx, tx, b)
		return err
	})
	return evs
}

func TestAMovedRowKeepsItsIdAndAnnouncesWhereItWent(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox, receipts := ids["INBOX"], ids["Receipts"]
	f.live(inbox, mail(5, "a"))
	f.live(receipts, mail(3, "old"))
	id := f.msgID(inbox, 5)

	res := f.move(store.ActionMove{
		FromFolderID: inbox, ToFolderID: receipts, DestUIDValidity: 7,
		Moves: []store.MovedRow{{ID: id, UID: 5, DestUID: 4}},
	})
	if len(res.Kept) != 1 || res.Kept[0] != id || len(res.Removed) != 0 {
		t.Fatalf("result = %+v, want the row kept", res)
	}
	if got := f.msgID(receipts, 4); got != id {
		t.Fatalf("the row at Receipts UID 4 is %d, want the moved row %d", got, id)
	}
	if n := f.int(`SELECT count(*) FROM messages WHERE folder_id = ?`, inbox); n != 0 {
		t.Errorf("%d rows left in the inbox", n)
	}
	if modseq := f.int(`SELECT modseq FROM messages WHERE id = ?`, id); modseq != 0 {
		t.Errorf("modseq = %d, want 0 for its new folder's pass to fill", modseq)
	}
	wantTypes(t, "the move", res.Events, "message.moved")
	moved := payload[store.ActionMoved](t, res.Events[0])
	if moved.MessageID != id || moved.FromFolderID != inbox || moved.ToFolderID == nil || *moved.ToFolderID != receipts ||
		moved.To == nil || *moved.To != receipts || moved.NewCopy {
		t.Errorf("payload = %s", describe(moved))
	}
	// The mark follows a UID that comes right after it, and only then.
	if mark := f.int(`SELECT max_seen_uid FROM folders WHERE id = ?`, receipts); mark != 4 {
		t.Errorf("Receipts' mark is %d, want 4", mark)
	}
	// Counts follow the row between folders.
	if n := f.int(`SELECT local_count FROM folders WHERE id = ?`, receipts); n != 2 {
		t.Errorf("Receipts counts %d, want 2", n)
	}
}

func TestTheMarkIsNotRaisedOverAGapThatMayBeNewMail(t *testing.T) {
	// UIDs 4 and 5 arrived in Receipts and no pass has fetched them yet; the
	// move landed at 6. Raising the mark to 6 would make 4 and 5 gaps, fetched
	// quietly, and new mail would never be announced.
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox, receipts := ids["INBOX"], ids["Receipts"]
	f.live(inbox, mail(5, "a"))
	f.live(receipts, mail(3, "old"))
	f.move(store.ActionMove{
		FromFolderID: inbox, ToFolderID: receipts, DestUIDValidity: 7,
		Moves: []store.MovedRow{{ID: f.msgID(inbox, 5), UID: 5, DestUID: 6}},
	})
	if mark := f.int(`SELECT max_seen_uid FROM folders WHERE id = ?`, receipts); mark != 3 {
		t.Fatalf("Receipts' mark is %d, want 3: the gap is new mail to fetch", mark)
	}
	// And the next pass fetching 4:* finds the moved row where it is.
	res := f.live(receipts, mail(4, "new"), mail(6, "a"))
	if res.Inserted != 1 || res.Updated != 1 {
		t.Fatalf("the pass inserted %d and updated %d, want the new mail inserted and the moved row found", res.Inserted, res.Updated)
	}
	wantTypes(t, "the pass", res.Events, "message.new")
}

func TestAMoveToAFolderThatIsNotSyncedTakesTheRowOutOfTheIndexAndRemembersIt(t *testing.T) {
	// Gmail's archive: All Mail is not synced. The row goes, the message is
	// not deleted, and its label copy becomes the one the listing shows.
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox, receipts, all := ids["INBOX"], ids["Receipts"], ids["[Gmail]/All Mail"]
	f.live(inbox, mail(5, "a"))
	f.live(receipts, mail(9, "a"))
	id, label := f.msgID(inbox, 5), f.msgID(receipts, 9)

	res := f.move(store.ActionMove{
		FromFolderID: inbox, ToFolderID: all, DestUIDValidity: 41,
		Moves: []store.MovedRow{{ID: id, UID: 5, DestUID: 700}},
	})
	if len(res.Removed) != 1 || res.Removed[0] != id {
		t.Fatalf("result = %+v, want the row removed", res)
	}
	wantTypes(t, "the archive", res.Events, "message.moved")
	moved := payload[store.ActionMoved](t, res.Events[0])
	if moved.To != nil || moved.ToFolderID != nil || moved.FolderID != inbox || moved.PrimaryID != label {
		t.Errorf("payload = %s, want to null from the inbox with the label copy primary", describe(moved))
	}
	if dup := f.int(`SELECT count(*) FROM messages WHERE id = ? AND dup_of IS NULL`, label); dup != 1 {
		t.Error("the label copy was not made the primary")
	}
	left, ok := f.db.LeftIndex(id)
	if !ok || left.FolderID != all || left.UID != 700 || left.UIDValidity != 41 || left.FromFolderID != inbox {
		t.Fatalf("remembered = %+v, %t", left, ok)
	}
}

func TestARowAPassRacedIntoTheDestinationGivesWayToTheMovedRow(t *testing.T) {
	// The destination's pass saw the moved message before the index followed
	// it. Having been told a move was coming, it did not call it new mail;
	// the moved row then takes its place and keeps its id.
	f := newIndex(t)
	ids := f.gmailFolders()
	receipts, inbox := ids["Receipts"], ids["INBOX"]
	f.live(receipts, mail(3, "a"))
	id := f.msgID(receipts, 3)

	f.db.ExpectMoves(f.account, []string{"mid:a@mail.example.com"})
	raced := f.live(inbox, mail(8, "a"))
	for _, ev := range raced.Events {
		if ev.Type == events.TypeMessageNew {
			t.Fatal("a pass announced a message a person moved as new mail")
		}
	}
	res := f.move(store.ActionMove{
		FromFolderID: receipts, ToFolderID: inbox, DestUIDValidity: 7,
		Moves: []store.MovedRow{{ID: id, UID: 3, DestUID: 8}},
	})
	if got := f.msgID(inbox, 8); got != id {
		t.Fatalf("the inbox row is %d, want the moved row %d", got, id)
	}
	if n := f.int(`SELECT count(*) FROM messages WHERE account_id = ?`, f.account); n != 1 {
		t.Fatalf("%d rows for one message", n)
	}
	wantTypes(t, "the move", res.Events, "message.moved", "message.moved")
}

func TestASummaryOfAUIDMovedAwayIsNotStoredAgain(t *testing.T) {
	// A pass fetched INBOX UID 5 before the move and applies it after:
	// storing it would put the moved message back in the inbox.
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox, receipts := ids["INBOX"], ids["Receipts"]
	f.live(inbox, mail(5, "a"))
	id := f.msgID(inbox, 5)
	f.move(store.ActionMove{
		FromFolderID: inbox, ToFolderID: receipts, DestUIDValidity: 7,
		Moves: []store.MovedRow{{ID: id, UID: 5, DestUID: 1}},
	})

	res := f.live(inbox, mail(5, "a", imap.FlagSeen))
	if res.Moved != 1 || res.Inserted != 0 || len(res.Events) != 0 {
		t.Fatalf("the late summary did %+v", res)
	}
	flags := f.flags(inbox, 0, provider.FlagUpdate{UID: 5, Flags: []imap.Flag{imap.FlagSeen}})
	if len(flags.Unknown) != 0 || len(flags.Events) != 0 {
		t.Fatalf("a late flag answer did %+v", flags)
	}
	if n := f.int(`SELECT count(*) FROM messages WHERE folder_id = ?`, inbox); n != 0 {
		t.Fatalf("the inbox holds %d rows again", n)
	}
}

func TestAFlagAnswerOlderThanAnEchoedChangeDoesNotUndoIt(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox := ids["INBOX"]
	sum := mail(5, "a")
	sum.ModSeq = 10
	f.live(inbox, sum)
	evs := f.actionFlags(store.ActionFlags{
		FolderID: inbox, Updates: []provider.FlagUpdate{{UID: 5, ModSeq: 12, Flags: []imap.Flag{imap.FlagSeen}}},
	})
	wantTypes(t, "marking read", evs, "message.flags")

	// A pass read the flags at modseq 11, before the change.
	late := f.flags(inbox, 0, provider.FlagUpdate{UID: 5, ModSeq: 11})
	if len(late.Events) != 0 {
		t.Fatalf("a stale answer announced %v", types(late.Events))
	}
	stale := mail(5, "a")
	stale.ModSeq = 11
	f.live(inbox, stale)
	if seen := f.int(`SELECT seen FROM messages WHERE folder_id = ? AND uid = 5`, inbox); seen != 1 {
		t.Fatal("a stale answer marked the message unread again")
	}
	// A newer one is applied.
	newer := f.flags(inbox, 0, provider.FlagUpdate{UID: 5, ModSeq: 13})
	wantTypes(t, "a newer answer", newer.Events, "message.flags")
}

func TestFlagsAnActionSetReachEveryCopyWhenFlagsAreShared(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox, receipts := ids["INBOX"], ids["Receipts"]
	f.live(inbox, mail(5, "a"))
	f.live(receipts, mail(9, "a"))
	f.live(receipts, mail(10, "b"))

	evs := f.actionFlags(store.ActionFlags{
		FolderID: inbox, Shared: true,
		Updates: []provider.FlagUpdate{{UID: 5, Flags: []imap.Flag{imap.FlagSeen, imap.FlagFlagged}}},
	})
	wantTypes(t, "starring on Gmail", evs, "message.flags", "message.flags")
	if n := f.int(`SELECT count(*) FROM messages WHERE seen = 1 AND flagged = 1`); n != 2 {
		t.Fatalf("%d rows read and starred, want the inbox row and its label copy", n)
	}
	if seen := f.int(`SELECT seen FROM messages WHERE folder_id = ? AND uid = 10`, receipts); seen != 0 {
		t.Error("another message was changed")
	}
	// Not shared: only the row the server echoed.
	f.actionFlags(store.ActionFlags{FolderID: inbox, Updates: []provider.FlagUpdate{{UID: 5}}})
	if seen := f.int(`SELECT seen FROM messages WHERE folder_id = ? AND uid = 9`, receipts); seen != 1 {
		t.Error("an unshared change reached the copy")
	}
}

func TestAMovedBackMessageIsIndexedAgainAsAMoveNotAsNewMail(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox, all := ids["INBOX"], ids["[Gmail]/All Mail"]
	f.live(inbox, mail(5, "a"))
	id := f.msgID(inbox, 5)
	f.move(store.ActionMove{
		FromFolderID: inbox, ToFolderID: all, DestUIDValidity: 41,
		Moves: []store.MovedRow{{ID: id, UID: 5, DestUID: 700}},
	})
	left, ok := f.db.LeftIndex(id)
	if !ok {
		t.Fatal("the archived row is not remembered")
	}
	f.db.ExpectReturn(left)

	var (
		back int64
		evs  []events.Event
	)
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		var err error
		back, evs, err = f.db.ApplyActionReturn(ctx, tx, store.ActionReturn{
			AccountID: f.account, LeftID: id, FromFolderID: all, ToFolderID: inbox, UIDValidity: 7, Summary: mail(6, "a"),
		})
		return err
	})
	if back == 0 || back == id {
		t.Fatalf("moved back as %d (it was %d): want a new row", back, id)
	}
	wantTypes(t, "moving back", evs, "message.moved")
	if moved := payload[store.ActionMoved](t, evs[0]); !moved.NewCopy || moved.FromFolderID != all ||
		moved.ToFolderID == nil || *moved.ToFolderID != inbox {
		t.Errorf("payload = %s", describe(moved))
	}
	if _, ok := f.db.LeftIndex(id); ok {
		t.Error("the old id is still offered for a move back")
	}
}

func TestAPassFindingAMovedMessageWithoutANewUIDDoesNotCallItNewMail(t *testing.T) {
	// No UIDPLUS, no Message-ID: the row left the index, and the inbox's
	// pass indexes the message again. It is not new mail.
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox, receipts := ids["INBOX"], ids["Receipts"]
	sum := mail(3, "a")
	sum.Envelope.MessageID = ""
	f.live(receipts, sum)
	row := f.msgID(receipts, 3)
	key := f.str(`SELECT group_key FROM messages WHERE id = ?`, row)
	f.db.ExpectMoves(f.account, []string{key})
	res := f.move(store.ActionMove{
		FromFolderID: receipts, ToFolderID: inbox, Moves: []store.MovedRow{{ID: row, UID: 3}},
	})
	if len(res.Removed) != 1 {
		t.Fatalf("result = %+v", res)
	}
	sum.UID = 11
	pass := f.live(inbox, sum)
	wantTypes(t, "the inbox's pass", pass.Events, "message.moved")
}

func TestActionsConsentKeepsTheFirstDateAndWithdrawingClearsIt(t *testing.T) {
	f := newIndex(t)
	ctx := context.Background()
	if c, err := f.db.ActionsConsentOf(ctx, "usr_1"); err != nil || c.At != 0 || c.Version != "" {
		t.Fatalf("a person starts with %+v, %v; want no consent", c, err)
	}
	first, err := f.db.GrantActionsConsent(ctx, "usr_1", "v1")
	if err != nil || first.At == 0 || first.Version != "v1" {
		t.Fatalf("grant = %+v, %v", first, err)
	}
	f.exec(`UPDATE users SET actions_consent_at = actions_consent_at - 100 WHERE id = 'usr_1'`)
	again, err := f.db.GrantActionsConsent(ctx, "usr_1", "v1")
	if err != nil || again.At != first.At-100 {
		t.Fatalf("granting the same version again moved the date: %+v, %v", again, err)
	}
	if err := f.db.WithdrawActionsConsent(ctx, "usr_1"); err != nil {
		t.Fatal(err)
	}
	if c, _ := f.db.ActionsConsentOf(ctx, "usr_1"); c.At != 0 || c.Version != "" {
		t.Fatalf("after withdrawing: %+v", c)
	}
	// Sync goes on: the two consents are separate.
	if n := f.int(`SELECT sync_consent_at FROM users WHERE id = 'usr_1'`); n == 0 {
		t.Error("withdrawing actions withdrew sync")
	}
	if _, err := f.db.ActionsConsentOf(ctx, "usr_nobody"); err == nil {
		t.Error("a person who does not exist has a consent")
	}
}

// flagsRead is a flag answer a pass read when ActionMark said read.
func (f *indexFixture) flagsRead(folderID int64, read uint64, updates ...provider.FlagUpdate) store.FlagResult {
	f.t.Helper()
	var res store.FlagResult
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		var err error
		res, err = f.db.ApplyFlags(ctx, tx, store.FlagBatch{
			AccountID: f.account, FolderID: folderID, UIDValidity: 7, Updates: updates, ActionMark: read, Now: f.now,
		})
		return err
	})
	return res
}

func TestAFlagAnswerReadBeforeAnActionDoesNotUndoItWithoutModificationSequences(t *testing.T) {
	// A server without CONDSTORE: no MODSEQ says which of two answers is
	// newer. A pass read the flags, a person marked the message read, and
	// the pass applies what it read.
	f := newIndex(t)
	inbox := f.gmailFolders()["INBOX"]
	f.live(inbox, mail(5, "a"))
	read := f.db.ActionMark()
	wantTypes(t, "marking read", f.actionFlags(store.ActionFlags{
		FolderID: inbox, Updates: []provider.FlagUpdate{{UID: 5, Flags: []imap.Flag{imap.FlagSeen}}},
	}), "message.flags")

	if late := f.flagsRead(inbox, read, provider.FlagUpdate{UID: 5}); len(late.Events) != 0 {
		t.Fatalf("an answer read before the action announced %v", types(late.Events))
	}
	res := f.apply(store.SummaryBatch{FolderID: inbox, Mode: store.ApplyLive, Summaries: []provider.Summary{mail(5, "a")},
		ActionMark: read})
	if res.Older != 1 || len(res.Events) != 0 {
		t.Fatalf("a summary read before the action did %+v", res)
	}
	if seen := f.int(`SELECT seen FROM messages WHERE folder_id = ? AND uid = 5`, inbox); seen != 1 {
		t.Fatal("an answer read before the action marked the message unread again")
	}
	// Read after it, an answer is the server's word: another client marked
	// the message unread since.
	after := f.flagsRead(inbox, f.db.ActionMark(), provider.FlagUpdate{UID: 5})
	wantTypes(t, "an answer read after the action", after.Events, "message.flags")
	if seen := f.int(`SELECT seen FROM messages WHERE folder_id = ? AND uid = 5`, inbox); seen != 0 {
		t.Fatal("an answer read after the action was ignored")
	}
}

func TestAFlagAnswerReadBeforeAnActionDoesNotUndoItOnAGmailLabelCopy(t *testing.T) {
	// On Gmail a flag is the message's: marking the inbox copy read reaches
	// the Receipts copy, which keeps its own MODSEQ — the action's echo is
	// the inbox copy's. Receipts' pass read the copy before the action, at
	// the MODSEQ the index still holds for it.
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox, receipts := ids["INBOX"], ids["Receipts"]
	a := mail(5, "a")
	a.ModSeq = 10
	f.live(inbox, a)
	copied := mail(9, "a")
	copied.ModSeq = 20
	f.live(receipts, copied)
	read := f.db.ActionMark()
	f.actionFlags(store.ActionFlags{
		FolderID: inbox, Shared: true,
		Updates: []provider.FlagUpdate{{UID: 5, ModSeq: 12, Flags: []imap.Flag{imap.FlagSeen}}},
	})
	if seen := f.int(`SELECT seen FROM messages WHERE folder_id = ? AND uid = 9`, receipts); seen != 1 {
		t.Fatal("the label copy was not marked read")
	}

	if late := f.flagsRead(receipts, read, provider.FlagUpdate{UID: 9, ModSeq: 20}); len(late.Events) != 0 {
		t.Fatalf("the copy's answer from before the action announced %v", types(late.Events))
	}
	res := f.apply(store.SummaryBatch{FolderID: receipts, Mode: store.ApplyLive, Summaries: []provider.Summary{copied},
		ActionMark: read})
	if res.Older != 1 {
		t.Fatalf("the copy's summary from before the action did %+v", res)
	}
	if seen := f.int(`SELECT seen FROM messages WHERE folder_id = ? AND uid = 9`, receipts); seen != 1 {
		t.Fatal("an answer read before the action marked the label copy unread again")
	}
	// Gmail gives the copy a new MODSEQ with the change; read after it, the
	// copy's answer is applied.
	after := f.flagsRead(receipts, f.db.ActionMark(),
		provider.FlagUpdate{UID: 9, ModSeq: 21, Flags: []imap.Flag{imap.FlagSeen, imap.FlagFlagged}})
	wantTypes(t, "the copy's answer after the action", after.Events, "message.flags")
}

func TestAMessageAMoveMayHaveTakenAwayIsAnnouncedAsMovedNeverAsDeleted(t *testing.T) {
	// A MOVE whose answer never came — the connection dropped — may have
	// moved the message. If it did, the diffs find it gone from the inbox;
	// it went somewhere, and nothing was deleted.
	f := newIndex(t)
	inbox := f.gmailFolders()["INBOX"]
	f.live(inbox, mail(5, "a"))
	id := f.msgID(inbox, 5)
	f.db.MayHaveMoved(inbox, 7, []imap.UID{5})
	if res := f.diff(inbox, 1); res.Tombstoned != 1 || len(res.Events) != 0 {
		t.Fatalf("the first diff did %+v", res)
	}
	res := f.diff(inbox, 1)
	wantTypes(t, "the diff that found it gone again", res.Events, "message.moved")
	moved := payload[store.MessageMoved](t, res.Events[0])
	if moved.MessageID != id || moved.To != nil || moved.NewCopy {
		t.Errorf("payload = %s", describe(moved))
	}

	// A message another client deleted is still a deletion.
	f.live(inbox, mail(6, "b"))
	f.diff(inbox, 1)
	wantTypes(t, "a deletion", f.diff(inbox, 1).Events, "message.deleted")
}

func TestAMoveIntoAFolderThatAlreadyHeldTheMessageIsRememberedAsTakingALabelAway(t *testing.T) {
	// Gmail: the message had the labels INBOX and Receipts. Moving the inbox
	// copy to Receipts took the Inbox label away, and the server reported
	// the Receipts copy it already had. Undoing that must add Inbox back, not
	// take Receipts away.
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox, receipts := ids["INBOX"], ids["Receipts"]
	f.live(inbox, mail(5, "a"))
	f.live(receipts, mail(9, "a"))
	id := f.msgID(inbox, 5)
	held, err := f.db.HeldCopies(context.Background(), receipts, 7, []string{"mid:a@mail.example.com"})
	if err != nil || !held[9] || len(held) != 1 {
		t.Fatalf("held = %v, %v", held, err)
	}
	f.move(store.ActionMove{
		FromFolderID: inbox, ToFolderID: receipts, DestUIDValidity: 7, Held: held,
		Moves: []store.MovedRow{{ID: id, UID: 5, DestUID: 9}},
	})
	l, ok := f.db.LabelMoveOf(id)
	if !ok || l.FromFolderID != inbox || l.ToFolderID != receipts {
		t.Fatalf("label move = %+v, %t", l, ok)
	}
	if got := f.msgID(receipts, 9); got != id {
		t.Fatalf("the Receipts row is %d, want the moved row %d", got, id)
	}

	// A row a pass raced in after the move was sent was not there before:
	// that move took the message out of its folder, as any move does.
	f.live(inbox, mail(6, "b"))
	other := f.msgID(inbox, 6)
	before := imap.UID(f.int(`SELECT max_seen_uid FROM folders WHERE id = ?`, receipts))
	f.live(receipts, mail(10, "b"))
	f.move(store.ActionMove{
		FromFolderID: inbox, ToFolderID: receipts, DestUIDValidity: 7, Held: map[imap.UID]bool{}, DestMark: before,
		Moves: []store.MovedRow{{ID: other, UID: 6, DestUID: 10}},
	})
	if _, ok := f.db.LabelMoveOf(other); ok {
		t.Fatal("a move a pass raced was remembered as taking a label away")
	}
	// A copy the index does not hold — old mail outside what it keeps — was
	// there too if the server reports a UID the folder had before the move.
	f.live(inbox, mail(7, "c"))
	third := f.msgID(inbox, 7)
	f.move(store.ActionMove{
		FromFolderID: inbox, ToFolderID: receipts, DestUIDValidity: 7, Held: map[imap.UID]bool{}, DestMark: 10,
		Moves: []store.MovedRow{{ID: third, UID: 7, DestUID: 2}},
	})
	if l, ok := f.db.LabelMoveOf(third); !ok || l.FromFolderID != inbox {
		t.Fatalf("a move reported at a UID the folder already had: %+v, %t", l, ok)
	}
	// Moving the row again supersedes what its last move was.
	f.move(store.ActionMove{
		FromFolderID: receipts, ToFolderID: inbox, DestUIDValidity: 7,
		Moves: []store.MovedRow{{ID: id, UID: 9, DestUID: 11}},
	})
	if _, ok := f.db.LabelMoveOf(id); ok {
		t.Fatal("a row moved again is still remembered for its earlier move")
	}
}
