package store_test

import (
	"context"
	"testing"

	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

func TestTheMailboxesOutsideTheOperatorWorkspaceAreCountedByKindLiveOrRemoved(t *testing.T) {
	// Two live mailboxes and a removed one in a person's personal
	// workspace, a live and a removed one in a team, and the operator's
	// two, one removed, which are not counted.
	f := &indexFixture{t: t, db: storetest.New(t), now: t0}
	ctx := context.Background()
	if got, err := f.db.CountWorkspaceMailboxes(ctx); err != nil || got != (store.WorkspaceMailboxes{}) || got.Total() != 0 {
		t.Fatalf("an empty database: %+v, %v", got, err)
	}

	f.exec(`INSERT INTO users(id, email, password_hash, role, password_changed_at, created_at, updated_at)
		VALUES ('usr_1', 'person@example.com', '$argon2id$', 'owner', 1, 1, 1)`)
	f.addAccount("acc_personal1", "usr_1")
	f.addAccount("acc_personal2", "usr_1")
	f.addAccount("acc_personal3", "usr_1")
	f.addAccount("acc_operator1", "")
	f.addAccount("acc_operator2", "")
	f.exec(`INSERT INTO workspaces(id, kind, source, name, created_at, updated_at)
		VALUES ('wsp_team', 'team', 'local', 'Support', 1, 1)`)
	for _, id := range []string{"acc_team1", "acc_team2"} {
		f.exec(`INSERT INTO accounts(id, workspace_id, email, provider, auth_kind, imap_host, imap_port, smtp_host,
			smtp_port, smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at)
			VALUES (?1, 'wsp_team', ?1 || '@example.com', 'imap', 'password', 'imap.example.com', 993,
			'smtp.example.com', 587, 'starttls', ?1, 1, 'active', 1, 1, 1)`, id)
	}
	for _, id := range []string{"acc_personal3", "acc_team2", "acc_operator2"} {
		f.exec(`UPDATE accounts SET removed_at = 100, removed_by = 'cli', purge_after = 100 + 2592000 WHERE id = ?`, id)
	}

	got, err := f.db.CountWorkspaceMailboxes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := store.WorkspaceMailboxes{PersonalLive: 2, PersonalRemoved: 1, TeamLive: 1, TeamRemoved: 1}
	if got != want || got.Total() != 5 {
		t.Errorf("CountWorkspaceMailboxes = %+v (total %d), want %+v: the operator's never counted", got, got.Total(), want)
	}
}
