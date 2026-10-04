package auth_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/store"
)

func invitesFor(t *testing.T, db *store.Store, email string) int {
	t.Helper()
	var n int
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT count(*) FROM invites WHERE email = ?`, email).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAnUnusedInviteIsDeletedThirtyDaysAfterItExpires(t *testing.T) {
	cheapKDF(t)
	users, db, clock := newUsers(t)
	start := *clock
	invite(t, users, "late@example.com", auth.RoleMember)
	used := invite(t, users, "used@example.com", auth.RoleMember)
	if _, _, _, err := users.SignUp(t.Context(), auth.SignUpRequest{
		Invite: used, Email: "used@example.com", Password: authtest.Password,
	}); err != nil {
		t.Fatal(err)
	}
	expired := start.Add(auth.InviteTTL)

	sweep := func(at time.Time, ahead time.Duration) int {
		t.Helper()
		*clock = at
		n, err := users.SweepInvites(t.Context(), ahead)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Expired, but not yet for thirty days: kept, so "my invite did not
	// work" still has an answer.
	if n := sweep(expired.Add(auth.InviteRetention), 0); n != 0 {
		t.Fatalf("an invite expired exactly thirty days ago was swept (%d)", n)
	}
	invite(t, users, "fresh@example.com", auth.RoleMember)
	if n := sweep(expired.Add(auth.InviteRetention+time.Second), 0); n != 1 {
		t.Fatalf("swept %d invites a second past the retention, want the one", n)
	}
	for email, want := range map[string]int{"late@example.com": 0, "used@example.com": 1, "fresh@example.com": 1} {
		if got := invitesFor(t, db, email); got != want {
			t.Errorf("%s: %d invites left, want %d", email, got, want)
		}
	}

	// A sweep that will not run again for an hour takes what would pass the
	// retention before then, so nothing outlives it by waiting.
	freshExpired := expired.Add(auth.InviteRetention).Add(auth.InviteTTL)
	if n := sweep(freshExpired.Add(auth.InviteRetention-30*time.Minute), time.Hour); n != 1 {
		t.Errorf("a sweep with an hour to its next run left an invite thirty minutes from its limit (%d)", n)
	}
}

func TestAPersonIsNotDeletedWhileAnAccountStillNamesThem(t *testing.T) {
	// Account ownership has no ON DELETE action on purpose: a deletion that
	// quietly turned someone's mailboxes into the instance's would hand them
	// to every owner. The accounts must go first, in the same transaction.
	users, db, _ := newUsers(t)
	authtest.NewUser(t, db, "owner@example.com", auth.RoleOwner)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	if _, err := db.Writer().ExecContext(t.Context(),
		`INSERT INTO accounts(id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port, smtp_tls,
		 login_user, save_sent_copy, state, state_changed_at, created_at, updated_at, owner_user_id)
		 VALUES ('acc_0000000000000001', 'ana@mail.example', 'imap', 'password', 'h', 993, 'h', 465, 'implicit',
		 'ana', 1, 'active', 0, 0, 0, ?)`, ana.ID); err != nil {
		t.Fatal(err)
	}

	err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := users.DeleteTx(t.Context(), tx, ana.ID, false)
		return err
	})
	if !errors.Is(err, auth.ErrOwnsAccounts) {
		t.Fatalf("DeleteTx with an account left: %v, want ErrOwnsAccounts", err)
	}
	if _, err := users.Get(t.Context(), ana.ID); err != nil {
		t.Fatalf("the refused deletion was not rolled back: %v", err)
	}
}
