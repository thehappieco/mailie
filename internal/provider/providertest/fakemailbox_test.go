package providertest_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
)

var t0 = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func newFake(t *testing.T, opts providertest.FakeOptions) (*providertest.FakeMailbox, *providertest.Clock) {
	t.Helper()
	clock := providertest.NewClock(t0)
	if opts.Clock == nil {
		opts.Clock = clock.Now
	}
	return providertest.NewFakeMailbox(opts), clock
}

func open(t *testing.T, m *providertest.FakeMailbox, role provider.Role, folder string) provider.Session {
	t.Helper()
	s, err := m.Open(context.Background(), role)
	if err != nil {
		t.Fatalf("Open %s: %v", role, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if folder != "" {
		if _, err := s.Select(context.Background(), folder, true, 0); err != nil {
			t.Fatalf("Select %s: %v", folder, err)
		}
	}
	return s
}

func msg(key string) providertest.FakeMessage {
	return providertest.FakeMessage{
		MessageID: key + "@example.com", Subject: "Subject " + key, From: "Sender <sender@example.com>",
		To: []string{"person@example.com"},
	}
}

func rangeFrom(lo imap.UID) imap.UIDSet {
	var set imap.UIDSet
	set.AddRange(lo, 0)
	return set
}

func TestAStarRangeAboveEveryUIDStillMatchesTheNewest(t *testing.T) {
	// "UID SEARCH UID 4:*" on a mailbox whose newest UID is 3 returns 3: the
	// engine must filter to UIDs above what it has seen, and the fake must
	// make it prove that it does.
	m, _ := newFake(t, providertest.FakeOptions{})
	for _, k := range []string{"a", "b", "c"} {
		m.Deliver("INBOX", msg(k))
	}
	s := open(t, m, provider.RoleSync, "INBOX")
	got, err := s.UIDs(context.Background(), rangeFrom(4), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []imap.UID{3}) {
		t.Fatalf("4:* = %v, want [3]", got)
	}
	empty := providertest.NewFakeMailbox(providertest.FakeOptions{})
	e := open(t, empty, provider.RoleSync, "INBOX")
	if got, _ := e.UIDs(context.Background(), rangeFrom(1), time.Time{}); len(got) != 0 {
		t.Fatalf("1:* on an empty mailbox = %v", got)
	}
}

func TestSinceComparesDatesNotInstants(t *testing.T) {
	m, _ := newFake(t, providertest.FakeOptions{})
	early := msg("early")
	early.InternalDate = t0.Add(-60 * time.Hour) // 22 September
	morning := msg("morning")
	morning.InternalDate = time.Date(2026, 9, 23, 0, 30, 0, 0, time.UTC)
	m.Deliver("INBOX", early)
	m.Deliver("INBOX", morning)
	s := open(t, m, provider.RoleSync, "INBOX")
	got, err := s.UIDs(context.Background(), rangeFrom(1), time.Date(2026, 9, 23, 18, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []imap.UID{2}) {
		t.Fatalf("SINCE 23-Sep = %v, want [2]: a message at 00:30 that day is on or after the date", got)
	}
}

func TestSinceReadsAnInternalDateInItsOwnZone(t *testing.T) {
	// A server compares SINCE with the date the message was received on
	// where it was received: 23:30 at -0300 on the 1st is the 1st, though it
	// is 02:30 on the 2nd in UTC.
	m, _ := newFake(t, providertest.FakeOptions{})
	late := msg("late")
	late.InternalDate = time.Date(2026, 9, 1, 23, 30, 0, 0, time.FixedZone("-0300", -3*3600))
	m.Deliver("INBOX", late)
	s := open(t, m, provider.RoleSync, "INBOX")
	for _, tc := range []struct {
		since time.Time
		want  int
	}{
		{time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), 0},
		{time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 1},
	} {
		got, err := s.UIDs(context.Background(), rangeFrom(1), tc.since)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != tc.want {
			t.Errorf("SINCE %s = %v, want %d messages", tc.since.Format("2-Jan-2006"), got, tc.want)
		}
	}
}

func TestCondStoreItemsExistOnlyWithCondStore(t *testing.T) {
	ctx := context.Background()
	plain, _ := newFake(t, providertest.FakeOptions{Caps: providertest.ExchangeCaps()})
	plain.Deliver("INBOX", msg("a"))
	s := open(t, plain, provider.RoleSync, "")
	st, err := s.Select(ctx, "INBOX", true, 0)
	if err != nil || st.HighestModSeq != 0 {
		t.Fatalf("select without CONDSTORE: %+v %v", st, err)
	}
	if err := s.FetchSummaries(ctx, rangeFrom(1), 1, func(provider.Summary) error { return nil }); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("CHANGEDSINCE without CONDSTORE: %v", err)
	}
	var sums []provider.Summary
	if err := s.FetchSummaries(ctx, rangeFrom(1), 0, func(sum provider.Summary) error {
		sums = append(sums, sum)
		return nil
	}); err != nil || len(sums) != 1 || sums[0].ModSeq != 0 {
		t.Fatalf("summaries without CONDSTORE: %+v %v", sums, err)
	}

	rich, _ := newFake(t, providertest.FakeOptions{Caps: providertest.GmailCaps()})
	rich.Deliver("INBOX", msg("a"))
	rich.Deliver("INBOX", msg("b"))
	r := open(t, rich, provider.RoleSync, "")
	st, err = r.Select(ctx, "INBOX", true, 0)
	if err != nil || st.HighestModSeq == 0 {
		t.Fatalf("select with CONDSTORE: %+v %v", st, err)
	}
	rich.SetFlags("INBOX", 1, imap.FlagSeen)
	updates, err := r.FetchFlags(ctx, rangeFrom(1), st.HighestModSeq)
	if err != nil || len(updates) != 1 || updates[0].UID != 1 || updates[0].ModSeq <= st.HighestModSeq {
		t.Fatalf("CHANGEDSINCE %d: %+v %v", st.HighestModSeq, updates, err)
	}
}

func TestUIDCountNeedsESearch(t *testing.T) {
	m, _ := newFake(t, providertest.FakeOptions{})
	m.Deliver("INBOX", msg("a"))
	s := open(t, m, provider.RoleSync, "INBOX")
	if _, err := s.UIDCount(context.Background(), rangeFrom(1)); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("UIDCount without ESEARCH: %v", err)
	}
	m.SetCaps(provider.Caps{ESearch: true})
	s2 := open(t, m, provider.RoleSync, "INBOX")
	if n, err := s2.UIDCount(context.Background(), rangeFrom(1)); err != nil || n != 1 {
		t.Fatalf("UIDCount: %d %v", n, err)
	}
}

