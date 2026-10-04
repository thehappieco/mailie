package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
)

func TestAnAccountSyncsOnlyOnceItsOwnerConsents(t *testing.T) {
	f := newIndex(t)
	ctx := context.Background()
	f.exec(`UPDATE users SET sync_consent_at = 0`)

	check := func(want bool, why string) {
		t.Helper()
		got, err := f.db.SyncEligible(ctx, f.account)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s: eligible = %t, want %t", why, got, want)
		}
		list, err := f.db.SyncEligibleAccounts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if (len(list) == 1) != want {
			t.Fatalf("%s: the eligible list %v disagrees with the single check", why, list)
		}
	}
	check(false, "before consent")
	f.exec(`UPDATE users SET sync_consent_at = 5, sync_consent_version = 'v'`)
	check(true, "after consent")
	f.exec(`UPDATE users SET status = 'disabled'`)
	check(false, "the person was disabled")
	f.exec(`UPDATE users SET status = 'active'`)
	f.exec(`UPDATE accounts SET state = 'needs_reauth'`)
	check(false, "the account needs consent at the provider")
	f.exec(`UPDATE accounts SET state = 'active'`)
	f.exec(`UPDATE users SET sync_consent_at = 0`)
	check(false, "consent withdrawn")
	if ok, err := f.db.SyncEligible(ctx, "acc_nobody"); err != nil || ok {
		t.Fatalf("an account that does not exist is eligible (%t, %v)", ok, err)
	}
}

func TestAnInstanceAccountSyncsOnlyWhenTheOperatorEnablesIt(t *testing.T) {
	f := newIndex(t)
	ctx := context.Background()
	f.addAccount("acc_cli", "")
	if ok, _ := f.db.SyncEligible(ctx, "acc_cli"); ok {
		t.Fatal("an unowned account syncs before the operator switched it on")
	}
	f.exec(`UPDATE accounts SET sync_enabled_at = 9, sync_enabled_by = 'cli' WHERE id = 'acc_cli'`)
	if ok, _ := f.db.SyncEligible(ctx, "acc_cli"); !ok {
		t.Fatal("an unowned account the operator enabled does not sync")
	}
}

func TestAnOperatorSwitchCannotStandInForAPersonsConsent(t *testing.T) {
	// sync_enabled_at on somebody's own mailbox must not sync it: consent is
	// theirs to give and to withdraw.
	f := newIndex(t)
	f.exec(`UPDATE users SET sync_consent_at = 0`)
	f.exec(`UPDATE accounts SET sync_enabled_at = 9`)
	if ok, _ := f.db.SyncEligible(context.Background(), f.account); ok {
		t.Fatal("an owned mailbox syncs without its owner's consent")
	}
}

func TestNothingIsIndexedOnceConsentIsWithdrawn(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	f.exec(`UPDATE users SET sync_consent_at = 0`)

	err := f.db.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := f.db.ApplySummaries(context.Background(), tx, store.SummaryBatch{
			AccountID: f.account, FolderID: ids["INBOX"], UIDValidity: 7, Summaries: []provider.Summary{mail(1, "a")},
		})
		return err
	})
	if !errors.Is(err, store.ErrNotEligible) {
		t.Fatalf("a batch after withdrawal: %v, want ErrNotEligible", err)
	}
	if n := f.int(`SELECT count(*) FROM messages`); n != 0 {
		t.Fatalf("%d rows were stored after consent was withdrawn", n)
	}
	err = f.db.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := f.db.SyncFolders(context.Background(), tx, store.FolderDiscovery{
			AccountID: f.account, Folders: []provider.Folder{folder("INBOX")}, Profile: provider.ProfileFor(provider.KindGmail),
		})
		return err
	})
	if !errors.Is(err, store.ErrNotEligible) {
		t.Fatalf("a folder discovery after withdrawal: %v, want ErrNotEligible", err)
	}
}

func TestFolderRolesComeFromTheListAndStayUnique(t *testing.T) {
	f := newIndex(t)
	// Exchange publishes no attributes; a mailbox that has both an English
	// and a Portuguese Sent folder must not file sent mail in two places.
	res := f.discover(provider.ProfileFor(provider.KindMicrosoft), nil,
		folder("Inbox"),
		folder("Sent Items"),
		folder("Itens Enviados"),
		folder("Deleted Items"),
		folder("Projects/ACME"),
		provider.Folder{Name: "Projects", Delim: '/', Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}},
	)
	roles := map[string]store.Folder{}
	for _, fo := range res.Folders {
		roles[fo.Name] = fo
	}
	if roles["Inbox"].Role != provider.RoleInbox {
		t.Errorf("Inbox role = %q", roles["Inbox"].Role)
	}
	// "Itens Enviados" sorts before "Sent Items": same source, the name decides.
	if roles["Itens Enviados"].Role != provider.RoleSent || roles["Sent Items"].Role != provider.RoleNone {
		t.Errorf("sent went to %q/%q, want Itens Enviados only", roles["Itens Enviados"].Role, roles["Sent Items"].Role)
	}
	if roles["Projects"].Synced || roles["Projects"].Selectable {
		t.Error("a \\Noselect folder is synced")
	}
	if !roles["Projects/ACME"].Synced || roles["Projects/ACME"].DisplayName != "ACME" {
		t.Errorf("a person's folder: %s", describe(roles["Projects/ACME"]))
	}
	wantTypes(t, "the first discovery", res.Events,
		"folder.changed", "folder.changed", "folder.changed", "folder.changed", "folder.changed", "folder.changed")

	// Gmail: the archive is listed but never synced.
	g := newIndex(t)
	ids := g.gmailFolders()
	var all store.Folder
	for _, fo := range mustFolders(t, g) {
		if fo.ID == ids["[Gmail]/All Mail"] {
			all = fo
		}
	}
	if all.Role != provider.RoleAll || all.Synced {
		t.Errorf("gmail's All Mail: role %q synced %t, want all and not synced", all.Role, all.Synced)
	}
}

