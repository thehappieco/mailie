package workspace_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// A snapshot of who holds what on a workspace's mailboxes (snapshot.go): what
// an extension reports elsewhere, read by the one rule, at one moment.

// snapshot reads a workspace's snapshot in a read of its own, removed
// mailboxes reported as live when asLive says so.
func (f *fixture) snapshot(workspaceID string, asLive bool) workspace.Snapshot {
	f.t.Helper()
	s, err := f.trySnapshot(workspaceID, asLive)
	if err != nil {
		f.t.Fatalf("the snapshot of %s: %v", workspaceID, err)
	}
	return s
}

func (f *fixture) trySnapshot(workspaceID string, asLive bool) (workspace.Snapshot, error) {
	f.t.Helper()
	tx, err := f.db.ReadTx(f.t.Context())
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	return workspace.SnapshotTx(f.t.Context(), tx, workspaceID, workspace.SnapshotOptions{RemovedAsLive: asLive})
}

// mailboxOf is a mailbox of a snapshot.
func mailboxOf(t *testing.T, s workspace.Snapshot, accountID string) workspace.SnapshotMailbox {
	t.Helper()
	for _, mb := range s.Mailboxes {
		if mb.ID == accountID {
			return mb
		}
	}
	t.Fatalf("%s is not in the snapshot of %s", accountID, s.WorkspaceID)
	return workspace.SnapshotMailbox{}
}

// personOf is a person of a snapshot's mailbox; false when they are not
// listed.
func personOf(mb workspace.SnapshotMailbox, userID string) (workspace.SnapshotPerson, bool) {
	for _, p := range mb.People {
		if p.UserID == userID {
			return p, true
		}
	}
	return workspace.SnapshotPerson{}, false
}

// activate makes a mailbox active, under its team's consent to sync.
func (f *fixture) activate(accountID string) {
	f.t.Helper()
	f.exec(`UPDATE accounts SET state = 'active', sync_enabled_at = 900, sync_enabled_by = 'cli' WHERE id = ?`, accountID)
}

func TestASnapshotListsLiveAndRemovedMailboxesWithTheirState(t *testing.T) {
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	support := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember})
	syncing := f.link(support.ID, ana.ID, "desk@example.org")
	f.activate(syncing.ID)
	waiting := f.link(support.ID, ana.ID, "waiting@example.org")
	f.exec(`UPDATE accounts SET state = 'pending_auth' WHERE id = ?`, waiting.ID)
	reauth := f.link(support.ID, ana.ID, "reauth@example.org")
	f.activate(reauth.ID)
	f.exec(`UPDATE accounts SET state = 'needs_reauth' WHERE id = ?`, reauth.ID)
	off := f.link(support.ID, ana.ID, "off@example.org")
	f.exec(`UPDATE accounts SET state = 'active' WHERE id = ?`, off.ID)
	removed := f.link(support.ID, ana.ID, "gone@example.org")
	f.activate(removed.ID)
	f.remove(removed.ID, ana.ID)
	purged := f.link(support.ID, ana.ID, "purged@example.org")
	f.exec(`DELETE FROM accounts WHERE id = ?`, purged.ID)

	s := f.snapshot(support.ID, true)
	if s.WorkspaceID != support.ID || s.Kind != workspace.KindTeam || s.Closed {
		t.Fatalf("the snapshot's workspace: %+v", s)
	}
	var got []string
	for _, mb := range s.Mailboxes {
		got = append(got, fmt.Sprintf("%s removed=%v purge=%d pause=%q", mb.ID, mb.Removed, mb.PurgeAfter, mb.Pause))
	}
	want := []string{
		syncing.ID + ` removed=false purge=0 pause=""`,
		waiting.ID + ` removed=false purge=0 pause="pending_auth"`,
		reauth.ID + ` removed=false purge=0 pause="needs_reauth"`,
		off.ID + ` removed=false purge=0 pause="consent_off"`,
		removed.ID + fmt.Sprintf(` removed=true purge=%d pause=""`, 900+30*24*60*60),
	}
	if !slices.Equal(got, want) {
		t.Errorf("the snapshot's mailboxes:\n%v\nwant\n%v", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Ana's personal workspace: her own mailbox, under her consent.
	personal := f.personal(ana.ID)
	own := f.link(personal, ana.ID, "ana@mail.example.org")
	f.exec(`UPDATE accounts SET state = 'active' WHERE id = ?`, own.ID)
	ps := f.snapshot(personal, true)
	if ps.Kind != workspace.KindPersonal || len(ps.Mailboxes) != 1 || ps.Mailboxes[0].Pause != store.PauseConsentOff {
		t.Errorf("Ana's personal snapshot: %+v", ps)
	}
	f.exec(`UPDATE users SET sync_consent_at = 900, sync_consent_version = 'sync-1' WHERE id = ?`, ana.ID)
	if ps := f.snapshot(personal, true); ps.Mailboxes[0].Pause != "" {
		t.Errorf("Ana's own mailbox under her consent: pause %q", ps.Mailboxes[0].Pause)
	}
	if _, err := f.trySnapshot("wsp_nobodyknows", true); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("the snapshot of a workspace nobody knows: %v", err)
	}
}

