package account_test

import (
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/provider"
)

// What usage rule 1 keeps of a mailbox (migration 0030) through the acts
// that keep its index, and through every way of deleting it for good.

// index writes into a mailbox's index what a sync would: a folder, three
// messages on two days, one before 1970 and one a copy of another, with
// parts, and an open draft.
func (f *pauseFixture) index(id string) {
	f.t.Helper()
	exec(f.t, f.db, `INSERT INTO folders(account_id, name, display_name, role, role_source, uidvalidity, sync_state,
		attrs_json) VALUES (?, 'Archive', 'Archive', '', '', 1, 'live', '["\\HasNoChildren"]')`, id)
	exec(f.t, f.db, `INSERT INTO messages(account_id, folder_id, uidvalidity, uid, message_id, group_key, subject,
		from_text, flags_json, internal_date, size, first_seen_at, updated_at)
		SELECT ?1, f.id, 1, v.uid, v.mid, 'mid:' || v.mid, v.subject, 'Ana', '["\\seen"]', v.at, 1000, 1, 1
		FROM folders f, (SELECT 1 AS uid, 'a@x' AS mid, 'Quarterly report' AS subject, 1761000000 AS at
		  UNION ALL SELECT 2, 'b@x', 'Before 1970', -5
		  UNION ALL SELECT 3, 'a@x', 'Quarterly report', 1761000000) v
		WHERE f.account_id = ?1 AND f.name = 'Archive'`, id)
	exec(f.t, f.db, `UPDATE messages SET dup_of = (SELECT min(id) FROM messages WHERE account_id = ?1)
		WHERE account_id = ?1 AND uid = 3`, id)
	exec(f.t, f.db, `INSERT INTO parts(msg_id, path, mime_type, filename, size, is_attachment)
		SELECT id, '2', 'application/pdf', 'relatório.pdf', 90000, 1 FROM messages WHERE account_id = ?`, id)
	exec(f.t, f.db, `INSERT INTO drafts(id, account_id, compose_json, compose_hash, message_id_hdr, state, created_at,
		updated_at, expires_at) VALUES ('drf_' || ?1, ?1, '{"subject":"Olá"}', 'h', 'd@example.com', 'open', 1, 1, 9)`, id)
	if n := countOf(f.t, f.db, `SELECT (SELECT count(*) FROM usage_days WHERE account_id = ?1 AND bytes > 0)
		+ (SELECT count(*) FROM usage_fixed WHERE account_id = ?1 AND bytes > 0)`, id); n < 3 {
		f.t.Fatalf("the index of %s keeps %d rows of totals, want its days and its fixed bytes", id, n)
	}
}

