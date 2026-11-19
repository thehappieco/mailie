package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"testing"
)

// The marks of a person whose own settings changed (migration 0027,
// changes.go), as this package's writes leave them. The seed at the latest
// schema: Fay (usr_6), whom an identity signs in, and Bea (usr_2), whom none
// does, both read the local team wsp_t1's mailbox acc_t1, which Owner
// (usr_1) owns.

const (
	marksBea = "usr_0000000000000002"
	marksFay = "usr_0000000000000006"
	marksOwn = "usr_0000000000000001"
)

// personMark is a person's mark, 0 when they have none.
func (f seededSchema) personMark(userID string) int64 {
	f.t.Helper()
	return queryInt(f.t, f.s, `SELECT coalesce((SELECT seq FROM person_changes WHERE user_id = ?), 0)`, userID)
}

// marked runs what, an act, and fails the test unless it moved Fay's mark
// up and left Bea, whom no identity signs in, unmarked.
func (f seededSchema) marked(what string, act func() error) {
	f.t.Helper()
	before := f.personMark(marksFay)
	if err := act(); err != nil {
		f.t.Fatalf("%s: %v", what, err)
	}
	if after := f.personMark(marksFay); after <= before {
		f.t.Errorf("%s left Fay's mark at %d (was %d)", what, after, before)
	}
	if f.personMark(marksBea) != 0 {
		f.t.Errorf("%s marked Bea, whom no identity signs in", what)
	}
}

// unmarked runs what and fails the test if it moved Fay's mark.
func (f seededSchema) unmarked(what string, act func() error) {
	f.t.Helper()
	before := f.personMark(marksFay)
	if err := act(); err != nil {
		f.t.Fatalf("%s: %v", what, err)
	}
	if after := f.personMark(marksFay); after != before {
		f.t.Errorf("%s moved Fay's mark from %d to %d", what, before, after)
	}
}

func TestAPersonIsMarkedWhenTheirConsentsChange(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	f.marked("a consent to sync", func() error { _, _, err := f.s.GrantSyncConsent(ctx, marksFay, "sync-2"); return err })
	f.unmarked("the same consent again", func() error {
		_, _, err := f.s.GrantSyncConsent(ctx, marksFay, "sync-2")
		return err
	})
	f.marked("a consent to sync withdrawn", func() error { _, err := f.s.WithdrawSyncConsent(ctx, marksFay); return err })
	f.marked("a consent to actions", func() error { _, err := f.s.GrantActionsConsent(ctx, marksFay, "actions-2"); return err })
	f.marked("a consent to actions withdrawn", func() error {
		_, err := f.s.WithdrawActionsConsent(ctx, marksFay)
		return err
	})
	f.marked("a consent to sending", func() error { _, err := f.s.GrantSendConsent(ctx, marksFay, "send-2"); return err })
	f.marked("a consent to sending withdrawn", func() error {
		_, err := f.s.WithdrawSendConsent(ctx, marksFay)
		return err
	})
	for _, c := range []struct{ what, query string }{
		{"an account key", `UPDATE users SET public_key = randomblob(32) WHERE id = ?`},
		{"a sign-out everywhere", `UPDATE users SET signed_out_everywhere_at = 990 WHERE id = ?`},
	} {
		f.marked(c.what, func() error { return f.exec(c.query, marksFay) })
	}
	// What a person's own settings do not show marks nothing.
	f.unmarked("their name, status and updated moment", func() error {
		return f.exec(`UPDATE users SET name = 'Fay F', updated_at = 999, password_changed_at = 999 WHERE id = ?`, marksFay)
	})
	// Bea's consents mark nobody: no identity signs her in.
	if _, err := f.s.GrantActionsConsent(ctx, marksBea, "actions-2"); err != nil {
		t.Fatal(err)
	}
	if f.personMark(marksBea) != 0 {
		t.Error("Bea, whom no identity signs in, was marked")
	}
}