func TestASnapshotsPeopleFollowTheReaderRule(t *testing.T) {
	// A keyed team mailbox: Ana holds its key and reads it; Bea holds read
	// and waits for the key; Cid has no account key and holds read; Dan
	// manages it alone; Eve's membership is disabled, her flags kept; Fay's
	// membership is held by a directory, her grant kept.
	f := newFixture(t)
	ana, bea, cid := f.person("ana@example.org"), f.person("bea@example.org"), f.legacy("cid@example.org")
	dan, eve, fay := f.person("dan@example.org"), f.person("eve@example.org"), f.person("fay@example.org")
	team := f.elsewhere("9f0c1d2e-0000-4000-8000-0000000000b2", "Support", map[string]workspace.Role{
		ana.ID: workspace.RoleOwner, bea.ID: workspace.RoleMember, cid.ID: workspace.RoleMember,
		dan.ID: workspace.RoleMember, eve.ID: workspace.RoleMember, fay.ID: workspace.RoleMember,
	})
	box := f.link(team.ID, ana.ID, "support@example.org")
	f.activate(box.ID)
	// Read before the key, which its first key gives each of them a grant
	// of: Bea's goes, as a reset would take it, and Eve's with her
	// membership's disable.
	for _, who := range []string{bea.ID, cid.ID, eve.ID, fay.ID} {
		f.grant(box.ID, who, readOnly())
	}
	f.grant(box.ID, eve.ID, workspace.Flags{Read: true, Manage: true})
	authtest.KeyMailbox(t, f.db, box.ID, ana.ID)
	f.grant(box.ID, dan.ID, manageOnly())
	f.exec(`DELETE FROM mailbox_grants WHERE account_id = ? AND user_id IN (?, ?)`, box.ID, bea.ID, eve.ID)
	f.exec(`UPDATE workspace_members SET status = 'disabled' WHERE workspace_id = ? AND user_id = ?`, team.ID, eve.ID)
	f.exec(`UPDATE workspace_members SET status = 'disabled', held_at = 950 WHERE workspace_id = ? AND user_id = ?`,
		team.ID, fay.ID)

	mb := mailboxOf(t, f.snapshot(team.ID, true), box.ID)
	type row struct {
		read, manage, reads, waiting, enrolled, holds bool
	}
	for who, want := range map[string]row{
		ana.ID: {read: true, reads: true, enrolled: true, holds: true},
		bea.ID: {read: true, waiting: true, enrolled: true},
		cid.ID: {read: true, waiting: true},
		dan.ID: {manage: true, enrolled: true},
		eve.ID: {read: true, manage: true, enrolled: true},
		fay.ID: {read: true, enrolled: true, holds: true},
	} {
		p, ok := personOf(mb, who)
		if !ok {
			t.Errorf("%s is not listed", who)
			continue
		}
		got := row{read: p.Flags.Read, manage: p.Flags.Manage, reads: p.Reads, waiting: p.Waiting, enrolled: p.Enrolled,
			holds: p.HoldsKey}
		if got != want {
			t.Errorf("%s: %+v, want %+v", who, got, want)
		}
		if p.Reads != f.reads(box.ID, who) || p.Waiting != f.waiting(who, box.ID) {
			t.Errorf("%s: the snapshot says reads %v, waiting %v, and the rule %v, %v", who, p.Reads, p.Waiting,
				f.reads(box.ID, who), f.waiting(who, box.ID))
		}
	}
	if len(mb.People) != 6 {
		t.Errorf("%d people listed, want the six who hold something", len(mb.People))
	}
	if !slices.Equal(mb.Protected, []string{ana.ID}) {
		t.Errorf("protected: %v, want Ana, its only reader", mb.Protected)
	}
}

