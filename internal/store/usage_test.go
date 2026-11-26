package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/thehappieco/mailie/internal/store"
)

// ruleBytes is what the rows query selects hold under the usage rule,
// counted in Go from the values the driver hands back, never by the rule's
// own SQL: 8 for an integer or a real, a text's or a blob's length in bytes,
// nothing for NULL.
func (f *indexFixture) ruleBytes(query string, args ...any) int64 {
	f.t.Helper()
	rows, err := f.db.Reader().QueryContext(context.Background(), query, args...)
	if err != nil {
		f.t.Fatalf("query %q: %v", query, err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		f.t.Fatal(err)
	}
	var total int64
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			f.t.Fatal(err)
		}
		for i, v := range values {
			switch v := v.(type) {
			case nil:
			case int64, float64:
				total += 8
			case string:
				total += int64(len(v))
			case []byte:
				total += int64(len(v))
			default:
				f.t.Fatalf("%s holds a %T the rule does not know", columns[i], v)
			}
		}
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return total
}

// heldBytes is everything rule 1 counts for an account, summed in Go: its
// live message rows, their parts, its folders and its open drafts.
func (f *indexFixture) heldBytes(accountID string) int64 {
	f.t.Helper()
	return f.ruleBytes(`SELECT * FROM messages WHERE account_id = ? AND vanished_at = 0`, accountID) +
		f.ruleBytes(`SELECT p.* FROM parts p JOIN messages m ON m.id = p.msg_id
			WHERE m.account_id = ? AND m.vanished_at = 0`, accountID) +
		f.ruleBytes(`SELECT * FROM folders WHERE account_id = ?`, accountID) +
		f.ruleBytes(`SELECT * FROM drafts WHERE account_id = ? AND state = 'open'`, accountID)
}

func (f *indexFixture) usageOf(accountID string) store.MailboxUsage {
	f.t.Helper()
	got, err := f.db.Usage(context.Background(), []string{accountID})
	if err != nil {
		f.t.Fatal(err)
	}
	return got[accountID]
}

func TestUsageCountsEveryFolderCopyStillInItsFolder(t *testing.T) {
	// A Gmail message under a label is a row in INBOX and a row in the
	// label, and counts in both, its size on the server included; a row
	// gone from its folder counts nowhere.
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
	want := store.MailboxUsage{Messages: 3, Bytes: f.heldBytes("acc_1"), ProviderBytes: 1234 + 1234 + 50_000}
	if len(got) != 1 || got["acc_1"] != want || want.Bytes == 0 {
		t.Fatalf("Usage = %+v, want acc_1 alone, %+v: three live copies, nothing for a mailbox not asked about or empty", got, want)
	}
	if got, err := f.db.Usage(context.Background(), nil); err != nil || len(got) != 0 {
		t.Fatalf("Usage of nothing = %v, %v", got, err)
	}
}