func TestAPersonIsMarkedWhenANoticeIsGivenDismissedWithdrawnOrSwept(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	var off string
	f.marked("a notice given", func() error {
		off = f.journal(MailboxSyncPaused, "acc_t1", marksOwn, 900, true)
		return nil
	})
	if n := queryInt(t, f.s, `SELECT count(*) FROM notices WHERE user_id = ?`, marksBea); n != 1 {
		t.Fatalf("Bea holds %d notices, want the one she was given", n)
	}
	f.marked("a notice dismissed", func() error { return f.s.DismissNotice(ctx, marksFay, off) })

	// A pause tells the other readers, and its end withdraws what it told.
	if _, err := f.s.SetMailboxSync(ctx, "acc_t1", true, marksOwn, "sync-1", nil); err != nil {
		t.Fatal(err)
	}
	f.marked("a pause's notice", func() error { _, err := f.s.PauseSync(ctx, "acc_t1", marksOwn, nil); return err })
	f.marked("a pause's notice withdrawn by its end", func() error {
		_, err := f.s.ResumeSync(ctx, "acc_t1", marksOwn, "sync-1", nil)
		return err
	})
	if n := queryInt(t, f.s, `SELECT count(*) FROM notices WHERE user_id = ? AND event_seq IN
		(SELECT seq FROM mailbox_events WHERE kind = 'paused')`, marksFay); n != 0 {
		t.Fatalf("the pause's notice is still Fay's (%d)", n)
	}
	// A notice swept with its journal entry, by the schema's cascade.
	f.journal(MailboxRemoved, "acc_t1", marksOwn, 950, true)
	f.marked("a notice swept with its entry", func() error {
		_, err := f.s.SweepMailboxEvents(ctx, 1<<40)
		return err
	})
	if n := queryInt(t, f.s, `SELECT count(*) FROM notices`); n != 0 {
		t.Fatalf("the sweep left %d notices", n)
	}
}

func TestAPersonIsMarkedWhenANoticeGoesWithItsJournalEntryOrItsClosedPersonsMailboxes(t *testing.T) {
	// What a notice shows of its entry: the label and hint a deletion for
	// good forgets, and its actor, forgotten with that person; and the
	// entry itself, gone with the mailboxes of a person who is closed.
	f := newSchemaLatest(t)
	f.journal(MailboxSyncPaused, "acc_t1", marksOwn, 900, true)
	f.marked("the mailbox's names forgotten", func() error {
		return f.write(func(ctx context.Context, tx *sql.Tx) error { return ForgetMailboxNamesTx(ctx, tx, []string{"acc_t1"}) })
	})
	f.marked("its actor forgotten", func() error {
		return f.write(func(ctx context.Context, tx *sql.Tx) error { return ForgetPersonInJournalTx(ctx, tx, marksOwn) })
	})
	f.marked("its entry deleted", func() error {
		return f.write(func(ctx context.Context, tx *sql.Tx) error {
			return DeleteMailboxEventsTx(ctx, tx, []string{"acc_t1"})
		})
	})
	if n := queryInt(t, f.s, `SELECT count(*) FROM notices`); n != 0 {
		t.Fatalf("the entry's deletion left %d notices", n)
	}
}

func TestAPersonNoIdentitySignsInIsNeverMarked(t *testing.T) {
	// Bea holds notices, consents and a key, and changes each: no identity
	// signs her in, so a server that follows no directory writes no mark.
	f := newSchemaLatest(t)
	ctx := context.Background()
	f.accepted("Fay unlinked", f.exec(`DELETE FROM user_identities WHERE user_id = ?`, marksFay))
	id := f.journal(MailboxSyncPaused, "acc_t1", marksOwn, 900, true)
	for _, act := range []func() error{
		func() error { _, _, err := f.s.GrantSyncConsent(ctx, marksBea, "sync-2"); return err },
		func() error { return f.exec(`UPDATE users SET public_key = randomblob(32) WHERE id = ?`, marksBea) },
		func() error { return f.exec(`UPDATE users SET signed_out_everywhere_at = 990 WHERE id = ?`, marksBea) },
		func() error { return f.s.DismissNotice(ctx, marksBea, id) },
		func() error { f.remove("acc_t1", marksOwn, 950); return nil },
	} {
		if err := act(); err != nil {
			t.Fatal(err)
		}
	}
	if n := queryInt(t, f.s, `SELECT count(*) FROM person_changes`); n != 0 {
		t.Errorf("%d people were marked, whom no identity signs in", n)
	}
}

