package account_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
	"github.com/thehappieco/mailie/internal/workspace"
)

// A mailbox's pause as the repository keeps it (migration 0025): a
// removal ends it, a recovery starts one where the mailbox's consent stands,
// and the account reads it.

// pauseFixture is Ana, who consents to sync, and Bob, both of the team
// Support, which Ana owns; the repository and the store on one clock.
type pauseFixture struct {
	t        *testing.T
	db       *store.Store
	repo     *account.Repository
	registry *account.Registry
	ana, bob auth.User
	team     string
	now      time.Time
}

func newPauseFixture(t *testing.T) *pauseFixture {
	t.Helper()
	f := &pauseFixture{t: t, now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	clock := func() time.Time { return f.now }
	f.db = storetest.NewAt(t, filepath.Join(t.TempDir(), "mail.db"), clock)
	keyring, err := secrets.NewKeyring(1, map[uint8][]byte{1: bytesOf(0xA1)})
	if err != nil {
		t.Fatal(err)
	}
	f.repo = account.NewRepository(f.db, keyring).WithClock(clock)
	f.registry = account.NewRegistry(t.Context(), f.repo, account.RegistryOptions{})
	t.Cleanup(func() { _ = f.registry.Close() })
	f.ana = authtest.NewUser(t, f.db, "ana@example.org", auth.RoleMember)
	f.bob = authtest.NewUser(t, f.db, "bob@example.org", auth.RoleMember)
	exec(t, f.db, `UPDATE users SET sync_consent_at = 100, sync_consent_version = 'sync-1' WHERE id = ?`, f.ana.ID)
	team, err := workspace.NewRepository(f.db, nil).CreateTeam(t.Context(), "Support", f.ana.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.team = team.ID
	exec(t, f.db, `INSERT INTO workspace_members(workspace_id, user_id, role, status, created_at, updated_at)
		VALUES (?, ?, 'member', 'active', 100, 100)`, f.team, f.bob.ID)
	return f
}

// mailbox links a connected mailbox: Ana's own ("personal"), the team's
// under its consent, read by Ana and Bob ("team"), or the operator's,
// switched on ("operator"); indexed adds a folder to its index.
func (f *pauseFixture) mailbox(kind, email string, indexed bool) string {
	f.t.Helper()
	ctx := f.t.Context()
	a := account.Account{ID: "acc_" + email[:3] + "0000000000000", Email: email, Provider: provider.KindIMAP,
		AuthKind: "password", IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example",
		SMTPPort: 465, SMTPTLS: "implicit", LoginUser: email}
	linker := ""
	switch kind {
	case "personal":
		a.WorkspaceID, linker = authtest.Personal(f.t, f.db, f.ana.ID), f.ana.ID
	case "team":
		a.WorkspaceID, linker = f.team, f.ana.ID
	}
	created, err := f.repo.Create(ctx, a, linker)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.repo.SavePassword(ctx, created.ID, "hunter2"); err != nil {
		f.t.Fatal(err)
	}
	switch kind {
	case "team":
		exec(f.t, f.db, `UPDATE accounts SET sync_enabled_at = 100, sync_enabled_by = ?, sync_consent_version = 'sync-1'
			WHERE id = ?`, f.ana.ID, created.ID)
		for _, user := range []string{f.ana.ID, f.bob.ID} {
			exec(f.t, f.db, `INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, granted_by, created_at,
				updated_at) VALUES (?, ?, ?, 1, ?, 100, 100) ON CONFLICT DO UPDATE SET read = 1`,
				created.ID, f.team, user, f.ana.ID)
		}
	case "operator":
		exec(f.t, f.db, `UPDATE accounts SET sync_enabled_at = 100, sync_enabled_by = 'cli' WHERE id = ?`, created.ID)
	}
	if indexed {
		exec(f.t, f.db, `INSERT INTO folders(account_id, name, display_name, role, role_source, uidvalidity)
			VALUES (?, 'INBOX', 'Inbox', 'inbox', 'inbox', 1)`, created.ID)
	}
	return created.ID
}

func (f *pauseFixture) notices(userID string) []string {
	f.t.Helper()
	got, err := f.db.Notices(f.t.Context(), userID)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, n := range got {
		out = append(out, string(n.Event.Kind))
	}
	return out
}

func (f *pauseFixture) remove(id, actor string) {
	f.t.Helper()
	if _, err := f.registry.Remove(f.t.Context(), id, actor, nil); err != nil {
		f.t.Fatalf("removing %s: %v", id, err)
	}
}

func (f *pauseFixture) recover(id, actor string) account.Account {
	f.t.Helper()
	a, err := f.registry.Recover(f.t.Context(), id, actor, nil)
	if err != nil {
		f.t.Fatalf("recovering %s: %v", id, err)
	}
	return a
}

// unpausedNorHeld reports a recovered mailbox that came back off.
func unpausedNorHeld(a account.Account) bool {
	return a.SyncPausedAt.IsZero() && a.SyncPausedBy == "" && a.SyncHeldAt.IsZero()
}

func TestRemovingAPausedMailboxEndsItsPauseAndWithdrawsItsNotices(t *testing.T) {
	f := newPauseFixture(t)
	id := f.mailbox("team", "support@mail.example", true)
	if switched, err := f.db.PauseSync(t.Context(), id, f.ana.ID, nil); err != nil || !slices.Equal(switched.Told, []string{f.bob.ID}) {
		t.Fatalf("Ana's pause: %+v (%v)", switched, err)
	}
	if got := f.notices(f.bob.ID); !slices.Equal(got, []string{"paused"}) {
		t.Fatalf("Bob's notices: %v", got)
	}
	f.now = f.now.Add(time.Hour)
	f.remove(id, f.ana.ID)
	removed, err := f.repo.GetRemoved(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !removed.SyncPausedAt.IsZero() || removed.SyncPausedBy != "" {
		t.Errorf("the removed mailbox is still paused: %v by %q", removed.SyncPausedAt, removed.SyncPausedBy)
	}
	if got := f.notices(f.bob.ID); !slices.Equal(got, []string{"removed"}) {
		t.Errorf("Bob's notices after the removal: %v, want the removal's alone", got)
	}
}

func TestARecoveredMailboxComesBackPausedByWhoeverRecoveredIt(t *testing.T) {
	f := newPauseFixture(t)
	for _, c := range []struct {
		kind, email string
		indexed     bool
		actor       string
	}{
		{"personal", "own@mail.example", true, ""},
		{"personal", "new@mail.example", false, ""},
		{"team", "sup@mail.example", true, ""},
		{"team", "fresh@mail.example", false, ""},
		{"operator", "ops@mail.example", true, "cli"},
		{"operator", "opn@mail.example", false, "key:abcd0001"},
	} {
		actor := c.actor
		if actor == "" {
			actor = f.ana.ID
		}
		id := f.mailbox(c.kind, c.email, c.indexed)
		f.remove(id, actor)
		f.now = f.now.Add(time.Minute)
		recovered := f.recover(id, actor)
		if !recovered.SyncPausedAt.Equal(f.now) || recovered.SyncPausedBy != actor || !recovered.SyncHeldAt.IsZero() {
			t.Errorf("%s (%s, indexed %v) recovered paused at %v by %q, held at %v; want paused now by %s",
				id, c.kind, c.indexed, recovered.SyncPausedAt, recovered.SyncPausedBy, recovered.SyncHeldAt, actor)
		}
		if n := countOf(t, f.db, `SELECT count(*) FROM mailbox_events WHERE account_id = ? AND kind = 'recovered'
			AND actor = ?`, id, actor); n != 1 {
			t.Errorf("%s: %d recoveries journaled", id, n)
		}
	}
	if n := countOf(t, f.db, `SELECT count(*) FROM mailbox_events WHERE kind = 'paused'`); n != 0 {
		t.Errorf("a recovery's pause journaled %d pauses", n)
	}
	// Bob was told of the team's removals, which their recoveries withdrew;
	// their pauses tell him nothing.
	if got := f.notices(f.bob.ID); len(got) != 0 {
		t.Errorf("Bob's notices: %v", got)
	}
}

func TestARecoveredMailboxWhoseConsentIsOffComesBackOff(t *testing.T) {
	f := newPauseFixture(t)
	ctx := t.Context()
	own := f.mailbox("personal", "own@mail.example", true)
	off := f.mailbox("team", "off@mail.example", true)
	never := f.mailbox("team", "nev@mail.example", false)
	ops := f.mailbox("operator", "ops@mail.example", true)
	exec(t, f.db, `UPDATE accounts SET sync_enabled_at = 0, sync_enabled_by = '', sync_consent_version = '' WHERE id = ?`, never)
	for _, id := range []string{own, off, never, ops} {
		f.remove(id, f.ana.ID)
	}
	// Ana takes her consent back, which deletes the index of her removed
	// mailbox too; the team turns one off, the operator its own.
	if _, err := f.db.WithdrawSyncConsent(ctx, f.ana.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{off, ops} {
		if _, err := f.db.SetMailboxSync(ctx, id, false, "cli", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{own, off, never, ops} {
		if recovered := f.recover(id, f.ana.ID); !unpausedNorHeld(recovered) {
			t.Errorf("%s came back paused at %v or held at %v, want off", id, recovered.SyncPausedAt, recovered.SyncHeldAt)
		}
	}
}

func TestARecoveryKeepsTheOperatorsHold(t *testing.T) {
	f := newPauseFixture(t)
	ctx := t.Context()
	live := f.mailbox("personal", "liv@mail.example", true)
	removed := f.mailbox("personal", "rem@mail.example", true)
	if _, err := f.db.StopSync(ctx, live, store.ActorOperator); err != nil {
		t.Fatal(err)
	}
	f.remove(live, f.ana.ID)
	f.remove(removed, f.ana.ID)
	if _, err := f.db.StopSync(ctx, removed, store.ActorOperator); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{live, removed} {
		recovered := f.recover(id, f.ana.ID)
		if recovered.SyncHeldAt.IsZero() || !recovered.SyncPausedAt.IsZero() {
			t.Errorf("%s recovered held at %v and paused at %v, want held", id, recovered.SyncHeldAt, recovered.SyncPausedAt)
		}
	}
}

func TestAMailboxASuspendedReaderWaitsForIsRecoveredPaused(t *testing.T) {
	f := newPauseFixture(t)
	id := f.mailbox("team", "sup@mail.example", true)
	exec(t, f.db, `DELETE FROM mailbox_access WHERE account_id = ? AND user_id = ?`, id, f.ana.ID)
	f.remove(id, f.ana.ID)
	exec(t, f.db, `UPDATE users SET status = 'disabled', suspended_at = 200 WHERE id = ?`, f.bob.ID)
	recovered := f.recover(id, f.ana.ID)
	if recovered.SyncPausedAt.IsZero() {
		t.Error("the mailbox Bob would read once resumed came back unpaused")
	}
	if ok, err := f.db.SyncEligible(t.Context(), id); err != nil || ok {
		t.Errorf("it syncs while it waits: %v (%v)", ok, err)
	}
}

func TestDeletingAPausedLinkAtOnceLeavesNoNoticeOfItsPause(t *testing.T) {
	f := newPauseFixture(t)
	ctx := t.Context()
	const followed = "0b4a3c5e-0000-4000-8000-000000000009"
	exec(t, f.db, `INSERT INTO workspaces(id, kind, source, name, created_at, updated_at)
		VALUES (?, 'team', 'platform', 'Elsewhere', 100, 100)`, followed)
	for _, user := range []string{f.ana.ID, f.bob.ID} {
		exec(t, f.db, `INSERT INTO workspace_members(workspace_id, user_id, role, status, created_at, updated_at)
			VALUES (?, ?, 'owner', 'active', 100, 100)`, followed, user)
	}
	a, err := f.repo.Create(ctx, account.Account{ID: "acc_00000000000000d1", WorkspaceID: followed,
		Email: "orders@example.org", Provider: provider.KindGmail, AuthKind: "oauth2", IMAPHost: "imap.example.com",
		IMAPPort: 993, SMTPHost: "smtp.example.com", SMTPPort: 587, SMTPTLS: "starttls", LoginUser: "orders@example.org"},
		f.ana.ID)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, f.db, `UPDATE accounts SET sync_enabled_at = 100, sync_enabled_by = ?, sync_consent_version = 'sync-1'
		WHERE id = ?`, f.ana.ID, a.ID)
	for _, user := range []string{f.ana.ID, f.bob.ID} {
		exec(t, f.db, `INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, granted_by, created_at, updated_at)
			VALUES (?, ?, ?, 1, 'directory', 100, 100) ON CONFLICT DO UPDATE SET read = 1`, a.ID, followed, user)
	}
	if _, err := f.db.PauseSync(ctx, a.ID, f.ana.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.notices(f.bob.ID); !slices.Equal(got, []string{"paused"}) {
		t.Fatalf("Bob's notices: %v", got)
	}
	if removal, err := f.registry.Remove(ctx, a.ID, f.ana.ID, nil); err != nil || !removal.Purged {
		t.Fatalf("abandoning the link: %+v (%v)", removal, err)
	}
	if got := f.notices(f.bob.ID); len(got) != 0 {
		t.Errorf("Bob's notices after the link went: %v", got)
	}
	if got := countOf(t, f.db, `SELECT count(*) FROM mailbox_events WHERE account_id = ?`, a.ID); got != 2 {
		t.Errorf("the followed workspace's journal holds %d entries of the link, want its pause and its deletion", got)
	}
}

func TestAPausedMailboxsConsentAlwaysStands(t *testing.T) {
	f := newPauseFixture(t)
	ctx := t.Context()
	own := f.mailbox("personal", "own@mail.example", true)
	team := f.mailbox("team", "sup@mail.example", true)
	ops := f.mailbox("operator", "ops@mail.example", true)
	stands := func(step string) {
		t.Helper()
		if n := countOf(t, f.db, `SELECT count(*) FROM accounts a WHERE a.sync_paused_at <> 0 AND NOT (`+
			store.ConsentStandsSQL("a")+`)`); n != 0 {
			t.Errorf("%s: %d paused mailboxes whose consent does not stand", step, n)
		}
	}
	pause := func(ids ...string) {
		t.Helper()
		for _, id := range ids {
			if _, err := f.db.PauseSync(ctx, id, f.ana.ID, nil); err != nil {
				t.Fatalf("pausing %s: %v", id, err)
			}
		}
	}
	pause(own, team, ops)
	stands("every one paused")
	for _, step := range []struct {
		what string
		do   func() error
	}{
		{"Ana's withdrawal", func() error { _, err := f.db.WithdrawSyncConsent(ctx, f.ana.ID); return err }},
		{"the team's turn-off", func() error { _, err := f.db.SetMailboxSync(ctx, team, false, f.ana.ID, "", nil); return err }},
		{"the operator's switch off", func() error { _, err := f.db.SetMailboxSync(ctx, ops, false, "cli", "", nil); return err }},
	} {
		if err := step.do(); err != nil {
			t.Fatalf("%s: %v", step.what, err)
		}
		stands(step.what)
	}
	// Agreed again, paused again, and removed and recovered while the
	// consent ends: a recovery pauses only where it stands.
	if _, _, err := f.db.GrantSyncConsent(ctx, f.ana.ID, "sync-1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{team, ops} {
		if _, err := f.db.SetMailboxSync(ctx, id, true, f.ana.ID, "sync-1", nil); err != nil {
			t.Fatal(err)
		}
	}
	pause(own, team, ops)
	for _, id := range []string{own, team, ops} {
		f.remove(id, f.ana.ID)
	}
	if _, err := f.db.WithdrawSyncConsent(ctx, f.ana.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.StopSync(ctx, team, store.ActorOperator); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{own, team, ops} {
		f.recover(id, f.ana.ID)
		stands("recovering " + id)
	}
	if n := countOf(t, f.db, `SELECT count(*) FROM accounts WHERE sync_paused_at <> 0`); n != 1 {
		t.Errorf("%d mailboxes came back paused, want the operator's alone, whose switch stood", n)
	}
}

func TestAnAccountReadsItsPauseAndWhoPausedIt(t *testing.T) {
	f := newPauseFixture(t)
	ctx := t.Context()
	id := f.mailbox("personal", "own@mail.example", true)
	if _, err := f.db.PauseSync(ctx, id, f.ana.ID, func(*sql.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	read := func(get func(context.Context, string) (account.Account, error)) {
		t.Helper()
		a, err := get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !a.SyncPausedAt.Equal(f.now) || a.SyncPausedBy != f.ana.ID {
			t.Errorf("the account reads its pause as %v by %q, want now by Ana", a.SyncPausedAt, a.SyncPausedBy)
		}
	}
	read(f.repo.Get)
	all, err := f.repo.List(ctx)
	if err != nil || len(all) != 1 || all[0].SyncPausedBy != f.ana.ID {
		t.Errorf("the listing: %+v (%v)", all, err)
	}
	f.remove(id, f.ana.ID)
	f.recover(id, f.bob.ID)
	if a, err := f.repo.Get(ctx, id); err != nil || a.SyncPausedBy != f.bob.ID {
		t.Errorf("recovered by Bob, it reads %q (%v)", a.SyncPausedBy, err)
	}
}
