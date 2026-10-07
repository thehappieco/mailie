package service_test

import (
	"os"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// storageFixture is three people's mailboxes and one nobody owns, indexed:
// ana's work mailbox holds 1000 and 2000 bytes, her home one 500, bob's 7000
// and the shared one 9000.
type storageFixture struct {
	*mailFixture
	ana, bob, olga         service.Principal
	work, home, bobs, team string
}

func newStorageFixture(t *testing.T) *storageFixture {
	t.Helper()
	m := newMailFixture(t)
	s := &storageFixture{mailFixture: m}
	s.ana = m.person(t, "ana@example.com", auth.RoleMember)
	s.bob = m.person(t, "bob@example.com", auth.RoleMember)
	s.olga = m.person(t, "olga@example.com", auth.RoleOwner)
	mailbox := func(owner service.Principal, email string, sizes ...int64) string {
		t.Helper()
		id, box := m.fakeAccount(t, owner, email)
		for i, size := range sizes {
			box.Deliver("INBOX", providertest.FakeMessage{
				MessageID: email + "-" + string(rune('a'+i)), Subject: "Message", From: "bea@example.org",
				To: []string{email}, InternalDate: time.Now().Add(-time.Hour), Size: size,
			})
		}
		m.index(t, id, box)
		return id
	}
	s.work = mailbox(s.ana, "ana@work.example", 1000, 2000)
	s.home = mailbox(s.ana, "ana@home.example", 500)
	s.bobs = mailbox(s.bob, "bob@mail.example", 7000)
	s.team = mailbox(service.Principal{}, "team@mail.example", 9000)
	return s
}

func (s *storageFixture) storage(t *testing.T, p service.Principal) service.Storage {
	t.Helper()
	got, err := s.svc.Storage(t.Context(), p, "")
	if err != nil {
		t.Fatalf("Storage: %v", err)
	}
	return got
}

// mailboxes is what a storage answer lists, by account.
func mailboxes(st service.Storage) map[string]service.MailboxStorage {
	out := map[string]service.MailboxStorage{}
	for _, m := range st.Mailboxes {
		out[m.AccountID] = m
	}
	return out
}

func TestAMemberSeesTheStorageOfHerOwnMailboxesOnly(t *testing.T) {
	s := newStorageFixture(t)
	st := s.storage(t, s.ana)
	got := mailboxes(st)
	personal := authtest.Personal(t, s.db, s.ana.UserID)
	want := map[string]service.MailboxStorage{
		s.work: {AccountID: s.work, WorkspaceID: personal, Email: "ana@work.example", Messages: 2, Bytes: 3000},
		s.home: {AccountID: s.home, WorkspaceID: personal, Email: "ana@home.example", Messages: 1, Bytes: 500},
	}
	if len(got) != len(want) || got[s.work] != want[s.work] || got[s.home] != want[s.home] {
		t.Fatalf("ana's storage lists %+v, want %+v", st.Mailboxes, want)
	}
	if st.Total != (service.StorageTotal{Messages: 3, Bytes: 3500}) {
		t.Errorf("total = %+v", st.Total)
	}
	// Summed per workspace: both are in her personal one.
	if len(st.Workspaces) != 1 || st.Workspaces[0] != (service.WorkspaceStorage{
		WorkspaceID: personal, Mailboxes: 2, Messages: 3, Bytes: 3500,
	}) {
		t.Errorf("per workspace = %+v", st.Workspaces)
	}
	// What the whole database takes is the operator's business, not hers:
	// not on her session, not on her key.
	if st.DatabaseBytes != nil {
		t.Errorf("a member is told the database's size: %d", *st.DatabaseBytes)
	}
	if st := s.storage(t, keyOf(t, s.fixture, s.ana, auth.ScopeRead)); st.DatabaseBytes != nil || len(st.Mailboxes) != 2 {
		t.Errorf("a key of her workspace holding both: %+v", st)
	}
}

func TestAnotherPersonsMailboxNeverAppearsInSomebodysStorage(t *testing.T) {
	s := newStorageFixture(t)
	for name, p := range map[string]service.Principal{
		"bob": s.bob, "a key of bob's workspace": keyOf(t, s.fixture, s.bob, auth.ScopeRead),
		"olga, an owner": s.olga, "a key of olga's workspace": keyOf(t, s.fixture, s.olga, auth.ScopeRead),
		"an instance key": reader(),
	} {
		for _, m := range s.storage(t, p).Mailboxes {
			if m.AccountID == s.work || m.AccountID == s.home {
				t.Errorf("%s sees ana's mailbox %s", name, m.Email)
			}
		}
	}
	if got := mailboxes(s.storage(t, s.bob)); len(got) != 1 || got[s.bobs].Bytes != 7000 {
		t.Errorf("bob's storage = %+v", got)
	}
}

func TestAnInstanceOwnerIsToldTheDatabaseSizeButSeesNoMailboxByBeingOne(t *testing.T) {
	s := newStorageFixture(t)
	st := s.storage(t, s.olga)
	if len(st.Mailboxes) != 0 || st.Total != (service.StorageTotal{}) {
		t.Fatalf("olga's storage lists %+v: an owner of the instance reads nobody's mailboxes by being one", st.Mailboxes)
	}
	if st.DatabaseBytes == nil {
		t.Fatal("an owner signed in is not told the database's size")
	}
	var want int64
	for _, p := range []string{s.db.Path(), s.db.Path() + "-wal"} {
		if info, err := os.Stat(p); err == nil {
			want += info.Size()
		}
	}
	if *st.DatabaseBytes != want || want == 0 {
		t.Errorf("database_bytes = %d, want the file and its log, %d", *st.DatabaseBytes, want)
	}
	// A key she created acts as nobody: the database's size is for her at
	// the console.
	if st := s.storage(t, keyOf(t, s.fixture, s.olga, auth.ScopeRead)); st.DatabaseBytes != nil {
		t.Errorf("an owner's key is told the database's size: %d", *st.DatabaseBytes)
	}
}

func TestAKeyHoldingOneMailboxSeesOnlyItsStorage(t *testing.T) {
	s := newStorageFixture(t)
	key := s.authenticate(t, authtest.NewWorkspaceKey(t, s.db, auth.ScopeRead, authtest.Personal(t, s.db, s.ana.UserID),
		s.ana.UserID, workspace.KeyGrant{AccountID: s.home, Flags: workspace.Flags{Read: true}}))
	st := s.storage(t, key)
	if got := mailboxes(st); len(got) != 1 || got[s.home].Bytes != 500 || st.Total.Bytes != 500 {
		t.Fatalf("a key for ana's home mailbox sees %+v, total %+v", st.Mailboxes, st.Total)
	}

	// An instance key answers for the operator: the mailbox nobody owns.
	instance := reader()
	if got := mailboxes(s.storage(t, instance)); len(got) != 1 || got[s.team].Bytes != 9000 {
		t.Fatalf("an instance key sees %+v", got)
	}
	instance.AccountIDs = []string{s.bobs}
	if st := s.storage(t, instance); len(st.Mailboxes) != 0 || st.Total != (service.StorageTotal{}) {
		t.Fatalf("an instance key restricted to bob's mailbox sees %+v", st)
	}
}
