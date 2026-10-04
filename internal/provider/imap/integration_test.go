//go:build integration

// Integration tests against a real Dovecot.
//
// These exist because the in-process IMAP server cannot speak CONDSTORE,
// SPECIAL-USE or QRESYNC — they are absent from the server library itself, not
// merely unimplemented — so the capability-dependent half of the sync engine
// has no other coverage at all. Dovecot is the closest thing to Gmail's
// capability set that can be run locally.
//
//	docker compose -f it/compose.yml up --wait
//	go test -tags integration ./internal/provider/imap/
package imap_test

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/provider"
	imapprovider "github.com/thehappieco/mailie/internal/provider/imap"
)

// dovecotAddr is where the compose file publishes the server. The rootless
// image listens on 31143 inside the container, not 143.
func dovecotAddr(t *testing.T) string {
	t.Helper()
	if addr := os.Getenv("MAIL_IT_IMAP_ADDR"); addr != "" {
		return addr
	}
	return "127.0.0.1:31143"
}

// newDovecotMailbox connects as a username nobody else in this run is using.
//
// The image's static passdb accepts any username with one password, so a fresh
// name per test is free isolation: no shared state, nothing to clean up, and
// tests can run in parallel without seeing each other's mail.
func newDovecotMailbox(t *testing.T) *imapprovider.Mailbox {
	t.Helper()
	user := strings.ToLower(strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())) +
		"-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@example.com"

	mb, err := imapprovider.New(provider.Config{
		Kind:     provider.KindIMAP,
		IMAPAddr: dovecotAddr(t),
		Credentials: provider.Credentials{
			User:     user,
			Password: envOr("MAIL_IT_IMAP_PASSWORD", "integration"),
		},
		SpoolDir: t.TempDir(),
		// The compose file runs Dovecot without TLS. The daemon has no way to
		// set this: production configuration cannot reach it.
		AllowInsecureAuth: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return mb
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func openDovecot(t *testing.T, role provider.Role) provider.Session {
	t.Helper()
	sess, err := newDovecotMailbox(t).Open(t.Context(), role)
	if err != nil {
		t.Fatalf("Open (is `docker compose -f it/compose.yml up --wait` running?): %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func TestARealServerAdvertisesTheCapabilitiesTheSyncEngineBranchesOn(t *testing.T) {
	sess := openDovecot(t, provider.RoleSync)
	caps := sess.Caps()

	// These four decide which synchronisation path an account takes. Reading
	// them wrong means either a needlessly slow sync or commands the server
	// answers with BAD, which ends the connection.
	if !caps.CondStore {
		t.Error("CONDSTORE was not detected; the fast flag path would never run")
	}
	if !caps.ESearch {
		t.Error("ESEARCH was not detected")
	}
	if !caps.Move {
		t.Error("MOVE was not detected")
	}
	if !caps.UIDPlus {
		t.Error("UIDPLUS was not detected")
	}
	if !caps.SpecialUse {
		t.Error("SPECIAL-USE was not detected; folder roles would fall back to a name table")
	}
}

func TestFolderRolesComeFromTheServersOwnAttributes(t *testing.T) {
	// Subtest names become usernames: letters and dashes only.
	t.Run("plain", func(t *testing.T) { folderRolesFromAttributes(t, false) })
	t.Run("with-status", func(t *testing.T) { folderRolesFromAttributes(t, true) })
}

// folderRolesFromAttributes lists with and without LIST-STATUS: a LIST with
// a RETURN option is an extended LIST, and Dovecot leaves the SPECIAL-USE
// attributes out of one unless they are asked for as well.
func folderRolesFromAttributes(t *testing.T, withStatus bool) {
	// The path Gmail takes, and the one the in-process server cannot exercise
	// because it never stores a special-use attribute.
	sess := openDovecot(t, provider.RoleSync)

	folders, err := sess.ListFolders(t.Context(), withStatus)
	if err != nil {
		t.Fatalf("ListFolders: %v", err)
	}
	found := map[provider.FolderRole]string{}
	for _, f := range folders {
		for _, attr := range f.Attrs {
			if role := provider.RoleFromAttr(attr); role != provider.RoleNone {
				found[role] = f.Name
			}
		}
	}
	for _, want := range []provider.FolderRole{
		provider.RoleSent, provider.RoleDrafts, provider.RoleTrash,
		provider.RoleJunk, provider.RoleArchive,
	} {
		if found[want] == "" {
			t.Errorf("no folder carried the %s attribute; found %v", want, found)
		}
	}
}

func TestCondstoreReportsOnlyWhatChanged(t *testing.T) {
	// The whole point of the fast path: after a flag changes, a fetch bounded
	// by the previous modification sequence returns that message and no other.
	sess := openDovecot(t, provider.RoleInteractive)
	appendMessages(t, sess, "INBOX", 3)

	status, err := sess.Select(t.Context(), "INBOX", false, 0)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if status.HighestModSeq == 0 {
		t.Fatal("the server reported no HIGHESTMODSEQ, so the fast path cannot run")
	}
	before := status.HighestModSeq

	if _, err := sess.StoreFlags(t.Context(), goimap.UIDSetNum(2), provider.FlagAdd,
		[]goimap.Flag{goimap.FlagSeen}, 0); err != nil {
		t.Fatalf("StoreFlags: %v", err)
	}

	changed, err := sess.FetchFlags(t.Context(), goimap.UIDSet{{Start: 1, Stop: 0}}, before)
	if err != nil {
		t.Fatalf("FetchFlags with CHANGEDSINCE: %v", err)
	}
	if len(changed) != 1 {
		t.Fatalf("CHANGEDSINCE returned %d messages, want only the one that changed", len(changed))
	}
	if changed[0].UID != 2 {
		t.Fatalf("changed uid = %d, want 2", changed[0].UID)
	}
	if changed[0].ModSeq <= before {
		t.Errorf("modseq did not advance: %d then %d", before, changed[0].ModSeq)
	}
}

func TestAConditionalStoreRefusesToOverwriteSomebodyElsesChange(t *testing.T) {
	// UNCHANGEDSINCE is how two clients marking the same message avoid
	// clobbering each other.
	sess := openDovecot(t, provider.RoleInteractive)
	appendMessages(t, sess, "INBOX", 1)
	if _, err := sess.Select(t.Context(), "INBOX", false, 0); err != nil {
		t.Fatal(err)
	}

	updates, err := sess.StoreFlags(t.Context(), goimap.UIDSetNum(1), provider.FlagAdd,
		[]goimap.Flag{goimap.FlagFlagged}, 0)
	if err != nil {
		t.Fatalf("StoreFlags: %v", err)
	}
	stale := updates[0].ModSeq - 1

	// A store bounded by a modification sequence that has already been passed
	// must not take effect.
	refused, err := sess.StoreFlags(t.Context(), goimap.UIDSetNum(1), provider.FlagAdd,
		[]goimap.Flag{goimap.FlagAnswered}, stale)
	if err != nil {
		t.Fatalf("conditional StoreFlags: %v", err)
	}
	for _, u := range refused {
		for _, f := range u.Flags {
			if strings.EqualFold(string(f), "\\answered") {
				t.Fatal("the conditional store overwrote a newer change")
			}
		}
	}
}

func TestMovingAMessageOnARealServerReportsTheNewUID(t *testing.T) {
	sess := openDovecot(t, provider.RoleInteractive)
	appendMessages(t, sess, "INBOX", 1)
	if _, err := sess.Select(t.Context(), "INBOX", false, 0); err != nil {
		t.Fatal(err)
	}

	result, err := sess.Move(t.Context(), goimap.UIDSetNum(1), "Archive")
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if len(result.Mapping) != 1 {
		t.Fatalf("COPYUID mapping = %v, want one entry so the index can follow the message", result.Mapping)
	}
	if result.DestUIDValidity == 0 {
		t.Error("the destination UIDVALIDITY was not reported")
	}
}

func TestCountingWithESearchAvoidsListingEveryUID(t *testing.T) {
	// On a large folder the difference is a number versus a few hundred
	// kilobytes of UIDs, every pass.
	sess := openDovecot(t, provider.RoleSync)
	appendMessages(t, sess, "INBOX", 5)
	if _, err := sess.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}

	n, err := sess.UIDCount(t.Context(), goimap.UIDSet{{Start: 1, Stop: 0}})
	if err != nil {
		t.Fatalf("UIDCount: %v", err)
	}
	if n != 5 {
		t.Fatalf("count = %d, want 5", n)
	}
}

func TestIdleOnARealServerSeesMailArriveOnAnotherConnection(t *testing.T) {
	mb := newDovecotMailbox(t)

	idle, err := mb.Open(t.Context(), provider.RoleIdle)
	if err != nil {
		t.Fatalf("Open idle: %v", err)
	}
	defer func() { _ = idle.Close() }()
	if _, err := idle.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}
	handle, err := idle.Idle(t.Context())
	if err != nil {
		t.Fatalf("Idle: %v", err)
	}
	defer func() { _ = handle.Stop() }()

	// The same mailbox, a second connection: exactly how mail arrives.
	worker, err := mb.Open(t.Context(), provider.RoleInteractive)
	if err != nil {
		t.Fatalf("Open worker: %v", err)
	}
	defer func() { _ = worker.Close() }()
	appendMessages(t, worker, "INBOX", 1)

	select {
	case ev := <-idle.Events():
		if ev.Kind != provider.IdleExists {
			t.Fatalf("event kind = %v, want an EXISTS signal", ev.Kind)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no signal arrived while idling against a real server")
	}
}

func TestMovingToAFolderThatDoesNotExistIsReportedAsNotFound(t *testing.T) {
	// A real server answers TRYCREATE here, and the classifier has to read it
	// as a missing folder rather than a dead connection.
	sess := openDovecot(t, provider.RoleInteractive)
	appendMessages(t, sess, "INBOX", 1)
	if _, err := sess.Select(t.Context(), "INBOX", false, 0); err != nil {
		t.Fatal(err)
	}

	_, err := sess.Move(t.Context(), goimap.UIDSetNum(1), "Projeto Fênix")
	if err == nil {
		t.Fatal("moving into a folder that does not exist should fail")
	}
	if !errors.Is(err, provider.ErrFolderNotFound) {
		t.Fatalf("want ErrFolderNotFound, got %v", err)
	}
}

func TestAnAccentedFolderNameCanBeCreatedSelectedAndMovedInto(t *testing.T) {
	// Both providers localise their default folder names, so accented names
	// are the common case, not an edge one. go-imap starts sending raw UTF-8
	// as soon as a server advertises IMAP4rev2, while the server keeps
	// decoding modified UTF-7 until something is enabled — which is why the
	// session enables UTF8=ACCEPT, and why it must not enable IMAP4rev2
	// instead: that makes expunges arrive as VANISHED, which this client
	// cannot parse.
	sess := openDovecot(t, provider.RoleInteractive)
	const name = "Projeto Fênix"

	if err := sess.Create(t.Context(), name); err != nil {
		t.Fatalf("Create: %v", err)
	}
	appendMessages(t, sess, "INBOX", 1)
	if _, err := sess.Select(t.Context(), "INBOX", false, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Move(t.Context(), goimap.UIDSetNum(1), name); err != nil {
		t.Fatalf("Move into an accented folder: %v", err)
	}

	status, err := sess.Select(t.Context(), name, true, 0)
	if err != nil {
		t.Fatalf("Select an accented folder: %v", err)
	}
	if status.NumMessages != 1 {
		t.Fatalf("the moved message is not there: %d messages", status.NumMessages)
	}

	folders, err := sess.ListFolders(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range folders {
		if f.Name == name {
			return
		}
	}
	t.Fatalf("%q was not listed back verbatim: %v", name, folders)
}

func TestFolderNamesArriveDecodedNotAsModifiedUTF7(t *testing.T) {
	// go-imap decodes on the way in and re-encodes for SELECT, so a name must
	// be stored exactly as it arrives. Decoding it a second time would corrupt
	// every name containing an ampersand.
	sess := openDovecot(t, provider.RoleSync)

	folders, err := sess.ListFolders(t.Context(), false)
	if err != nil {
		t.Fatalf("ListFolders: %v", err)
	}
	if len(folders) == 0 {
		t.Fatal("no folders at all")
	}
	for _, f := range folders {
		if strings.HasPrefix(f.Name, "&") && strings.HasSuffix(f.Name, "-") {
			t.Errorf("folder %q is still in modified UTF-7", f.Name)
		}
	}
}

const integrationMessage = "From: Ana <ana@example.com>\r\n" +
	"To: Person <person@example.com>\r\n" +
	"Subject: Integração\r\n" +
	"Message-ID: <integration@example.com>\r\n" +
	"Date: Mon, 21 Sep 2026 10:15:00 -0300\r\n" +
	"\r\n" +
	"Corpo.\r\n"

func appendMessages(t *testing.T, sess provider.Session, folder string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		body := strings.Replace(integrationMessage, "<integration@example.com>",
			"<integration-"+strconv.Itoa(i)+"@example.com>", 1)
		if _, err := sess.Append(t.Context(), folder, strings.NewReader(body),
			int64(len(body)), nil, time.Now()); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
}

func TestSummariesFromARealServerCarryReferencesAndModSeq(t *testing.T) {
	// The summary FETCH asks for one header field beside ENVELOPE and
	// BODYSTRUCTURE. A server that answered it in a shape the decoder does not
	// know would drop the connection on every sync batch, so it is checked on
	// the wire, with CONDSTORE on, as Gmail would run it.
	sess := openDovecot(t, provider.RoleSync)
	raw := "From: Ana <ana@example.com>\r\nTo: person@example.com\r\nSubject: Re: thread\r\n" +
		"Message-ID: <child@example.com>\r\nIn-Reply-To: <parent@example.com>\r\n" +
		"References: <root@example.com>\r\n <parent@example.com>\r\n\r\nbody\r\n"
	if _, err := sess.Append(t.Context(), "INBOX", strings.NewReader(raw), int64(len(raw)), nil, time.Now()); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := sess.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatalf("Select: %v", err)
	}
	var got []provider.Summary
	if err := sess.FetchSummaries(t.Context(), goimap.UIDSet{{Start: 1, Stop: 0}}, 0, func(s provider.Summary) error {
		got = append(got, s)
		return nil
	}); err != nil {
		t.Fatalf("FetchSummaries: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d summaries", len(got))
	}
	s := got[0]
	if len(s.References) != 2 || s.References[0] != "root@example.com" || s.References[1] != "parent@example.com" {
		t.Errorf("References = %q", s.References)
	}
	if s.ModSeq == 0 {
		t.Error("MODSEQ missing from a CONDSTORE server's summary")
	}
	if s.Envelope == nil || s.Envelope.MessageID != "child@example.com" {
		t.Errorf("envelope = %+v", s.Envelope)
	}
	for _, f := range s.Flags {
		if f == goimap.Flag(`\seen`) {
			t.Error("the summary fetch marked the message read")
		}
	}
}

func TestAStoreInAFolderOpenedReadOnlyOnARealServerSelectsItForWritingFirst(t *testing.T) {
	// A server refuses a STORE in a mailbox opened with EXAMINE. The session
	// re-opens it read-write, under the UIDVALIDITY it had, and the echo
	// carries the new modification sequence the index records.
	sess := openDovecot(t, provider.RoleInteractive)
	appendMessages(t, sess, "INBOX", 1)
	st, err := sess.Select(t.Context(), "INBOX", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	updates, err := sess.StoreFlags(t.Context(), goimap.UIDSetNum(1), provider.FlagAdd,
		[]goimap.Flag{goimap.FlagSeen, goimap.FlagFlagged}, 0)
	if err != nil {
		t.Fatalf("StoreFlags after EXAMINE: %v", err)
	}
	if len(updates) != 1 || updates[0].ModSeq <= st.HighestModSeq {
		t.Fatalf("echo = %+v, want the new MODSEQ above %d", updates, st.HighestModSeq)
	}
	if sel := sess.Selected(); sel == nil || sel.ReadOnly || sel.UIDValidity != st.UIDValidity {
		t.Errorf("selected = %+v", sel)
	}
}

func TestACopyAndASearchByMessageIDOnARealServer(t *testing.T) {
	sess := openDovecot(t, provider.RoleInteractive)
	appendMessages(t, sess, "INBOX", 2)
	if _, err := sess.Select(t.Context(), "INBOX", true, 0); err != nil {
		t.Fatal(err)
	}
	res, err := sess.Copy(t.Context(), goimap.UIDSetNum(2), "Archive")
	if err != nil || res.Mapping[2] == 0 || res.DestUIDValidity == 0 {
		t.Fatalf("Copy = %+v, %v", res, err)
	}
	if left, err := sess.UIDs(t.Context(), goimap.UIDSet{{Start: 1, Stop: 0}}, time.Time{}); err != nil || len(left) != 2 {
		t.Fatalf("the inbox after a copy: %v, %v", left, err)
	}
	if _, err := sess.Select(t.Context(), "Archive", true, 0); err != nil {
		t.Fatal(err)
	}
	found, err := sess.SearchMessageID(t.Context(), "integration-1@example.com")
	if err != nil || len(found) != 1 || found[0] != res.Mapping[2] {
		t.Fatalf("search = %v, %v; want [%d]", found, err, res.Mapping[2])
	}
	if none, err := sess.SearchMessageID(t.Context(), "integration-0@example.com"); err != nil || len(none) != 0 {
		t.Fatalf("a message not in the folder was found: %v, %v", none, err)
	}
}
