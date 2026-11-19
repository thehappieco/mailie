package workspace_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/workspace"
)

// A workspace's settings (settings.go): what an extension reports beside a
// snapshot, read in the same transaction, with bounds of its own.

// settings reads a workspace's settings and its snapshot in one read.
func (f *fixture) settings(workspaceID string) (workspace.Settings, workspace.Snapshot, error) {
	f.t.Helper()
	tx, err := f.db.ReadTx(f.t.Context())
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	s, err := workspace.SettingsTx(f.t.Context(), tx, workspaceID)
	if err != nil {
		return workspace.Settings{}, workspace.Snapshot{}, err
	}
	snap, err := workspace.SnapshotTx(f.t.Context(), tx, workspaceID, workspace.SnapshotOptions{})
	return s, snap, err
}

// settingsOf is a mailbox of a workspace's settings.
func settingsOf(t *testing.T, s workspace.Settings, accountID string) workspace.MailboxSettings {
	t.Helper()
	for _, mb := range s.Mailboxes {
		if mb.ID == accountID {
			return mb
		}
	}
	t.Fatalf("%s is not in the settings", accountID)
	return workspace.MailboxSettings{}
}

func TestTheSettingsReadTheSnapshotsMailboxesInItsOrder(t *testing.T) {
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	support := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleAdmin})
	var linked []string
	for _, email := range []string{"zeta@example.org", "alpha@example.org", "mid@example.org", "gone@example.org"} {
		linked = append(linked, f.link(support.ID, ana.ID, email).ID)
	}
	f.remove(linked[3], ana.ID)
	s, snap, err := f.settings(support.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got, inSnapshot []string
	for _, mb := range s.Mailboxes {
		got = append(got, mb.ID)
	}
	for _, mb := range snap.Mailboxes {
		inSnapshot = append(inSnapshot, mb.ID)
	}
	if !slices.Equal(got, linked) || !slices.Equal(got, inSnapshot) {
		t.Errorf("the settings list %v, the snapshot %v; want both in the order they were linked, %v", got, inSnapshot, linked)
	}
	if s.Kind != workspace.KindTeam || s.Closed || s.MailboxesOver || s.MembersOver {
		t.Errorf("the team's settings: kind %s, closed %v, over %v/%v", s.Kind, s.Closed, s.MailboxesOver, s.MembersOver)
	}
	var members []string
	for _, m := range s.Members {
		members = append(members, m.UserID+" "+string(m.Role))
	}
	want := []string{ana.ID + " owner", bea.ID + " admin"}
	slices.Sort(want)
	if !slices.Equal(members, want) {
		t.Errorf("the members: %v, want %v", members, want)
	}
	if _, _, err := f.settings("wsp_nobodyknows"); err == nil {
		t.Error("the settings of a workspace nobody knows were read")
	}
}

// stringsIn lists every string a value holds, its fields' and its elements'.
func stringsIn(v reflect.Value) []string {
	switch v.Kind() {
	case reflect.String:
		return []string{v.String()}
	case reflect.Struct:
		var out []string
		for i := range v.NumField() {
			out = append(out, stringsIn(v.Field(i))...)
		}
		return out
	case reflect.Slice:
		var out []string
		for i := range v.Len() {
			out = append(out, stringsIn(v.Index(i))...)
		}
		return out
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return stringsIn(v.Elem())
	}
	return nil
}