func mustFolders(t *testing.T, f *indexFixture) []store.Folder {
	t.Helper()
	out, err := f.db.Folders(context.Background(), f.account)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAnOverrideMovesARoleWithoutCollidingOnTheWay(t *testing.T) {
	f := newIndex(t)
	profile := provider.ProfileFor(provider.KindIMAP)
	f.discover(profile, nil, folder("INBOX"), folder("Sent"), folder("Enviadas"))

	res := f.discover(profile, map[string]string{"sent": "Enviadas"}, folder("INBOX"), folder("Sent"), folder("Enviadas"))
	for _, fo := range res.Folders {
		switch fo.Name {
		case "Enviadas":
			if fo.Role != provider.RoleSent || fo.RoleSource != provider.RoleSourceOverride {
				t.Errorf("Enviadas: %q from %q, want sent from the override", fo.Role, fo.RoleSource)
			}
		case "Sent":
			if fo.Role != provider.RoleNone {
				t.Errorf("Sent kept the role %q the override gave away", fo.Role)
			}
		}
	}
	if len(res.Updated) != 2 {
		t.Errorf("updated folders = %v, want the two that swapped the role", res.Updated)
	}
}

func TestAFolderMissingOnceKeepsItsRowsAndTwiceIsDeleted(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	gmail := provider.ProfileFor(provider.KindGmail)
	// One message only in the label, one in the inbox and the label.
	f.live(ids["INBOX"], mail(1, "both"))
	f.live(ids["Receipts"], mail(1, "only"), mail(2, "both"))
	onlyID := f.msgID(ids["Receipts"], 1)
	copyID := f.msgID(ids["Receipts"], 2)

	without := []provider.Folder{
		folder("INBOX"), folder("[Gmail]/Sent Mail", imap.MailboxAttrSent),
		folder("[Gmail]/Trash", imap.MailboxAttrTrash), folder("[Gmail]/All Mail", imap.MailboxAttrAll),
	}
	res := f.discover(gmail, nil, without...)
	if len(res.Missing) != 1 || res.Missing[0] != ids["Receipts"] || len(res.Removed) != 0 {
		t.Fatalf("first absence: missing %v removed %v", res.Missing, res.Removed)
	}
	if n := f.int(`SELECT count(*) FROM messages WHERE folder_id = ?`, ids["Receipts"]); n != 2 {
		t.Fatalf("a folder missing once lost its rows: %d left", n)
	}

	// Back again: the absence was not consecutive.
	f.discover(gmail, nil, append(without, folder("Receipts"))...)
	if n := f.int(`SELECT missing_since FROM folders WHERE id = ?`, ids["Receipts"]); n != 0 {
		t.Fatal("a folder that came back is still marked missing")
	}
	f.discover(gmail, nil, without...)
	res = f.discover(gmail, nil, without...)
	if len(res.Removed) != 1 || res.Removed[0] != ids["Receipts"] {
		t.Fatalf("second consecutive absence removed %v", res.Removed)
	}
	if n := f.int(`SELECT count(*) FROM messages WHERE folder_id = ?`, ids["Receipts"]); n != 0 {
		t.Fatalf("%d rows survived their folder", n)
	}
	wantTypes(t, "removing the folder", res.Events, "message.deleted", "message.moved", "folder.changed")
	for _, ev := range res.Events {
		switch ev.Type {
		case "message.deleted":
			if p := payload[store.MessageDeleted](t, ev); p.MessageID != onlyID {
				t.Errorf("deleted %d, want the label-only message %d", p.MessageID, onlyID)
			}
		case "message.moved":
			p := payload[store.MessageMoved](t, ev)
			if p.MessageID != copyID || p.To != nil || p.NewCopy || p.PrimaryID != f.msgID(ids["INBOX"], 1) {
				t.Errorf("the label copy's removal: %+v", p)
			}
		}
	}
}

func TestAnInitialBatchAnnouncesNothingAndRecordsItsResumePoint(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	cursor := imap.UID(3)
	before := f.journaled()
	res := f.apply(store.SummaryBatch{
		FolderID: ids["INBOX"], Mode: store.ApplyQuiet, BackfillCursor: &cursor,
		Summaries: []provider.Summary{mail(5, "e"), mail(4, "d"), mail(3, "c")},
	})
	if res.Inserted != 3 || len(res.Events) != 0 || f.journaled() != before {
		t.Fatalf("initial batch: %+v, %d journaled; want 3 inserted and nothing announced", res, f.journaled()-before)
	}
	fo, err := f.db.Folder(context.Background(), f.account, ids["INBOX"])
	if err != nil {
		t.Fatal(err)
	}
	if fo.BackfillCursor != 3 || fo.MaxSeenUID != 5 || fo.InitialFetched != 3 || fo.LocalCount != 3 {
		t.Fatalf("folder after the batch: cursor %d max %d fetched %d local %d",
			fo.BackfillCursor, fo.MaxSeenUID, fo.InitialFetched, fo.LocalCount)
	}

	// Resuming after a crash repeats the batch; nothing is counted twice.
	f.apply(store.SummaryBatch{
		FolderID: ids["INBOX"], Mode: store.ApplyQuiet, BackfillCursor: &cursor,
		Summaries: []provider.Summary{mail(5, "e"), mail(4, "d"), mail(3, "c")},
	})
	if n := f.int(`SELECT initial_fetched FROM folders WHERE id = ?`, ids["INBOX"]); n != 3 {
		t.Fatalf("a repeated batch counted again: initial_fetched = %d", n)
	}

	var evs []string
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		done, err := f.db.FinishInitial(ctx, tx, f.account, ids["INBOX"], f.now)
		evs = types(done)
		if err == nil && len(done) == 1 {
			if p := payload[store.FolderChanged](t, done[0]); p.Change != store.FolderInitialDone || p.Count != 3 {
				t.Errorf("initial_done payload: %+v", p)
			}
		}
		return err
	})
	if len(evs) != 1 || evs[0] != "folder.changed" {
		t.Fatalf("finishing the initial sync announced %v, want one folder.changed", evs)
	}
	if n := f.int(`SELECT backfill_cursor FROM folders WHERE id = ?`, ids["INBOX"]); n != 0 {
		t.Fatal("the backfill cursor is not zero after the initial sync")
	}
}

