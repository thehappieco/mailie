package service_test

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store/storetest"
	"github.com/thehappieco/mailie/internal/workspace"
)

// What each mailbox of a followed workspace holds under usage rule 1, read
// from the kept totals in one read (usage.go), for an extension.

// bytesSince is the moment the dated count starts from in these tests: its
// UTC day, 2026-10-13, whatever its hour.
var bytesSince = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// indexAt indexes a mailbox as sync would, with one message delivered at
// each moment given; the mailbox must be one that may sync.
func (f *fixture) indexAt(t *testing.T, accountID string, at ...time.Time) {
	t.Helper()
	box := providertest.NewFakeMailbox(providertest.FakeOptions{})
	for i, when := range at {
		box.Deliver("INBOX", providertest.FakeMessage{
			MessageID: fmt.Sprintf("%s-%d@example.org", accountID, i), Subject: fmt.Sprintf("Message %d", i),
			From: "bea@example.org", To: []string{"support@team.example.com"}, InternalDate: when, Size: 1000,
		})
	}
	storetest.IndexMailbox(t, f.db, accountID, box)
}

// teamSyncOn switches a team mailbox's sync on as owner, at the current
// sync text.
func (d directoryTeam) teamSyncOn(t *testing.T, accountID string) {
	t.Helper()
	on := true
	if _, err := d.svc.SetMailboxSync(t.Context(), d.ana, accountID,
		service.MailboxSyncRequest{Enabled: &on, Version: d.consent().Sync}); err != nil {
		t.Fatal(err)
	}
}

// anasHome is a mailbox in Ana's personal workspace from the directory,
// with her consent to sync.
func (d directoryTeam) anasHome(t *testing.T) string {
	t.Helper()
	id := d.teamBox(t, personalOf("ana@example.com"), d.ana.UserID, "ana@home.example")
	if err := d.repo.SavePassword(t.Context(), id, "app-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.svc.GrantSyncConsent(t.Context(), d.ana, d.consent().Sync); err != nil {
		t.Fatal(err)
	}
	return id
}

// workspaceBytes reads what a workspace's mailboxes hold, by mailbox.
func (f *fixture) workspaceBytes(t *testing.T, workspaceID string, since time.Time) (service.WorkspaceBytes,
	map[string]service.MailboxBytes,
) {
	t.Helper()
	got, err := f.svc.WorkspaceBytes(t.Context(), workspaceID, since)
	if err != nil {
		t.Fatalf("the bytes of %s: %v", workspaceID, err)
	}
	if got.WorkspaceID != workspaceID || got.Mailboxes == nil {
		t.Fatalf("the bytes of %s: %+v", workspaceID, got)
	}
	by := map[string]service.MailboxBytes{}
	for _, mb := range got.Mailboxes {
		by[mb.ID] = mb
	}
	return got, by
}

// mailboxIDs are the mailboxes a workspace's bytes list, in their order.
func mailboxIDs(w service.WorkspaceBytes) []string {
	out := []string{}
	for _, mb := range w.Mailboxes {
		out = append(out, mb.ID)
	}
	return out
}

