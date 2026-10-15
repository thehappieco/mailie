package workspace_test

import (
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// A removed mailbox (migration 0020) is kept for a recovery, with its
// grants, and used by nothing meanwhile.

// remove removes a mailbox as the repository will, for 30 days, by who.
func (f *fixture) remove(accountID, who string) {
	f.t.Helper()
	f.exec(`UPDATE accounts SET removed_at = 900, removed_by = ?, purge_after = 900 + 2592000 WHERE id = ?`, who, accountID)
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if err := f.db.Write(f.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(f.t.Context(), query, args...)
		return err
	}); err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
}

// lastKeptReader is a team whose mailbox Bea alone reads, Ana its owner
// reading nothing, removed by Ana since: Bea would read it once recovered.
func lastKeptReader(t *testing.T, f *fixture) (ana, bea auth.User, support workspace.Workspace, shared string) {
	t.Helper()
	ana, bea = f.person("ana@example.org"), f.person("bea@example.org")
	support = f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember})
	shared = f.link(support.ID, ana.ID, "support@example.org").ID
	f.grant(shared, bea.ID, readOnly())
	if _, err := f.ws.Revoke(t.Context(), shared, ana.ID, workspace.Flags{Read: true}, nil); err != nil {
		t.Fatal(err)
	}
	f.remove(shared, ana.ID)
	return ana, bea, support, shared
}

func TestARemovedMailboxKeepsProtectingItsLastReaderUntilItsPurge(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	_, bea, support, shared := lastKeptReader(t, f)

	// Kept, it protects Bea as it did live: she neither leaves, is removed
	// nor is disabled, and closing her is asked about it.
	want(t, "Bea, its last reader, leaving", f.ws.RemoveMember(ctx, support.ID, bea.ID, nil), workspace.ErrLastReader)
	_, err := f.ws.SetMember(ctx, support.ID, bea.ID, workspace.MemberChange{Status: status(workspace.StatusDisabled)}, nil)
	want(t, "Bea, its last reader, disabled", err, workspace.ErrLastReader)
	if b, err := f.ws.Blocks(ctx, bea.ID); err != nil || !slices.Equal(b.LastReaderOf, []string{shared}) {
		t.Errorf("what closing Bea would break: %+v (%v), want the removed mailbox", b, err)
	}
	// Listed in advance, as for a live one.
	if last, err := f.ws.LastReaders(ctx, support.ID); err != nil || !slices.Equal(last[bea.ID], []string{shared}) {
		t.Errorf("the team's last readers: %v (%v)", last, err)
	}
	if m, err := f.ws.Member(ctx, support.ID, bea.ID); err != nil || !slices.Equal(m.LastReaderOf, []string{shared}) {
		t.Errorf("Bea's membership: %+v (%v)", m, err)
	}
	if n := f.count(`SELECT count(*) FROM mailbox_access WHERE account_id = ? AND user_id = ? AND read = 1`,
		shared, bea.ID); n != 1 {
		t.Error("a refused change took Bea's flag on the removed mailbox")
	}
}

func TestThePurgeEndsARemovedMailboxsProtection(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	_, bea, support, shared := lastKeptReader(t, f)

	// Deleted for good, as the sweep or an owner does: its grants go with
	// it, and so does what they protected.
	f.exec(`DELETE FROM accounts WHERE id = ?`, shared)

	if b, err := f.ws.Blocks(ctx, bea.ID); err != nil || b.Any() {
		t.Errorf("closing Bea is blocked by %+v (%v) once the mailbox is deleted for good", b, err)
	}
	if last, err := f.ws.LastReaders(ctx, support.ID); err != nil || len(last) != 0 {
		t.Errorf("the team's last readers: %v (%v)", last, err)
	}
	if err := f.ws.RemoveMember(ctx, support.ID, bea.ID, nil); err != nil {
		t.Errorf("Bea leaving once the mailbox is deleted for good: %v", err)
	}
}