func TestNewInboxMailIsAnnouncedOnce(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	res := f.live(ids["INBOX"], mail(10, "hello"))
	wantTypes(t, "an inbox arrival", res.Events, "message.new")
	p := payload[store.MessageNew](t, res.Events[0])
	if p.AccountID != f.account || p.MessageID != f.msgID(ids["INBOX"], 10) || p.FolderID != ids["INBOX"] ||
		p.FolderRole != "inbox" || p.Subject != "Subject hello" || p.From == nil ||
		p.From.Email != "sender-hello@example.com" || !p.FirstCopy || !p.FirstInboxCopy ||
		p.InternalDate != t0.Add(-time.Hour).Unix() {
		t.Fatalf("message.new payload: %+v", p)
	}
	again := f.live(ids["INBOX"], mail(10, "hello"))
	if len(again.Events) != 0 || again.Updated != 1 {
		t.Fatalf("the same summary again: %+v", again)
	}
	if n := f.int(`SELECT max_seen_uid FROM folders WHERE id = ?`, ids["INBOX"]); n != 10 {
		t.Fatalf("max_seen_uid = %d, want 10", n)
	}
}

func TestGmailLabelCopiesDoNotEmitNewMail(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	wantTypes(t, "the inbox copy", f.live(ids["INBOX"], mail(1, "x")).Events, "message.new")
	res := f.live(ids["Receipts"], mail(40, "x"))
	wantTypes(t, "the label copy", res.Events, "message.moved")
	p := payload[store.MessageMoved](t, res.Events[0])
	inbox, label := f.msgID(ids["INBOX"], 1), f.msgID(ids["Receipts"], 40)
	if !p.NewCopy || p.To == nil || *p.To != ids["Receipts"] || p.PrimaryID != inbox || p.MessageID != label {
		t.Fatalf("label copy payload: %+v", p)
	}
	if dup := f.int(`SELECT dup_of FROM messages WHERE id = ?`, label); dup != inbox {
		t.Fatalf("the label copy points at %d, want the inbox row %d", dup, inbox)
	}
	if n := f.int(`SELECT count(*) FROM messages WHERE dup_of IS NULL`); n != 1 {
		t.Fatalf("%d primaries for one message", n)
	}
}

func TestAGmailLabelSyncedBeforeInboxStillReportsNewInboxMail(t *testing.T) {
	// A filter labels the message on arrival, and the label happens to be
	// synced first. The inbox arrival must still be announced.
	f := newIndex(t)
	ids := f.gmailFolders()
	res := f.live(ids["Receipts"], mail(40, "x"))
	wantTypes(t, "the label first", res.Events, "message.new")
	if p := payload[store.MessageNew](t, res.Events[0]); !p.FirstCopy || p.FirstInboxCopy || p.FolderRole != "" {
		t.Fatalf("label payload: %+v", p)
	}
	res = f.live(ids["INBOX"], mail(1, "x"))
	wantTypes(t, "the inbox second", res.Events, "message.new")
	if p := payload[store.MessageNew](t, res.Events[0]); p.FirstCopy || !p.FirstInboxCopy || p.FolderRole != "inbox" {
		t.Fatalf("inbox payload: %+v", p)
	}
}

func TestMailFiledInSentOrTrashIsNeverNew(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	for _, name := range []string{"[Gmail]/Sent Mail", "[Gmail]/Trash"} {
		res := f.live(ids[name], mail(1, name))
		wantTypes(t, name, res.Events, "message.moved")
		if p := payload[store.MessageMoved](t, res.Events[0]); !p.NewCopy {
			t.Errorf("%s: %+v", name, p)
		}
	}
}

func TestRecentIsNeitherStoredNorAChange(t *testing.T) {
	// \Recent goes to whichever connection saw a message first. The sync
	// connection reads it, the next reading from another connection does
	// not: that is not a flag change, and the index never holds it.
	f := newIndex(t)
	inbox := f.gmailFolders()["INBOX"]
	f.live(inbox, mail(1, "a", "\\Recent", imap.FlagSeen))
	if got := f.str(`SELECT flags_json FROM messages WHERE id = ?`, f.msgID(inbox, 1)); got != `["\\seen"]` {
		t.Fatalf("flags stored as %s", got)
	}
	res := f.flags(inbox, 0, provider.FlagUpdate{UID: 1, Flags: []imap.Flag{imap.FlagSeen}})
	wantTypes(t, "\\Recent going away", res.Events)
	res = f.flags(inbox, 0, provider.FlagUpdate{UID: 1, Flags: []imap.Flag{"\\RECENT", imap.FlagSeen}})
	wantTypes(t, "\\Recent coming back", res.Events)
}