func next(t *testing.T, s provider.Session) provider.IdleEvent {
	t.Helper()
	select {
	case ev := <-s.Events():
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no idle signal")
		return provider.IdleEvent{}
	}
}

func TestAChangeByAnotherClientSignalsTheSessionWatchingTheFolder(t *testing.T) {
	m, clock := newFake(t, providertest.FakeOptions{Caps: providertest.ExchangeCaps()})
	m.CreateFolder("Archive")
	idle := open(t, m, provider.RoleIdle, "INBOX")
	other := open(t, m, provider.RoleSync, "Archive")
	h, err := idle.Idle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Stop() }()

	clock.Advance(time.Minute)
	uid := m.Deliver("INBOX", msg("a"))
	m.Deliver("INBOX", msg("b"))
	if ev := next(t, idle); ev.Kind != provider.IdleExists || ev.NumMessages != 1 || !ev.At.Equal(t0.Add(time.Minute)) {
		t.Fatalf("delivery: %+v", ev)
	}
	next(t, idle)
	m.SetFlags("INBOX", uid, imap.FlagSeen)
	if ev := next(t, idle); ev.Kind != provider.IdleFetch || ev.SeqNum != 1 {
		t.Fatalf("flag change: %+v", ev)
	}
	m.Expunge("INBOX", uid)
	if ev := next(t, idle); ev.Kind != provider.IdleExpunge || ev.SeqNum != 1 {
		t.Fatalf("expunge: %+v", ev)
	}
	select {
	case ev := <-other.Events():
		t.Fatalf("a session on another folder heard %+v", ev)
	default:
	}
}

func TestSignalsOverflowIntoAFlag(t *testing.T) {
	m, _ := newFake(t, providertest.FakeOptions{})
	s := open(t, m, provider.RoleIdle, "INBOX")
	for i := range 100 {
		m.Deliver("INBOX", msg(string(rune('a'+i%26))+strings.Repeat("x", i)))
	}
	if !s.Overflowed() {
		t.Fatal("a hundred unread signals did not overflow")
	}
	if s.Overflowed() {
		t.Fatal("Overflowed does not clear on read")
	}
}

func TestUIDsAreNeverReused(t *testing.T) {
	m, _ := newFake(t, providertest.FakeOptions{})
	a := m.Deliver("INBOX", msg("a"))
	b := m.Deliver("INBOX", msg("b"))
	m.Expunge("INBOX", b)
	c := m.Deliver("INBOX", msg("c"))
	if a != 1 || b != 2 || c != 3 {
		t.Fatalf("uids %d %d %d, want 1 2 3", a, b, c)
	}
	st, _ := m.Folder("INBOX")
	if !slices.Equal(st.UIDs, []imap.UID{1, 3}) || st.UIDNext != 4 {
		t.Fatalf("folder: %+v", st)
	}
}

func TestAUIDValidityChangeRenumbersAndDropsTheSessionsOnIt(t *testing.T) {
	ctx := context.Background()
	m, _ := newFake(t, providertest.FakeOptions{})
	m.Deliver("INBOX", msg("a"))
	m.Deliver("INBOX", msg("b"))
	m.Expunge("INBOX", 1)
	before, _ := m.Folder("INBOX")
	watching := open(t, m, provider.RoleSync, "INBOX")

	after := m.ChangeUIDValidity("INBOX")
	st, _ := m.Folder("INBOX")
	if after == before.UIDValidity || !slices.Equal(st.UIDs, []imap.UID{1}) || st.UIDNext != 2 {
		t.Fatalf("after the change: %+v", st)
	}
	if _, err := watching.UIDs(ctx, rangeFrom(1), time.Time{}); !errors.Is(err, provider.ErrConnClosed) {
		t.Fatalf("a session on the folder: %v, want the connection dropped", err)
	}
	select {
	case <-watching.Closed():
	default:
		t.Fatal("the dropped session's Closed did not fire")
	}
	s := open(t, m, provider.RoleSync, "")
	sel, err := s.Select(ctx, "INBOX", true, before.UIDValidity)
	if !errors.Is(err, provider.ErrUIDValidityChanged) || sel.UIDValidity != after {
		t.Fatalf("select expecting the old UIDVALIDITY: %+v %v", sel, err)
	}
	if len(m.Violations()) != 0 {
		t.Fatalf("violations: %v", m.Violations())
	}
}