func TestPersonChangesListEachPersonOnceAtTheirLastMark(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	const gil = "usr_0000000000000007"
	f.accepted("Gil signs in with an identity", f.exec(`INSERT INTO user_identities(issuer, subject, user_id, created_at)
		VALUES ('https://accounts.example.com', 'subject-of-gil', ?, 900)`, gil))
	for i, who := range []string{marksFay, gil, marksFay, marksFay} {
		if _, err := f.s.GrantActionsConsent(ctx, who, fmt.Sprintf("actions-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	marks, head, err := f.s.PersonChanges(ctx, 0, MaxPersonChanges)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range marks {
		got = append(got, m.UserID)
	}
	if !slices.Equal(got, []string{gil, marksFay}) || head != marks[len(marks)-1].Seq {
		t.Fatalf("the marks: %+v, head %d; want Gil's then Fay's last, once each", marks, head)
	}
	// A page, then the rest from its last seq; nothing after the head.
	first, _, err := f.s.PersonChanges(ctx, 0, 1)
	if err != nil || len(first) != 1 || first[0].UserID != gil {
		t.Fatalf("a page of one: %+v (%v)", first, err)
	}
	rest, _, err := f.s.PersonChanges(ctx, first[0].Seq, MaxPersonChanges)
	if err != nil || len(rest) != 1 || rest[0].UserID != marksFay {
		t.Errorf("the rest: %+v (%v)", rest, err)
	}
	if none, h, err := f.s.PersonChanges(ctx, head, MaxPersonChanges); err != nil || len(none) != 0 || h != head {
		t.Errorf("after the head: %+v, head %d (%v)", none, h, err)
	}
}

func TestADeletedPersonsMarkGoesWithThem(t *testing.T) {
	// Ivy signs in with an identity, reads acc_t1 and holds a notice of it;
	// deleted by the schema's cascade alone, her identity and her notices
	// going with her in whatever order it runs, nothing names her after.
	f := newSchemaLatest(t)
	const ivy = "usr_0000000000000009"
	execAll(t, f.s, person(ivy, "ivy@example.org", "member", "active", 0, "")...)
	execAll(t, f.s,
		`INSERT INTO user_identities(issuer, subject, user_id, created_at)
		 VALUES ('https://accounts.example.com', 'subject-of-ivy', '`+ivy+`', 900)`,
		`INSERT INTO workspace_members(workspace_id, user_id, role, status, created_at, updated_at)
		 VALUES ('wsp_t1', '`+ivy+`', 'member', 'active', 900, 900)`,
		`INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, granted_by, created_at, updated_at)
		 VALUES ('acc_t1', 'wsp_t1', '`+ivy+`', 1, 'cli', 900, 900)`)
	f.journal(MailboxSyncPaused, "acc_t1", marksOwn, 950, true)
	if f.personMark(ivy) == 0 {
		t.Fatal("Ivy's notice did not mark her; the test would prove nothing")
	}
	execAll(t, f.s, `DELETE FROM workspaces WHERE person_id = '`+ivy+`'`, `DELETE FROM users WHERE id = '`+ivy+`'`)
	if n := queryInt(t, f.s, `SELECT count(*) FROM person_changes WHERE user_id = ?`, ivy); n != 0 {
		t.Errorf("Ivy's mark outlived her: %d rows", n)
	}
	if f.personMark(marksFay) == 0 {
		t.Error("Fay's mark went with Ivy")
	}
}