func TestAFlagChangeIsAnnouncedOnlyWhenFlagsDiffer(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox := ids["INBOX"]
	f.live(inbox, mail(1, "a"), mail(2, "b", imap.FlagSeen))
	if n := f.int(`SELECT unseen_count FROM folders WHERE id = ?`, inbox); n != 1 {
		t.Fatalf("unseen = %d, want 1", n)
	}

	res := f.flags(inbox, 900,
		provider.FlagUpdate{UID: 1, ModSeq: 800, Flags: []imap.Flag{"\\Seen", "\\Flagged"}},
		provider.FlagUpdate{UID: 2, ModSeq: 801, Flags: []imap.Flag{"\\SEEN"}},
		provider.FlagUpdate{UID: 3, ModSeq: 802, Flags: nil},
	)
	wantTypes(t, "the flag batch", res.Events, "message.flags")
	p := payload[store.MessageFlags](t, res.Events[0])
	if p.MessageID != f.msgID(inbox, 1) || !p.Seen || !p.Flagged || p.Answered {
		t.Fatalf("flags payload: %+v", p)
	}
	if len(res.Unknown) != 1 || res.Unknown[0] != 3 {
		t.Fatalf("unknown = %v, want [3] for the caller to fetch", res.Unknown)
	}
	if n := f.int(`SELECT unseen_count FROM folders WHERE id = ?`, inbox); n != 0 {
		t.Fatalf("unseen = %d after both were read", n)
	}
	if n := f.int(`SELECT highest_modseq FROM folders WHERE id = ?`, inbox); n != 900 {
		t.Fatalf("highest_modseq = %d, want 900", n)
	}
	if n := f.int(`SELECT modseq FROM messages WHERE id = ?`, f.msgID(inbox, 2)); n != 801 {
		t.Fatalf("an unchanged row's modseq = %d, want 801", n)
	}
}

func TestAnExpungedUIDIsDeletedAfterTwoConsecutiveScans(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox := ids["INBOX"]
	f.live(inbox, mail(1, "a"), mail(2, "b"), mail(3, "c"))
	id2 := f.msgID(inbox, 2)

	first := f.diff(inbox, 1, 1, 3)
	if first.Tombstoned != 1 || first.Deleted != 0 || len(first.Events) != 0 {
		t.Fatalf("first absence: %+v", first)
	}
	if n := f.int(`SELECT local_count FROM folders WHERE id = ?`, inbox); n != 2 {
		t.Fatalf("a tombstoned row still counts: local_count = %d", n)
	}
	if n := f.int(`SELECT count(*) FROM messages WHERE id = ?`, id2); n != 1 {
		t.Fatal("a row missing once was deleted")
	}

	second := f.diff(inbox, 1, 1, 3)
	if second.Deleted != 1 {
		t.Fatalf("second absence: %+v", second)
	}
	wantTypes(t, "the deletion", second.Events, "message.deleted")
	if p := payload[store.MessageDeleted](t, second.Events[0]); p.MessageID != id2 || p.FolderRole != "inbox" {
		t.Fatalf("deleted payload: %+v", p)
	}
	if n := f.int(`SELECT count(*) FROM messages WHERE id = ?`, id2); n != 0 {
		t.Fatal("a row missing twice survived")
	}
	if n := f.int(`SELECT count(*) FROM parts WHERE msg_id = ?`, id2); n != 0 {
		t.Fatal("the deleted row's parts survived")
	}
}

func TestAnAbsenceThatIsNotConsecutiveDeletesNothing(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox := ids["INBOX"]
	f.live(inbox, mail(1, "a"), mail(2, "b"))
	f.diff(inbox, 1, 1)
	back := f.diff(inbox, 1, 1, 2)
	if back.Revived != 1 || len(back.Missing) != 0 {
		t.Fatalf("a UID listed again: %+v", back)
	}
	again := f.diff(inbox, 1, 1)
	if again.Deleted != 0 || again.Tombstoned != 1 {
		t.Fatalf("absent, present, absent deleted something: %+v", again)
	}
}

func TestDeletingTheNewestMessageDoesNotLowerMaxSeenUID(t *testing.T) {
	// max(uid) would drop, and the next pass would fetch old mail as new.
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox := ids["INBOX"]
	f.live(inbox, mail(1, "a"), mail(2, "b"))
	f.diff(inbox, 1, 1)
	f.diff(inbox, 1, 1)
	if n := f.int(`SELECT max_seen_uid FROM folders WHERE id = ?`, inbox); n != 2 {
		t.Fatalf("max_seen_uid = %d after deleting the newest, want 2", n)
	}
	var lowered imap.UID = 1
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return store.UpdateFolderSync(ctx, tx, inbox, store.FolderSync{MaxSeenUID: &lowered})
	})
	if n := f.int(`SELECT max_seen_uid FROM folders WHERE id = ?`, inbox); n != 2 {
		t.Fatalf("an explicit lower mark was written: %d", n)
	}
}

func TestTheDiffFindsServerUIDsTheIndexLacks(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox := ids["INBOX"]
	f.live(inbox, mail(10, "a"), mail(12, "b"))
	f.diff(inbox, 10, 10) // 12 is tombstoned
	res := f.diff(inbox, 10, 5, 10, 11, 12, 13)
	// 5 is below the floor; 12 is held (and revived); 11 and 13 are missing.
	if len(res.Missing) != 2 || res.Missing[0] != 11 || res.Missing[1] != 13 || res.Revived != 1 {
		t.Fatalf("diff: %+v", res)
	}

	// With a ceiling, rows above it are not judged by an older search.
	f.live(inbox, mail(20, "late"))
	var res2 store.DiffResult
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		var err error
		res2, err = f.db.ApplyUIDDiff(ctx, tx, store.UIDDiff{
			AccountID: f.account, FolderID: inbox, UIDValidity: 7, Floor: 10, Upto: 13,
			ServerUIDs: []imap.UID{10, 12}, Now: f.now,
		})
		return err
	})
	if res2.Tombstoned != 0 {
		t.Fatalf("a row indexed after the search was tombstoned: %+v", res2)
	}
}