func TestReassignUIDKeepsTheMessage(t *testing.T) {
	ctx := context.Background()
	m, _ := newFake(t, providertest.FakeOptions{})
	uid := m.Deliver("INBOX", msg("report"))
	fresh := m.ReassignUID("INBOX", uid)
	if fresh <= uid {
		t.Fatalf("reassigned %d -> %d", uid, fresh)
	}
	s := open(t, m, provider.RoleSync, "INBOX")
	flags, err := s.FetchFlags(ctx, imap.UIDSetNum(uid), 0)
	if err != nil || len(flags) != 0 {
		t.Fatalf("the old UID still answers: %+v %v", flags, err)
	}
	var got provider.Summary
	if err := s.FetchSummaries(ctx, imap.UIDSetNum(fresh), 0, func(sum provider.Summary) error {
		got = sum
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got.Envelope == nil || got.Envelope.MessageID != "report@example.com" || got.InternalDate != t0 {
		t.Fatalf("the reassigned message: %+v", got)
	}
}

func TestGmailLabelCopiesShareFlagsWhenAsked(t *testing.T) {
	m, _ := newFake(t, providertest.FakeOptions{Kind: provider.KindGmail, SharedFlags: true})
	m.CreateFolder("Receipts")
	uid := m.Deliver("INBOX", msg("x"))
	copied := m.CopyTo("INBOX", uid, "Receipts")
	m.SetFlags("INBOX", uid, imap.FlagSeen)
	if got := m.Flags("Receipts", copied); !slices.Equal(got, []imap.Flag{`\seen`}) {
		t.Fatalf("the label copy's flags: %v", got)
	}
	s := open(t, m, provider.RoleInteractive, "Receipts")
	if _, err := s.StoreFlags(context.Background(), imap.UIDSetNum(copied), provider.FlagAdd, []imap.Flag{imap.FlagFlagged}, 0); err != nil {
		t.Fatal(err)
	}
	if got := m.Flags("INBOX", uid); !slices.Equal(got, []imap.Flag{`\flagged`, `\seen`}) {
		t.Fatalf("the inbox copy after a store on the label: %v", got)
	}
}

func TestInjectedFailuresHitTheirMethodOnly(t *testing.T) {
	ctx := context.Background()
	m, _ := newFake(t, providertest.FakeOptions{})
	m.Deliver("INBOX", msg("a"))

	m.FailNext(providertest.MethodOpen, provider.ErrNeedsReauth)
	if _, err := m.Open(ctx, provider.RoleSync); !errors.Is(err, provider.ErrNeedsReauth) {
		t.Fatalf("Open: %v", err)
	}
	s := open(t, m, provider.RoleSync, "INBOX")
	m.FailNext(providertest.MethodFetchSummaries, provider.ErrConnClosed)
	if err := s.FetchSummaries(ctx, rangeFrom(1), 0, func(provider.Summary) error { return nil }); !errors.Is(err, provider.ErrConnClosed) {
		t.Fatalf("injected: %v", err)
	}
	select {
	case <-s.Closed():
	default:
		t.Fatal("a dropped connection did not close the session")
	}

	s2 := open(t, m, provider.RoleSync, "INBOX")
	m.FailTimes(providertest.MethodUIDs, 2, provider.ErrTemporary)
	for range 2 {
		if _, err := s2.UIDs(ctx, rangeFrom(1), time.Time{}); !errors.Is(err, provider.ErrTemporary) {
			t.Fatalf("injected temporary: %v", err)
		}
	}
	if got, err := s2.UIDs(ctx, rangeFrom(1), time.Time{}); err != nil || len(got) != 1 {
		t.Fatalf("after the injected failures ran out: %v %v", got, err)
	}
	m.FailAlways(providertest.MethodNoop, provider.ErrTemporary)
	for range 3 {
		if err := s2.Noop(ctx); !errors.Is(err, provider.ErrTemporary) {
			t.Fatalf("always: %v", err)
		}
	}
	m.ClearFailures()
	if err := s2.Noop(ctx); err != nil {
		t.Fatalf("after ClearFailures: %v", err)
	}
	if n := m.CallCount(providertest.MethodNoop); n != 4 {
		t.Fatalf("logged %d NOOPs, want 4", n)
	}
}

func TestTwoCommandsInFlightOnOneSessionIsAViolation(t *testing.T) {
	m, _ := newFake(t, providertest.FakeOptions{})
	s := open(t, m, provider.RoleSync, "INBOX")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m.OnCall(func(ctx context.Context, c providertest.Call) error {
		if c.Method == providertest.MethodUIDs {
			once.Do(func() { close(entered) })
			<-release
		}
		return nil
	})
	done := make(chan error, 1)
	go func() {
		_, err := s.UIDs(context.Background(), rangeFrom(1), time.Time{})
		done <- err
	}()
	<-entered
	if err := s.Noop(context.Background()); err == nil {
		t.Fatal("a second command in flight was answered")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if v := m.Violations(); len(v) != 1 || !strings.Contains(v[0], "in flight") {
		t.Fatalf("violations: %v", v)
	}
}

func TestIdleIsRefusedWhenTheServerDoesNotAdvertiseIt(t *testing.T) {
	m, _ := newFake(t, providertest.FakeOptions{})
	s := open(t, m, provider.RoleIdle, "INBOX")
	if _, err := s.Idle(context.Background()); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("IDLE without the capability: %v", err)
	}
	if v := m.Violations(); len(v) != 0 {
		t.Fatalf("asking is not a misuse; the engine learns from the answer: %v", v)
	}
}

func TestACommandDuringIdleIsAViolation(t *testing.T) {
	m, _ := newFake(t, providertest.FakeOptions{Caps: providertest.ExchangeCaps()})
	s := open(t, m, provider.RoleIdle, "INBOX")
	h, err := s.Idle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Noop(context.Background()); err == nil {
		t.Fatal("NOOP during IDLE was answered")
	}
	if err := h.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop is not idempotent: %v", err)
	}
	if err := s.Noop(context.Background()); err != nil {
		t.Fatalf("NOOP after IDLE: %v", err)
	}
	syncSession := open(t, m, provider.RoleSync, "INBOX")
	if _, err := syncSession.Idle(context.Background()); err == nil {
		t.Fatal("the sync session was allowed to IDLE")
	}
	if v := m.Violations(); len(v) != 2 {
		t.Fatalf("violations: %v", v)
	}
}

func TestKillingSessionsClosesThemAndCountsThePeak(t *testing.T) {
	ctx := context.Background()
	m, _ := newFake(t, providertest.FakeOptions{MaxSessions: 3})
	idle := open(t, m, provider.RoleIdle, "INBOX")
	open(t, m, provider.RoleSync, "")
	open(t, m, provider.RoleInteractive, "")
	if _, err := m.Open(ctx, provider.RoleSync); !errors.Is(err, provider.ErrTooManyConnections) {
		t.Fatalf("a fourth connection: %v", err)
	}
	if n := m.KillSessions(provider.RoleIdle); n != 1 {
		t.Fatalf("killed %d idle sessions", n)
	}
	<-idle.Closed()
	if err := idle.Noop(ctx); !errors.Is(err, provider.ErrConnClosed) {
		t.Fatalf("a call on a dropped connection: %v", err)
	}
	if m.OpenSessions() != 2 || m.PeakSessions() != 3 || m.PeakSessions(provider.RoleIdle) != 1 || m.Opens(provider.RoleIdle) != 1 {
		t.Fatalf("open %d peak %d", m.OpenSessions(), m.PeakSessions())
	}
	if v := m.Violations(); len(v) != 0 {
		t.Fatalf("a dropped connection is not the caller's fault: %v", v)
	}
	if err := idle.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestACallOnASessionItsOwnerClosedIsAViolation(t *testing.T) {
	m, _ := newFake(t, providertest.FakeOptions{})
	s := open(t, m, provider.RoleSync, "INBOX")
	_ = s.Close()
	if _, open := <-s.Events(); open {
		t.Fatal("Close left the events channel open; the adapter closes it")
	}
	m.Deliver("INBOX", msg("after")) // must not panic on the closed channel
	if err := s.Noop(context.Background()); !errors.Is(err, provider.ErrConnClosed) {
		t.Fatalf("a closed session answered: %v", err)
	}
	if len(m.Violations()) != 1 {
		t.Fatalf("violations: %v", m.Violations())
	}
}

func TestMoveAndAppendFollowUIDPlus(t *testing.T) {
	ctx := context.Background()
	m, _ := newFake(t, providertest.FakeOptions{Caps: providertest.ExchangeCaps()})
	m.CreateFolder("Archive")
	a := m.Deliver("INBOX", msg("a"))
	s := open(t, m, provider.RoleInteractive, "INBOX")
	res, err := s.Move(ctx, imap.UIDSetNum(a), "Archive")
	if err != nil || res.Mapping[a] != 1 {
		t.Fatalf("move with UIDPLUS: %+v %v", res, err)
	}
	if _, err := s.Move(ctx, imap.UIDSetNum(a), "Nowhere"); !errors.Is(err, provider.ErrFolderNotFound) {
		t.Fatalf("move to a missing folder: %v", err)
	}
	raw := "From: Ann <ann@example.com>\r\nTo: person@example.com\r\nSubject: Appended\r\n" +
		"Message-ID: <app@example.com>\r\nReferences: <r1@example.com> <r2@example.com>\r\n\r\nhi\r\n"
	app, err := s.Append(ctx, "INBOX", strings.NewReader(raw), int64(len(raw)), []imap.Flag{imap.FlagSeen}, t0)
	if err != nil || app.UID == 0 {
		t.Fatalf("append with UIDPLUS: %+v %v", app, err)
	}
	var got provider.Summary
	if err := s.FetchSummaries(ctx, imap.UIDSetNum(app.UID), 0, func(sum provider.Summary) error {
		got = sum
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got.Envelope.Subject != "Appended" || got.Envelope.MessageID != "app@example.com" ||
		got.Envelope.From[0].Addr() != "ann@example.com" || !slices.Equal(got.References, []string{"r1@example.com", "r2@example.com"}) ||
		!slices.Equal(got.Flags, []imap.Flag{`\seen`}) {
		t.Fatalf("the appended message: %+v %+v", got, got.Envelope)
	}

	bare, _ := newFake(t, providertest.FakeOptions{Caps: provider.Caps{Move: true}})
	bare.CreateFolder("Archive")
	b := bare.Deliver("INBOX", msg("b"))
	bs := open(t, bare, provider.RoleInteractive, "INBOX")
	if res, err := bs.Move(ctx, imap.UIDSetNum(b), "Archive"); err != nil || res.Mapping != nil {
		t.Fatalf("move without UIDPLUS: %+v %v", res, err)
	}
}

func TestStoreFlagsEchoesTheNewFlags(t *testing.T) {
	ctx := context.Background()
	m, _ := newFake(t, providertest.FakeOptions{Caps: providertest.GmailCaps()})
	uid := m.Deliver("INBOX", msg("a"))
	s := open(t, m, provider.RoleInteractive, "INBOX")
	for _, step := range []struct {
		op    provider.FlagOp
		flags []imap.Flag
		want  []imap.Flag
	}{
		{provider.FlagAdd, []imap.Flag{"\\Seen", "\\Flagged"}, []imap.Flag{`\flagged`, `\seen`}},
		{provider.FlagDel, []imap.Flag{"\\Flagged"}, []imap.Flag{`\seen`}},
		{provider.FlagSet, []imap.Flag{"\\Answered"}, []imap.Flag{`\answered`}},
	} {
		updates, err := s.StoreFlags(ctx, imap.UIDSetNum(uid), step.op, step.flags, 0)
		if err != nil || len(updates) != 1 || !slices.Equal(updates[0].Flags, step.want) || updates[0].ModSeq == 0 {
			t.Fatalf("store %v %v: %+v %v", step.op, step.flags, updates, err)
		}
	}
}

func TestFetchSummariesCopiesItsAnswers(t *testing.T) {
	ctx := context.Background()
	m, _ := newFake(t, providertest.FakeOptions{})
	m.Deliver("INBOX", msg("a"))
	s := open(t, m, provider.RoleSync, "INBOX")
	fetch := func() provider.Summary {
		var out provider.Summary
		if err := s.FetchSummaries(ctx, rangeFrom(1), 0, func(sum provider.Summary) error {
			out = sum
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := fetch()
	first.Envelope.Subject = "changed"
	first.Parts[0].Path[0] = 9
	second := fetch()
	if second.Envelope.Subject != "Subject a" || second.Parts[0].Path[0] != 1 {
		t.Fatal("a caller's edit reached the fake's state")
	}
	errStop := errors.New("stop")
	calls := 0
	m.Deliver("INBOX", msg("b"))
	err := s.FetchSummaries(ctx, rangeFrom(1), 0, func(provider.Summary) error {
		calls++
		return errStop
	})
	if !errors.Is(err, errStop) || calls != 1 {
		t.Fatalf("a callback error: %v after %d calls", err, calls)
	}
}

func TestACancelledCallLeavesTheConnectionForTheNextCaller(t *testing.T) {
	// As the real adapter: a caller that has gone away sends nothing, and the
	// connection is as it was — the interactive one is shared, and a person
	// cancels a read every time they open another message.
	m, _ := newFake(t, providertest.FakeOptions{})
	s := open(t, m, provider.RoleSync, "INBOX")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.UIDs(ctx, rangeFrom(1), time.Time{}); !errors.Is(err, provider.ErrConnClosed) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call: %v", err)
	}
	select {
	case <-s.Closed():
		t.Fatal("a cancelled call closed the connection")
	default:
	}
	if _, err := s.UIDs(context.Background(), rangeFrom(1), time.Time{}); err != nil {
		t.Fatalf("the next call on the same connection: %v", err)
	}
}

func TestListFoldersReportsStatusOnlyWithListStatus(t *testing.T) {
	ctx := context.Background()
	m, _ := newFake(t, providertest.FakeOptions{Caps: providertest.GmailCaps()})
	m.CreateFolder("[Gmail]/Sent Mail", imap.MailboxAttrSent)
	m.CreateContainer("[Gmail]")
	m.Deliver("INBOX", msg("a"))
	s := open(t, m, provider.RoleSync, "")
	folders, err := s.ListFolders(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]provider.Folder{}
	for _, f := range folders {
		byName[f.Name] = f
	}
	if st := byName["INBOX"].Status; st == nil || st.NumMessages != 1 || st.NumUnseen != 1 || st.HighestModSeq == 0 {
		t.Fatalf("INBOX status: %+v", st)
	}
	if byName["[Gmail]"].Selectable || byName["[Gmail]"].Status != nil {
		t.Fatalf("a container: %+v", byName["[Gmail]"])
	}
	if !byName["[Gmail]/Sent Mail"].HasAttr(imap.MailboxAttrSent) {
		t.Fatal("SPECIAL-USE attributes were lost")
	}
	m.SetCaps(provider.Caps{})
	s2 := open(t, m, provider.RoleSync, "")
	folders, _ = s2.ListFolders(ctx, true)
	for _, f := range folders {
		if f.Status != nil {
			t.Fatalf("status without LIST-STATUS: %+v", f)
		}
	}
	if _, err := s2.Select(ctx, "[Gmail]", true, 0); !errors.Is(err, provider.ErrFolderNotFound) {
		t.Fatalf("selecting a container: %v", err)
	}
}

func TestAnUnheardChangeLeavesOnlyTheOverflowFlag(t *testing.T) {
	// What a full event buffer does to a client: the change happened, the
	// notification did not arrive, and all the session can say is that it
	// missed something.
	m, _ := newFake(t, providertest.FakeOptions{Caps: providertest.ExchangeCaps()})
	uid := m.Deliver("INBOX", msg("a"))
	s := open(t, m, provider.RoleIdle, "INBOX")
	m.Unheard(func() {
		m.SetFlags("INBOX", uid, imap.FlagSeen)
		m.Deliver("INBOX", msg("b"))
	})
	select {
	case ev := <-s.Events():
		t.Fatalf("an unheard change was heard: %+v", ev)
	default:
	}
	if !s.Overflowed() {
		t.Fatal("the session does not report that it missed notifications")
	}
	if state, _ := m.Folder("INBOX"); len(state.UIDs) != 2 {
		t.Fatalf("the unheard delivery did not happen: %v", state.UIDs)
	}
	m.Deliver("INBOX", msg("c"))
	if ev := <-s.Events(); ev.Kind != provider.IdleExists {
		t.Fatalf("after Unheard, %+v; want the next EXISTS heard", ev)
	}
}

func TestLogoutsAreCountedAndAnUnendedIdleIsNoted(t *testing.T) {
	m, _ := newFake(t, providertest.FakeOptions{Caps: providertest.GmailCaps()})
	ended := open(t, m, provider.RoleIdle, "INBOX")
	h, err := ended.Idle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(); err != nil {
		t.Fatal(err)
	}
	_ = ended.Close()
	_ = ended.Close() // idempotent: one logout
	abandoned := open(t, m, provider.RoleIdle, "INBOX")
	if _, err := abandoned.Idle(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = abandoned.Close()
	dropped := open(t, m, provider.RoleSync, "")
	m.KillSessions(provider.RoleSync)
	if n := m.Logouts(provider.RoleIdle); n != 2 {
		t.Errorf("%d idle logouts, want 2", n)
	}
	if n := m.ClosedIdling(); n != 1 {
		t.Errorf("%d sessions closed mid-IDLE, want the one never stopped", n)
	}
	if n := m.Logouts(provider.RoleSync); n != 0 {
		t.Errorf("a dropped connection counted as %d logouts", n)
	}
	_ = dropped
}

func TestTheFakeServesPartsTheWayAServerDescribesThem(t *testing.T) {
	// A message given as raw bytes is described by the BODYSTRUCTURE a real
	// server would build for it, and each section it lists can be fetched,
	// still transfer-encoded, without the flags changing.
	m, _ := newFake(t, providertest.FakeOptions{})
	raw := strings.ReplaceAll(`Subject: parts
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="b"

--b
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: quoted-printable

caf=C3=A9
--b
Content-Type: application/pdf
Content-Disposition: attachment; filename="a.pdf"
Content-Transfer-Encoding: base64

JVBERi0=
--b--
`, "\n", "\r\n")
	uid := m.Deliver("INBOX", providertest.FakeMessage{Subject: "parts", Raw: []byte(raw)})
	s := open(t, m, provider.RoleInteractive, "INBOX")

	var summary provider.Summary
	if err := s.FetchSummaries(context.Background(), imap.UIDSetNum(uid), 0, func(sum provider.Summary) error {
		summary = sum
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(summary.Parts) != 2 || summary.Parts[1].Filename != "a.pdf" || !summary.Parts[1].IsAttachment ||
		summary.Parts[0].Encoding != "quoted-printable" || !summary.Parts[0].IsBody {
		t.Fatalf("parts = %+v", summary.Parts)
	}
	if summary.Size != int64(len(raw)) {
		t.Errorf("size = %d, want the raw message's %d", summary.Size, len(raw))
	}
	for i, want := range []string{"caf=C3=A9", "JVBERi0="} {
		part, err := s.FetchPart(context.Background(), uid, summary.Parts[i], 1<<20)
		if err != nil {
			t.Fatalf("FetchPart %s: %v", summary.Parts[i].PathString(), err)
		}
		body := make([]byte, 64)
		n, _ := part.Body.Read(body)
		if got := string(body[:n]); got != want {
			t.Errorf("section %s = %q, want %q", summary.Parts[i].PathString(), got, want)
		}
		_ = part.Body.Close()
	}
	if _, err := s.FetchPart(context.Background(), uid, summary.Parts[1], 3); !errors.Is(err, provider.ErrTooLarge) {
		t.Errorf("a section over the limit = %v, want ErrTooLarge", err)
	}
	if _, err := s.FetchPart(context.Background(), uid, provider.PartInfo{Path: []int{7}}, 1<<20); !errors.Is(err, provider.ErrMessageGone) {
		t.Errorf("a section that does not exist = %v", err)
	}
	if flags := m.Flags("INBOX", uid); len(flags) != 0 {
		t.Errorf("fetching parts changed the flags to %v", flags)
	}
	calls := m.Calls()
	if last := calls[len(calls)-1]; last.Method != providertest.MethodFetchPart || last.Section != "7" {
		t.Errorf("the call log says %+v", last)
	}
}

func TestAMoveWithNeitherMOVENorUIDPLUSChangesNothing(t *testing.T) {
	ctx := context.Background()
	m, _ := newFake(t, providertest.FakeOptions{Caps: provider.Caps{CondStore: true}})
	m.CreateFolder("Trash")
	uid := m.Deliver("INBOX", msg("a"))
	s := open(t, m, provider.RoleInteractive, "INBOX")
	m.ResetCalls()
	if _, err := s.Move(ctx, imap.UIDSetNum(uid), "Trash"); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("Move = %v, want ErrUnsupported", err)
	}
	if calls := m.Calls(); len(calls) != 0 {
		t.Fatalf("a refused move reached the server: %+v", calls)
	}
	if inbox, _ := m.Folder("INBOX"); len(inbox.UIDs) != 1 {
		t.Fatalf("the inbox holds %v", inbox.UIDs)
	}
	if trash, _ := m.Folder("Trash"); len(trash.UIDs) != 0 {
		t.Fatalf("the trash holds %v", trash.UIDs)
	}
}

func TestAMoveWithoutMOVEStillMovesOnlyItsUIDsWithUIDPLUS(t *testing.T) {
	ctx := context.Background()
	m, _ := newFake(t, providertest.FakeOptions{Caps: provider.Caps{UIDPlus: true}})
	m.CreateFolder("Trash")
	a := m.Deliver("INBOX", msg("a"))
	b := m.Deliver("INBOX", providertest.FakeMessage{MessageID: "b@example.com", Flags: []imap.Flag{imap.FlagDeleted}})
	s := open(t, m, provider.RoleInteractive, "INBOX")
	res, err := s.Move(ctx, imap.UIDSetNum(a), "Trash")
	if err != nil || res.Mapping[a] == 0 || res.DestUIDValidity == 0 {
		t.Fatalf("Move = %+v, %v", res, err)
	}
	if inbox, _ := m.Folder("INBOX"); len(inbox.UIDs) != 1 || inbox.UIDs[0] != b {
		t.Fatalf("the inbox holds %v, want only %d", inbox.UIDs, b)
	}
}

func TestAWriteOnAnExaminedFolderSelectsItReadWriteFirst(t *testing.T) {
	// As the adapter does, and in the log: Gmail refuses a STORE in a
	// folder opened with EXAMINE.
	ctx := context.Background()
	m, _ := newFake(t, providertest.FakeOptions{Caps: providertest.GmailCaps()})
	m.CreateFolder("Archive")
	uid := m.Deliver("INBOX", msg("a"))
	s := open(t, m, provider.RoleInteractive, "INBOX")
	m.ResetCalls()
	if _, err := s.StoreFlags(ctx, imap.UIDSetNum(uid), provider.FlagAdd, []imap.Flag{imap.FlagSeen}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(ctx, imap.UIDSetNum(uid), "Archive"); err != nil {
		t.Fatal(err)
	}
	calls := m.Calls()
	if len(calls) != 3 || calls[0].Method != providertest.MethodSelect || calls[0].ReadOnly ||
		calls[1].Method != providertest.MethodStoreFlags || calls[1].Flags != `+FLAGS (\seen)` ||
		calls[2].Method != providertest.MethodMove || calls[2].Dest != "Archive" {
		t.Fatalf("calls = %+v, want one read-write SELECT, the STORE and the MOVE", calls)
	}
}

func TestACopyLeavesTheOriginalAndASearchFindsItByMessageID(t *testing.T) {
	ctx := context.Background()
	m, _ := newFake(t, providertest.FakeOptions{Caps: providertest.GmailCaps()})
	m.CreateFolder("Archive")
	uid := m.Deliver("INBOX", msg("a"))
	m.Deliver("Archive", msg("b"))
	s := open(t, m, provider.RoleInteractive, "INBOX")
	res, err := s.Copy(ctx, imap.UIDSetNum(uid), "Archive")
	if err != nil || res.Mapping[uid] == 0 {
		t.Fatalf("Copy = %+v, %v", res, err)
	}
	if inbox, _ := m.Folder("INBOX"); len(inbox.UIDs) != 1 {
		t.Fatal("a copy took the original away")
	}
	if _, err := s.Select(ctx, "Archive", true, 0); err != nil {
		t.Fatal(err)
	}
	found, err := s.SearchMessageID(ctx, "<a@example.com>")
	if err != nil || len(found) != 1 || found[0] != res.Mapping[uid] {
		t.Fatalf("search = %v, %v; want [%d]", found, err, res.Mapping[uid])
	}
}

func TestWithLabelsAMessageIsInAFolderAtMostOnce(t *testing.T) {
	// Gmail's labels: moving a message to a label it already has takes the
	// source label away and reports the UID it has under the other; copying
	// it to one it lacks adds it.
	ctx := context.Background()
	m, _ := newFake(t, providertest.FakeOptions{Caps: providertest.GmailCaps(), SharedFlags: true, Labels: true})
	m.CreateFolder("Work")
	m.CreateFolder("Travel")
	uid := m.Deliver("INBOX", msg("a"))
	inTravel := m.CopyTo("INBOX", uid, "Travel")
	s := open(t, m, provider.RoleInteractive, "INBOX")
	res, err := s.Move(ctx, imap.UIDSetNum(uid), "Travel")
	if err != nil || res.Mapping[uid] != inTravel {
		t.Fatalf("Move = %+v, %v; want the UID it has in Travel, %d", res, err, inTravel)
	}
	if travel, _ := m.Folder("Travel"); len(travel.UIDs) != 1 {
		t.Fatalf("Travel holds %v: a second copy of the message", travel.UIDs)
	}
	if inbox, _ := m.Folder("INBOX"); len(inbox.UIDs) != 0 {
		t.Fatalf("the inbox holds %v", inbox.UIDs)
	}
	if _, err := s.Select(ctx, "Travel", true, 0); err != nil {
		t.Fatal(err)
	}
	res, err = s.Copy(ctx, imap.UIDSetNum(inTravel), "Work")
	if err != nil || res.Mapping[inTravel] == 0 {
		t.Fatalf("Copy = %+v, %v", res, err)
	}
	if work, _ := m.Folder("Work"); len(work.UIDs) != 1 || work.UIDs[0] != res.Mapping[inTravel] {
		t.Fatalf("Work holds %v", work.UIDs)
	}
}

func TestReversedCopyUIDReportsTheAscendingPairingNotTheOneItMade(t *testing.T) {
	// What go-imap hands the adapter for Gmail's COPYUID of several UIDs:
	// both sets right, sorted, and paired by position — which is not how the
	// server paired them. One UID is reported as it was made.
	ctx := context.Background()
	m, _ := newFake(t, providertest.FakeOptions{Caps: providertest.GmailCaps(), ReversedCopyUID: true})
	m.CreateFolder("Trash")
	a := m.Deliver("INBOX", msg("a"))
	b := m.Deliver("INBOX", msg("b"))
	c := m.Deliver("INBOX", msg("c"))
	s := open(t, m, provider.RoleInteractive, "INBOX")
	res, err := s.Move(ctx, imap.UIDSetNum(a, b), "Trash")
	if err != nil || len(res.Mapping) != 2 || res.Paired() {
		t.Fatalf("Move = %+v, %v", res, err)
	}
	if res.Mapping[a] >= res.Mapping[b] {
		t.Fatalf("mapping = %v, want both sides in ascending order", res.Mapping)
	}
	trash, _ := m.Folder("Trash")
	if !slices.Equal(trash.UIDs, []imap.UID{res.Mapping[a], res.Mapping[b]}) {
		t.Fatalf("the trash holds %v, want the UIDs the mapping reports, %v", trash.UIDs, res.Mapping)
	}
	idAt := func(folder string, uid imap.UID) string {
		t.Helper()
		if _, err := s.Select(ctx, folder, true, 0); err != nil {
			t.Fatal(err)
		}
		var id string
		if err := s.FetchSummaries(ctx, imap.UIDSetNum(uid), 0, func(sum provider.Summary) error {
			id = sum.Envelope.MessageID
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if got := idAt("Trash", res.Mapping[a]); got != "b@example.com" {
		t.Fatalf("the mapping's UID for a holds %q, want b's message: the pairing the server made was the reverse", got)
	}

	if _, err := s.Select(ctx, "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}
	res, err = s.Move(ctx, imap.UIDSetNum(c), "Trash")
	if err != nil || !res.Paired() {
		t.Fatalf("Move of one UID = %+v, %v", res, err)
	}
	if got := idAt("Trash", res.Mapping[c]); got != "c@example.com" {
		t.Fatalf("the mapping of one UID points at %q", got)
	}
}

func TestADroppedSessionSaysWhyItClosed(t *testing.T) {
	// As the adapter's: the server hanging up is ErrServerEnded, a
	// connection that ended any other way is only ErrConnClosed, and a
	// command on a closed session fails with why it closed.
	ctx := context.Background()
	m, _ := newFake(t, providertest.FakeOptions{})
	syncing := open(t, m, provider.RoleSync, "INBOX")
	idle := open(t, m, provider.RoleIdle, "INBOX")
	interactive := open(t, m, provider.RoleInteractive, "INBOX")
	if err := syncing.CloseCause(); err != nil {
		t.Fatalf("CloseCause of an open session = %v", err)
	}

	m.KillSessions(provider.RoleSync)
	if err := syncing.CloseCause(); !errors.Is(err, provider.ErrServerEnded) {
		t.Fatalf("CloseCause after the server hung up = %v", err)
	}
	if err := syncing.Noop(ctx); !errors.Is(err, provider.ErrServerEnded) {
		t.Fatalf("NOOP after the server hung up = %v", err)
	}

	unparseable := fmt.Errorf("%w: the server sent a response this client cannot parse", provider.ErrConnClosed)
	m.KillSessionsWith(unparseable, provider.RoleIdle)
	if err := idle.CloseCause(); !errors.Is(err, unparseable) || errors.Is(err, provider.ErrServerEnded) {
		t.Fatalf("CloseCause after an unparseable response = %v", err)
	}

	if err := interactive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := interactive.CloseCause(); !errors.Is(err, provider.ErrConnClosed) || errors.Is(err, provider.ErrServerEnded) {
		t.Fatalf("CloseCause after Close = %v", err)
	}
}
