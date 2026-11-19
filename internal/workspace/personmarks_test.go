package workspace_test

import (
	"testing"

	"github.com/thehappieco/mailie/internal/workspace"
)

// Whether a notice's actor is still a member of its workspace is part of
// what an extension reports of the notice (migration 0027): their leaving
// and their rejoining mark each holder once, however many of their notices
// a holder has.

func TestALocalLeaveAndRejoinMarkTheHoldersOfTheirNoticesOnceAndCommit(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	ana, bea, fay := f.person("ana@example.org"), f.person("bea@example.org"), f.person("fay@example.org")
	support := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleAdmin, fay.ID: workspace.RoleMember})
	box := f.link(support.ID, ana.ID, "support@example.org")
	f.grant(box.ID, bea.ID, readOnly())
	f.grant(box.ID, fay.ID, readOnly())
	f.exec(`INSERT INTO user_identities(issuer, subject, user_id, created_at) VALUES ('https://id.example.org', 'sub-fay', ?, 1)`,
		fay.ID)
	// Bea turns its sync on and off twice: Fay holds two notices of one
	// mailbox, by one actor.
	for range 2 {
		if _, err := f.db.SetMailboxSync(ctx, box.ID, true, bea.ID, "sync-1", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.SetMailboxSync(ctx, box.ID, false, bea.ID, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.count(`SELECT count(*) FROM notices WHERE user_id = ?`, fay.ID); n != 2 {
		t.Fatalf("Fay holds %d notices, want two by Bea", n)
	}
	mark := func() int {
		return f.count(`SELECT coalesce((SELECT seq FROM person_changes WHERE user_id = ?), 0)`, fay.ID)
	}
	for _, step := range []struct {
		what string
		act  func() error
	}{
		{"Bea leaves", func() error { return f.ws.RemoveMember(ctx, support.ID, bea.ID, nil) }},
		{"Bea rejoins", func() error { f.join(support.ID, bea.ID, workspace.RoleAdmin); return nil }},
	} {
		before := mark()
		if err := step.act(); err != nil {
			t.Fatalf("%s: %v", step.what, err)
		}
		if mark() <= before {
			t.Errorf("%s left Fay's mark at %d", step.what, before)
		}
		if n := f.count(`SELECT count(*) FROM person_changes WHERE user_id = ?`, fay.ID); n != 1 {
			t.Errorf("%s: Fay has %d marks", step.what, n)
		}
	}
	// Ana, whom no identity signs in, holds the same notices and is never
	// marked.
	if n := f.count(`SELECT count(*) FROM person_changes WHERE user_id IN (?, ?)`, ana.ID, bea.ID); n != 0 {
		t.Errorf("%d people no identity signs in were marked", n)
	}
}
