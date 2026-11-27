package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
)

// What usage rule 1 counts of each mailbox, kept by UTC day as its rows
// change (migration 0030): the triggers move the totals in the transaction
// of every write, and nothing but those writes moves them.

// valueBytes is what one value counts under the rule, read in Go from what
// the driver hands back, never by the rule's own SQL: 8 for an integer or a
// real, a text's or a blob's length in bytes, nothing for NULL.
func valueBytes(t *testing.T, column string, v any) int64 {
	t.Helper()
	switch v := v.(type) {
	case nil:
		return 0
	case int64, float64:
		return 8
	case string:
		return int64(len(v))
	case []byte:
		return int64(len(v))
	default:
		t.Fatalf("%s holds a %T the rule does not know", column, v)
		return 0
	}
}

// tableRows reads every row query selects, each as its values by column and
// what the rule counts of them.
func tableRows(t *testing.T, db *store.Store, query string, args ...any) []countedRow {
	t.Helper()
	rows, err := db.Reader().QueryContext(t.Context(), query, args...)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []countedRow
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		row := countedRow{values: map[string]any{}}
		for i, v := range values {
			row.values[columns[i]] = v
			row.bytes += valueBytes(t, columns[i], v)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

type countedRow struct {
	values map[string]any
	bytes  int64
}

func (r countedRow) text(column string) string {
	if s, ok := r.values[column].(string); ok {
		return s
	}
	return fmt.Sprint(r.values[column])
}

func (r countedRow) int(column string) int64 {
	n, _ := r.values[column].(int64)
	return n
}

// floorDay is the UTC day of a unix time, rounded down: -1 is day -1.
func floorDay(unix int64) int64 {
	day := unix / 86400
	if unix%86400 < 0 {
		day--
	}
	return day
}

// totals is a database's bytes by mailbox: dated, by UTC day; fixed, its
// folders and open drafts. A total of 0 is left out, as a mailbox with no
// total at all.
type totals struct {
	days  map[string]map[int64]int64
	fixed map[string]int64
}

func (s totals) addDay(account string, day, bytes int64) {
	if s.days[account] == nil {
		s.days[account] = map[int64]int64{}
	}
	s.days[account][day] += bytes
}

// prune leaves out what sums to nothing.
func (s totals) prune() totals {
	for account, days := range s.days {
		maps.DeleteFunc(days, func(_, bytes int64) bool { return bytes == 0 })
		if len(days) == 0 {
			delete(s.days, account)
		}
	}
	maps.DeleteFunc(s.fixed, func(_ string, bytes int64) bool { return bytes == 0 })
	return s
}

func (s totals) of(account string) int64 {
	total := s.fixed[account]
	for _, bytes := range s.days[account] {
		total += bytes
	}
	return total
}

func (s totals) String() string {
	var lines []string
	for _, account := range slices.Sorted(maps.Keys(s.days)) {
		for _, day := range slices.Sorted(maps.Keys(s.days[account])) {
			lines = append(lines, fmt.Sprintf("%s day %d: %d", account, day, s.days[account][day]))
		}
	}
	for _, account := range slices.Sorted(maps.Keys(s.fixed)) {
		lines = append(lines, fmt.Sprintf("%s fixed: %d", account, s.fixed[account]))
	}
	return strings.Join(lines, "\n")
}

func (s totals) equal(o totals) bool {
	return maps.EqualFunc(s.days, o.days, maps.Equal) && maps.Equal(s.fixed, o.fixed)
}

// heldTotals is rule 1 counted in Go from every row the database holds.
func heldTotals(t *testing.T, db *store.Store) totals {
	t.Helper()
	out := totals{days: map[string]map[int64]int64{}, fixed: map[string]int64{}}
	type live struct {
		account string
		day     int64
	}
	messages := map[int64]live{}
	for _, m := range tableRows(t, db, `SELECT * FROM messages`) {
		if m.int("vanished_at") != 0 {
			continue
		}
		at := live{m.text("account_id"), floorDay(m.int("internal_date"))}
		messages[m.int("id")] = at
		out.addDay(at.account, at.day, m.bytes)
	}
	for _, p := range tableRows(t, db, `SELECT * FROM parts`) {
		if at, ok := messages[p.int("msg_id")]; ok {
			out.addDay(at.account, at.day, p.bytes)
		}
	}
	for _, f := range tableRows(t, db, `SELECT * FROM folders`) {
		out.fixed[f.text("account_id")] += f.bytes
	}
	for _, d := range tableRows(t, db, `SELECT * FROM drafts WHERE state = 'open'`) {
		out.fixed[d.text("account_id")] += d.bytes
	}
	return out.prune()
}

// keptTotals is what usage_days and usage_fixed keep, NULL read as 0.
func keptTotals(t *testing.T, db *store.Store) totals {
	t.Helper()
	out := totals{days: map[string]map[int64]int64{}, fixed: map[string]int64{}}
	for _, r := range tableRows(t, db, `SELECT account_id, day, coalesce(bytes, 0) AS bytes FROM usage_days`) {
		out.addDay(r.text("account_id"), r.int("day"), r.int("bytes"))
	}
	for _, r := range tableRows(t, db, `SELECT account_id, coalesce(bytes, 0) AS bytes FROM usage_fixed`) {
		out.fixed[r.text("account_id")] += r.int("bytes")
	}
	return out.prune()
}

// keptAreHeld checks that the kept totals are rule 1's, day by day, as Go
// counts the rows.
func keptAreHeld(t *testing.T, db *store.Store, step string) {
	t.Helper()
	held, kept := heldTotals(t, db), keptTotals(t, db)
	if !held.equal(kept) {
		t.Fatalf("after %s the totals kept are\n%v\nand the rows hold\n%v", step, kept, held)
	}
}

// totalsHold checks that the kept totals are rule 1's, day by day, as Go
// counts the rows, as CheckUsageTotals reads them (no mailbox drifted) and
// as Usage scans them (each mailbox's sum).
func totalsHold(t *testing.T, db *store.Store, step string) {
	t.Helper()
	keptAreHeld(t, db, step)
	kept := keptTotals(t, db)
	drifted, err := db.CheckUsageTotals(t.Context())
	if err != nil || len(drifted) != 0 {
		t.Fatalf("after %s the check finds %v drifted (%v)", step, drifted, err)
	}
	var accounts []string
	for _, r := range tableRows(t, db, `SELECT id FROM accounts ORDER BY id`) {
		accounts = append(accounts, r.text("id"))
	}
	usage, err := db.Usage(t.Context(), accounts)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range accounts {
		if got, want := kept.of(id), usage[id].Bytes; got != want {
			t.Fatalf("after %s %s keeps %d bytes and Usage scans %d", step, id, got, want)
		}
	}
}

// partsOf gives a summary more parts, one under a path it already has,
// which the index ignores.
func partsOf(sum provider.Summary) provider.Summary {
	sum.Parts = append(sum.Parts,
		provider.PartInfo{Path: []int{1}, MIMEType: "text/html", Size: 7, IsBody: true},
		provider.PartInfo{Path: []int{2}, MIMEType: "application/pdf", Disposition: "attachment", Filename: "relatório.pdf",
			ContentID: "cid-1", Size: 90_000, IsAttachment: true})
	return sum
}

func ptr[T any](v T) *T { return &v }

func TestTheKeptTotalsEqualTheUsageRuleAfterEveryWrite(t *testing.T) {
	f := newIndex(t)
	hold := func(step string) {
		t.Helper()
		totalsHold(t, f.db, step)
	}
	gmail := provider.ProfileFor(provider.KindGmail)
	listed := []provider.Folder{
		folder("INBOX"), folder("[Gmail]/Sent Mail", imap.MailboxAttrSent), folder("[Gmail]/Trash", imap.MailboxAttrTrash),
		folder("[Gmail]/All Mail", imap.MailboxAttrAll), folder("Receipts"),
	}

	// The folder upsert's insert path, and UpdateFolderSync.
	ids := f.gmailFolders()
	inbox, sent, receipts, all := ids["INBOX"], ids["[Gmail]/Sent Mail"], ids["Receipts"], ids["[Gmail]/All Mail"]
	hold("the folders listed")

	// insertRow and its parts, a part listed twice under one path ignored.
	f.live(inbox, mail(1, "a"), mail(2, "b"), partsOf(mail(3, "c")), mail(4, "d"))
	if n := f.int(`SELECT count(*) FROM parts WHERE msg_id = ?`, f.msgID(inbox, 3)); n != 2 {
		t.Fatalf("the message with a part listed twice has %d parts, want 2", n)
	}
	hold("new mail")

	// A label copy: settleGroup points it at its group's primary.
	f.live(receipts, mail(1, "a"), mail(2, "e"))
	primaryA := f.msgID(inbox, 1)
	if n := f.int(`SELECT count(*) FROM messages WHERE dup_of = ?`, primaryA); n != 1 {
		t.Fatalf("%d copies point at the inbox's row, want the label's", n)
	}
	hold("a label copy")

	// refreshRow on a live row: through a flag pass, and through a pass
	// finding a row it indexed already with other flags.
	f.flags(inbox, 0, provider.FlagUpdate{UID: 1, Flags: []imap.Flag{imap.FlagSeen, imap.FlagFlagged}})
	hold("a flag pass")
	f.live(inbox, mail(2, "b", imap.FlagSeen, imap.FlagAnswered))
	hold("a pass finding new flags")
	f.live(inbox, mail(2, "b", imap.FlagSeen, imap.FlagAnswered))
	hold("a pass finding the same flags")

	// ApplyUIDDiff tombstoning the primary, whose copy takes its place, and
	// reviving it.
	f.diff(inbox, 1, 2, 3, 4)
	if n := f.int(`SELECT count(*) FROM messages WHERE id = ? AND vanished_at <> 0`, primaryA); n != 1 {
		t.Fatal("the diff did not tombstone the row")
	}
	hold("a row gone from its folder once")
	f.diff(inbox, 1, 1, 2, 3, 4)
	hold("the row listed again")

	// refreshRow reviving a tombstone.
	f.diff(inbox, 1, 1, 3, 4)
	f.live(inbox, mail(2, "b", imap.FlagSeen))
	if n := f.int(`SELECT vanished_at FROM messages WHERE id = ?`, f.msgID(inbox, 2)); n != 0 {
		t.Fatal("the pass did not revive the row")
	}
	hold("a pass reviving a row")

	// Deleted on the second miss, its parts with it; then a primary whose
	// copy the foreign key's ON DELETE SET NULL changes.
	f.diff(inbox, 1, 1, 2, 4)
	f.diff(inbox, 1, 1, 2, 4)
	if f.int(`SELECT count(*) FROM messages WHERE folder_id = ? AND uid = 3`, inbox) != 0 {
		t.Fatal("the row missing twice is still there")
	}
	hold("a row deleted with its parts")
	if n := f.int(`SELECT count(*) FROM messages WHERE dup_of = ?`, primaryA); n != 1 {
		t.Fatalf("%d copies point at the primary about to go, want 1", n)
	}
	f.diff(inbox, 1, 2, 4)
	f.diff(inbox, 1, 2, 4)
	if f.int(`SELECT count(*) FROM messages WHERE id = ?`, primaryA) != 0 {
		t.Fatal("the primary missing twice is still there")
	}
	hold("a primary deleted under its copy")

	// A UIDVALIDITY change: BeginResync, a batch claiming and inserting, and
	// FinishResync deleting what nothing claimed.
	f.live(sent, mail(1, "s1"), mail(2, "s2"), mail(3, "s3"))
	f.folderPassed(sent, t0)
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := f.db.BeginResync(ctx, tx, f.account, sent, provider.FolderStatus{UIDValidity: 8, UIDNext: 100},
			store.ResyncFromLive)
		return err
	})
	hold("a resync begun")
	late := mail(52, "s4")
	late.InternalDate = t0.Add(time.Minute)
	f.apply(store.SummaryBatch{FolderID: sent, UIDValidity: 8, Mode: store.ApplyResync,
		Summaries: []provider.Summary{mail(50, "s1"), mail(51, "s2", imap.FlagSeen), late}})
	hold("a resync batch")
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := f.db.FinishResync(ctx, tx, f.account, sent, f.now)
		return err
	})
	hold("a resync finished")

	// The folder upsert's DO UPDATE path, a role let go of on the way, and
	// another pass's sync positions and texts.
	moved := slices.Clone(listed)
	moved[4] = folder("Receipts", imap.MailboxAttrHasNoChildren)
	f.discover(gmail, map[string]string{"trash": "Receipts"}, moved...)
	hold("a folder listed with new attributes and a role moved")
	f.discover(gmail, nil, listed...)
	hold("the role moved back")
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return store.UpdateFolderSync(ctx, tx, receipts, store.FolderSync{
			SyncState: ptr("live"), SyncError: ptr("a connection reset after a long wait"), LastSyncedAt: ptr(t0),
		})
	})
	hold("a folder's sync written")
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return store.UpdateFolderSync(ctx, tx, receipts, store.FolderSync{SyncError: ptr("")})
	})
	hold("a folder's error cleared")

	// A folder no longer selectable loses its rows and its sync position
	// (unsyncFolderTx); one gone from the list twice goes with its rows
	// (deleteFolderTx).
	projects := folder("Projects")
	f.discover(gmail, nil, append(slices.Clone(listed), projects)...)
	projectsID := f.int(`SELECT id FROM folders WHERE account_id = ? AND name = 'Projects'`, f.account)
	f.setUIDValidity(projectsID, 7)
	f.live(projectsID, mail(1, "p1"), partsOf(mail(2, "d")))
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return store.UpdateFolderSync(ctx, tx, projectsID, store.FolderSync{SyncState: ptr("live"), SyncError: ptr("slow")})
	})
	hold("a person's folder indexed")
	unselectable := provider.Folder{Name: "Projects", Delim: '/', Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}}
	if res := f.discover(gmail, nil, append(slices.Clone(listed), unselectable)...); len(res.Unsynced) != 1 {
		t.Fatalf("the folder no longer selectable: %+v", res)
	}
	hold("a folder no longer synced")
	f.live(receipts, mail(3, "r3"), partsOf(mail(4, "r4")))
	f.discover(gmail, nil, append(slices.Clone(listed[:4]), unselectable)...)
	if res := f.discover(gmail, nil, append(slices.Clone(listed[:4]), unselectable)...); len(res.Removed) != 1 {
		t.Fatalf("the folder gone twice: %+v", res)
	}
	hold("a folder gone from the list twice")
	f.discover(gmail, nil, listed...)
	receipts = f.int(`SELECT id FROM folders WHERE account_id = ? AND name = 'Receipts'`, f.account)
	f.setUIDValidity(receipts, 7)
	hold("the folder listed again")

	// ApplyActionMove: a row a pass raced into the destination gives way
	// to the moved one, and a move into a folder not synced takes the row
	// out of the index. ApplyActionReturn indexes it again, and
	// ApplyActionFlags writes what a STORE echoed.
	f.live(receipts, mail(3, "m"))
	f.live(inbox, mail(8, "m"))
	mover := f.msgID(receipts, 3)
	res := f.move(store.ActionMove{FromFolderID: receipts, ToFolderID: inbox, DestUIDValidity: 7,
		Moves: []store.MovedRow{{ID: mover, UID: 3, DestUID: 8}}})
	if len(res.Kept) != 1 || f.msgID(inbox, 8) != mover {
		t.Fatalf("the move onto a raced row: %+v", res)
	}
	hold("a move onto a row a pass raced in")
	archived := f.msgID(inbox, 2)
	res = f.move(store.ActionMove{FromFolderID: inbox, ToFolderID: all, DestUIDValidity: 41,
		Moves: []store.MovedRow{{ID: archived, UID: 2, DestUID: 700}}})
	if len(res.Removed) != 1 {
		t.Fatalf("the move into a folder not synced: %+v", res)
	}
	hold("a move out of the index")
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, _, err := f.db.ApplyActionReturn(ctx, tx, store.ActionReturn{AccountID: f.account, LeftID: archived,
			FromFolderID: all, ToFolderID: inbox, UIDValidity: 7, Summary: mail(9, "b")})
		return err
	})
	hold("a message moved back")
	f.actionFlags(store.ActionFlags{FolderID: inbox, Shared: true,
		Updates: []provider.FlagUpdate{{UID: 9, Flags: []imap.Flag{imap.FlagSeen, imap.FlagFlagged, "$Label1"}}}})
	hold("an action's flags")

	// An open draft is fixed, and stays when the index goes (ForgetIndexTx):
	// a turn-off deletes the messages, their parts and the folders.
	f.exec(`INSERT INTO drafts(id, account_id, compose_json, compose_hash, message_id_hdr, state, created_at, updated_at,
		expires_at) VALUES ('drf_0000000000000001', ?, '{"subject":"Olá"}', 'h', 'd1@example.com', 'open', 1, 1, 9)`, f.account)
	hold("an open draft")
	f.write(func(ctx context.Context, tx *sql.Tx) error { return store.ForgetIndexTx(ctx, tx, []string{f.account}) })
	if kept := keptTotals(t, f.db); len(kept.days[f.account]) != 0 || kept.fixed[f.account] == 0 {
		t.Fatalf("after the index went the account keeps %v", kept)
	}
	hold("the index forgotten")

	// Then raw SQL, as no code writes it today: every table, drafts
	// changing state, a part's size a real and its content id a blob,
	// liveness set by hand, rows moving between mailboxes and days, and
	// several columns changed at once.
	f.addAccount("acc_2", "")
	rawWrites(t, f, rand.New(rand.NewPCG(30, 2026)), []string{"acc_1", "acc_2"}, 200, eachInGo(t, f.db, 25))
	hold("the raw writes")
}