func TestTheUsageRuleCountsExactlyTheBytesOfAMailboxsRows(t *testing.T) {
	f := newIndex(t)
	folders := f.gmailFolders()
	f.live(folders["INBOX"], mail(1, "kept"), mail(2, "gone"))
	f.live(folders["Receipts"], mail(1, "kept"))
	// Another mailbox's rows never count in this one's figure.
	f.addAccount("acc_2", "usr_1")
	f.account = "acc_2"
	f.live(f.gmailFolders()["INBOX"], mail(1, "elsewhere"))
	f.account = "acc_1"

	before := f.usageOf("acc_1")
	if before.Bytes != f.heldBytes("acc_1") || before.Messages != 3 {
		t.Fatalf("Usage = %+v, want every value of acc_1's rows, %d bytes", before, f.heldBytes("acc_1"))
	}

	// A part written by hand, value by value: its message's id and an
	// integer flag twice are 8 each, its size stored as a real 8 too, a
	// text its bytes in UTF-8 (café is five), a blob its length, NULL
	// nothing.
	kept := f.msgID(folders["INBOX"], 1)
	f.exec(`INSERT INTO parts(msg_id, path, mime_type, charset, encoding, disposition, filename, content_id, size,
		is_body, is_attachment, sha256) VALUES (?, '2', 'image/png', '', 'base64', 'attachment', 'café.png',
		X'0102030405', 2.5, 0, 1, NULL)`, kept)
	// msg_id 8, path 1, mime_type 9, charset 0, encoding 6, disposition 10,
	// filename 9 (é is two bytes), content_id 5, size 8, is_body 8,
	// is_attachment 8, sha256 0.
	const part int64 = 8 + 1 + 9 + 0 + 6 + 10 + 9 + 5 + 8 + 8 + 8 + 0
	withPart := f.usageOf("acc_1")
	if withPart.Bytes-before.Bytes != part || withPart.ProviderBytes != before.ProviderBytes {
		t.Fatalf("a part of %d bytes moved the figure by %d", part, withPart.Bytes-before.Bytes)
	}

	// An open draft counts; one sent does not.
	draft := []string{"drf_0000000000000001", "acc_1", `{"subject":"Olá"}`, "hash", "uuid@example.com", "open", "", ""}
	f.exec(`INSERT INTO drafts(id, account_id, compose_json, compose_hash, message_id_hdr, state, error, created_by,
		created_at, updated_at, sent_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, 1, 0, 1)`,
		draft[0], draft[1], draft[2], draft[3], draft[4], draft[5], draft[6], draft[7])
	f.exec(`INSERT INTO drafts(id, account_id, compose_json, compose_hash, message_id_hdr, state, created_at,
		updated_at, expires_at) VALUES ('drf_0000000000000002', 'acc_1', '{}', 'h', 'sent@example.com', 'sent', 1, 1, 1)`)
	open := int64(4 * 8)
	for _, v := range draft {
		open += int64(len(v))
	}
	withDraft := f.usageOf("acc_1")
	if withDraft.Bytes-withPart.Bytes != open {
		t.Fatalf("an open draft of %d bytes moved the figure by %d", open, withDraft.Bytes-withPart.Bytes)
	}

	// What the rule leaves out: sends, the events journal and a body
	// fetched on demand.
	gone := f.msgID(folders["INBOX"], 2)
	f.exec(`INSERT INTO sends(account_id, idempotency_key, compose_hash, message_id_hdr, state, created_at, updated_at)
		VALUES ('acc_1', 'k-1', 'h', 'm@example.com', 'sent', 1, 1)`)
	f.exec(`INSERT INTO events(type, account_id, payload_json, created_at) VALUES ('message.new', 'acc_1', '{"subject":"x"}', 1)`)
	f.exec(`INSERT INTO bodies(msg_id, headers_raw, text_html, size, fetched_at, last_access_at)
		VALUES (?, X'00', '<p>a long body</p>', 18, 1, 1)`, kept)
	if got := f.usageOf("acc_1"); got != withDraft {
		t.Fatalf("sends, events and bodies moved the figure: %+v, then %+v", withDraft, got)
	}

	// A row gone from its folder no longer counts, nor do its parts.
	vanishing := f.ruleBytes(`SELECT * FROM messages WHERE id = ?`, gone) +
		f.ruleBytes(`SELECT * FROM parts WHERE msg_id = ?`, gone)
	f.exec(`UPDATE messages SET vanished_at = 5 WHERE id = ?`, gone)
	// The row's own value changed with it (still an integer, still 8), and
	// so did its folder's counters.
	after := f.usageOf("acc_1")
	if after.Messages != 2 || after.ProviderBytes != withDraft.ProviderBytes-1234 || withDraft.Bytes-after.Bytes != vanishing {
		t.Fatalf("a vanished row of %d bytes: %+v, then %+v", vanishing, withDraft, after)
	}
	if after.Bytes != f.heldBytes("acc_1") {
		t.Fatalf("Usage = %d bytes, want %d", after.Bytes, f.heldBytes("acc_1"))
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