func TestARemovedMailboxIsReportedAsLiveByDefaultInASnapshot(t *testing.T) {
	// Ana alone reads a keyed team mailbox, which is removed: reported as
	// live, she reads it, holds its key and is protected, as she would be
	// once it is recovered; Bea, waiting for its key, still waits. Nothing
	// here changes: she reads nothing, and protects nothing in what this
	// server refuses.
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	support := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember})
	box := f.link(support.ID, ana.ID, "support@example.org")
	f.activate(box.ID)
	f.grant(box.ID, bea.ID, readOnly())
	authtest.KeyMailbox(t, f.db, box.ID, ana.ID)
	f.exec(`DELETE FROM mailbox_grants WHERE account_id = ? AND user_id = ?`, box.ID, bea.ID)
	f.remove(box.ID, ana.ID)

	mb := mailboxOf(t, f.snapshot(support.ID, true), box.ID)
	if a, _ := personOf(mb, ana.ID); !a.Reads || !a.HoldsKey || a.Waiting {
		t.Errorf("Ana on the removed mailbox, reported as live: %+v", a)
	}
	if b, _ := personOf(mb, bea.ID); b.Reads || b.HoldsKey || !b.Waiting {
		t.Errorf("Bea on the removed mailbox, reported as live: %+v", b)
	}
	if !slices.Equal(mb.Protected, []string{ana.ID}) {
		t.Errorf("protected on the removed mailbox, reported as live: %v, want Ana", mb.Protected)
	}
	if f.reads(box.ID, ana.ID) || f.hasReader(box.ID) {
		t.Error("a snapshot made Ana a reader of a removed mailbox here")
	}
}

func TestARemovedMailboxProtectsNobodyAndHoldsNoKeyInASnapshot(t *testing.T) {
	// The other way of the one switch: a removed mailbox as the rules here
	// see it.
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	support := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember})
	box := f.link(support.ID, ana.ID, "support@example.org")
	f.activate(box.ID)
	f.grant(box.ID, bea.ID, readOnly())
	authtest.KeyMailbox(t, f.db, box.ID, ana.ID)
	f.exec(`DELETE FROM mailbox_grants WHERE account_id = ? AND user_id = ?`, box.ID, bea.ID)
	f.remove(box.ID, ana.ID)

	mb := mailboxOf(t, f.snapshot(support.ID, false), box.ID)
	if !mb.Removed || mb.Pause != "" || len(mb.Protected) != 0 {
		t.Errorf("the removed mailbox: removed %v, pause %q, protected %v", mb.Removed, mb.Pause, mb.Protected)
	}
	if len(mb.People) != 2 {
		t.Errorf("%d people listed on the removed mailbox, want the two who hold something", len(mb.People))
	}
	for _, p := range mb.People {
		if p.Reads || p.Waiting || p.HoldsKey {
			t.Errorf("%s on a removed mailbox: %+v, want nothing read, waited for or held", p.UserID, p)
		}
	}
}