// eachInGo checks the totals after each write as Go counts them, and every
// n-th write as the check and Usage read them too.
func eachInGo(t *testing.T, db *store.Store, n int) func(string) {
	writes := 0
	return func(step string) {
		t.Helper()
		writes++
		if writes%n == 0 {
			totalsHold(t, db, step)
			return
		}
		keptAreHeld(t, db, step)
	}
}

// rawWrites runs n writes of raw SQL over the four tables the rule counts, of
// two accounts, chosen by r, and calls after once each is done.
func rawWrites(t *testing.T, f *indexFixture, r *rand.Rand, accounts []string, n int, after func(string)) {
	t.Helper()
	texts := []string{"", "a", "café", "日本語の件名", "🙂 emoji", strings.Repeat("long ", 40), `["\\seen","$label"]`}
	text := func() string { return texts[r.IntN(len(texts))] }
	seq := 0
	next := func() int { seq++; return seq }
	pick := func(query string) (int64, bool) {
		ids := tableRows(t, f.db, query)
		if len(ids) == 0 {
			return 0, false
		}
		row := ids[r.IntN(len(ids))]
		for _, v := range row.values {
			id, _ := v.(int64)
			return id, true
		}
		return 0, false
	}
	pickText := func(query string) (string, bool) {
		rows := tableRows(t, f.db, query)
		if len(rows) == 0 {
			return "", false
		}
		for _, v := range rows[r.IntN(len(rows))].values {
			return fmt.Sprint(v), true
		}
		return "", false
	}
	// A day near 1970 on either side, today's, or one ahead.
	date := func() int64 {
		switch r.IntN(4) {
		case 0:
			return int64(r.IntN(400_000)) - 200_000
		case 1:
			return t0.Unix() + int64(r.IntN(200_000))
		case 2:
			return -1
		default:
			return t0.Add(400*24*time.Hour).Unix() + int64(r.IntN(86_400))
		}
	}
	addFolder := func() int64 {
		k := next()
		f.exec(`INSERT INTO folders(account_id, name, display_name, attrs_json, sync_error) VALUES (?, ?, ?, ?, ?)`,
			accounts[r.IntN(2)], fmt.Sprintf("raw-%d", k), text(), text(), text())
		return f.int(`SELECT max(id) FROM folders`)
	}
	ops := []struct {
		name string
		run  func()
	}{
		{"a folder inserted", func() { addFolder() }},
		{"a message inserted", func() {
			folderID, ok := pick(`SELECT id FROM folders`)
			if !ok {
				folderID = addFolder()
			}
			var dup any
			if id, ok := pick(`SELECT id FROM messages`); ok && r.IntN(3) == 0 {
				dup = id
			}
			vanished := 0
			if r.IntN(4) == 0 {
				vanished = 900
			}
			f.exec(`INSERT INTO messages(account_id, folder_id, uidvalidity, uid, message_id, group_key, dup_of, subject,
				from_json, flags_json, snippet, internal_date, size, vanished_at, first_seen_at, updated_at, body_text)
				VALUES (?, ?, 7, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 1, ?)`,
				accounts[r.IntN(2)], folderID, next(), text(), "g:"+text(), dup, text(), text(), text(), text(), date(),
				r.IntN(5000), vanished, text())
		}},
		{"a part inserted", func() {
			id, ok := pick(`SELECT id FROM messages`)
			if !ok {
				return
			}
			var size any = r.IntN(9000)
			if r.IntN(3) == 0 {
				size = 2.5
			}
			var contentID any = text()
			if r.IntN(3) == 0 {
				contentID = []byte{1, 2, 3, 4, 5}
			}
			var sha any
			if r.IntN(2) == 0 {
				sha = "aa11bb22"
			}
			f.exec(`INSERT INTO parts(msg_id, path, mime_type, filename, content_id, size, sha256) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				id, fmt.Sprintf("p%d", next()), "text/plain", text(), contentID, size, sha)
		}},
		{"a message's texts changed", func() {
			if id, ok := pick(`SELECT id FROM messages`); ok {
				f.exec(`UPDATE messages SET flags_json = ?, subject = ?, from_text = ? WHERE id = ?`, text(), text(), text(), id)
			}
		}},
		{"a message's liveness set by hand", func() {
			if id, ok := pick(`SELECT id FROM messages`); ok {
				f.exec(`UPDATE messages SET vanished_at = CASE vanished_at WHEN 0 THEN 5 ELSE 0 END WHERE id = ?`, id)
			}
		}},
		{"a message's copy pointer changed", func() {
			id, ok := pick(`SELECT id FROM messages`)
			if !ok {
				return
			}
			var dup any
			if other, ok := pick(`SELECT id FROM messages`); ok && other != id && r.IntN(2) == 0 {
				dup = other
			}
			f.exec(`UPDATE messages SET dup_of = ? WHERE id = ?`, dup, id)
		}},
		{"a message moved to another mailbox or day", func() {
			if id, ok := pick(`SELECT id FROM messages`); ok {
				f.exec(`UPDATE messages SET account_id = ?, internal_date = ? WHERE id = ?`, accounts[r.IntN(2)], date(), id)
			}
		}},
		{"several of a message's columns changed at once", func() {
			if id, ok := pick(`SELECT id FROM messages`); ok {
				f.exec(`UPDATE messages SET vanished_at = ?, flags_json = ?, internal_date = internal_date + ?,
					account_id = ? WHERE id = ?`, r.IntN(2)*7, text(), r.IntN(3)*86_400, accounts[r.IntN(2)], id)
			}
		}},
		{"a message deleted", func() {
			if id, ok := pick(`SELECT id FROM messages`); ok {
				f.exec(`DELETE FROM messages WHERE id = ?`, id)
			}
		}},
		{"a part deleted", func() {
			if id, ok := pick(`SELECT rowid FROM parts`); ok {
				f.exec(`DELETE FROM parts WHERE rowid = ?`, id)
			}
		}},
		{"a part changed", func() {
			id, ok := pick(`SELECT rowid FROM parts`)
			if !ok {
				return
			}
			var sha any
			if r.IntN(2) == 0 {
				sha = text()
			}
			to, ok := pick(`SELECT id FROM messages`)
			if !ok {
				return
			}
			f.exec(`UPDATE parts SET filename = ?, sha256 = ?, size = ?, msg_id = ? WHERE rowid = ?`,
				text(), sha, []any{1, 3.75, "seven"}[r.IntN(3)], to, id)
		}},
		{"a folder changed", func() {
			if id, ok := pick(`SELECT id FROM folders`); ok {
				f.exec(`UPDATE folders SET display_name = ?, sync_error = ?, account_id = ? WHERE id = ?`,
					text(), text(), accounts[r.IntN(2)], id)
			}
		}},
		{"a folder's counters changed", func() {
			if id, ok := pick(`SELECT id FROM folders`); ok {
				f.exec(`UPDATE folders SET local_count = local_count + 1, unseen_count = 3 WHERE id = ?`, id)
			}
		}},
		{"a folder deleted with its messages", func() {
			if r.IntN(3) != 0 {
				return
			}
			if id, ok := pick(`SELECT id FROM folders`); ok {
				f.exec(`DELETE FROM folders WHERE id = ?`, id)
			}
		}},
		{"a draft inserted", func() {
			f.exec(`INSERT INTO drafts(id, account_id, compose_json, compose_hash, message_id_hdr, state, error, created_at,
				updated_at, expires_at) VALUES (?, ?, ?, 'h', 'm@example.com', ?, ?, 1, 1, 9)`,
				fmt.Sprintf("drf_raw%d", next()), accounts[r.IntN(2)], text(), []string{"open", "sending", "sent", "failed"}[r.IntN(4)],
				text())
		}},
		{"a draft's state or text changed", func() {
			if id, ok := pickText(`SELECT id FROM drafts`); ok {
				f.exec(`UPDATE drafts SET state = ?, compose_json = ?, account_id = ? WHERE id = ?`,
					[]string{"open", "sending", "sent", "failed"}[r.IntN(4)], text(), accounts[r.IntN(2)], id)
			}
		}},
		{"a draft deleted", func() {
			if id, ok := pickText(`SELECT id FROM drafts`); ok {
				f.exec(`DELETE FROM drafts WHERE id = ?`, id)
			}
		}},
	}
	for i := range n {
		op := ops[r.IntN(len(ops))]
		op.run()
		after(fmt.Sprintf("raw write %d, %s", i, op.name))
	}
}

func TestTheUsageTriggersMarkNoWorkspaceOrPerson(t *testing.T) {
	// A report measures what a sync writes (0027): a usage trigger that
	// marked would send every snapshot again at every sync.
	f := newFollowedIndex(t)
	f.exec(`INSERT INTO accounts(id, workspace_id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port,
		smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at, owner_user_id)
		VALUES ('acc_2', 'ext_personal', 'acc_2@example.com', 'imap', 'password', 'imap.example.com', 993,
		'smtp.example.com', 465, 'implicit', 'acc_2@example.com', 0, 'active', 1, 1, 1, 'usr_1')`)
	ws, person := f.marks()
	if ws == 0 || person == 0 {
		t.Fatalf("the seed marked the workspace %d and the person %d; the test would prove nothing", ws, person)
	}
	totals := eachInGo(t, f.db, 25)
	same := func(step string) {
		t.Helper()
		if w, p := f.marks(); w != ws || p != person {
			t.Fatalf("%s marked the workspace %d (was %d) and the person %d (was %d)", step, w, ws, p, person)
		}
		totals(step)
	}
	ids := f.gmailFolders()
	f.live(ids["INBOX"], mail(1, "a"), partsOf(mail(2, "b")))
	f.live(ids["Receipts"], mail(1, "a"))
	f.flags(ids["INBOX"], 0, provider.FlagUpdate{UID: 1, Flags: []imap.Flag{imap.FlagSeen}})
	f.diff(ids["INBOX"], 1, 2)
	f.diff(ids["INBOX"], 1, 2)
	same("a sync's writes")
	rawWrites(t, f, rand.New(rand.NewPCG(30, 27)), []string{"acc_1", "acc_2"}, 120, same)
	totalsHold(t, f.db, "the raw writes")

	var marking []string
	for _, r := range tableRows(t, f.db, `SELECT name FROM sqlite_schema WHERE type = 'trigger' AND name LIKE 'usage\_%' ESCAPE '\'
		AND (sql LIKE '%workspace_changes%' OR sql LIKE '%person_changes%')`) {
		marking = append(marking, r.text("name"))
	}
	if n := f.int(`SELECT count(*) FROM sqlite_schema WHERE type = 'trigger' AND name LIKE 'usage\_%' ESCAPE '\'`); n == 0 ||
		len(marking) != 0 {
		t.Errorf("of the %d usage triggers, %v write a mark", n, marking)
	}
}

func TestAMailboxsBytesAreDatedByTheUTCDayOfItsInternalDate(t *testing.T) {
	f := newIndex(t)
	inbox := f.gmailFolders()["INBOX"]
	utcDay := func(year int, month time.Month, day int) int64 {
		return time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Unix() / 86400
	}
	// The same evening in two zones: 23:30 at -03:00 is 02:30 of the 15th
	// in UTC, and 01:30 at +03:00 is 22:30 of the 14th. A count of what is
	// dated from 2026-07-15 on, in UTC (a window of 90 days measured on
	// 2026-10-13), takes the first and leaves the second. Before 1970 a
	// second is a day earlier, never day 0: floor, not truncation.
	cases := []struct {
		key string
		at  time.Time
		day int64
	}{
		{"west", time.Date(2026, 7, 14, 23, 30, 0, 0, time.FixedZone("-03:00", -3*3600)), utcDay(2026, 7, 15)},
		{"east", time.Date(2026, 7, 15, 1, 30, 0, 0, time.FixedZone("+03:00", 3*3600)), utcDay(2026, 7, 14)},
		{"midnight", time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC), utcDay(2026, 10, 13)},
		{"future", time.Date(2027, 3, 1, 23, 59, 59, 0, time.UTC), utcDay(2027, 3, 1)},
		{"epoch", time.Unix(0, 0), 0},
		{"second-before-epoch", time.Unix(-1, 0), -1},
		{"day-and-a-second-before-epoch", time.Unix(-86401, 0), -2},
	}
	for i, c := range cases {
		f.live(inbox, partsOf(dated(mail(imap.UID(i+1), c.key), c.at)))
	}
	kept := keptTotals(t, f.db).days[f.account]
	for i, c := range cases {
		id := f.msgID(inbox, imap.UID(i+1))
		want := f.ruleBytes(`SELECT * FROM messages WHERE id = ?`, id) + f.ruleBytes(`SELECT * FROM parts WHERE msg_id = ?`, id)
		if kept[c.day] != want || want == 0 {
			t.Errorf("%s (%s) keeps %d bytes on day %d, want its %d", c.key, c.at, kept[c.day], c.day, want)
		}
	}
	if len(kept) != len(cases) {
		t.Errorf("the mailbox keeps %d days, want one per message: %v", len(kept), kept)
	}
	totalsHold(t, f.db, "dated mail")
}

func TestADriftedTotalNeverStopsAWriteAndIsRepaired(t *testing.T) {
	f := newIndex(t)
	ids := f.gmailFolders()
	f.live(ids["INBOX"], mail(1, "a"), partsOf(mail(2, "b")))
	f.addAccount("acc_2", "usr_1")
	f.account = "acc_2"
	other := f.gmailFolders()
	f.live(other["INBOX"], mail(1, "c"), partsOf(mail(2, "d")))
	totalsHold(t, f.db, "two mailboxes indexed")
	drifted := func(want ...string) {
		t.Helper()
		got, err := f.db.CheckUsageTotals(t.Context())
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("the check finds %v drifted (%v), want %v", got, err, want)
		}
	}
	repairs := func(step string, want int) {
		t.Helper()
		n, err := f.db.ReconcileUsageTotals(t.Context())
		if err != nil || n != want {
			t.Fatalf("after %s the reconcile repaired %d mailboxes (%v), want %d", step, n, err, want)
		}
		totalsHold(t, f.db, step)
	}
	// A day too high, another below zero, the fixed total NULL: nothing
	// refuses a delta that moves them further.
	drift := func() {
		t.Helper()
		f.exec(`UPDATE usage_days SET bytes = bytes + 1000 WHERE account_id = 'acc_2'`)
		f.exec(`INSERT INTO usage_days(account_id, day, bytes) VALUES ('acc_2', -5, -7)
			ON CONFLICT DO UPDATE SET bytes = -7`)
		f.exec(`UPDATE usage_fixed SET bytes = NULL WHERE account_id = 'acc_2'`)
	}

	// A sync's writes go through, and the reconcile repairs that mailbox
	// alone.
	drift()
	drifted("acc_2")
	f.live(other["INBOX"], mail(3, "e"))
	f.flags(other["INBOX"], 0, provider.FlagUpdate{UID: 1, Flags: []imap.Flag{imap.FlagSeen}})
	f.diff(other["INBOX"], 1, 2, 3)
	drifted("acc_2")
	repairs("a sync over drifted totals", 1)

	// So does a turn-off, which deletes the index.
	drift()
	f.write(func(ctx context.Context, tx *sql.Tx) error { return store.ForgetIndexTx(ctx, tx, []string{"acc_2"}) })
	repairs("a turn-off over drifted totals", 1)

	// And a deletion for good, which leaves nothing of the mailbox.
	drift()
	f.exec(`DELETE FROM accounts WHERE id = 'acc_2'`)
	if n := f.int(`SELECT (SELECT count(*) FROM usage_days WHERE account_id = 'acc_2')
		+ (SELECT count(*) FROM usage_fixed WHERE account_id = 'acc_2')`); n != 0 {
		t.Fatalf("the mailbox deleted for good keeps %d rows of totals", n)
	}
	repairs("a deletion for good over drifted totals", 0)

	// Rows of a mailbox that is not there, and a total kept under the wrong
	// day, are found and repaired too.
	f.exec(`INSERT INTO usage_days(account_id, day, bytes) VALUES ('acc_gone', 1, 5)`)
	f.exec(`INSERT INTO usage_fixed(account_id, bytes) VALUES ('acc_gone', 9)`)
	f.exec(`UPDATE usage_days SET day = day + 1 WHERE account_id = 'acc_1'`)
	drifted("acc_1", "acc_gone")
	repairs("a mailbox gone and a total on the wrong day", 2)
	if n := f.int(`SELECT count(*) FROM usage_days WHERE account_id = 'acc_gone'`); n != 0 {
		t.Errorf("the mailbox that is not there keeps %d days", n)
	}
}

func TestTheDupOfIndexServesTheCopiesLookup(t *testing.T) {
	// Every delete of a message runs its ON DELETE SET NULL by this lookup:
	// without the index, a scan of every message per row deleted.
	f := newIndex(t)
	var plan []string
	for _, r := range tableRows(t, f.db, `EXPLAIN QUERY PLAN SELECT id FROM messages WHERE dup_of = ?`, 1) {
		plan = append(plan, r.text("detail"))
	}
	if !slices.ContainsFunc(plan, func(step string) bool { return strings.Contains(step, "INDEX messages_dup_of (dup_of=?)") }) {
		t.Errorf("the copies of a message are found by %v, want the index messages_dup_of", plan)
	}
}
