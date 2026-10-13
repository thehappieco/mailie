package auth_test

import (
	"errors"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/workspace"
)

func TestRevokingKeysCreatedBeforeATimeLeavesTheLaterOnes(t *testing.T) {
	keys, db, clock := newKeys(t)
	ctx := t.Context()
	ana := authtest.NewUser(t, db, "ana@example.org", auth.RoleMember)
	bea := authtest.NewUser(t, db, "bea@example.org", auth.RoleMember)
	team := teamOf(t, db, ana.ID)
	own := authtest.Personal(t, db, ana.ID)
	start := *clock
	at := func(after time.Duration, workspaceID, createdBy string) string {
		t.Helper()
		*clock = start.Add(after)
		_, key := issue(t, keys, auth.NewKeyRequest{Scope: auth.ScopeRead, WorkspaceID: workspaceID, CreatedBy: createdBy,
			TermsVersion: "terms"})
		return key.Prefix
	}
	// Before the moment: two of Ana's keys, in two workspaces, and one she
	// revoked herself; Bea's; and an instance key. At and after it: two
	// more of Ana's.
	ownBefore := at(0, own, ana.ID)
	teamBefore := at(time.Minute, team.ID, ana.ID)
	revoked := at(2*time.Minute, own, ana.ID)
	if err := keys.RevokeCreatedBy(ctx, revoked, ana.ID); err != nil {
		t.Fatal(err)
	}
	beas := at(3*time.Minute, authtest.Personal(t, db, bea.ID), bea.ID)
	_, instance := issue(t, keys, auth.NewKeyRequest{Scope: auth.ScopeAdmin, CreatedBy: "cli"})
	moment := start.Add(10 * time.Minute)
	atMoment := at(10*time.Minute, team.ID, ana.ID)
	after := at(20*time.Minute, own, ana.ID)
	*clock = start.Add(30 * time.Minute)

	for _, reason := range []auth.KeyStopReason{"", "expired", "MANUAL"} {
		if n, err := keys.RevokeCreatedBefore(ctx, ana.ID, moment, reason); !errors.Is(err, auth.ErrStopReason) || n != 0 {
			t.Errorf("the reason %q: %d keys, %v", reason, n, err)
		}
	}
	if n, err := keys.RevokeCreatedBefore(ctx, "", moment, workspace.KeyStopAccountRecovered); err != nil || n != 0 {
		t.Errorf("nobody's keys: %d keys, %v", n, err)
	}
	n, err := keys.RevokeCreatedBefore(ctx, ana.ID, moment, workspace.KeyStopAccountRecovered)
	if err != nil || n != 2 {
		t.Fatalf("revoked %d keys (%v), want Ana's two live ones from before the moment", n, err)
	}

	want := map[string]struct {
		live   bool
		reason workspace.KeyStopReason
	}{
		ownBefore:       {false, workspace.KeyStopAccountRecovered},
		teamBefore:      {false, workspace.KeyStopAccountRecovered},
		revoked:         {false, workspace.KeyStopManual},
		beas:            {true, ""},
		instance.Prefix: {true, ""},
		atMoment:        {true, ""},
		after:           {true, ""},
	}
	for prefix, w := range want {
		key, err := keys.Get(ctx, prefix)
		if err != nil {
			t.Fatal(err)
		}
		if key.Live(*clock) != w.live || key.StopReason != w.reason {
			t.Errorf("key %s created %s: live %v for %q, want live %v for %q", prefix,
				key.CreatedAt.Sub(start), key.Live(*clock), key.StopReason, w.live, w.reason)
		}
		if !w.live && prefix != revoked && !key.RevokedAt.Equal(*clock) {
			t.Errorf("key %s was revoked at %s, want now", prefix, key.RevokedAt)
		}
	}
	// Again: nothing left to stop.
	if n, err := keys.RevokeCreatedBefore(ctx, ana.ID, moment, workspace.KeyStopAccountRecovered); err != nil || n != 0 {
		t.Errorf("again: %d keys, %v", n, err)
	}
}