func TestASuspendedHolderHoldsTheKeyButNeitherReadsNorWaitsNorIsProtected(t *testing.T) {
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	support := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember})
	box := f.link(support.ID, ana.ID, "support@example.org")
	f.activate(box.ID)
	f.grant(box.ID, bea.ID, readOnly())
	authtest.KeyMailbox(t, f.db, box.ID, ana.ID)
	f.exec(`DELETE FROM mailbox_access WHERE account_id = ? AND user_id = ?`, box.ID, ana.ID)
	f.suspend(bea.ID)

	mb := mailboxOf(t, f.snapshot(support.ID, true), box.ID)
	b, ok := personOf(mb, bea.ID)
	if !ok || !b.HoldsKey || b.Reads || b.Waiting || !b.Flags.Read {
		t.Errorf("Bea, suspended, holding the key: %+v (listed %v)", b, ok)
	}
	if len(mb.Protected) != 0 {
		t.Errorf("protected: %v, want nobody: a suspended person reads nothing", mb.Protected)
	}
	if mb.Pause != store.PauseNoReader {
		t.Errorf("the pause: %q, want %q", mb.Pause, store.PauseNoReader)
	}
}

func TestASnapshotReadsIgnoreTheTeamsClosure(t *testing.T) {
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	support := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember})
	box := f.link(support.ID, ana.ID, "support@example.org")
	f.activate(box.ID)
	f.grant(box.ID, bea.ID, readOnly())
	open := mailboxOf(t, f.snapshot(support.ID, true), box.ID)
	f.exec(`UPDATE workspaces SET closed_at = 990 WHERE id = ?`, support.ID)

	s := f.snapshot(support.ID, true)
	closed := mailboxOf(t, s, box.ID)
	if !s.Closed || closed.Pause != store.PauseTeamClosed {
		t.Errorf("the closed team: closed %v, pause %q", s.Closed, closed.Pause)
	}
	if fmt.Sprint(closed.People) != fmt.Sprint(open.People) || !slices.Equal(closed.Protected, open.Protected) {
		t.Errorf("the closed team's mailbox:\n%+v\nopen:\n%+v", closed, open)
	}
	for _, p := range closed.People {
		if !p.Reads {
			t.Errorf("%s reads nothing once the team is closed: %+v", p.UserID, p)
		}
	}
}

func TestASnapshotNeverHoldsAnAddress(t *testing.T) {
	f := newFixture(t)
	ana := f.person("ana@example.org")
	support := f.team("Support", ana, nil)
	named := f.link(support.ID, ana.ID, "helpdesk@corp.example.org")
	f.exec(`UPDATE accounts SET display_name = 'Helpdesk Team' WHERE id = ?`, named.ID)
	addressed := f.link(support.ID, ana.ID, "billing@corp.example.org")
	f.exec(`UPDATE accounts SET display_name = 'billing@corp.example.org' WHERE id = ?`, addressed.ID)
	plain := f.link(support.ID, ana.ID, "sales@corp.example.org")
	f.exec(`UPDATE accounts SET display_name = 'Front desk' WHERE id = ?`, plain.ID)
	// Its local part, whatever the case: the label goes.
	cased := f.link(support.ID, ana.ID, "orders@corp.example.org")
	f.exec(`UPDATE accounts SET display_name = 'ORDERS desk' WHERE id = ?`, cased.ID)

	s := f.snapshot(support.ID, true)
	labels := map[string]string{}
	for _, mb := range s.Mailboxes {
		labels[mb.ID] = mb.Label + " | " + mb.Hint
	}
	for id, want := range map[string]string{
		named.ID:     " | h…@corp.example.org",
		addressed.ID: " | b…@corp.example.org",
		plain.ID:     "Front desk | s…@corp.example.org",
		cased.ID:     " | o…@corp.example.org",
	} {
		if labels[id] != want {
			t.Errorf("%s: %q, want %q", id, labels[id], want)
		}
	}
	// Every field but the ids, which the test's own links derive from the
	// address.
	for i := range s.Mailboxes {
		s.Mailboxes[i].ID = "id"
	}
	whole := fmt.Sprintf("%+v", s)
	for _, needle := range []string{"helpdesk", "billing", "sales", "orders"} {
		if strings.Contains(strings.ToLower(whole), needle) {
			t.Errorf("the snapshot holds %q: %s", needle, whole)
		}
	}
}

