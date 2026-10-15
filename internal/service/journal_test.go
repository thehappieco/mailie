package service_test

import (
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// What an extension reads to report a followed workspace (journal.go,
// snapshot.go): the journal as an outbox, the followed workspaces, their
// change marks and a snapshot of who holds what.

// directoryTeam is a daemon following a directory, whose team Ana owns and
// Bea is a member of, with a mailbox Ana linked, connected once, that both
// read.
type directoryTeam struct {
	*fixture
	ana, bea service.Principal
	box      string
}

func newDirectoryTeam(t *testing.T, o fixtureOptions) directoryTeam {
	t.Helper()
	o.source = workspace.Platform()
	o.registry = account.RegistryOptions{
		Google:    account.OAuthClient{ClientID: "google-client"},
		Microsoft: account.OAuthClient{ClientID: "ms-client", Tenant: "common"},
	}
	f := newFixtureWith(t, o)
	d := directoryTeam{fixture: f}
	d.ana = f.followed(t, "ana@example.com")
	f.apply(t, document("ana@example.com", 1, inTeam(workspace.RoleOwner, 1)))
	d.bea = f.followed(t, "bea@example.com")
	f.apply(t, document("bea@example.com", 1, inTeam(workspace.RoleMember, 1)))
	d.box = f.teamBox(t, dirTeamID, d.ana.UserID, "support@team.example.com")
	if err := f.repo.SavePassword(t.Context(), d.box, "hunter2"); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE accounts SET display_name = 'Help desk' WHERE id = ?`, d.box)
	f.giveRead(t, d.box, d.bea.UserID)
	return d
}

// journal reads a workspace's journal after a seq.
func (d directoryTeam) journal(t *testing.T, workspaceID string, after int64, limit int) []service.JournalEntry {
	t.Helper()
	got, err := d.svc.MailboxJournal(t.Context(), service.JournalQuery{WorkspaceID: workspaceID, After: after, Limit: limit})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestTheJournalIsReadPerWorkspaceFromACursor(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	ctx := t.Context()
	if head, err := d.svc.MailboxJournalHead(ctx); err != nil || head != 0 {
		t.Fatalf("the head of an empty journal: %d (%v)", head, err)
	}
	removed, err := d.svc.RemoveAccount(ctx, d.ana, d.box, service.RemoveAccountRequest{Confirm: d.box})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.svc.RecoverAccount(ctx, d.ana, d.box); err != nil {
		t.Fatal(err)
	}
	// Ana's own mailbox, in her personal workspace, removed too.
	mine := d.teamBox(t, personalOf("ana@example.com"), d.ana.UserID, "ana@mail.example.com")
	if err := d.repo.SavePassword(ctx, mine, "hunter2"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.svc.RemoveAccount(ctx, d.ana, mine, service.RemoveAccountRequest{Confirm: mine}); err != nil {
		t.Fatal(err)
	}
	head, err := d.svc.MailboxJournalHead(ctx)
	if err != nil || head == 0 {
		t.Fatalf("the head: %d (%v)", head, err)
	}

	first := d.journal(t, dirTeamID, 0, 1)
	if len(first) != 1 || first[0].Kind != "removed" || first[0].AccountID != d.box || first[0].WorkspaceID != dirTeamID ||
		first[0].Label != "Help desk" || first[0].Hint != "s…@team.example.com" || first[0].Actor != d.ana.UserID ||
		first[0].ActorUserID != d.ana.UserID || first[0].PurgeAfter.Unix() != removed.PurgeAfter || first[0].Cause != "" {
		t.Fatalf("the team's first entry: %+v (removal %+v)", first, removed)
	}
	next := d.journal(t, dirTeamID, first[0].Seq, service.MaxJournalPage)
	if len(next) != 1 || next[0].Kind != "recovered" || !next[0].PurgeAfter.IsZero() || next[0].Seq <= first[0].Seq {
		t.Fatalf("the team's entries after the first: %+v", next)
	}
	if rest := d.journal(t, dirTeamID, next[0].Seq, service.MaxJournalPage); len(rest) != 0 {
		t.Errorf("the team's entries after the last: %+v", rest)
	}
	personal := d.journal(t, personalOf("ana@example.com"), 0, service.MaxJournalPage)
	if len(personal) != 1 || personal[0].AccountID != mine || personal[0].Seq != head {
		t.Errorf("Ana's personal workspace's entries: %+v, head %d", personal, head)
	}
	for _, limit := range []int{0, service.MaxJournalPage + 1} {
		if _, err := d.svc.MailboxJournal(ctx, service.JournalQuery{WorkspaceID: dirTeamID, Limit: limit}); service.CodeOf(err) != service.CodeBadRequest {
			t.Errorf("a page of %d: %v, want bad_request", limit, err)
		}
	}
}

func TestTheJournalNamesAKeysCreatorAsItsActor(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	ctx := t.Context()
	created, err := d.svc.CreateWorkspaceKey(ctx, d.ana, dirTeamID, keyRequest("tool", "read", reads(d.box)))
	if err != nil {
		t.Fatal(err)
	}
	for _, actor := range []string{"key:" + created.Prefix, "key:zzzzzzzz", "cli", store.ActorDirectory} {
		if err := d.db.Write(ctx, func(tx *sql.Tx) error {
			_, _, err := store.JournalMailboxTx(ctx, tx, store.MailboxEvent{
				Kind: store.MailboxSyncPaused, AccountID: d.box, Actor: actor, At: time.Now().Unix(),
			}, nil, false)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	got := d.journal(t, dirTeamID, 0, service.MaxJournalPage)
	var actors []string
	for _, e := range got {
		actors = append(actors, e.Actor+" "+e.ActorUserID)
	}
	want := []string{"key:" + created.Prefix + " " + d.ana.UserID, "key:zzzzzzzz ", "cli ", "directory "}
	if !slices.Equal(actors, want) {
		t.Errorf("the actors: %q, want %q", actors, want)
	}
}

func TestTheJournalsReadersAreWhoReadTheMailboxThen(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	ctx := t.Context()
	if _, err := d.svc.RemoveAccount(ctx, d.ana, d.box, service.RemoveAccountRequest{Confirm: d.box}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.svc.RecoverAccount(ctx, d.ana, d.box); err != nil {
		t.Fatal(err)
	}
	d.takeRead(t, d.box, d.bea.UserID)
	if _, err := d.svc.RemoveAccount(ctx, d.ana, d.box, service.RemoveAccountRequest{Confirm: d.box}); err != nil {
		t.Fatal(err)
	}
	got := d.journal(t, dirTeamID, 0, service.MaxJournalPage)
	if len(got) != 3 {
		t.Fatalf("%d entries, want a removal, a recovery and a removal", len(got))
	}
	both := []string{d.ana.UserID, d.bea.UserID}
	slices.Sort(both)
	for i, want := range [][]string{both, both, {d.ana.UserID}} {
		if !slices.Equal(got[i].Readers, want) {
			t.Errorf("entry %d (%s): readers %v, want %v", i, got[i].Kind, got[i].Readers, want)
		}
	}
}

func TestAnExtensionListsTheFollowedWorkspacesAndWhichChanged(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	ctx := t.Context()
	followed, err := d.svc.FollowedWorkspaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range followed {
		got = append(got, w.ID+" "+string(w.Kind)+" "+w.PersonID)
	}
	want := []string{
		personalOf("ana@example.com") + " personal " + d.ana.UserID, dirTeamID + " team ",
		personalOf("bea@example.com") + " personal " + d.bea.UserID,
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the followed workspaces: %q, want %q", got, want)
	}
	marks, head, err := d.svc.WorkspaceChanges(ctx, 0, 100)
	if err != nil || head == 0 || len(marks) != 3 {
		t.Fatalf("the marks: %+v, head %d (%v)", marks, head, err)
	}
	d.takeRead(t, d.box, d.bea.UserID)
	after, newHead, err := d.svc.WorkspaceChanges(ctx, head, 100)
	if err != nil || len(after) != 1 || after[0].WorkspaceID != dirTeamID || newHead <= head {
		t.Errorf("the marks after Bea lost read: %+v, head %d (%v)", after, newHead, err)
	}
	if _, _, err := d.svc.WorkspaceChanges(ctx, 0, 0); service.CodeOf(err) != service.CodeBadRequest {
		t.Errorf("a page of no changes: %v, want bad_request", err)
	}
}

func TestAnAccessSnapshotIsOnlyOfAWorkspaceFromTheDirectorysSource(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	ctx := t.Context()
	s, err := d.svc.AccessSnapshot(ctx, dirTeamID)
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind != workspace.KindTeam || len(s.Mailboxes) != 1 || s.MeasuredAt.IsZero() {
		t.Fatalf("the team's snapshot: %+v", s)
	}
	mb := s.Mailboxes[0]
	if mb.ID != d.box || mb.Label != "Help desk" || mb.Hint != "s…@team.example.com" || mb.Pause != service.PauseConsentOff ||
		len(mb.People) != 2 || len(mb.Protected) != 0 {
		t.Errorf("the team's mailbox: %+v", mb)
	}
	// The operator workspace, and a workspace nobody knows, are not the
	// directory's.
	for _, id := range []string{workspace.OperatorID, "wsp_nobodyknows"} {
		if _, err := d.svc.AccessSnapshot(ctx, id); service.CodeOf(err) != service.CodeNotFound {
			t.Errorf("the snapshot of %s: %v, want not_found", id, err)
		}
	}
	// A server whose workspaces are local follows nothing.
	local := newFixture(t)
	if _, err := local.svc.AccessSnapshot(ctx, workspace.OperatorID); service.CodeOf(err) != service.CodeConflict {
		t.Errorf("a snapshot where workspaces are local: %v, want conflict", err)
	}
}

func TestOneSwitchSaysWhatASnapshotReportsOfARemovedMailbox(t *testing.T) {
	// Bea alone reads the team's mailbox, which Ana removes. By default it is
	// reported as it would be once recovered, until it is deleted for good;
	// the other way, as the rules here see it.
	for _, c := range []struct {
		what  string
		rule  service.RemovedInSnapshot
		reads bool
	}{
		{"by default", service.RemovedAsKept, true},
		{"the other way", service.RemovedAsNobody, false},
	} {
		d := newDirectoryTeam(t, fixtureOptions{removedInSnapshot: c.rule})
		ctx := t.Context()
		d.takeRead(t, d.box, d.ana.UserID)
		removed, err := d.svc.RemoveAccount(ctx, d.ana, d.box, service.RemoveAccountRequest{Confirm: d.box})
		if err != nil {
			t.Fatal(err)
		}
		s, err := d.svc.AccessSnapshot(ctx, dirTeamID)
		if err != nil {
			t.Fatal(err)
		}
		mb := s.Mailboxes[0]
		if !mb.Removed || mb.PurgeAfter.Unix() != removed.PurgeAfter || mb.Pause != "" {
			t.Errorf("%s: the removed mailbox %+v", c.what, mb)
		}
		var bea service.SnapshotPerson
		for _, p := range mb.People {
			if p.UserID == d.bea.UserID {
				bea = p
			}
		}
		if bea.Reads != c.reads || (len(mb.Protected) == 1) != c.reads {
			t.Errorf("%s: Bea %+v, protected %v", c.what, bea, mb.Protected)
		}
	}
}