func TestARemovedMailboxStillReachesNobody(t *testing.T) {
	// Bea is protected as the removed mailbox's last reader, and reads
	// nothing of it: no access, no reader, nothing given or taken on it.
	f := newFixture(t)
	ctx := t.Context()
	ana, bea, support, shared := lastKeptReader(t, f)

	held, err := f.ws.Access(ctx, bea.ID, []string{shared})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := held[shared]; ok {
		t.Errorf("Bea holds %+v on the removed mailbox", held[shared])
	}
	if f.reads(shared, bea.ID) || f.hasReader(shared) {
		t.Error("the removed mailbox has a reader")
	}
	if directory, err := f.ws.Directory(ctx, support.ID); err != nil || len(directory) != 0 {
		t.Errorf("the directory lists %+v (%v)", directory, err)
	}
	_, err = f.ws.Revoke(ctx, shared, bea.ID, workspace.Flags{}, nil)
	want(t, "Bea's read revoked", err, workspace.ErrNoMailbox)
	_, err = f.ws.SetGrant(ctx, shared, ana.ID, readOnly(), ana.ID, nil)
	want(t, "Ana given read again", err, workspace.ErrNoMailbox)
}

func TestTheDirectoryAndCountsLeaveRemovedMailboxesOut(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	support := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember})
	shared := f.link(support.ID, ana.ID, "support@example.org")
	sales := f.link(support.ID, ana.ID, "sales@example.org")
	f.grant(shared.ID, bea.ID, readOnly())
	presented := authtest.NewWorkspaceKey(t, f.db, auth.ScopeRead, support.ID, ana.ID,
		workspace.KeyGrant{AccountID: shared.ID, Flags: readOnly()},
		workspace.KeyGrant{AccountID: sales.ID, Flags: readOnly()})
	prefix, _, _ := strings.Cut(presented, ".")
	both := []string{shared.ID, sales.ID}

	f.remove(shared.ID, ana.ID)

	directory, err := f.ws.Directory(ctx, support.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(directory) != 1 || directory[0].AccountID != sales.ID {
		t.Errorf("the directory lists %+v, want only %s", directory, sales.ID)
	}
	all, err := f.ws.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range all {
		if w.ID == support.ID && w.Mailboxes != 1 {
			t.Errorf("the operator's listing counts %d mailboxes in Support, want the 1 live", w.Mailboxes)
		}
	}
	for name, who := range map[string]string{"Ana, its owner": ana.ID, "Bea, who read it": bea.ID} {
		held, err := f.ws.Access(ctx, who, both)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := held[shared.ID]; ok {
			t.Errorf("%s holds %+v on the removed mailbox", name, held[shared.ID])
		}
		if _, err := f.ws.KeyState(ctx, shared.ID, who); err == nil {
			t.Errorf("%s reads the removed mailbox's key", name)
		}
	}
	held, err := f.ws.KeyAccessOf(ctx, prefix, both)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := held[shared.ID]; ok || !held[sales.ID].Read {
		t.Errorf("the key holds %+v, want only the live mailbox", held)
	}
}

func TestNothingIsGrantedKeyedOrManagedOnARemovedMailbox(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	support := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember})
	shared := f.link(support.ID, ana.ID, "support@example.org")
	f.grant(shared.ID, bea.ID, readOnly())
	presented := authtest.NewWorkspaceKey(t, f.db, auth.ScopeWrite, support.ID, ana.ID,
		workspace.KeyGrant{AccountID: shared.ID, Flags: readOnly()})
	prefix, _, _ := strings.Cut(presented, ".")

	f.remove(shared.ID, ana.ID)

	_, err := f.ws.SetGrant(ctx, shared.ID, bea.ID, readAct(), ana.ID, nil)
	want(t, "a grant changed", err, workspace.ErrNoMailbox)
	_, err = f.ws.Revoke(ctx, shared.ID, bea.ID, workspace.Flags{}, nil)
	want(t, "a grant revoked", err, workspace.ErrNoMailbox)
	_, err = f.ws.SetKeyAccess(ctx, prefix, shared.ID, readAct(), ana.ID, nil)
	want(t, "a key given more", err, workspace.ErrNoMailbox)
	err = f.db.Write(ctx, func(tx *sql.Tx) error { return workspace.ManagesTx(ctx, tx, shared.ID, ana.ID) })
	want(t, "its owner managing it", err, workspace.ErrNoMailbox)
	err = f.db.Write(ctx, func(tx *sql.Tx) error {
		return workspace.PutKeyAccessTx(ctx, tx, prefix, support.ID, workspace.KeyGrant{AccountID: shared.ID, Flags: readOnly()},
			ana.ID, f.db.Now())
	})
	want(t, "a new key given it", err, workspace.ErrNoMailbox)
	// What it held is kept, untouched, for a recovery.
	if n := f.count(`SELECT count(*) FROM mailbox_access WHERE account_id = ? AND user_id = ? AND read = 1 AND act = 0`,
		shared.ID, bea.ID); n != 1 {
		t.Error("Bea's grant on the removed mailbox changed")
	}
}

