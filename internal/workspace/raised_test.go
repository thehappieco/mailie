package workspace_test

import (
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// When a grant last gained a flag (mailbox_access.raised_at, migration
// 0029): what a request that takes flags away compares with.

func TestRaisedAtMovesOnlyWhenAFlagIsAdded(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	olga, ana, dan := f.person("olga@example.org"), f.person("ana@example.org"), f.legacy("dan@example.org")
	team := f.team("Support", olga, map[string]workspace.Role{ana.ID: workspace.RoleAdmin, dan.ID: workspace.RoleMember})
	box := f.link(team.ID, ana.ID, "support@example.org")
	if got, created := f.count(`SELECT raised_at FROM mailbox_access WHERE account_id = ? AND user_id = ?`, box.ID, ana.ID),
		f.count(`SELECT created_at FROM mailbox_access WHERE account_id = ? AND user_id = ?`, box.ID, ana.ID); got == 0 || got != created {
		t.Errorf("the linker's grant was raised at %d, want when it was made, %d", got, created)
	}
	clock := time.Unix(1_000, 0)
	f.ws.WithClock(func() time.Time { return clock })
	raised := func(what string, want int64) {
		t.Helper()
		if got := f.count(`SELECT raised_at FROM mailbox_access WHERE account_id = ? AND user_id = ?`, box.ID, dan.ID); int64(got) != want {
			t.Errorf("after %s dan's grant was raised at %d, want %d", what, got, want)
		}
	}
	at := func(sec int64, step func()) {
		t.Helper()
		clock = time.Unix(sec, 0)
		step()
	}

	at(1_000, func() { f.grant(box.ID, dan.ID, readOnly()) })
	raised("a grant given", 1_000)
	at(2_000, func() { f.grant(box.ID, dan.ID, workspace.Flags{Read: true, Send: true}) })
	raised("a flag added", 2_000)
	at(3_000, func() { f.grant(box.ID, dan.ID, readOnly()) })
	raised("a flag taken by setting fewer", 2_000)
	at(3_500, func() { f.grant(box.ID, dan.ID, readOnly()) })
	raised("the same flags set again", 2_000)
	at(4_000, func() { f.grant(box.ID, dan.ID, workspace.Flags{Read: true, Manage: true}) })
	raised("manage added", 4_000)
	at(4_500, func() {
		if _, err := f.ws.Revoke(ctx, box.ID, dan.ID, manageOnly(), nil); err != nil {
			t.Fatal(err)
		}
	})
	raised("a flag revoked", 4_000)

	// The key supplied to a member who holds the flag is no flag.
	authtest.KeyMailbox(t, f.db, box.ID, ana.ID)
	f.enrol(dan.ID)
	at(5_000, func() {
		if _, err := f.ws.SupplyGrant(ctx, box.ID, dan.ID, ana.ID, 1, *f.sealed(dan.ID, 1), nil); err != nil {
			t.Fatal(err)
		}
	})
	raised("the key supplied", 4_000)

	// A flag a request adds is raised at the request's first moment, never
	// later than now; one it takes away leaves the time as it was.
	req := &store.Request{Extension: "ext", ID: "r1", UserID: ana.ID, Act: "raise_flags",
		FirstIssued: time.Unix(6_500, 400_000_000), KeepUntil: time.Now().Add(time.Hour)}
	at(7_000, func() {
		from := workspace.Flags{Read: true}
		if _, changed, err := f.ws.SetGrantFor(ctx, workspace.GrantChange{AccountID: box.ID, UserID: dan.ID,
			Flags: workspace.Flags{Read: true, Act: true}, From: &from, GrantedBy: ana.ID, Request: req}); err != nil || !changed {
			t.Fatalf("a raise by a request: %v, changed %v", err, changed)
		}
	})
	raised("a raise by a request", 6_500)
	req2 := &store.Request{Extension: "ext", ID: "r2", UserID: ana.ID, Act: "drop_flags",
		FirstIssued: time.Unix(8_000, 0), KeepUntil: time.Now().Add(time.Hour)}
	at(8_500, func() {
		if _, changed, err := f.ws.RevokeFor(ctx, req2, box.ID, dan.ID, workspace.Flags{Act: true}, nil); err != nil || !changed {
			t.Fatalf("a drop by a request: %v, changed %v", err, changed)
		}
	})
	raised("a drop by a request", 6_500)
}

func TestAGrantChangeDrawnFromFlagsNoLongerHeldIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	olga, dan := f.person("olga@example.org"), f.legacy("dan@example.org")
	team := f.team("Support", olga, map[string]workspace.Role{dan.ID: workspace.RoleMember})
	box := f.link(team.ID, olga.ID, "support@example.org")
	none := workspace.Flags{}
	change := func(from workspace.Flags, to workspace.Flags) error {
		_, _, err := f.ws.SetGrantFor(ctx, workspace.GrantChange{AccountID: box.ID, UserID: dan.ID, Flags: to, From: &from,
			GrantedBy: olga.ID})
		return err
	}
	want(t, "a change drawn from read nobody holds", change(readOnly(), readAct()), workspace.ErrGrantChanged)
	if err := change(none, readOnly()); err != nil {
		t.Fatalf("a change drawn from nothing held: %v", err)
	}
	if _, err := f.ws.Revoke(ctx, box.ID, dan.ID, workspace.Flags{}, nil); err != nil {
		t.Fatal(err)
	}
	// Read taken away since: never given back by the change drawn before.
	want(t, "a change drawn before read was taken away", change(readOnly(), readAct()), workspace.ErrGrantChanged)
	if n := f.count(`SELECT count(*) FROM mailbox_access WHERE account_id = ? AND user_id = ?`, box.ID, dan.ID); n != 0 {
		t.Errorf("the refused change left %d grants", n)
	}
}
