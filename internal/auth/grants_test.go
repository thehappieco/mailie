package auth_test

import (
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// What being disabled and being reset take of a person's mailbox keys
// (docs/key-scheme.md sections 12.6 and 12.13): every grant sealed to them,
// in the transaction that disables or resets them; their flags stay.

// readsBy reports whether a person reads a mailbox now, by the one rule.
func readsBy(t *testing.T, db *store.Store, accountID, userID string) bool {
	t.Helper()
	held, err := workspace.NewRepository(db, nil).Access(t.Context(), userID, []string{accountID})
	if err != nil {
		t.Fatal(err)
	}
	return held[accountID].Read
}

func TestDisablingAPersonDeletesTheirGrantsAndKeepsTheirFlags(t *testing.T) {
	users, db, _ := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleOwner)
	bea := authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	team := teamOf(t, db, ana.ID)
	addMember(t, db, team.ID, bea.ID)
	mailbox := teamMailbox(t, db, team.ID, ana.ID)
	grantRead(t, db, mailbox, bea.ID)
	authtest.KeyMailbox(t, db, mailbox, ana.ID)
	if !readsBy(t, db, mailbox, bea.ID) {
		t.Fatal("bea does not read the mailbox keyed with her grant")
	}

	if _, err := users.Disable(t.Context(), bea.ID, false); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT count(*) FROM mailbox_grants WHERE user_id = ?`, bea.ID); n != 0 {
		t.Errorf("a disabled person keeps %d grants", n)
	}
	if n := count(t, db, `SELECT count(*) FROM mailbox_access WHERE user_id = ? AND read = 1`, bea.ID); n != 1 {
		t.Errorf("a disabled person holds read on %d mailboxes, want the flag kept", n)
	}
	if n := count(t, db, `SELECT count(*) FROM mailbox_grants WHERE user_id = ?`, ana.ID); n != 1 {
		t.Errorf("ana holds %d grants after bea was disabled, want hers", n)
	}
	// Switched back on, she holds the flag and waits for the key.
	if err := users.SetDisabled(t.Context(), bea.ID, false); err != nil {
		t.Fatal(err)
	}
	if readsBy(t, db, mailbox, bea.ID) {
		t.Error("switched back on, bea reads without a grant")
	}
	waiting, err := workspace.NewRepository(db, nil).WaitingForKey(t.Context(), bea.ID, []string{mailbox})
	if err != nil || !waiting[mailbox] {
		t.Errorf("switched back on, bea is not waiting for the key: %v, %v", waiting, err)
	}
}

func TestAResetDeletesEveryGrantOfThePersonAndKeepsTheirFlags(t *testing.T) {
	cheapKDF(t)
	users, db, _ := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleOwner)
	bea := authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	team := teamOf(t, db, ana.ID)
	addMember(t, db, team.ID, bea.ID)
	keyed := teamMailbox(t, db, team.ID, ana.ID)
	grantRead(t, db, keyed, bea.ID)
	authtest.KeyMailbox(t, db, keyed, ana.ID)
	keyless := teamMailbox(t, db, team.ID, ana.ID)
	grantRead(t, db, keyless, bea.ID)

	code, _, err := users.CreateReset(t.Context(), bea.ID, false, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := users.CompleteReset(t.Context(), code, "bea@example.com", authtest.Enrolment(t), "test"); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT count(*) FROM mailbox_grants WHERE user_id = ?`, bea.ID); n != 0 {
		t.Errorf("a reset left %d grants sealed to the old key", n)
	}
	if n := count(t, db, `SELECT count(*) FROM mailbox_access WHERE user_id = ? AND read = 1`, bea.ID); n != 2 {
		t.Errorf("after the reset bea holds read on %d mailboxes, want both flags kept", n)
	}
	// The mailbox with a key waits for a reader to supply it; the one
	// without is read by the flag, as before.
	if readsBy(t, db, keyed, bea.ID) || !readsBy(t, db, keyless, bea.ID) {
		t.Errorf("after the reset bea reads the keyed mailbox %v, the keyless one %v",
			readsBy(t, db, keyed, bea.ID), readsBy(t, db, keyless, bea.ID))
	}
	if !readsBy(t, db, keyed, ana.ID) {
		t.Error("bea's reset took ana's grant")
	}
}

func TestAResetOfTheLastReaderOfATeamMailboxWithoutAKeyNeedsNoForce(t *testing.T) {
	cheapKDF(t)
	users, db, _ := newUsers(t)
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleOwner)
	bea := authtest.NewUser(t, db, "bea@example.com", auth.RoleMember)
	team := teamOf(t, db, ana.ID)
	addMember(t, db, team.ID, bea.ID)
	mailbox := teamMailbox(t, db, team.ID, ana.ID)

	// She alone reads it, by the flag, which the reset does not take.
	code, _, err := users.CreateReset(t.Context(), ana.ID, false, "cli")
	if err != nil {
		t.Fatalf("a reset that takes nothing from a mailbox without a key: %v", err)
	}
	if _, err := users.OpenReset(t.Context(), code, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := users.CompleteReset(t.Context(), code, "ana@example.com", authtest.Enrolment(t), "test"); err != nil {
		t.Fatal(err)
	}
	if !readsBy(t, db, mailbox, ana.ID) {
		t.Error("the reset took a mailbox without a key from its reader")
	}
}
