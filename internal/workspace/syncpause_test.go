package workspace_test

import (
	"database/sql"
	"testing"

	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// A mailbox's sync paused (migration 0025), as workspaces show it: the
// access directory says when and by whom, a person's deletion forgets who,
// and a snapshot reports it as the last cause.

func TestTheAccessDirectoryShowsAPauseAndWhoMadeIt(t *testing.T) {
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	support := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleAdmin})
	desk := f.link(support.ID, ana.ID, "desk@example.org")
	f.activate(desk.ID)
	if mb := f.directoryEntry(support.ID, desk.ID); !mb.Pause.At.IsZero() || mb.Pause.By != "" {
		t.Errorf("a mailbox nobody paused: %+v", mb.Pause)
	}
	// A person's pause.
	if _, err := f.db.PauseSync(t.Context(), desk.ID, bea.ID, nil); err != nil {
		t.Fatal(err)
	}
	if mb := f.directoryEntry(support.ID, desk.ID); mb.Pause.At.IsZero() || mb.Pause.By != bea.ID {
		t.Errorf("Bea's pause: %+v", mb.Pause)
	}
	// A recovery's, naming who recovered it.
	if _, err := f.db.ResumeSync(t.Context(), desk.ID, ana.ID, "sync-1", nil); err != nil {
		t.Fatal(err)
	}
	f.remove(desk.ID, ana.ID)
	f.exec(`UPDATE accounts SET removed_at = 0, removed_by = '', purge_after = 0, sync_paused_at = 950,
		sync_paused_by = ? WHERE id = ?`, ana.ID, desk.ID)
	if mb := f.directoryEntry(support.ID, desk.ID); mb.Pause.At.Unix() != 950 || mb.Pause.By != ana.ID {
		t.Errorf("the recovery's pause: %+v", mb.Pause)
	}
}

func TestDeletingAPersonForgetsWhoPausedAMailboxAndKeepsThePause(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	ana, bob := f.person("ana@example.org"), f.person("bob@example.org")
	support := f.team("Support", ana, map[string]workspace.Role{bob.ID: workspace.RoleAdmin})
	desk := f.link(support.ID, bob.ID, "desk@example.org")
	f.activate(desk.ID)
	f.grant(desk.ID, bob.ID, workspace.Flags{Read: true})
	if _, err := f.db.PauseSync(ctx, desk.ID, ana.ID, nil); err != nil {
		t.Fatal(err)
	}
	paused := f.count(`SELECT sync_paused_at FROM accounts WHERE id = ?`, desk.ID)
	if err := f.db.Write(ctx, func(tx *sql.Tx) error {
		if _, err := workspace.DeletePersonTx(ctx, tx, ana.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, ana.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM accounts WHERE id = ? AND sync_paused_at = ? AND sync_paused_by = ''`,
		desk.ID, paused); n != 1 || paused == 0 {
		t.Error("the mailbox Ana paused is not kept paused without her name")
	}
	if n := f.count(`SELECT count(*) FROM mailbox_events WHERE kind = 'paused' AND actor = ''`); n != 1 {
		t.Error("the journal of her pause still names her, or is gone")
	}
}

func TestASnapshotReportsAPausedMailboxPausedOnlyWhenNothingElseStopsIt(t *testing.T) {
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	support := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember})
	desk := f.link(support.ID, ana.ID, "desk@example.org")
	f.activate(desk.ID)
	f.grant(desk.ID, bea.ID, workspace.Flags{Read: true})
	pause := func() string {
		t.Helper()
		return mailboxOf(t, f.snapshot(support.ID), desk.ID).Pause
	}
	if got := pause(); got != "" {
		t.Fatalf("the syncing mailbox's pause: %q", got)
	}
	if _, err := f.db.PauseSync(t.Context(), desk.ID, ana.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got := pause(); got != store.PausePaused {
		t.Errorf("the paused mailbox: %q, want %q", got, store.PausePaused)
	}
	f.exec(`UPDATE accounts SET state = 'needs_reauth' WHERE id = ?`, desk.ID)
	if got := pause(); got != store.PauseNeedsReauth {
		t.Errorf("paused and needing authorization: %q, want %q", got, store.PauseNeedsReauth)
	}
	f.exec(`UPDATE accounts SET state = 'active' WHERE id = ?`, desk.ID)
	f.exec(`DELETE FROM mailbox_access WHERE account_id = ? AND user_id = ?`, desk.ID, ana.ID)
	f.exec(`UPDATE mailbox_access SET read = 0, act = 0, send = 0, manage = 1 WHERE account_id = ?`, desk.ID)
	if got := pause(); got != store.PauseNoReader {
		t.Errorf("paused and read by nobody: %q, want %q", got, store.PauseNoReader)
	}
	f.exec(`UPDATE mailbox_access SET read = 1 WHERE account_id = ? AND user_id = ?`, desk.ID, bea.ID)
	if got := pause(); got != store.PausePaused {
		t.Errorf("read again: %q, want %q", got, store.PausePaused)
	}
}