func TestASnapshotRefusesMoreThanItsLimits(t *testing.T) {
	f := newFixture(t)
	ana := f.person("ana@example.org")
	support := f.team("Support", ana, nil)
	box := f.link(support.ID, ana.ID, "support@example.org")
	// As many people on one mailbox as a snapshot holds, then one more.
	f.exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO users(id, email, name, password_hash, role, status, password_changed_at, created_at, updated_at)
		SELECT printf('usr_crowd%07d', i), printf('p%d@crowd.example.org', i), '', '', 'member', 'active', 1, 1, 1 FROM n`,
		workspace.SnapshotMaxPeople)
	f.exec(`INSERT INTO workspace_members(workspace_id, user_id, role, status, created_at, updated_at)
		SELECT ?, id, 'member', 'active', 1, 1 FROM users WHERE id GLOB 'usr_crowd*'`, support.ID)
	f.exec(`INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act, send, manage, granted_by,
		created_at, updated_at)
		SELECT ?, ?, id, 0, 0, 0, 1, 'cli', 1, 1 FROM users WHERE id GLOB 'usr_crowd*' AND id <> 'usr_crowd0000001'`,
		box.ID, support.ID)
	if s, err := f.trySnapshot(support.ID, true); err != nil || len(mailboxOf(t, s, box.ID).People) != workspace.SnapshotMaxPeople {
		t.Fatalf("a mailbox with as many people as a snapshot holds: %v", err)
	}
	f.exec(`INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act, send, manage, granted_by,
		created_at, updated_at) VALUES (?, ?, 'usr_crowd0000001', 0, 0, 0, 1, 'cli', 1, 1)`, box.ID, support.ID)
	if _, err := f.trySnapshot(support.ID, true); !errors.Is(err, workspace.ErrSnapshotTooLarge) {
		t.Errorf("one person too many: %v, want ErrSnapshotTooLarge", err)
	}

	// As many mailboxes as a snapshot holds, then one more.
	other := f.team("Ops", ana, nil)
	f.exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO accounts(id, workspace_id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port,
		  smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at)
		SELECT printf('acc_many%07d', i), ?, printf('m%d@ops.example.org', i), 'imap', 'password', 'imap.example.org',
		  993, 'smtp.example.org', 587, 'starttls', 'm', 1, 'active', 1, 1, 1 FROM n`,
		workspace.SnapshotMaxMailboxes, other.ID)
	if s, err := f.trySnapshot(other.ID, true); err != nil || len(s.Mailboxes) != workspace.SnapshotMaxMailboxes {
		t.Fatalf("a workspace with as many mailboxes as a snapshot holds: %v", err)
	}
	f.link(other.ID, ana.ID, "one-more@ops.example.org")
	if _, err := f.trySnapshot(other.ID, true); !errors.Is(err, workspace.ErrSnapshotTooLarge) {
		t.Errorf("one mailbox too many: %v, want ErrSnapshotTooLarge", err)
	}
}

func TestASnapshotIsOneConsistentRead(t *testing.T) {
	// A read begun before Bea is given read sees none of it, however late it
	// reads the people; the next read sees all of it.
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	support := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember})
	box := f.link(support.ID, ana.ID, "support@example.org")
	f.activate(box.ID)

	tx, err := f.db.ReadTx(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var mailboxes int
	if err := tx.QueryRowContext(t.Context(), `SELECT count(*) FROM accounts WHERE workspace_id = ?`, support.ID).
		Scan(&mailboxes); err != nil || mailboxes != 1 {
		t.Fatalf("the read's first query: %d, %v", mailboxes, err)
	}
	f.grant(box.ID, bea.ID, readOnly())
	f.link(support.ID, ana.ID, "later@example.org")

	before, err := workspace.SnapshotTx(t.Context(), tx, support.ID, workspace.SnapshotOptions{RemovedAsLive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Mailboxes) != 1 {
		t.Errorf("the read begun before sees %d mailboxes, want 1", len(before.Mailboxes))
	}
	if _, ok := personOf(mailboxOf(t, before, box.ID), bea.ID); ok {
		t.Error("the read begun before sees Bea's read")
	}
	after := f.snapshot(support.ID, true)
	if b, ok := personOf(mailboxOf(t, after, box.ID), bea.ID); len(after.Mailboxes) != 2 || !ok || !b.Reads {
		t.Errorf("the next read: %d mailboxes, Bea %+v (listed %v)", len(after.Mailboxes), b, ok)
	}
}