func TestDeletingAPersonForgetsThatTheyRemovedAMailbox(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	ana, bob := f.person("ana@example.org"), f.person("bob@example.org")
	support := f.team("Support", bob, map[string]workspace.Role{ana.ID: workspace.RoleOwner})
	shared := f.link(support.ID, bob.ID, "support@example.org")
	orders := f.link(support.ID, bob.ID, "orders@example.org")
	f.grant(orders.ID, ana.ID, readOnly())
	journal := func(kind store.MailboxEventKind, accountID, actor string, readers []string) {
		t.Helper()
		if err := f.db.Write(ctx, func(tx *sql.Tx) error {
			_, _, err := store.JournalMailboxTx(ctx, tx, store.MailboxEvent{Kind: kind, AccountID: accountID, Actor: actor,
				At: 900}, readers, true)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Ana removed the team's mailbox, which Bob read, and turned the sync
	// of another off; Bob turned off one Ana read.
	// The removal is recorded on the mailbox first, which the journal reads
	// its end from.
	f.remove(shared.ID, ana.ID)
	journal(store.MailboxRemoved, shared.ID, ana.ID, []string{bob.ID})
	journal(store.MailboxSyncPaused, orders.ID, ana.ID, []string{ana.ID, bob.ID})
	journal(store.MailboxSyncPaused, orders.ID, bob.ID, []string{ana.ID, bob.ID})

	if err := f.db.Write(ctx, func(tx *sql.Tx) error {
		if _, err := workspace.DeletePersonTx(ctx, tx, ana.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, ana.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if n := f.count(`SELECT count(*) FROM accounts WHERE removed_by = ?`, ana.ID) +
		f.count(`SELECT count(*) FROM mailbox_events WHERE actor = ?`, ana.ID) +
		f.count(`SELECT count(*) FROM mailbox_event_readers WHERE user_id = ?`, ana.ID) +
		f.count(`SELECT count(*) FROM notices WHERE user_id = ?`, ana.ID); n != 0 {
		t.Errorf("%d rows still name Ana", n)
	}
	// What she did stays done, saying nobody did it.
	if n := f.count(`SELECT count(*) FROM accounts WHERE id = ? AND removed_at = 900 AND removed_by = ''
		AND purge_after = 900 + 2592000`, shared.ID); n != 1 {
		t.Error("the mailbox she removed is not kept as it was, without her name")
	}
	if got := strings.Join(queryAll(t, f, `SELECT kind || ' ' || actor FROM mailbox_events ORDER BY seq`), ", "); got !=
		"removed , sync_paused , sync_paused "+bob.ID {
		t.Errorf("the journal holds %q", got)
	}
	// Bob is still told what she did.
	if n := f.count(`SELECT count(*) FROM notices WHERE user_id = ?`, bob.ID); n != 2 {
		t.Errorf("Bob has %d notices, want the 2 of what she did", n)
	}
}

func queryAll(t *testing.T, f *fixture, query string, args ...any) []string {
	t.Helper()
	rows, err := f.db.Reader().QueryContext(t.Context(), query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestClosingASuspendedPersonIsAskedAboutARemovedMailboxTheyWouldRead(t *testing.T) {
	// Bea, suspended, would read the removed mailbox once resumed and it is
	// recovered: closing her is asked about it, as about a live one, and
	// what that would strand includes it.
	f := newFixture(t)
	ctx := t.Context()
	_, bea, _, shared := lastKeptReader(t, f)
	f.suspend(bea.ID)

	if b, err := f.ws.Blocks(ctx, bea.ID); err != nil || !slices.Equal(b.LastReaderOf, []string{shared}) {
		t.Errorf("what closing Bea, suspended, would break: %+v (%v), want the removed mailbox", b, err)
	}
	var strands []string
	f.write(func(tx *sql.Tx) error {
		var err error
		strands, err = workspace.StrandCandidatesTx(ctx, tx, bea.ID, workspace.StrandScope{Outlived: true})
		return err
	})
	if !slices.Equal(strands, []string{shared}) {
		t.Errorf("closing Bea would strand %v, want the removed mailbox", strands)
	}
}