func TestRemovingALabelIsACopyRemovedNotADeletion(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	f.live(ids["INBOX"], mail(1, "x"))
	f.live(ids["Receipts"], mail(40, "x"))
	inboxRow, labelRow := f.msgID(ids["INBOX"], 1), f.msgID(ids["Receipts"], 40)

	// The inbox copy goes (archived): the label copy stands in for the message.
	f.diff(ids["INBOX"], 1)
	if n := f.int(`SELECT dup_of IS NULL FROM messages WHERE id = ?`, labelRow); n != 1 {
		t.Fatal("with the primary tombstoned, the live label copy did not become the primary")
	}
	res := f.diff(ids["INBOX"], 1)
	wantTypes(t, "archiving", res.Events, "message.moved")
	p := payload[store.MessageMoved](t, res.Events[0])
	if p.MessageID != inboxRow || p.To != nil || p.NewCopy || p.PrimaryID != labelRow {
		t.Fatalf("archive payload: %+v", p)
	}
	// And the last copy going is a deletion.
	f.diff(ids["Receipts"], 1)
	res = f.diff(ids["Receipts"], 1)
	wantTypes(t, "the last copy", res.Events, "message.deleted")
}

func TestMicrosoftReassignedUIDIsRewrittenInPlace(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox := ids["INBOX"]
	f.live(inbox, mail(5, "report"))
	id := f.msgID(inbox, 5)
	f.exec(`UPDATE parts SET sha256 = 'cached' WHERE msg_id = ?`, id)
	before := f.journaled()

	// Exchange hands the same message UID 9. The old one is gone from the
	// server, which the caller confirms before passing the reclaim.
	moved := mail(9, "report")
	cands, err := f.db.ReclaimCandidates(context.Background(), inbox, 7, []provider.Summary{moved})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].RowID != id || cands[0].OldUID != 5 || cands[0].UID != 9 {
		t.Fatalf("candidates: %+v", cands)
	}
	res := f.apply(store.SummaryBatch{
		FolderID: inbox, Mode: store.ApplyLive, Summaries: []provider.Summary{moved},
		Reclaim: map[imap.UID]int64{9: id},
	})
	if res.Reclaimed != 1 || res.Inserted != 0 || len(res.Events) != 0 || f.journaled() != before {
		t.Fatalf("reclaim: %+v", res)
	}
	if got := f.msgID(inbox, 9); got != id {
		t.Fatalf("the message has id %d under its new UID, want %d", got, id)
	}
	if f.str(`SELECT sha256 FROM parts WHERE msg_id = ?`, id) != "cached" {
		t.Fatal("the reclaimed row lost what was cached for it")
	}
}

func TestAGenuineDuplicateIsNotReclaimed(t *testing.T) {
	// Same Message-ID, date and size, and the old UID still exists: two
	// messages. Without the caller's confirmation nothing is rewritten.
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox := ids["INBOX"]
	f.live(inbox, mail(5, "dup"))
	res := f.live(inbox, mail(9, "dup"))
	if res.Inserted != 1 || res.Reclaimed != 0 {
		t.Fatalf("duplicate: %+v", res)
	}
	wantTypes(t, "a duplicate in the same inbox", res.Events, "message.moved")
	if n := f.int(`SELECT count(*) FROM messages WHERE folder_id = ?`, inbox); n != 2 {
		t.Fatalf("%d rows, want both", n)
	}
}

func TestAUIDValidityChangeKeepsIdsAndEmitsNothingForKnownMessages(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox := ids["INBOX"]
	f.live(inbox, mail(1, "a"), mail(2, "b"), mail(3, "gone"))
	idA, idB := f.msgID(inbox, 1), f.msgID(inbox, 2)
	f.exec(`UPDATE folders SET last_synced_at = ? WHERE id = ?`, t0.Unix(), inbox)

	var start store.ResyncStart
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		var err error
		start, err = f.db.BeginResync(ctx, tx, f.account, inbox, provider.FolderStatus{UIDValidity: 8, UIDNext: 100},
			store.ResyncFromLive)
		return err
	})
	if !start.Oldest.Equal(t0.Add(-time.Hour)) || start.From != store.ResyncFromLive {
		t.Fatalf("resync starts %+v, want the oldest stale row, from live", start)
	}
	if n := f.int(`SELECT local_count FROM folders WHERE id = ?`, inbox); n != 3 {
		t.Fatalf("readers see %d rows mid-resync, want all 3", n)
	}

	late := mail(52, "new")
	late.InternalDate = t0.Add(time.Minute)
	res := f.apply(store.SummaryBatch{
		FolderID: inbox, UIDValidity: 8, Mode: store.ApplyResync,
		Summaries: []provider.Summary{mail(50, "a"), mail(51, "b", imap.FlagSeen), late},
	})
	if res.Claimed != 2 || res.Inserted != 1 {
		t.Fatalf("resync batch: %+v", res)
	}
	wantTypes(t, "the resync batch", res.Events, "message.flags", "message.new")
	if f.int(`SELECT id FROM messages WHERE folder_id = ? AND uidvalidity = 8 AND uid = 50`, inbox) != idA ||
		f.int(`SELECT id FROM messages WHERE folder_id = ? AND uidvalidity = 8 AND uid = 51`, inbox) != idB {
		t.Fatal("a known message changed id across the UIDVALIDITY change")
	}

	var done []string
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		evs, err := f.db.FinishResync(ctx, tx, f.account, inbox, f.now)
		done = types(evs)
		return err
	})
	if len(done) != 2 || done[0] != "message.deleted" || done[1] != "folder.changed" {
		t.Fatalf("finishing the resync announced %v", done)
	}
	if n := f.int(`SELECT count(*) FROM messages WHERE folder_id = ?`, inbox); n != 3 {
		t.Fatalf("%d rows after the resync, want a, b and the new one", n)
	}
	if n := f.int(`SELECT count(*) FROM messages WHERE stale = 1`); n != 0 {
		t.Fatal("stale rows survived the resync")
	}
}

