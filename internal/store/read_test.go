package store_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
)

// receivedAt is mail(uid, key) received at a given time.
func receivedAt(uid imap.UID, key string, at time.Time, flags ...imap.Flag) provider.Summary {
	sum := mail(uid, key, flags...)
	sum.InternalDate = at
	sum.Envelope.Date = at
	return sum
}

// search runs a query over the fixture's account and returns the subjects,
// in the order listed.
func (f *indexFixture) search(q store.MessageQuery) []string {
	f.t.Helper()
	if q.AccountIDs == nil {
		q.AccountIDs = []string{f.account}
	}
	if q.Limit == 0 {
		q.Limit = 100
	}
	rows, err := f.db.SearchMessages(context.Background(), q)
	if err != nil {
		f.t.Fatalf("SearchMessages(%+v): %v", q, err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, strings.TrimPrefix(r.Subject, "Subject "))
	}
	return out
}

func (f *indexFixture) match(text string) []string {
	f.t.Helper()
	expr, err := store.MatchQuery(text)
	if err != nil {
		f.t.Fatalf("MatchQuery(%q): %v", text, err)
	}
	return f.search(store.MessageQuery{Match: expr})
}

func TestSearchPagesStayStableWhileMailArrives(t *testing.T) {
	// A person scrolls a listing while new mail arrives and the initial sync
	// is still filling in older mail. Continuing from the last row seen must
	// neither repeat a row nor skip one that was already there.
	f := newIndex(t)
	inbox := f.gmailFolders()["INBOX"]
	var sums []provider.Summary
	for i := range 7 {
		// Two share each instant, so the id breaks the tie.
		at := t0.Add(-time.Duration(i/2) * time.Hour)
		sums = append(sums, receivedAt(imap.UID(10+i), string(rune('a'+i)), at))
	}
	f.live(inbox, sums...)
	before := f.search(store.MessageQuery{})

	var seen []string
	var after *store.Cursor
	for page := 0; ; page++ {
		rows, err := f.db.SearchMessages(context.Background(), store.MessageQuery{
			AccountIDs: []string{f.account}, Limit: 3, After: after,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			seen = append(seen, strings.TrimPrefix(r.Subject, "Subject "))
		}
		if len(rows) < 3 {
			break
		}
		last := rows[len(rows)-1]
		after = &store.Cursor{InternalDate: last.InternalDate, ID: last.ID}
		if page == 0 {
			// Between two pages: one message newer than everything, one as
			// old as the page boundary, one older than everything.
			f.live(inbox,
				receivedAt(30, "newest", t0.Add(time.Hour)),
				receivedAt(31, "tie", time.Unix(last.InternalDate, 0)),
				receivedAt(32, "oldest", t0.Add(-48*time.Hour)))
		}
	}
	for _, want := range before {
		if n := countOf(seen, want); n != 1 {
			t.Errorf("%q was listed %d times across the pages, want once: %v", want, n, seen)
		}
	}
	if slices.Contains(seen, "newest") {
		t.Errorf("mail newer than the first page appeared on a later one: %v", seen)
	}
	if !slices.Contains(seen, "oldest") {
		t.Errorf("mail older than the boundary, indexed meanwhile, never appeared: %v", seen)
	}
}

func countOf(list []string, s string) int {
	n := 0
	for _, v := range list {
		if v == s {
			n++
		}
	}
	return n
}

func TestTypedSearchTextCannotBeAnFTSOperator(t *testing.T) {
	// Whatever a person types is words to find, never FTS5 syntax: no
	// operator, column filter, grouping or quote may change what the query
	// means or make it fail.
	f := newIndex(t)
	inbox := f.gmailFolders()["INBOX"]
	f.live(inbox, mail(1, "Invoice"), mail(2, "Lunch"), mail(3, "NOT"), mail(4, "Ação"))

	for _, c := range []struct {
		typed string
		want  []string
	}{
		{`invoice`, []string{"Invoice"}},
		{`INV`, []string{"Invoice"}}, // the last word is a prefix
		{`invoice OR lunch`, nil},    // OR is a word to find, not an operator
		{`NOT`, []string{"NOT"}},
		{`lunch NOT`, nil},
		{`"invoice`, []string{"Invoice"}},
		{`in"voice`, nil},
		{`subject:lunch`, []string{"Lunch"}}, // not a column filter
		{`{subject} : lunch`, []string{"Lunch"}},
		{`body_text:x`, nil},
		{`NEAR(invoice lunch)`, nil},
		{`(invoice`, []string{"Invoice"}},
		{`invoice)`, []string{"Invoice"}},
		{`^invoice`, []string{"Invoice"}},
		{`inv*`, []string{"Invoice"}},
		{`*`, []string{"Ação", "NOT", "Lunch", "Invoice"}}, // nothing searchable: no text filter
		{`- + " ' ; --`, []string{"Ação", "NOT", "Lunch", "Invoice"}},
		{`acao`, []string{"Ação"}}, // diacritics folded, as the index folds them
		{`sender-lunch@example.com`, []string{"Lunch"}},
		{"lunch\x00\u202e", []string{"Lunch"}},
		{`'); DROP TABLE messages; --`, nil},
	} {
		got := f.match(c.typed)
		if !slices.Equal(got, c.want) && (len(got) != 0 || len(c.want) != 0) {
			t.Errorf("searching %q found %v, want %v", c.typed, got, c.want)
		}
	}
	if n := f.int(`SELECT count(*) FROM messages`); n != 4 {
		t.Fatalf("the index lost rows: %d", n)
	}
}

func TestSearchTextThatIsTooLongIsRefusedNotCut(t *testing.T) {
	// Ignoring part of what was typed would show more than was asked for.
	if _, err := store.MatchQuery(strings.Repeat("a", store.MaxMatchRunes+1)); !errors.Is(err, store.ErrBadMatch) {
		t.Errorf("an over-long search = %v, want ErrBadMatch", err)
	}
	if _, err := store.MatchQuery(strings.Repeat("word ", store.MaxMatchTerms+1)); !errors.Is(err, store.ErrBadMatch) {
		t.Errorf("too many words = %v, want ErrBadMatch", err)
	}
	if expr, err := store.MatchQuery(strings.Repeat("word ", store.MaxMatchTerms)); err != nil || expr == "" {
		t.Errorf("exactly the limit = %q, %v", expr, err)
	}
}

func TestSearchNeverLooksAtABody(t *testing.T) {
	// The index has no body to search, and the query is limited to what it
	// has: a body_text written by anything would still not be matched.
	f := newIndex(t)
	inbox := f.gmailFolders()["INBOX"]
	f.live(inbox, mail(1, "a"))
	f.exec(`UPDATE messages SET body_text = 'zanzibar'`)
	if got := f.match("zanzibar"); len(got) != 0 {
		t.Fatalf("a body word matched %v", got)
	}
}

func TestASearchListsEachMessageOnceUnlessAFolderIsNamed(t *testing.T) {
	// Gmail shows a message once per label. A listing of everything shows it
	// once; a listing of one label shows that label's copy.
	f := newIndex(t)
	folders := f.gmailFolders()
	f.live(folders["INBOX"], mail(1, "labelled"), mail(2, "plain"))
	f.live(folders["Receipts"], mail(1, "labelled"))

	if got := f.search(store.MessageQuery{}); len(got) != 2 {
		t.Fatalf("everything listed %v, want each message once", got)
	}
	if got := f.search(store.MessageQuery{FolderID: folders["Receipts"]}); !slices.Equal(got, []string{"labelled"}) {
		t.Fatalf("the label listed %v", got)
	}
	labelled := f.msgID(folders["INBOX"], 1)
	copies, err := f.db.CopyCounts(context.Background(), []int64{labelled, f.msgID(folders["INBOX"], 2)})
	if err != nil {
		t.Fatal(err)
	}
	if copies[labelled] != 1 || copies[f.msgID(folders["INBOX"], 2)] != 0 {
		t.Fatalf("copies = %v, want 1 for the labelled message and 0 for the other", copies)
	}
}

func TestVanishedAndStaleRowsAreNeverListed(t *testing.T) {
	// A tombstoned row is gone from its folder until a diff confirms or
	// revives it; a stale one waits for a resync to find its new UID. Neither
	// can be opened as indexed.
	f := newIndex(t)
	inbox := f.gmailFolders()["INBOX"]
	f.live(inbox, mail(1, "live"), mail(2, "vanished"), mail(3, "stale"))
	f.exec(`UPDATE messages SET vanished_at = 5 WHERE uid = 2`)
	f.exec(`UPDATE messages SET stale = 1 WHERE uid = 3`)
	for _, q := range []store.MessageQuery{{}, {FolderID: inbox}} {
		if got := f.search(q); !slices.Equal(got, []string{"live"}) {
			t.Errorf("%+v listed %v, want only the live row", q, got)
		}
	}
}

func TestSearchFiltersBySenderFlagsAttachmentsAndDate(t *testing.T) {
	f := newIndex(t)
	inbox := f.gmailFolders()["INBOX"]
	withFile := receivedAt(3, "file", t0.Add(-72*time.Hour))
	withFile.Parts = append(withFile.Parts, provider.PartInfo{
		Path: []int{2}, MIMEType: "application/pdf", Encoding: "base64", Size: 900, IsAttachment: true,
	})
	f.live(inbox,
		receivedAt(1, "read", t0.Add(-time.Hour), imap.FlagSeen),
		receivedAt(2, "starred", t0.Add(-2*time.Hour), imap.FlagFlagged),
		withFile)

	yes, no := true, false
	for _, c := range []struct {
		q    store.MessageQuery
		want []string
	}{
		{store.MessageQuery{Seen: &no}, []string{"starred", "file"}},
		{store.MessageQuery{Seen: &yes}, []string{"read"}},
		{store.MessageQuery{Flagged: &yes}, []string{"starred"}},
		{store.MessageQuery{HasAttachments: &yes}, []string{"file"}},
		{store.MessageQuery{From: "sender-star"}, []string{"starred"}},
		{store.MessageQuery{From: "SENDER READ"}, []string{"read"}},
		{store.MessageQuery{From: "%"}, nil},
		{store.MessageQuery{From: "_"}, nil},
		{store.MessageQuery{Since: t0.Add(-2 * time.Hour).Unix()}, []string{"read", "starred"}},
		{store.MessageQuery{Until: t0.Add(-2 * time.Hour).Unix()}, []string{"file"}},
	} {
		got := f.search(c.q)
		if !slices.Equal(got, c.want) && (len(got) != 0 || len(c.want) != 0) {
			t.Errorf("%+v listed %v, want %v", c.q, got, c.want)
		}
	}
	if got := f.search(store.MessageQuery{AccountIDs: []string{"acc_other"}}); len(got) != 0 {
		t.Errorf("another account's search found %v", got)
	}
}

func TestPartsComeBackInSectionOrder(t *testing.T) {
	f := newIndex(t)
	inbox := f.gmailFolders()["INBOX"]
	sum := mail(1, "parts")
	sum.Parts = []provider.PartInfo{
		{Path: []int{10}, MIMEType: "image/png", Encoding: "base64", Size: 5, IsAttachment: true},
		{Path: []int{2}, MIMEType: "application/pdf", Encoding: "base64", Size: 5, IsAttachment: true, Filename: "a.pdf"},
		{Path: []int{1, 2}, MIMEType: "text/html", Params: map[string]string{"CHARSET": "iso-8859-1"},
			Encoding: "quoted-printable", Size: 5, IsBody: true},
		{Path: []int{1, 1}, MIMEType: "text/plain", Params: map[string]string{"charset": "utf-8"}, Encoding: "7bit",
			Size: 5, IsBody: true},
	}
	f.live(inbox, sum)
	parts, err := f.db.MessageParts(context.Background(), f.msgID(inbox, 1))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, p := range parts {
		paths = append(paths, p.PathString())
	}
	if !slices.Equal(paths, []string{"1.1", "1.2", "2", "10"}) {
		t.Fatalf("paths = %v", paths)
	}
	if html := parts[1]; html.Params["charset"] != "iso-8859-1" || html.Encoding != "quoted-printable" || !html.IsBody {
		t.Errorf("the HTML part lost what decoding it needs: %+v", html)
	}
	if parts[2].Filename != "a.pdf" || !parts[2].IsAttachment {
		t.Errorf("the attachment = %+v", parts[2])
	}
}

func TestAPartPathIsNumbersFromOneJoinedByDots(t *testing.T) {
	for _, ok := range []string{"1", "1.2", "10.3.1"} {
		if _, valid := store.ParsePartPath(ok); !valid {
			t.Errorf("%q was refused", ok)
		}
	}
	for _, bad := range []string{"", "0", "1.", ".1", "1..2", "01", "-1", "1.a", "1/2", "../1", "1 ", "9999999",
		strings.Repeat("1.", 40) + "1"} {
		if _, valid := store.ParsePartPath(bad); valid {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestAMessageIsReadWithItsFolder(t *testing.T) {
	f := newIndex(t)
	folders := f.gmailFolders()
	f.live(folders["[Gmail]/Sent Mail"], mail(4, "sent"))
	id := f.msgID(folders["[Gmail]/Sent Mail"], 4)
	r, err := f.db.Message(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if r.FolderName != "[Gmail]/Sent Mail" || r.FolderRole != provider.RoleSent || r.UID != 4 || r.UIDValidity != 7 {
		t.Errorf("row = %+v", r)
	}
	if len(r.From) != 1 || r.From[0].Email != "sender-sent@example.com" || r.MessageID != "sent@mail.example.com" {
		t.Errorf("headers = %+v %q", r.From, r.MessageID)
	}
	if _, err := f.db.Message(context.Background(), id+100); !errors.Is(err, store.ErrNoMessage) {
		t.Errorf("an unknown id = %v, want ErrNoMessage", err)
	}
}

func TestAFolderBeingResyncedDoesNotHideAMessageWhoseOtherCopyIsLive(t *testing.T) {
	// Gmail files a message in the INBOX and in All Mail, and the INBOX row,
	// the older, stands for it in the listing of everything. When the INBOX's
	// UIDVALIDITY changes its rows wait for the resync to find their new
	// UIDs — minutes, on a large folder — and the message must stay listed
	// the whole time, as its All Mail copy, which can still be read.
	f := newIndex(t)
	folders := f.gmailFolders()
	inbox, all := folders["INBOX"], folders["[Gmail]/All Mail"]
	f.live(inbox, mail(1, "both"), mail(2, "inbox"))
	f.live(all, mail(1, "both"))
	inboxRow, allRow := f.msgID(inbox, 1), f.msgID(all, 1)

	f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := f.db.BeginResync(ctx, tx, f.account, inbox, provider.FolderStatus{UIDValidity: 8, UIDNext: 100},
			store.ResyncFromLive)
		return err
	})
	if got := f.search(store.MessageQuery{}); !slices.Equal(got, []string{"both"}) {
		t.Fatalf("during the resync everything listed %v, want the message with a live copy, once", got)
	}
	if got := f.search(store.MessageQuery{FolderID: all}); !slices.Equal(got, []string{"both"}) {
		t.Fatalf("All Mail listed %v during the INBOX's resync", got)
	}

	// The resync finds both INBOX rows again: the old INBOX row is the
	// message's primary once more, and nothing is listed twice.
	res := f.apply(store.SummaryBatch{FolderID: inbox, UIDValidity: 8, Mode: store.ApplyResync,
		Summaries: []provider.Summary{mail(40, "both"), mail(41, "inbox")}})
	if res.Claimed != 2 {
		t.Fatalf("resync batch: %+v", res)
	}
	if got := f.search(store.MessageQuery{}); len(got) != 2 {
		t.Fatalf("after the resync everything listed %v, want each message once", got)
	}
	if f.int(`SELECT count(*) FROM messages WHERE id = ? AND dup_of IS NULL`, inboxRow) != 1 ||
		f.int(`SELECT coalesce(dup_of, 0) FROM messages WHERE id = ?`, allRow) != inboxRow {
		t.Fatal("the INBOX row did not become the message's primary again once the resync found it")
	}
}
