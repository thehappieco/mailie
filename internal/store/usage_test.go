package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/thehappieco/mailie/internal/store"
)

func TestUsageCountsEveryFolderCopyStillInItsFolder(t *testing.T) {
	// A Gmail message under a label is a row in INBOX and a row in the
	// label, and counts in both, size included; a row gone from its folder
	// counts nowhere.
	f := newIndex(t)
	folders := f.gmailFolders()
	big := mail(3, "big")
	big.Size = 50_000
	f.live(folders["INBOX"], mail(1, "labelled"), mail(2, "vanished"), big)
	f.live(folders["Receipts"], mail(1, "labelled"))
	f.exec(`UPDATE messages SET vanished_at = 5 WHERE folder_id = ? AND uid = 2`, folders["INBOX"])

	f.addAccount("acc_2", "usr_1")
	f.addAccount("acc_empty", "usr_1")
	f.account = "acc_2"
	other := f.gmailFolders()
	f.live(other["INBOX"], mail(1, "elsewhere"))

	got, err := f.db.Usage(context.Background(), []string{"acc_1", "acc_empty", "acc_nobody"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]store.MailboxUsage{"acc_1": {Messages: 3, Bytes: 1234 + 1234 + 50_000}}
	if len(got) != len(want) || got["acc_1"] != want["acc_1"] {
		t.Fatalf("Usage = %+v, want %+v: three live copies, nothing for a mailbox not asked about or empty", got, want)
	}
	if got, err := f.db.Usage(context.Background(), nil); err != nil || len(got) != 0 {
		t.Fatalf("Usage of nothing = %v, %v", got, err)
	}
}

func TestTheDatabaseSizeIsTheFileAndItsWriteAheadLog(t *testing.T) {
	f := newIndex(t)
	f.live(f.gmailFolders()["INBOX"], mail(1, "a"))
	got, err := f.db.DiskBytes()
	if err != nil {
		t.Fatal(err)
	}
	var want int64
	for _, p := range []string{f.db.Path(), f.db.Path() + "-wal"} {
		if info, err := os.Stat(p); err == nil {
			want += info.Size()
		}
	}
	if got != want || got == 0 {
		t.Fatalf("DiskBytes = %d, want %d", got, want)
	}
}