func TestAnUnmatchedOldMessageInAResyncIsNotNewMail(t *testing.T) {
	// A message without a usable Message-ID that the resync cannot match is
	// inserted again, but it is not an arrival.
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox := ids["INBOX"]
	f.exec(`UPDATE folders SET last_synced_at = ? WHERE id = ?`, t0.Unix(), inbox)
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := f.db.BeginResync(ctx, tx, f.account, inbox, provider.FolderStatus{UIDValidity: 8}, store.ResyncFromLive)
		return err
	})
	res := f.apply(store.SummaryBatch{FolderID: inbox, UIDValidity: 8, Mode: store.ApplyResync,
		Summaries: []provider.Summary{mail(1, "old")}})
	if res.Inserted != 1 || len(res.Events) != 0 {
		t.Fatalf("old unmatched message: %+v", res)
	}
}

func TestABatchFromAnOldUIDValidityIsRefused(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	err := f.db.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := f.db.ApplySummaries(context.Background(), tx, store.SummaryBatch{
			AccountID: f.account, FolderID: ids["INBOX"], UIDValidity: 6, Summaries: []provider.Summary{mail(1, "a")},
		})
		return err
	})
	if !errors.Is(err, store.ErrUIDValidityMismatch) {
		t.Fatalf("got %v, want ErrUIDValidityMismatch", err)
	}
}

func TestFolderCountersFollowEveryChange(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox := ids["INBOX"]
	counts := func() (int64, int64) {
		return f.int(`SELECT local_count FROM folders WHERE id = ?`, inbox),
			f.int(`SELECT unseen_count FROM folders WHERE id = ?`, inbox)
	}
	check := func(what string, local, unseen int64) {
		t.Helper()
		l, u := counts()
		if l != local || u != unseen {
			t.Fatalf("%s: local %d unseen %d, want %d and %d", what, l, u, local, unseen)
		}
	}
	f.live(inbox, mail(1, "a"), mail(2, "b"), mail(3, "c", imap.FlagSeen))
	check("three indexed, one read", 3, 2)
	f.flags(inbox, 0, provider.FlagUpdate{UID: 1, Flags: []imap.Flag{imap.FlagSeen}})
	check("one more read", 3, 1)
	f.diff(inbox, 1, 1, 3)
	check("an unread one tombstoned", 2, 0)
	f.diff(inbox, 1, 1, 2, 3)
	check("and revived", 3, 1)
	f.diff(inbox, 1, 3)
	f.diff(inbox, 1, 3)
	check("two deleted", 1, 0)
}