func TestASettingsReadNeverCarriesAnAddress(t *testing.T) {
	// Every mailbox's name and host hold no address and nothing of one:
	// whatever the read carries, no string of it is one.
	f := newFixture(t)
	ana := f.person("ana@example.org")
	support := f.team("Support", ana, nil)
	for _, email := range []string{"helpdesk@example.org", "billing@example.org"} {
		box := f.link(support.ID, ana.ID, email)
		f.exec(`UPDATE accounts SET display_name = 'Desk', state = 'error', state_reason = 'a reason' WHERE id = ?`, box.ID)
	}
	s, _, err := f.settings(support.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Every field but the ids, which the test's own links derive from the
	// address.
	for i := range s.Mailboxes {
		s.Mailboxes[i].ID = "id"
	}
	all := stringsIn(reflect.ValueOf(s))
	if len(all) < 10 {
		t.Fatalf("the read holds %d strings; the test would prove nothing", len(all))
	}
	for _, v := range all {
		if strings.Contains(v, "@") || strings.Contains(v, "helpdesk") || strings.Contains(v, "billing") {
			t.Errorf("the settings carry %q", v)
		}
	}
}

func TestASettingsReadSaysWhenAListIsOverItsBoundAndReadsTheOtherWhole(t *testing.T) {
	f := newFixture(t)
	ana := f.person("ana@example.org")
	// As many members as the bound, with Ana, then one more.
	support := f.team("Support", ana, nil)
	f.exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO users(id, email, name, password_hash, role, status, password_changed_at, created_at, updated_at)
		SELECT printf('usr_crowd%07d', i), printf('p%d@crowd.example.org', i), '', '', 'member', 'active', 1, 1, 1 FROM n`,
		workspace.SnapshotMaxPeople)
	f.exec(`INSERT INTO workspace_members(workspace_id, user_id, role, status, created_at, updated_at)
		SELECT ?, id, 'member', 'active', 1, 1 FROM users WHERE id GLOB 'usr_crowd*' AND id <> 'usr_crowd0000001'`,
		support.ID)
	box := f.link(support.ID, ana.ID, "support@example.org")
	if s, _, err := f.settings(support.ID); err != nil || len(s.Members) != workspace.SnapshotMaxPeople || s.MembersOver {
		t.Fatalf("a team with as many members as the bound: %d members, over %v (%v)", len(s.Members), s.MembersOver, err)
	}
	f.exec(`INSERT INTO workspace_members(workspace_id, user_id, role, status, created_at, updated_at)
		VALUES (?, 'usr_crowd0000001', 'member', 'active', 1, 1)`, support.ID)
	s, _, err := f.settings(support.ID)
	if err != nil || !s.MembersOver || s.Members != nil {
		t.Fatalf("one member too many: %d members, over %v (%v)", len(s.Members), s.MembersOver, err)
	}
	if s.MailboxesOver || len(s.Mailboxes) != 1 || s.Mailboxes[0].ID != box.ID {
		t.Errorf("the mailboxes beside members over their bound: %+v, over %v", s.Mailboxes, s.MailboxesOver)
	}

	// As many mailboxes as the bound, then one more, beside members read
	// whole.
	other := f.team("Ops", ana, nil)
	f.exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO accounts(id, workspace_id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port,
		  smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at)
		SELECT printf('acc_many%07d', i), ?, printf('m%d@ops.example.org', i), 'imap', 'password', 'imap.example.org',
		  993, 'smtp.example.org', 587, 'starttls', 'm', 1, 'active', 1, 1, 1 FROM n`,
		workspace.SnapshotMaxMailboxes, other.ID)
	if s, _, err := f.settings(other.ID); err != nil || len(s.Mailboxes) != workspace.SnapshotMaxMailboxes || s.MailboxesOver {
		t.Fatalf("a team with as many mailboxes as the bound: %d, over %v (%v)", len(s.Mailboxes), s.MailboxesOver, err)
	}
	f.link(other.ID, ana.ID, "one-more@ops.example.org")
	tx, err := f.db.ReadTx(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	s, err = workspace.SettingsTx(t.Context(), tx, other.ID)
	if err != nil || !s.MailboxesOver || s.Mailboxes != nil || len(s.Members) != 1 || s.MembersOver {
		t.Errorf("one mailbox too many: %d mailboxes, over %v, %d members (%v)", len(s.Mailboxes), s.MailboxesOver,
			len(s.Members), err)
	}
}

func TestARemovedMailboxKeepsItsIndexExactlyWhileFoldersOrMessagesRemain(t *testing.T) {
	// A team mailbox migration 0011 left stopped and bound to its linker
	// keeps its index with its consent off; one whose consent stands may
	// have indexed nothing. Whether the index is kept is the index itself,
	// never the consent.
	f := newFixture(t)
	ana := f.person("ana@example.org")
	support := f.team("Support", ana, nil)
	bound := f.link(support.ID, ana.ID, "bound@example.org")
	f.exec(`UPDATE accounts SET sync_enabled_at = 0, sync_enabled_by = ?, sync_enabled_via = 'migration' WHERE id = ?`,
		ana.ID, bound.ID)
	f.exec(`INSERT INTO folders(account_id, name, display_name, role, role_source, uidvalidity) VALUES (?, 'INBOX', 'Inbox',
		'inbox', 'inbox', 7)`, bound.ID)
	empty := f.link(support.ID, ana.ID, "empty@example.org")
	f.activate(empty.ID)
	messages := f.link(support.ID, ana.ID, "messages@example.org")
	f.activate(messages.ID)
	f.exec(`INSERT INTO folders(id, account_id, name, display_name, role, role_source, uidvalidity) VALUES (77, ?, 'INBOX',
		'Inbox', 'inbox', 'inbox', 7)`, messages.ID)
	f.exec(`INSERT INTO messages(account_id, folder_id, uidvalidity, uid, message_id, group_key, subject, from_text, to_text,
		internal_date, size, first_seen_at, updated_at, body_text) VALUES (?, 77, 7, 1, 'm@x', 'mid:m@x', 'S', 'F', 'T', 1,
		1, 1, 1, 'b')`, messages.ID)
	for _, box := range []string{bound.ID, empty.ID, messages.ID} {
		f.remove(box, ana.ID)
	}
	s, _, err := f.settings(support.ID)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]struct{ kept, stands bool }{
		bound.ID:    {kept: true},
		empty.ID:    {stands: true},
		messages.ID: {kept: true, stands: true},
	} {
		mb := settingsOf(t, s, id)
		if mb.IndexKept != want.kept || mb.ConsentStands != want.stands || mb.RemovedAt == 0 {
			t.Errorf("%s: index kept %v, consent stands %v, removed at %d; want %v, %v", id, mb.IndexKept,
				mb.ConsentStands, mb.RemovedAt, want.kept, want.stands)
		}
	}
	if mb := settingsOf(t, s, bound.ID); !mb.Bound || mb.SyncEnabledAt != 0 {
		t.Errorf("the stopped bound mailbox: bound %v, agreed at %d", mb.Bound, mb.SyncEnabledAt)
	}
}