// totals lists every row usage_days and usage_fixed keep.
func (f *pauseFixture) totals() []string {
	f.t.Helper()
	rows, err := f.db.Reader().QueryContext(f.t.Context(), `
		SELECT account_id || ' day ' || day || ': ' || coalesce(bytes, 0) FROM usage_days
		UNION ALL SELECT account_id || ' fixed: ' || coalesce(bytes, 0) FROM usage_fixed ORDER BY 1`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// totalsHold checks that no mailbox's totals drifted from its rows, and
// that each one's sum is what Usage scans of it.
func (f *pauseFixture) totalsHold(step string) {
	f.t.Helper()
	ctx := f.t.Context()
	drifted, err := f.db.CheckUsageTotals(ctx)
	if err != nil || len(drifted) != 0 {
		f.t.Fatalf("after %s the totals of %v drifted (%v)", step, drifted, err)
	}
	ids := f.accounts()
	usage, err := f.db.Usage(ctx, ids)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, id := range ids {
		kept := countOf(f.t, f.db, `SELECT (SELECT coalesce(sum(bytes), 0) FROM usage_days WHERE account_id = ?1)
			+ (SELECT coalesce(sum(bytes), 0) FROM usage_fixed WHERE account_id = ?1)`, id)
		if int64(kept) != usage[id].Bytes {
			f.t.Errorf("after %s %s keeps %d bytes and Usage scans %d", step, id, kept, usage[id].Bytes)
		}
	}
}

// accounts lists every mailbox, live or removed.
func (f *pauseFixture) accounts() []string {
	f.t.Helper()
	rows, err := f.db.Reader().QueryContext(f.t.Context(), `SELECT id FROM accounts ORDER BY id`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			f.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return ids
}

// link creates a mailbox in a workspace, connected or not.
func (f *pauseFixture) link(id, workspaceID, linker string, connected bool) string {
	f.t.Helper()
	email := strings.TrimPrefix(id, "acc_") + "@mail.example"
	a, err := f.repo.Create(f.t.Context(), account.Account{ID: id, WorkspaceID: workspaceID, Email: email,
		Provider: provider.KindIMAP, AuthKind: "password", IMAPHost: "imap.mail.example", IMAPPort: 993,
		SMTPHost: "smtp.mail.example", SMTPPort: 465, SMTPTLS: "implicit", LoginUser: email}, linker)
	if err != nil {
		f.t.Fatal(err)
	}
	if connected {
		if err := f.repo.SavePassword(f.t.Context(), a.ID, "hunter2"); err != nil {
			f.t.Fatal(err)
		}
	}
	return a.ID
}

func TestEveryDeletionForGoodSucceedsAndLeavesNoTotals(t *testing.T) {
	// Each runs DELETE FROM accounts, whose cascade fires the usage
	// triggers of every row it takes: none of them may fail it, and the
	// mailbox's totals go with it.
	f := newPauseFixture(t)
	ctx := t.Context()
	kept := f.mailbox("team", "support@mail.example", true)
	f.index(kept)
	cid := authtest.NewUser(t, f.db, "cid@example.org", auth.RoleMember)
	cidsOwn := f.link("acc_00000000000000c1", authtest.Personal(t, f.db, cid.ID), cid.ID, true)
	byRepository := f.mailbox("operator", "ops@mail.example", true)
	never := f.link("acc_00000000000000e1", f.team, f.ana.ID, false)
	early := f.mailbox("team", "early@mail.example", true)
	expired := f.mailbox("personal", "expired@mail.example", true)
	for _, id := range []string{cidsOwn, byRepository, never, early, expired} {
		f.index(id)
	}
	f.totalsHold("six mailboxes indexed")

	for _, c := range []struct {
		what   string
		id     string
		delete func() error
	}{
		{"the repository's delete", byRepository, func() error { return f.repo.Delete(ctx, byRepository) }},
		{"a person's closure", cidsOwn, func() error {
			n, err := f.registry.RemoveOwner(ctx, cid.ID, nil, func(*sql.Tx) error { return nil })
			if err == nil && n != 1 {
				t.Errorf("the closure deleted %d mailboxes, want the person's one", n)
			}
			return err
		}},
		{"a link that never connected, removed", never, func() error {
			removal, err := f.registry.Remove(ctx, never, f.ana.ID, nil)
			if err == nil && !removal.Purged {
				t.Errorf("the link that never connected was kept: %+v", removal)
			}
			return err
		}},
		{"a removed mailbox deleted early", early, func() error {
			if _, err := f.registry.Remove(ctx, early, f.ana.ID, nil); err != nil {
				return err
			}
			return f.registry.Purge(ctx, early, f.ana.ID, nil)
		}},
		{"the sweep past 30 days", expired, func() error {
			if _, err := f.registry.Remove(ctx, expired, f.ana.ID, nil); err != nil {
				return err
			}
			purged, err := f.registry.PurgeExpired(ctx, f.now.Add(account.RemovedKeptFor), nil)
			if err == nil && !slices.Equal(purged, []string{expired}) {
				t.Errorf("the sweep deleted %v, want %s", purged, expired)
			}
			return err
		}},
	} {
		if err := c.delete(); err != nil {
			t.Fatalf("%s: %v", c.what, err)
		}
		if n := countOf(t, f.db, `SELECT count(*) FROM accounts WHERE id = ?`, c.id); n != 0 {
			t.Errorf("%s left the mailbox", c.what)
		}
		if n := countOf(t, f.db, `SELECT (SELECT count(*) FROM usage_days WHERE account_id = ?1)
			+ (SELECT count(*) FROM usage_fixed WHERE account_id = ?1)`, c.id); n != 0 {
			t.Errorf("%s left %d rows of the mailbox's totals", c.what, n)
		}
		f.totalsHold(c.what)
	}
	if got := f.totals(); len(got) == 0 || !strings.HasPrefix(got[0], kept+" ") {
		t.Errorf("the totals left are %v, want the kept mailbox's alone", got)
	}
}

func TestRemovalRecoveryPauseAndHoldsLeaveTheTotalsAsTheyWere(t *testing.T) {
	// None of them writes the index: a removed mailbox keeps its index,
	// and counts, until it is deleted for good.
	f := newPauseFixture(t)
	ctx := t.Context()
	team := f.mailbox("team", "support@mail.example", true)
	own := f.mailbox("personal", "own@mail.example", true)
	f.index(team)
	f.index(own)
	before := f.totals()
	same := func(step string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if got := f.totals(); !slices.Equal(got, before) {
			t.Fatalf("%s moved the totals to\n%v\nfrom\n%v", step, got, before)
		}
		f.totalsHold(step)
	}

	_, err := f.db.PauseSync(ctx, team, f.ana.ID, nil)
	same("a pause", err)
	_, err = f.db.ResumeSync(ctx, team, f.ana.ID, "sync-1", nil)
	same("a resume", err)
	_, err = f.registry.Remove(ctx, team, f.ana.ID, nil)
	same("a removal", err)
	_, err = f.registry.Recover(ctx, team, f.ana.ID, nil)
	same("a recovery, paused", err)
	_, err = f.db.ResumeSync(ctx, team, f.ana.ID, "sync-1", nil)
	same("the recovered mailbox resumed", err)

	exec(t, f.db, `UPDATE accounts SET sync_held_at = 950 WHERE id = ?`, own)
	same("a hold", nil)
	_, err = f.registry.Remove(ctx, own, f.ana.ID, nil)
	same("a held mailbox removed", err)
	recovered, err := f.registry.Recover(ctx, own, f.ana.ID, nil)
	if err == nil && recovered.SyncHeldAt.IsZero() {
		t.Error("the recovery did not keep the hold")
	}
	same("a held mailbox recovered", err)
	_, err = f.db.LiftSyncHold(ctx, own, nil)
	same("the hold lifted", err)
}
