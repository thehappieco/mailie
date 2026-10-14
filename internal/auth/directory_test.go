package auth_test

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// strandedJournal is a team whose mailbox Bea alone reads, stranded from
// her by Ana and by the directory, and turned off by her: the journal names
// her as the person each stranding lost and as an actor.
func strandedJournal(t *testing.T, db *store.Store, ana, bea string) {
	t.Helper()
	team := teamOf(t, db, ana)
	ws := workspace.NewRepository(db, nil)
	ctx := t.Context()
	if err := db.Write(ctx, func(tx *sql.Tx) error {
		return ws.AddMemberTx(ctx, tx, team.ID, bea, workspace.RoleMember, db.Now())
	}); err != nil {
		t.Fatal(err)
	}
	const box = "acc_00000000000000aa"
	linkMailbox(t, db, box, team.ID, ana, "support@mail.example")
	if _, err := ws.SetGrant(ctx, box, bea, workspace.Flags{Read: true}, ana, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Write(ctx, func(tx *sql.Tx) error {
		for _, by := range []string{ana, store.ActorDirectory} {
			if _, err := workspace.JournalStrandedTx(ctx, tx, []string{box}, bea, store.CauseMembershipEnded, by,
				db.Now()); err != nil {
				return err
			}
		}
		_, _, err := store.JournalMailboxTx(ctx, tx, store.MailboxEvent{Kind: store.MailboxSyncPaused, AccountID: box,
			Actor: bea, At: db.Now().Unix()}, nil, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func journalNames(t *testing.T, db *store.Store) []string {
	t.Helper()
	rows, err := db.Reader().QueryContext(t.Context(), `SELECT kind || '|' || actor || '|' || lost_user FROM mailbox_events
		ORDER BY seq`)
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

func deletePerson(t *testing.T, users *auth.Users, db *store.Store, id string) {
	t.Helper()
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := users.DeleteTx(t.Context(), tx, id, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDeletingAnUnfollowedPersonForgetsThemAsTheLostReaderToo(t *testing.T) {
	// No directory follows Bea: deleting her forgets her in the journal at
	// once, as the one who acted and as the one a stranding lost.
	users, db, _ := newUsers(t)
	authtest.NewUser(t, db, "owner@example.org", auth.RoleOwner)
	ana := authtest.NewUser(t, db, "ana@example.org", auth.RoleMember)
	bea := authtest.NewUser(t, db, "bea@example.org", auth.RoleMember)
	strandedJournal(t, db, ana.ID, bea.ID)
	deletePerson(t, users, db, bea.ID)
	if got := journalNames(t, db); !slices.Equal(got, []string{
		"stranded|" + ana.ID + "|", "stranded|directory|", "sync_paused||",
	}) {
		t.Errorf("after Bea's deletion the journal holds %v", got)
	}
}

func TestDeletingAPersonMarksTheirDirectoryRowGone(t *testing.T) {
	// A directory follows Bea: deleting her marks what it said of her gone
	// and keeps her name in the journal, for an extension to deliver what
	// it says of her, until it releases her.
	users, db, clock := newUsers(t)
	authtest.NewUser(t, db, "owner@example.org", auth.RoleOwner)
	ana := authtest.NewUser(t, db, "ana@example.org", auth.RoleMember)
	bea := authtest.NewUser(t, db, "bea@example.org", auth.RoleMember)
	strandedJournal(t, db, ana.ID, bea.ID)
	const issuer = "https://id.example.org"
	write := func(fn func(ctx context.Context, tx *sql.Tx) error) {
		t.Helper()
		if err := db.Write(t.Context(), func(tx *sql.Tx) error { return fn(t.Context(), tx) }); err != nil {
			t.Fatal(err)
		}
	}
	write(func(ctx context.Context, tx *sql.Tx) error {
		if _, err := store.EnsureDirectoryTx(ctx, tx, issuer, "g1", 100); err != nil {
			return err
		}
		return store.PutDirectoryPersonTx(ctx, tx, store.DirectoryPerson{Issuer: issuer, UserID: bea.ID,
			Subject: "sub-bea", Generation: "g1", Version: 1, Status: "active", AppliedAt: 100})
	})
	deletePerson(t, users, db, bea.ID)
	var gone int64
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT gone_at FROM directory_people WHERE user_id = ?`,
		bea.ID).Scan(&gone); err != nil || gone != clock.Unix() {
		t.Errorf("Bea's row is gone at %d (%v), want %d", gone, err, clock.Unix())
	}
	if got := journalNames(t, db); !slices.Equal(got, []string{
		"stranded|" + ana.ID + "|" + bea.ID, "stranded|directory|" + bea.ID, "sync_paused|" + bea.ID + "|",
	}) {
		t.Errorf("after Bea's deletion the journal holds %v", got)
	}
	write(func(ctx context.Context, tx *sql.Tx) error {
		return store.ReleaseDirectoryPersonTx(ctx, tx, issuer, bea.ID)
	})
	if got := journalNames(t, db); !slices.Equal(got, []string{
		"stranded|" + ana.ID + "|", "stranded|directory|", "sync_paused||",
	}) {
		t.Errorf("after Bea's release the journal holds %v", got)
	}
}