func TestSummariesAreStoredAsMetadataOnly(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	sum := mail(7, "meta")
	sum.Envelope.MessageID = " <meta@mail.example.com> "
	sum.Envelope.InReplyTo = []string{"<parent@mail.example.com>"}
	sum.Envelope.Cc = []imap.Address{{Name: "Colleague", Mailbox: "colleague", Host: "example.org"}}
	sum.References = []string{"<root@mail.example.com>", "parent@mail.example.com"}
	sum.Parts = append(sum.Parts, provider.PartInfo{
		Path: []int{2}, MIMEType: "application/PDF", Encoding: "BASE64", Disposition: "Attachment",
		Filename: "invoice.pdf", ContentID: "<cid-1>", Size: 50000, IsAttachment: true,
	})
	sum.ModSeq = 44
	f.live(ids["INBOX"], sum)
	id := f.msgID(ids["INBOX"], 7)

	var (
		messageID, inReplyTo, refs, fromJSON, fromAddr, fromText, toText, body, snippet, groupKey string
		hasAttach, modseq                                                                         int64
	)
	err := f.db.Reader().QueryRowContext(context.Background(), `SELECT message_id, in_reply_to, references_json,
		from_json, from_addr, from_text, to_text, body_text, snippet, group_key, has_attachments, modseq
		FROM messages WHERE id = ?`, id).Scan(&messageID, &inReplyTo, &refs, &fromJSON, &fromAddr, &fromText,
		&toText, &body, &snippet, &groupKey, &hasAttach, &modseq)
	if err != nil {
		t.Fatal(err)
	}
	if messageID != "meta@mail.example.com" || groupKey != "mid:meta@mail.example.com" {
		t.Errorf("message_id %q group %q: ids are stored bare", messageID, groupKey)
	}
	if inReplyTo != "parent@mail.example.com" || refs != `["root@mail.example.com","parent@mail.example.com"]` {
		t.Errorf("threading: in_reply_to %q references %s", inReplyTo, refs)
	}
	if fromAddr != "sender-meta@example.com" || fromText != "Sender meta sender-meta@example.com" ||
		toText != "Person person@example.com, Colleague colleague@example.org" {
		t.Errorf("addresses: %q / %q / %q / %s", fromAddr, fromText, toText, fromJSON)
	}
	if body != "" || snippet != "" {
		t.Error("phase 2 stored message text")
	}
	if hasAttach != 1 || modseq != 44 {
		t.Errorf("has_attachments %d modseq %d", hasAttach, modseq)
	}
	if n := f.int(`SELECT count(*) FROM parts WHERE msg_id = ?`, id); n != 2 {
		t.Fatalf("%d parts, want 2", n)
	}
	if got := f.str(`SELECT mime_type || '|' || encoding || '|' || disposition || '|' || content_id || '|' || is_attachment
		FROM parts WHERE msg_id = ? AND path = '2'`, id); got != "application/pdf|base64|attachment|cid-1|1" {
		t.Errorf("attachment part: %s", got)
	}
	if got := f.str(`SELECT charset FROM parts WHERE msg_id = ? AND path = '1'`, id); got != "utf-8" {
		t.Errorf("body part charset %q", got)
	}
	for _, q := range []string{"meta", "colleague", "sender"} {
		if n := f.int(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?`, q); n != 1 {
			t.Errorf("full-text search for %q found %d", q, n)
		}
	}
}

func TestAMessageWithoutMessageIDGroupsByContent(t *testing.T) {
	a := store.GroupKey("", "Hello", "a@example.com", 100)
	b := store.GroupKey("", "Hello", "a@example.com", 100)
	c := store.GroupKey("", "Hello", "a@example.com", 101)
	if a != b || a == c || a[:2] != "h:" {
		t.Fatalf("content keys: %q %q %q", a, b, c)
	}
	if got := store.GroupKey("no-at-sign", "Hello", "a@example.com", 100); got != a {
		t.Fatalf("an id without @ is not usable; got %q", got)
	}
}

func TestSyncSummaryCountsProgress(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	total, fetched := 10, 0
	for _, name := range []string{"INBOX", "Receipts", "[Gmail]/Sent Mail", "[Gmail]/Trash"} {
		f.write(func(ctx context.Context, tx *sql.Tx) error {
			return store.UpdateFolderSync(ctx, tx, ids[name], store.FolderSync{InitialTotal: &total, InitialFetched: &fetched})
		})
	}
	cursor := imap.UID(1)
	f.apply(store.SummaryBatch{FolderID: ids["INBOX"], Mode: store.ApplyQuiet, BackfillCursor: &cursor,
		Summaries: []provider.Summary{mail(1, "a"), mail(2, "b"), mail(3, "c"), mail(4, "d")}})
	sum, err := f.db.SyncSummary(context.Background(), f.account)
	if err != nil {
		t.Fatal(err)
	}
	if sum.FoldersTotal != 4 || sum.FoldersSynced != 0 || sum.Messages != 4 || sum.InitialProgress() != 10 {
		t.Fatalf("summary %+v, progress %d", sum, sum.InitialProgress())
	}
	for _, name := range []string{"INBOX", "Receipts", "[Gmail]/Sent Mail", "[Gmail]/Trash"} {
		f.write(func(ctx context.Context, tx *sql.Tx) error {
			_, err := f.db.FinishInitial(ctx, tx, f.account, ids[name], f.now)
			return err
		})
	}
	if sum, _ = f.db.SyncSummary(context.Background(), f.account); sum.FoldersSynced != 4 || sum.InitialProgress() != 100 {
		t.Fatalf("after every folder finished: %+v", sum)
	}
}

func TestSyncBookkeepingCountsFailuresAndClearsOnSuccess(t *testing.T) {
	f := newIndex(t)
	ctx := context.Background()
	for want := 1; want <= 3; want++ {
		n, err := f.db.RecordSyncFailure(ctx, f.account, "connection_closed", t0.Add(time.Minute))
		if err != nil || n != want {
			t.Fatalf("failure %d recorded as %d (%v)", want, n, err)
		}
	}
	if err := f.db.RecordSyncOK(ctx, f.account, t0); err != nil {
		t.Fatal(err)
	}
	sum, err := f.db.SyncSummary(ctx, f.account)
	if err != nil {
		t.Fatal(err)
	}
	if sum.ConsecutiveFailures != 0 || sum.LastError != "" || !sum.NextRetryAt.IsZero() || !sum.LastOKAt.Equal(t0) {
		t.Fatalf("after a good pass: %+v", sum)
	}
}

func TestWithdrawalResetsTheSyncBookkeepingAndALateWorkerCannotWriteItBack(t *testing.T) {
	f := newIndex(t)
	ctx := context.Background()
	f.exec(`UPDATE accounts SET sync_tier_resolved = 'condstore'`)
	if err := f.db.RecordSyncOK(ctx, f.account, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.RecordSyncFailure(ctx, f.account, "connection_closed", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.WithdrawSyncConsent(ctx, "usr_1"); err != nil {
		t.Fatal(err)
	}
	clean := `SELECT count(*) FROM accounts WHERE id = ? AND sync_tier_resolved = '' AND last_ok_at = 0
		AND last_error = '' AND consecutive_failures = 0 AND next_retry_at = 0 AND last_idle_event_at = 0`
	if n := f.int(clean, f.account); n != 1 {
		t.Fatal("the withdrawal left the account's sync bookkeeping behind")
	}
	// A worker that has not heard yet.
	if err := f.db.RecordSyncOK(ctx, f.account, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.RecordSyncFailure(ctx, f.account, "connection_closed", t0); err != nil {
		t.Fatal(err)
	}
	if err := f.db.RecordIdleEvent(ctx, f.account, t0); err != nil {
		t.Fatal(err)
	}
	if n := f.int(clean, f.account); n != 1 {
		t.Error("a late worker wrote its bookkeeping back after the withdrawal")
	}
}

// folderPassed records that the folder's last pass started at t.
func (f *indexFixture) folderPassed(folderID int64, at time.Time) {
	f.t.Helper()
	f.exec(`UPDATE folders SET last_synced_at = ? WHERE id = ?`, at.Unix(), folderID)
}

// dated is a summary with its INTERNALDATE set.
func dated(sum provider.Summary, at time.Time) provider.Summary {
	sum.InternalDate = at
	sum.Envelope.Date = at
	return sum
}

func TestOldMailMovedOutOfTheInboxIntoAFolderWithNoRoleIsNotNewMail(t *testing.T) {
	// On Microsoft, iCloud or any IMAP server, a person drags last week's
	// email from the inbox into a folder of their own. The folder's next pass
	// finds a row the index never held; it is a move, not mail arriving,
	// whether the inbox copy is still tombstoned or already deleted.
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("inbox copy deleted=%t", deleted), func(t *testing.T) {
			f := newIndex(t)
			ids := f.gmailFolders()
			inbox, projects := ids["INBOX"], ids["Receipts"]
			weekOld := t0.AddDate(0, 0, -7)
			f.live(inbox, dated(mail(1, "report"), weekOld))
			f.folderPassed(projects, t0.Add(-15*time.Minute))

			f.diff(inbox, 1) // gone from the inbox: tombstoned
			if deleted {
				f.diff(inbox, 1) // and deleted on the confirmation diff
			}
			res := f.live(projects, dated(mail(30, "report"), weekOld))
			wantTypes(t, "the moved message", res.Events, "message.moved")
			if p := payload[store.MessageMoved](t, res.Events[0]); !p.NewCopy || p.To == nil || *p.To != projects {
				t.Fatalf("moved payload: %+v", p)
			}
		})
	}
}

func TestALabelAddedToOldArchivedMailIsNotNewMail(t *testing.T) {
	// Gmail: a label put on a message archived a month ago. The label's copy
	// is the first the index sees — All Mail is never synced — but its
	// INTERNALDATE is the month-old one.
	f := newIndex(t)
	ids := f.gmailFolders()
	label := ids["Receipts"]
	f.folderPassed(label, t0.Add(-15*time.Minute))
	res := f.live(label, dated(mail(12, "invoice"), t0.AddDate(0, -1, 0)))
	wantTypes(t, "the label on old mail", res.Events, "message.moved")
}

func TestMailMovedOutOfTheInboxSoonAfterArrivingIsNotNewTwice(t *testing.T) {
	// Announced when it reached the inbox, moved to a folder of the person's
	// within the folder's pass interval: its date alone looks like an
	// arrival there, but the inbox copy the diff tombstoned says it moved.
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox, projects := ids["INBOX"], ids["Receipts"]
	f.folderPassed(projects, t0.Add(-15*time.Minute))
	arrived := t0.Add(-5 * time.Minute)
	wantTypes(t, "the arrival", f.live(inbox, dated(mail(1, "fresh"), arrived)).Events, "message.new")
	f.diff(inbox, 1)
	res := f.live(projects, dated(mail(30, "fresh"), arrived))
	wantTypes(t, "the move", res.Events, "message.moved")
}

func TestMailAFilterFilesInAFolderWithNoRoleIsStillNewMail(t *testing.T) {
	// A server-side filter delivers straight into a label or a folder: it
	// never touches the inbox, and it is mail arriving. Its INTERNALDATE is
	// after the folder's last pass — or a little before, when the server's
	// clock runs behind this one's.
	f := newIndex(t)
	ids := f.gmailFolders()
	label := ids["Receipts"]
	lastPass := t0.Add(-15 * time.Minute)
	f.folderPassed(label, lastPass)
	res := f.live(label, dated(mail(40, "receipt"), t0.Add(-5*time.Minute)),
		dated(mail(41, "lagging clock"), lastPass.Add(-time.Minute)))
	wantTypes(t, "filtered arrivals", res.Events, "message.new", "message.new")
	if p := payload[store.MessageNew](t, res.Events[0]); !p.FirstCopy || p.FolderRole != "" {
		t.Fatalf("new payload: %+v", p)
	}
}

func TestAResumedResyncEndsTheWayItsFirstAttemptWouldHave(t *testing.T) {
	// The resync is interrupted and resumed: by then the folder says
	// "resync" (or "error"), not what it was. What it was when the resync
	// began decides how it ends — resync_done for a folder that had been
	// live, initial_done for one whose initial sync it finished.
	for _, tc := range []struct {
		first, resumed, want string
	}{
		{store.ResyncFromLive, store.ResyncFromInitial, store.FolderResyncDone},
		{store.ResyncFromInitial, store.ResyncFromLive, store.FolderInitialDone},
	} {
		t.Run(tc.first, func(t *testing.T) {
			f := newIndex(t)
			ids := f.gmailFolders()
			inbox := ids["INBOX"]
			f.live(inbox, mail(1, "a"))
			for _, from := range []string{tc.first, tc.resumed} {
				f.write(func(ctx context.Context, tx *sql.Tx) error {
					start, err := f.db.BeginResync(ctx, tx, f.account, inbox, provider.FolderStatus{UIDValidity: 8}, from)
					if err == nil && start.From != tc.first {
						t.Errorf("BeginResync(%s) says the resync began from %s, want %s", from, start.From, tc.first)
					}
					return err
				})
			}
			var evs []events.Event
			f.write(func(ctx context.Context, tx *sql.Tx) error {
				var err error
				evs, err = f.db.FinishResync(ctx, tx, f.account, inbox, f.now)
				return err
			})
			last := evs[len(evs)-1]
			if p := payload[store.FolderChanged](t, last); p.Change != tc.want {
				t.Fatalf("the resync ended with %q, want %q", p.Change, tc.want)
			}
			if from := f.str(`SELECT resync_from FROM folders WHERE id = ?`, inbox); from != "" {
				t.Errorf("resync_from = %q after the resync ended", from)
			}
		})
	}
}

func TestAResyncClaimsOutsideTheWindowButNeverInsertsThere(t *testing.T) {
	// What a resync fetches from beyond the folder's initial window is only
	// there to find the rows the index already held. One that claims nothing
	// is not stored: a resync must not widen what is kept.
	f := newIndex(t)
	ids := f.gmailFolders()
	inbox := ids["INBOX"]
	f.live(inbox, mail(1, "held"))
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := f.db.BeginResync(ctx, tx, f.account, inbox, provider.FolderStatus{UIDValidity: 8}, store.ResyncFromLive)
		return err
	})
	res := f.apply(store.SummaryBatch{
		FolderID: inbox, UIDValidity: 8, Mode: store.ApplyResync,
		Summaries: []provider.Summary{mail(10, "held"), mail(11, "never held")},
		ClaimOnly: map[imap.UID]bool{10: true, 11: true},
	})
	if res.Claimed != 1 || res.Inserted != 0 || res.Skipped != 1 {
		t.Fatalf("claim-only batch: %+v", res)
	}
	if n := f.int(`SELECT count(*) FROM messages WHERE subject = 'Subject never held'`); n != 0 {
		t.Fatal("a message outside the window was stored by the resync")
	}
}