// followedBytes is what FollowedWorkspaceBytes says.
func (f *fixture) followedBytes(t *testing.T) map[string]int64 {
	t.Helper()
	got, err := f.svc.FollowedWorkspaceBytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// ruleBytes is what the usage rule's own scan counts for a mailbox, 0 for
// one that holds nothing.
func (f *fixture) ruleBytes(t *testing.T, accountID string) int64 {
	t.Helper()
	usage, err := f.db.Usage(t.Context(), []string{accountID})
	if err != nil {
		t.Fatal(err)
	}
	return usage[accountID].Bytes
}

func TestTheWorkspaceBytesListLiveAndRemovedMailboxesUntilTheirPurge(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	ctx := t.Context()
	d.teamSyncOn(t, d.box)
	d.indexAt(t, d.box, bytesSince, bytesSince.Add(-time.Hour))
	// A mailbox linked after it that never held a credential: pending, and
	// holding nothing.
	pending := d.teamBox(t, dirTeamID, d.ana.UserID, "new@team.example.com")

	got, by := d.workspaceBytes(t, dirTeamID, bytesSince)
	if ids := mailboxIDs(got); !slices.Equal(ids, []string{d.box, pending}) {
		t.Fatalf("the team's mailboxes: %v, want %s then %s", ids, d.box, pending)
	}
	held := d.ruleBytes(t, d.box)
	box := by[d.box]
	if box.Removed || !box.PurgeAfter.IsZero() || box.Pause != "" || box.Pending || held == 0 || box.Bytes != held ||
		box.Fixed <= 0 || box.Fixed >= box.Bytes || box.DatedSince != box.Bytes-box.Fixed {
		t.Errorf("the live mailbox: %+v, want %d bytes under the rule", box, held)
	}
	if mb := by[pending]; !mb.Pending || mb.Removed || mb.Bytes != 0 || mb.DatedSince != 0 || mb.Fixed != 0 ||
		mb.Pause != service.PauseConsentOff {
		t.Errorf("the pending mailbox: %+v", mb)
	}
	followed := d.followedBytes(t)
	if followed[dirTeamID] != held {
		t.Errorf("the team holds %d, want %d", followed[dirTeamID], held)
	}
	for _, id := range []string{personalOf("ana@example.com"), personalOf("bea@example.com")} {
		if bytes, ok := followed[id]; !ok || bytes != 0 {
			t.Errorf("%s, followed and empty: %d (listed %v)", id, bytes, ok)
		}
	}
	if _, ok := followed[workspace.OperatorID]; ok {
		t.Errorf("the operator workspace, which is local, is listed: %v", followed)
	}

	// Removed, it is listed as it was, its index kept, until its purge.
	removed, err := d.svc.RemoveAccount(ctx, d.ana, d.box, service.RemoveAccountRequest{Confirm: d.box})
	if err != nil || removed.Purged {
		t.Fatalf("removing it: %+v (%v)", removed, err)
	}
	_, by = d.workspaceBytes(t, dirTeamID, bytesSince)
	if mb := by[d.box]; !mb.Removed || mb.PurgeAfter.Unix() != removed.PurgeAfter || mb.Pause != "" || mb.Pending ||
		mb.Bytes != held || mb.Fixed != box.Fixed || mb.DatedSince != box.DatedSince {
		t.Errorf("the removed mailbox: %+v, want it as it was, kept until %d", mb, removed.PurgeAfter)
	}
	if followed := d.followedBytes(t); followed[dirTeamID] != held {
		t.Errorf("the team holds %d with its mailbox removed, want %d", followed[dirTeamID], held)
	}

	// Deleted for good, it is gone, and so is what it held.
	if err := d.svc.PurgeAccount(ctx, d.ana, d.box, service.PurgeAccountRequest{Confirm: d.box}); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.workspaceBytes(t, dirTeamID, bytesSince); !slices.Equal(mailboxIDs(got), []string{pending}) {
		t.Errorf("the team's mailboxes once it is deleted for good: %v", mailboxIDs(got))
	}
	if followed := d.followedBytes(t); followed[dirTeamID] != 0 {
		t.Errorf("the team holds %d once its mailbox is deleted for good, want 0", followed[dirTeamID])
	}
}

func TestTheWorkspaceBytesAnswerOnlyAFollowedWorkspace(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	ctx := t.Context()
	if got, _ := d.workspaceBytes(t, dirTeamID, bytesSince); len(got.Mailboxes) != 1 {
		t.Errorf("the team's mailboxes: %+v", got.Mailboxes)
	}
	// A followed workspace that holds no mailbox answers none.
	if got, _ := d.workspaceBytes(t, personalOf("bea@example.com"), bytesSince); len(got.Mailboxes) != 0 {
		t.Errorf("Bea's personal workspace: %+v", got.Mailboxes)
	}
	// The operator workspace is local, and nobody knows the other.
	for _, id := range []string{workspace.OperatorID, "wsp_nobodyknows"} {
		if _, err := d.svc.WorkspaceBytes(ctx, id, bytesSince); service.CodeOf(err) != service.CodeNotFound {
			t.Errorf("the bytes of %s: %v, want not_found", id, err)
		}
	}
	// A server whose workspaces are local follows nothing, and sums
	// nothing.
	local := newFixture(t)
	if _, err := local.svc.WorkspaceBytes(ctx, workspace.OperatorID, bytesSince); service.CodeOf(err) != service.CodeConflict {
		t.Errorf("the bytes where workspaces are local: %v, want conflict", err)
	}
	if followed := local.followedBytes(t); len(followed) != 0 {
		t.Errorf("the followed workspaces' bytes where workspaces are local: %v", followed)
	}
}

func TestTheWorkspaceBytesCountFutureDatesAsRecent(t *testing.T) {
	// The team's mailbox holds a message dated on the UTC day of
	// bytesSince, though its own zone says the day before, and one dated a
	// year ahead of it; Ana's holds two dated the day before by UTC, one of
	// them on bytesSince's day in its own zone.
	d := newDirectoryTeam(t, fixtureOptions{})
	west, east := time.FixedZone("-03", -3*3600), time.FixedZone("+03", 3*3600)
	d.teamSyncOn(t, d.box)
	future := bytesSince.AddDate(1, 0, 0)
	d.indexAt(t, d.box, time.Date(2026, 10, 12, 21, 30, 0, 0, west), future)
	home := d.anasHome(t)
	d.indexAt(t, home, time.Date(2026, 10, 13, 1, 30, 0, 0, east), time.Date(2026, 10, 12, 23, 59, 59, 0, time.UTC))

	_, team := d.workspaceBytes(t, dirTeamID, bytesSince)
	_, personal := d.workspaceBytes(t, personalOf("ana@example.com"), bytesSince)
	box, mine := team[d.box], personal[home]
	if dated := box.Bytes - box.Fixed; dated <= 0 || box.DatedSince != dated {
		t.Errorf("the team's mailbox, all of it on or after the day: %+v", box)
	}
	if dated := mine.Bytes - mine.Fixed; dated <= 0 || mine.DatedSince != 0 {
		t.Errorf("Ana's mailbox, all of it before the day: %+v", mine)
	}

	// The day starts the count, not the moment: its last second reads the
	// same as its noon.
	if _, late := d.workspaceBytes(t, dirTeamID, time.Date(2026, 10, 13, 23, 59, 59, 0, time.UTC)); late[d.box].DatedSince != box.DatedSince {
		t.Errorf("at the day's last second: %+v, want %d dated", late[d.box], box.DatedSince)
	}
	// From the next day only the future message counts, and nothing from
	// after it.
	_, tomorrow := d.workspaceBytes(t, dirTeamID, bytesSince.AddDate(0, 0, 1))
	if got := tomorrow[d.box].DatedSince; got <= 0 || got >= box.DatedSince {
		t.Errorf("from the next day: %d dated, want the future message's alone, of %d", got, box.DatedSince)
	}
	// The UTC day of the moment given, whatever its zone says: 22:00 on the
	// 13th west of Greenwich is the 14th.
	if _, zoned := d.workspaceBytes(t, dirTeamID, time.Date(2026, 10, 13, 22, 0, 0, 0, west)); zoned[d.box].DatedSince != tomorrow[d.box].DatedSince {
		t.Errorf("from 22:00 on the 13th at -03: %+v, want %d dated", zoned[d.box], tomorrow[d.box].DatedSince)
	}
	_, after := d.workspaceBytes(t, dirTeamID, future.AddDate(0, 0, 1))
	if got := after[d.box]; got.DatedSince != 0 || got.Bytes != box.Bytes {
		t.Errorf("from after the future message: %+v", got)
	}
}

func TestTheWorkspaceBytesAreTheUsageRulesFigureOfEachMailbox(t *testing.T) {
	// Every mailbox of every followed workspace, live, removed and pending,
	// read from the kept totals, holds what the rule's own scan of its rows
	// counts, and each workspace's sum is theirs.
	d := newDirectoryTeam(t, fixtureOptions{})
	ctx := t.Context()
	d.teamSyncOn(t, d.box)
	d.indexAt(t, d.box, bytesSince, bytesSince.AddDate(0, -4, 0), bytesSince.AddDate(0, 0, 3))
	home := d.anasHome(t)
	d.indexAt(t, home, bytesSince.AddDate(-2, 0, 0))
	removed := d.teamBox(t, dirTeamID, d.ana.UserID, "old@team.example.com")
	if err := d.repo.SavePassword(ctx, removed, "app-password"); err != nil {
		t.Fatal(err)
	}
	d.teamSyncOn(t, removed)
	d.indexAt(t, removed, bytesSince.AddDate(0, -1, 0), bytesSince)
	if _, err := d.svc.RemoveAccount(ctx, d.ana, removed, service.RemoveAccountRequest{Confirm: removed}); err != nil {
		t.Fatal(err)
	}
	d.teamBox(t, dirTeamID, d.ana.UserID, "new@team.example.com")

	followed, err := d.svc.FollowedWorkspaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sums, listed := d.followedBytes(t), 0
	for _, w := range followed {
		got, _ := d.workspaceBytes(t, w.ID, bytesSince)
		var sum int64
		for _, mb := range got.Mailboxes {
			if want := d.ruleBytes(t, mb.ID); mb.Bytes != want {
				t.Errorf("%s holds %d by its kept totals, %d by the rule", mb.ID, mb.Bytes, want)
			}
			sum += mb.Bytes
			listed++
		}
		if sums[w.ID] != sum {
			t.Errorf("%s sums %d, its mailboxes %d", w.ID, sums[w.ID], sum)
		}
	}
	ids := []string{}
	for _, w := range followed {
		ids = append(ids, w.ID)
	}
	slices.Sort(ids)
	if listed != 4 || !slices.Equal(slices.Sorted(maps.Keys(sums)), ids) {
		t.Errorf("%d mailboxes listed, sums of %v, for the followed workspaces %v", listed, sums, ids)
	}
}
