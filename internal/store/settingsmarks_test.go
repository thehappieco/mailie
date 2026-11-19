package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

// What a sync writes is measured when an extension reports a mailbox, never
// marked (migration 0027): how much of it is indexed and when it last synced
// move with the next report.

// newFollowedIndex is a database whose one person, signed in by an identity,
// has their personal workspace from another source, and in it the account
// the index writes into.
func newFollowedIndex(t *testing.T) *indexFixture {
	t.Helper()
	f := &indexFixture{t: t, db: storetest.New(t), account: "acc_1", now: t0}
	f.exec(`INSERT INTO users(id, email, password_hash, role, password_changed_at, created_at, updated_at, sync_consent_at,
		sync_consent_version) VALUES ('usr_1', 'person@example.com', '$argon2id$', 'owner', 1, 1, 1, 1, 'test')`)
	f.exec(`INSERT INTO user_identities(issuer, subject, user_id, created_at)
		VALUES ('https://directory.example.org', 'sub-person', 'usr_1', 1)`)
	f.exec(`INSERT INTO workspaces(id, kind, source, person_id, created_at, updated_at)
		VALUES ('ext_personal', 'personal', 'platform', 'usr_1', 1, 1)`)
	f.exec(`INSERT INTO workspace_members(workspace_id, user_id, role, created_at, updated_at)
		VALUES ('ext_personal', 'usr_1', 'owner', 1, 1)`)
	f.exec(`INSERT INTO accounts(id, workspace_id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port,
		smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at, owner_user_id)
		VALUES ('acc_1', 'ext_personal', 'acc_1@example.com', 'gmail', 'oauth2', 'imap.gmail.com', 993, 'smtp.gmail.com',
		465, 'implicit', 'acc_1@example.com', 0, 'active', 1, 1, 1, 'usr_1')`)
	return f
}

// marks are the workspace's and the person's marks.
func (f *indexFixture) marks() (workspace, person int64) {
	f.t.Helper()
	return f.int(`SELECT seq FROM workspace_changes WHERE workspace_id = 'ext_personal'`),
		f.int(`SELECT seq FROM person_changes WHERE user_id = 'usr_1'`)
}

func TestAMailboxsSyncBookkeepingFoldersAndMessagesMarkNothing(t *testing.T) {
	f := newFollowedIndex(t)
	ctx := context.Background()
	ws, person := f.marks()
	if ws == 0 || person == 0 {
		t.Fatalf("the seed marked the workspace %d and the person %d; the test would prove nothing", ws, person)
	}
	ids := f.gmailFolders()
	f.live(ids["INBOX"], mail(1, "a"), mail(2, "b", imap.FlagSeen))
	f.live(ids["Receipts"], mail(1, "c"))
	f.flags(ids["INBOX"], 0, provider.FlagUpdate{UID: 1, Flags: []imap.Flag{imap.FlagFlagged}})
	if err := f.db.RecordSyncOK(ctx, f.account, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.RecordSyncFailure(ctx, f.account, "transient", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := f.db.RecordIdleEvent(ctx, f.account, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n := f.int(`SELECT count(*) FROM messages WHERE account_id = ?`, f.account); n != 3 {
		t.Fatalf("the index holds %d messages, want the 3 written", n)
	}
	if w, p := f.marks(); w != ws || p != person {
		t.Errorf("a sync's writes marked the workspace %d (was %d) and the person %d (was %d)", w, ws, p, person)
	}
	// Turning the person's sync off, which deletes the index, is theirs to
	// see: it marks both.
	if _, err := f.db.WithdrawSyncConsent(ctx, "usr_1"); err != nil {
		t.Fatal(err)
	}
	if w, p := f.marks(); w <= ws || p <= person {
		t.Errorf("a withdrawal marked the workspace %d (was %d) and the person %d (was %d)", w, ws, p, person)
	}
}
