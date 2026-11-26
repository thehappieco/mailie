package service_test

import (
	"os"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// storageFixture is three people's mailboxes and one nobody owns, indexed:
// ana's work mailbox holds 1000 and 2000 bytes on the mail server, her home
// one 500, bob's 7000 and the operator's 9000.
type storageFixture struct {
	*mailFixture
	ana, bob, olga         service.Principal
	work, home, bobs, team string
}

func newStorageFixture(t *testing.T) *storageFixture {
	t.Helper()
	return newStorageFixtureWith(t, fixtureOptions{})
}

func newStorageFixtureWith(t *testing.T, o fixtureOptions) *storageFixture {
	t.Helper()
	m := newMailFixtureWith(t, o)
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
		// A password, so that removing it keeps it: a link that never
		// connected is deleted at once.
		if err := m.repo.SavePassword(t.Context(), id, "app-password"); err != nil {
			t.Fatal(err)
		}
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
	return s.storageIn(t, p, "")
}

func (s *storageFixture) storageIn(t *testing.T, p service.Principal, workspaceID string) service.Storage {
	t.Helper()
	got, err := s.svc.Storage(t.Context(), p, workspaceID)
	if err != nil {
		t.Fatalf("Storage: %v", err)
	}
	if got.RuleVersion != store.RuleVersion {
		t.Fatalf("rule_version = %d, want %d", got.RuleVersion, store.RuleVersion)
	}
	return got
}

// held is what the store counts for a mailbox under the usage rule.
func (s *storageFixture) held(t *testing.T, id string) store.MailboxUsage {
	t.Helper()
	usage, err := s.db.Usage(t.Context(), []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if usage[id].Bytes == 0 {
		t.Fatalf("the store counts nothing for %s", id)
	}
	return usage[id]
}

// remove removes a mailbox as p, kept for its 30 days.
func (s *storageFixture) remove(t *testing.T, p service.Principal, id string) {
	t.Helper()
	removal, err := s.svc.RemoveAccount(t.Context(), p, id, service.RemoveAccountRequest{Confirm: id})
	if err != nil {
		t.Fatalf("removing %s: %v", id, err)
	}
	if removal.Purged {
		t.Fatalf("%s was deleted for good at once, not kept", id)
	}
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
	work, home := s.held(t, s.work), s.held(t, s.home)
	want := map[string]service.MailboxStorage{
		s.work: {AccountID: s.work, WorkspaceID: personal, Email: "ana@work.example", Messages: 2, Bytes: work.Bytes, ProviderBytes: 3000},
		s.home: {AccountID: s.home, WorkspaceID: personal, Email: "ana@home.example", Messages: 1, Bytes: home.Bytes, ProviderBytes: 500},
	}
	if len(got) != len(want) || got[s.work] != want[s.work] || got[s.home] != want[s.home] {
		t.Fatalf("ana's storage lists %+v, want %+v", st.Mailboxes, want)
	}
	if st.Total != (service.StorageTotal{Messages: 3, Bytes: work.Bytes + home.Bytes, ProviderBytes: 3500}) {
		t.Errorf("total = %+v", st.Total)
	}
	// Summed per workspace: both are in her personal one.
	if len(st.Workspaces) != 1 || st.Workspaces[0] != (service.WorkspaceStorage{
		WorkspaceID: personal, Mailboxes: 2, Messages: 3, Bytes: work.Bytes + home.Bytes, ProviderBytes: 3500,
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
		"an instance key": reader(), "the operator": admin(),
	} {
		for _, m := range s.storage(t, p).Mailboxes {
			if m.AccountID == s.work || m.AccountID == s.home {
				t.Errorf("%s sees ana's mailbox %s", name, m.Email)
			}
		}
	}
	if got := mailboxes(s.storage(t, s.bob)); len(got) != 1 || got[s.bobs].ProviderBytes != 7000 ||
		got[s.bobs].Bytes != s.held(t, s.bobs).Bytes {
		t.Errorf("bob's storage = %+v", got)
	}
}

func TestOnlyTheOperatorIsToldTheDatabaseSize(t *testing.T) {
	s := newStorageFixture(t)
	// An owner of the instance, signed in, reads nobody's mailboxes by
	// being one, and is no longer told what the whole database takes.
	st := s.storage(t, s.olga)
	if len(st.Mailboxes) != 0 || st.Total != (service.StorageTotal{}) {
		t.Fatalf("olga's storage lists %+v: an owner of the instance reads nobody's mailboxes by being one", st.Mailboxes)
	}
	if st.DatabaseBytes != nil {
		t.Errorf("an owner signed in is told the database's size: %d", *st.DatabaseBytes)
	}

	// The operator, an unrestricted instance key with the admin scope.
	st = s.storage(t, admin())
	if st.DatabaseBytes == nil {
		t.Fatal("the operator is not told the database's size")
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

	restricted := admin()
	restricted.AccountIDs = []string{s.team}
	for name, p := range map[string]service.Principal{
		"an instance key that reads": reader(), "an admin instance key restricted to a mailbox": restricted,
		"a key olga created": keyOf(t, s.fixture, s.olga, auth.ScopeRead),
		"a key of ana's":     keyOf(t, s.fixture, s.ana, auth.ScopeRead), "ana": s.ana,
	} {
		if st := s.storage(t, p); st.DatabaseBytes != nil {
			t.Errorf("%s is told the database's size: %d", name, *st.DatabaseBytes)
		}
	}
}

func TestAKeyHoldingOneMailboxSeesOnlyItsStorage(t *testing.T) {
	s := newStorageFixture(t)
	key := s.authenticate(t, authtest.NewWorkspaceKey(t, s.db, auth.ScopeRead, authtest.Personal(t, s.db, s.ana.UserID),
		s.ana.UserID, workspace.KeyGrant{AccountID: s.home, Flags: workspace.Flags{Read: true}}))
	st := s.storage(t, key)
	home := s.held(t, s.home)
	if got := mailboxes(st); len(got) != 1 || got[s.home].ProviderBytes != 500 || st.Total.ProviderBytes != 500 ||
		got[s.home].Bytes != home.Bytes || st.Total.Bytes != home.Bytes {
		t.Fatalf("a key for ana's home mailbox sees %+v, total %+v", st.Mailboxes, st.Total)
	}

	// An instance key answers for the operator: the mailbox nobody owns.
	instance := reader()
	if got := mailboxes(s.storage(t, instance)); len(got) != 1 || got[s.team].ProviderBytes != 9000 {
		t.Fatalf("an instance key sees %+v", got)
	}
	instance.AccountIDs = []string{s.bobs}
	if st := s.storage(t, instance); len(st.Mailboxes) != 0 || st.Total != (service.StorageTotal{}) {
		t.Fatalf("an instance key restricted to bob's mailbox sees %+v", st)
	}
}

func TestARemovedMailboxCountsUntilItIsDeletedForGood(t *testing.T) {
	// Platform decision 0042: what a removed mailbox keeps counts until its
	// purge, and it is told to whoever lists it on the removed mailboxes'
	// own paths, its person here.
	s := newStorageFixture(t)
	before := s.storage(t, s.ana)
	s.remove(t, s.ana, s.home)

	st := s.storage(t, s.ana)
	got := mailboxes(st)
	if m := got[s.home]; len(got) != 2 || !m.Removed || m.Messages != 1 || m.ProviderBytes != 500 ||
		m.Bytes != s.held(t, s.home).Bytes || got[s.work].Removed {
		t.Fatalf("after removing her home mailbox, ana's storage lists %+v", st.Mailboxes)
	}
	if st.Total != before.Total || len(st.Workspaces) != 1 || st.Workspaces[0] != before.Workspaces[0] {
		t.Errorf("a removed mailbox still counts: total %+v and %+v, before %+v and %+v",
			st.Total, st.Workspaces, before.Total, before.Workspaces)
	}
	// Narrowed to her personal workspace, the same.
	if got := mailboxes(s.storageIn(t, s.ana, authtest.Personal(t, s.db, s.ana.UserID))); !got[s.home].Removed {
		t.Errorf("narrowed to her workspace, ana's storage lists %+v", got)
	}
	// Nobody who does not list it removed is told of it: a key of her
	// workspace, which never lists removed mailboxes, the operator, bob.
	for name, p := range map[string]service.Principal{
		"a key of her workspace": keyOf(t, s.fixture, s.ana, auth.ScopeRead), "the operator": admin(), "bob": s.bob,
	} {
		if _, seen := mailboxes(s.storage(t, p))[s.home]; seen {
			t.Errorf("%s is told of ana's removed mailbox", name)
		}
	}

	// Deleted for good, it counts no more.
	if err := s.svc.PurgeAccount(t.Context(), s.ana, s.home, service.PurgeAccountRequest{Confirm: s.home}); err != nil {
		t.Fatal(err)
	}
	st = s.storage(t, s.ana)
	if got := mailboxes(st); len(got) != 1 || got[s.work].Removed {
		t.Fatalf("after deleting it for good, ana's storage lists %+v", st.Mailboxes)
	}
	if want := before.Total.ProviderBytes - 500; st.Total.ProviderBytes != want ||
		st.Total.Bytes != s.held(t, s.work).Bytes {
		t.Errorf("after deleting it for good, the total is %+v", st.Total)
	}
}

func TestARemovedTeamMailboxCountsForTheTeamsOwnersAlone(t *testing.T) {
	s := &storageFixture{mailFixture: newMailFixture(t)}
	tm := newKeysTeam(t, s.fixture)
	if err := s.repo.SavePassword(t.Context(), tm.shared, "app-password"); err != nil {
		t.Fatal(err)
	}
	tm.grant(t, tm.shared, workspace.Flags{Read: true})
	s.seedIndex(t, tm.shared, "Quartz")
	held := s.held(t, tm.shared)
	s.remove(t, tm.ana, tm.shared)

	// Ana, the team's active owner, lists it removed, and so is told what
	// it keeps, here and narrowed to the team.
	for _, workspaceID := range []string{"", tm.id} {
		st := s.storageIn(t, tm.ana, workspaceID)
		if m := mailboxes(st)[tm.shared]; !m.Removed || m.WorkspaceID != tm.id || m.Bytes != held.Bytes ||
			m.Messages != held.Messages {
			t.Fatalf("in %q, ana's storage lists %+v", workspaceID, st.Mailboxes)
		}
	}
	// Bea, who read it, Carol, an admin, and the operator do not list it
	// removed, and are not told of it.
	for name, p := range map[string]service.Principal{"Bea": tm.bea, "Carol": tm.carol, "the operator": admin()} {
		if _, seen := mailboxes(s.storage(t, p))[tm.shared]; seen {
			t.Errorf("%s is told of the team's removed mailbox", name)
		}
	}
	if st := s.storageIn(t, tm.carol, tm.id); len(st.Mailboxes) != 0 {
		t.Errorf("narrowed to the team, Carol's storage lists %+v", st.Mailboxes)
	}
}

func TestTheOperatorCountsTheOperatorWorkspacesRemovedMailboxes(t *testing.T) {
	s := newStorageFixture(t)
	s.remove(t, admin(), s.team)
	got := mailboxes(s.storage(t, admin()))
	if m := got[s.team]; len(got) != 1 || !m.Removed || m.ProviderBytes != 9000 || m.Bytes != s.held(t, s.team).Bytes {
		t.Fatalf("the operator's storage lists %+v", got)
	}
	// An instance key that only reads never lists a removed mailbox, nor
	// does one restricted to it.
	restricted := admin()
	restricted.AccountIDs = []string{s.team}
	for name, p := range map[string]service.Principal{"a reading instance key": reader(), "a restricted one": restricted} {
		if got := mailboxes(s.storage(t, p)); len(got) != 0 {
			t.Errorf("%s is told of the removed operator mailbox: %+v", name, got)
		}
	}
}

func TestARemovedTeamMailboxIsNotCountedWhereTeamsAreWithheld(t *testing.T) {
	s := &storageFixture{mailFixture: newMailFixtureWith(t, fixtureOptions{teamsWithheld: true})}
	tm := newKeysTeam(t, s.fixture)
	s.seedIndex(t, tm.shared, "Quartz")
	own, _ := s.fakeAccount(t, tm.ana, "ana@home.example")
	s.seedIndex(t, own, "Onyx")
	s.removeBySQL(t, tm.shared, tm.ana.UserID)
	s.removeBySQL(t, own, tm.ana.UserID)
	got := mailboxes(s.storage(t, tm.ana))
	if _, seen := got[tm.shared]; seen {
		t.Errorf("a withheld team's removed mailbox is counted for its owner: %+v", got)
	}
	if !got[own].Removed || got[own].Bytes != s.held(t, own).Bytes {
		t.Errorf("her own removed mailbox is not counted as ever: %+v", got)
	}
}
