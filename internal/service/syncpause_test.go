package service_test

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Pausing one mailbox's sync (syncpause.go; docs/workspaces.md, "Pausing a
// mailbox's sync"): who pauses and resumes it, what a team's resume agrees
// to, what keeps working while it is paused, and every way a pause ends.

// pause pauses a mailbox as p and fails the test unless it is paused now.
func (f *fixture) pause(t *testing.T, p service.Principal, id string) {
	t.Helper()
	st, err := f.svc.PauseMailboxSync(t.Context(), p, id)
	if err != nil || !st.Paused || st.PausedSince == 0 {
		t.Fatalf("pausing %s: %+v (%v)", id, st, err)
	}
}

// paused reports whether a mailbox's sync is paused, as the database holds
// it.
func (f *fixture) paused(t *testing.T, id string) bool {
	t.Helper()
	return f.count(t, `SELECT sync_paused_at FROM accounts WHERE id = ?`, id) != 0
}

// syncedTeam is the Support team with its mailbox, connected, under the
// team's consent to sync: Ana, its owner, and Bea, a member, read it; Carol
// is an admin who reads nothing.
func syncedTeam(t *testing.T, f *fixture) keysTeam {
	t.Helper()
	tm := newKeysTeam(t, f)
	f.connect(t, tm.shared)
	tm.grant(t, tm.shared, workspace.Flags{Read: true})
	tm.syncOn(t, f, tm.shared)
	return tm
}

// ownSynced is a person's own mailbox, connected, under their consent.
func (f *fixture) ownSynced(t *testing.T, p service.Principal, email string) string {
	t.Helper()
	id := f.mailbox(t, p, email)
	if _, err := f.svc.GrantSyncConsent(t.Context(), p, f.consent().Sync); err != nil {
		t.Fatal(err)
	}
	return id
}

// opsSynced is an operator mailbox, switched on.
func (f *fixture) opsSynced(t *testing.T, email string) string {
	t.Helper()
	id := f.mailbox(t, admin(), email)
	if _, err := f.svc.SetMailboxSync(t.Context(), admin(), id, switchSync(true)); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAPersonPausesAndResumesTheirOwnMailboxAndNobodyElseCan(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	bea := f.person(t, "bea@example.com", auth.RoleOwner)
	own := f.ownSynced(t, ana, "ana@mail.example")
	key := keyOf(t, f, ana, auth.ScopeAdmin)
	for name, p := range map[string]service.Principal{"Bea": bea, "the operator": admin()} {
		_, err := f.svc.PauseMailboxSync(ctx, p, own)
		wantCode(t, name+" pausing Ana's mailbox", err, service.CodeNotFound)
	}
	_, err := f.svc.PauseMailboxSync(ctx, key, own)
	wantCode(t, "Ana's key pausing her mailbox", err, service.CodeNotAuthorized)
	f.pause(t, ana, own)
	if ok := f.eligible(t, own); ok {
		t.Error("the paused mailbox syncs")
	}
	for name, p := range map[string]service.Principal{"Bea": bea, "the operator": admin()} {
		_, err := f.svc.ResumeMailboxSync(ctx, p, own, service.SyncResumeRequest{})
		wantCode(t, name+" resuming Ana's mailbox", err, service.CodeNotFound)
	}
	_, err = f.svc.ResumeMailboxSync(ctx, key, own, service.SyncResumeRequest{})
	wantCode(t, "Ana's key resuming her mailbox", err, service.CodeNotAuthorized)
	_, err = f.svc.ResumeMailboxSync(ctx, ana, own, service.SyncResumeRequest{Version: f.consent().Sync})
	wantCode(t, "Ana resuming with a text to agree to", err, service.CodeBadRequest)
	st, err := f.svc.ResumeMailboxSync(ctx, ana, own, service.SyncResumeRequest{})
	if err != nil || st.Paused || !st.Enabled {
		t.Fatalf("Ana resuming: %+v (%v)", st, err)
	}
	if !f.eligible(t, own) {
		t.Error("the resumed mailbox does not sync")
	}
}

func TestATeamsOwnersAndAdminsPauseAndResumeItsMailboxButAMemberCannot(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	// Bea manages the mailbox by a stored flag, which is its card, never its
	// sync.
	tm.grant(t, tm.shared, workspace.Flags{Read: true, Manage: true})
	member := func(what string) {
		t.Helper()
		_, err := f.svc.PauseMailboxSync(ctx, tm.bea, tm.shared)
		wantCode(t, "Bea pausing it, "+what, err, service.CodeNotAuthorized)
		for _, version := range []string{f.consent().Sync, "an-older-text"} {
			_, err := f.svc.ResumeMailboxSync(ctx, tm.bea, tm.shared, service.SyncResumeRequest{Version: version})
			wantCode(t, "Bea resuming it to "+version+", "+what, err, service.CodeNotAuthorized)
			if !strings.Contains(service.MessageOf(err), "owner or an admin") {
				t.Errorf("Bea is told %q", service.MessageOf(err))
			}
		}
	}
	member("not paused")
	f.pause(t, tm.ana, tm.shared)
	member("paused")
	if _, err := f.svc.ResumeMailboxSync(ctx, tm.carol, tm.shared, service.SyncResumeRequest{Version: f.consent().Sync}); err != nil {
		t.Fatalf("Carol, an admin, resuming it: %v", err)
	}
	f.pause(t, tm.carol, tm.shared)
	off := false
	if _, err := f.svc.SetMailboxSync(ctx, tm.ana, tm.shared, service.MailboxSyncRequest{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	member("switched off")
	if f.paused(t, tm.shared) {
		t.Error("turning its sync off left it paused")
	}
}

func TestATeamResumeNamesTheCurrentSyncTextAndAgreesForTheTeam(t *testing.T) {
	hook := &freshHook{}
	f := newFixtureWith(t, fixtureOptions{freshness: hook})
	ctx := t.Context()
	tm := syncedTeam(t, f)
	// The team's consent is still the one migration 0011 bound to Bea.
	f.exec(t, `UPDATE accounts SET sync_enabled_via = 'migration', sync_enabled_by = ? WHERE id = ?`,
		tm.bea.UserID, tm.shared)
	f.pause(t, tm.ana, tm.shared)
	hook.take()
	agreement := func() string {
		t.Helper()
		return strings.Join(queryStrings(t, f, `SELECT sync_enabled_at || ' ' || sync_enabled_by || ' ' ||
			sync_consent_version || ' ' || sync_enabled_via || ' ' || sync_paused_at FROM accounts WHERE id = ?`, tm.shared), "")
	}
	before := agreement()
	for _, version := range []string{"", "an-older-text"} {
		_, err := f.svc.ResumeMailboxSync(ctx, tm.carol, tm.shared, service.SyncResumeRequest{Version: version})
		wantCode(t, "Carol resuming it to "+strconv.Quote(version), err, service.CodeBadRequest)
	}
	if got := agreement(); got != before {
		t.Errorf("a refused resume wrote %q, was %q", got, before)
	}
	if asked := hook.take(); len(asked) != 0 {
		t.Errorf("a refused resume asked %+v", asked)
	}
	if _, err := f.svc.ResumeMailboxSync(ctx, tm.carol, tm.shared, service.SyncResumeRequest{Version: f.consent().Sync}); err != nil {
		t.Fatal(err)
	}
	dir, err := f.svc.AccessDirectory(ctx, tm.ana, tm.id)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(dir, func(mb service.MailboxAccess) bool { return mb.AccountID == tm.shared })
	if i < 0 || dir[i].Sync == nil || !dir[i].Sync.Enabled || dir[i].Sync.EnabledBy != tm.carol.UserID ||
		dir[i].Sync.Version != f.consent().Sync || dir[i].Sync.Migrated || dir[i].PausedAt != 0 || dir[i].PausedBy != "" {
		t.Errorf("the directory after Carol's resume: %+v", dir)
	}
}

func TestATeamMailboxNobodyReadsIsNotResumed(t *testing.T) {
	hook := &freshHook{}
	f := newFixtureWith(t, fixtureOptions{freshness: hook})
	ctx := t.Context()
	tm := syncedTeam(t, f)
	f.pause(t, tm.ana, tm.shared)
	f.exec(t, `DELETE FROM mailbox_access WHERE account_id = ?`, tm.shared)
	hook.take()
	_, err := f.svc.ResumeMailboxSync(ctx, tm.ana, tm.shared, service.SyncResumeRequest{Version: f.consent().Sync})
	wantCode(t, "resuming a mailbox nobody reads", err, service.CodeConflict)
	if !strings.Contains(service.MessageOf(err), "nobody in the team reads this mailbox") {
		t.Errorf("the refusal says %q", service.MessageOf(err))
	}
	if !f.paused(t, tm.shared) {
		t.Error("the refused resume ended the pause")
	}
	if asked := hook.take(); len(asked) != 0 {
		t.Errorf("the refused resume asked %+v", asked)
	}
}

func TestAPausedTeamMailboxASuspendedReaderWaitsForIsResumedOnceTheyAreBack(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	f.exec(t, `DELETE FROM mailbox_access WHERE account_id = ? AND user_id = ?`, tm.shared, tm.ana.UserID)
	f.pause(t, tm.ana, tm.shared)
	f.suspend(t, tm.bea.UserID)
	resume := service.SyncResumeRequest{Version: f.consent().Sync}
	_, err := f.svc.ResumeMailboxSync(ctx, tm.ana, tm.shared, resume)
	wantCode(t, "resuming it while Bea is suspended", err, service.CodeConflict)
	f.resume(t, tm.bea.UserID)
	if st, err := f.svc.ResumeMailboxSync(ctx, tm.ana, tm.shared, resume); err != nil || st.Paused {
		t.Errorf("resuming it once Bea is back: %+v (%v)", st, err)
	}
}

func TestAPersonalOrOperatorResumeTakesNoVersion(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	for p, id := range map[*service.Principal]string{
		&ana: f.ownSynced(t, ana, "ana@mail.example"), new(service.Principal): f.opsSynced(t, "ops@mail.example"),
	} {
		who := *p
		if who.UserID == "" {
			who = admin()
		}
		f.pause(t, who, id)
		consent := queryStrings(t, f, `SELECT sync_enabled_at || ' ' || sync_enabled_by || ' ' || sync_consent_version
			FROM accounts WHERE id = ?`, id)
		_, err := f.svc.ResumeMailboxSync(ctx, who, id, service.SyncResumeRequest{Version: f.consent().Sync})
		wantCode(t, id+" resumed with a version", err, service.CodeBadRequest)
		if !f.paused(t, id) {
			t.Errorf("%s was resumed all the same", id)
		}
		if st, err := f.svc.ResumeMailboxSync(ctx, who, id, service.SyncResumeRequest{}); err != nil || st.Paused {
			t.Errorf("%s resumed: %+v (%v)", id, st, err)
		}
		if got := queryStrings(t, f, `SELECT sync_enabled_at || ' ' || sync_enabled_by || ' ' || sync_consent_version
			FROM accounts WHERE id = ?`, id); !slices.Equal(got, consent) {
			t.Errorf("%s's resume recorded %v, was %v", id, got, consent)
		}
	}
}

func TestAKeyNeverPausesOrResumesAMailbox(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	own := f.ownSynced(t, tm.ana, "ana@mail.example")
	ops := f.opsSynced(t, "ops@mail.example")
	f.pause(t, tm.ana, own)
	f.pause(t, tm.ana, tm.shared)
	restricted := service.Principal{KeyPrefix: "cccccccc", Scope: auth.ScopeAdmin, AccountIDs: []string{ops},
		WorkspaceID: workspace.OperatorID}
	for name, c := range map[string]struct {
		p  service.Principal
		id string
	}{
		"Ana's own key, on her mailbox":         {keyOf(t, f, tm.ana, auth.ScopeAdmin), own},
		"the team's key, on its mailbox":        {keyOf(t, f, tm.ana, auth.ScopeAdmin, tm.id), tm.shared},
		"a restricted instance key, on its own": {restricted, ops},
	} {
		_, err := f.svc.PauseMailboxSync(ctx, c.p, c.id)
		wantCode(t, name+" pausing", err, service.CodeNotAuthorized)
		_, err = f.svc.ResumeMailboxSync(ctx, c.p, c.id, service.SyncResumeRequest{Version: f.consent().Sync})
		wantCode(t, name+" resuming", err, service.CodeNotAuthorized)
		_, err = f.svc.ResumeMailboxSync(ctx, c.p, c.id, service.SyncResumeRequest{})
		wantCode(t, name+" resuming with no version", err, service.CodeNotAuthorized)
	}
}

func TestTheOperatorPausesOnlyAnOperatorMailbox(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	own := f.ownSynced(t, tm.ana, "ana@mail.example")
	ops := f.opsSynced(t, "ops@mail.example")
	for _, id := range []string{own, tm.shared} {
		_, err := f.svc.PauseMailboxSync(ctx, admin(), id)
		wantCode(t, "the operator pausing "+id, err, service.CodeNotFound)
	}
	f.pause(t, admin(), ops)
	_, err := f.svc.ResumeMailboxSync(ctx, admin(), ops, service.SyncResumeRequest{Version: f.consent().Sync})
	wantCode(t, "the operator resuming its own with a version", err, service.CodeBadRequest)
	if st, err := f.svc.ResumeMailboxSync(ctx, admin(), ops, service.SyncResumeRequest{}); err != nil || st.Paused {
		t.Errorf("the operator resuming its own: %+v (%v)", st, err)
	}
}

func TestAPausedMailboxIsStillSearchedOpenedActedOnAndSentFrom(t *testing.T) {
	b := genericBox(t)
	ctx := t.Context()
	if _, err := b.m.svc.PauseMailboxSync(ctx, b.owner, b.id); err != nil {
		t.Fatal(err)
	}
	page, err := b.m.svc.SearchMessages(ctx, b.owner, service.SearchRequest{AccountID: b.id, Query: "Lunch"})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("searching the paused mailbox: %+v (%v)", page, err)
	}
	a, other := b.row(t, "INBOX", "a"), b.row(t, "INBOX", "b")
	if _, err := b.m.svc.GetMessage(ctx, b.owner, service.GetMessageRequest{ID: a}); err != nil {
		t.Errorf("opening a message of the paused mailbox: %v", err)
	}
	if _, err := b.m.svc.SetFlags(ctx, b.owner, service.SetFlagsRequest{IDs: []int64{a}, Seen: yes()}); err != nil {
		t.Fatalf("marking it read while paused: %v", err)
	}
	if seen := b.m.count(t, `SELECT seen FROM messages WHERE id = ?`, a); seen != 1 {
		t.Error("the index does not say it was marked read")
	}
	work := b.folder(t, "Work")
	res, err := b.m.svc.MoveMessages(ctx, b.owner, service.MoveRequest{IDs: []int64{other}, To: strconv.FormatInt(work, 10)})
	if err != nil || len(res.Messages) != 1 || res.Messages[0].FolderID != work {
		t.Fatalf("moving it while paused: %+v (%v)", res, err)
	}
	if !b.m.paused(t, b.id) {
		t.Error("acting resumed the mailbox")
	}

	// Sending from a paused mailbox, the original marked answered in the
	// index.
	s := anaSends(t)
	uid := s.box.Deliver("INBOX", providertest.FakeMessage{
		MessageID: "question@example.org", Subject: "Lunch?", From: "bea@example.org", To: []string{"ana@mail.example"},
		InternalDate: time.Now().Add(-time.Hour),
	})
	s.m.index(t, s.id, s.box)
	original := s.m.messageID(t, s.id, "INBOX", uid)
	if _, err := s.m.svc.GrantActionsConsent(ctx, s.owner, service.DefaultActionsConsentVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.svc.PauseMailboxSync(ctx, s.owner, s.id); err != nil {
		t.Fatal(err)
	}
	c := s.compose("bea@example.org")
	c.InReplyTo = original
	if sent, err := s.send(t, s.owner, "paused", c); err != nil || sent.State != service.SendStateSent {
		t.Fatalf("replying from the paused mailbox: %+v (%v)", sent, err)
	}
	msg, err := s.m.svc.GetMessage(ctx, s.owner, service.GetMessageRequest{ID: original})
	if err != nil || !msg.Answered {
		t.Errorf("the index says answered = %t (%v)", msg.Answered, err)
	}
}

func TestARecoveredMailboxIsSearchedAndActedOnBeforeItIsResumed(t *testing.T) {
	b := genericBox(t)
	ctx := t.Context()
	b.m.connect(t, b.id)
	b.m.removed(t, b.owner, b.id)
	recovered, err := b.m.svc.RecoverAccount(ctx, b.owner, b.id)
	if err != nil || !recovered.Sync.Paused {
		t.Fatalf("recovering: %+v (%v), want it paused", recovered.Sync, err)
	}
	if page, err := b.m.svc.SearchMessages(ctx, b.owner, service.SearchRequest{AccountID: b.id, Query: "Quarterly"}); err != nil ||
		len(page.Messages) != 1 {
		t.Errorf("searching the recovered mailbox: %+v (%v)", page, err)
	}
	a := b.row(t, "INBOX", "a")
	if _, err := b.m.svc.SetFlags(ctx, b.owner, service.SetFlagsRequest{IDs: []int64{a}, Flagged: yes()}); err != nil {
		t.Fatalf("starring a message of the recovered mailbox: %v", err)
	}
	if flagged := b.m.count(t, `SELECT flagged FROM messages WHERE id = ?`, a); flagged != 1 {
		t.Error("the index does not say it was starred")
	}
}

func TestARecoveryReconcilesTheMailbox(t *testing.T) {
	engine := newFakeSync()
	f := newFixtureWith(t, fixtureOptions{sync: engine})
	ctx := t.Context()
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	own := f.ownSynced(t, ana, "ana@mail.example")
	f.removed(t, ana, own)
	before := reconciledOf(engine, own)
	if _, err := f.svc.RecoverAccount(ctx, ana, own); err != nil {
		t.Fatal(err)
	}
	if n := reconciledOf(engine, own) - before; n != 1 {
		t.Errorf("the recovery told the engine %d times, want once", n)
	}
}

// reconciledOf counts the times the engine was told of an account.
func reconciledOf(s *fakeSync, id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.reconciled {
		if r == id {
			n++
		}
	}
	return n
}

func TestPausingAndResumingTellTheEngine(t *testing.T) {
	engine := newFakeSync()
	f := newFixtureWith(t, fixtureOptions{sync: engine})
	ctx := t.Context()
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	own := f.ownSynced(t, ana, "ana@mail.example")
	told := func(what string, want int) {
		t.Helper()
		before := reconciledOf(engine, own)
		var err error
		switch what {
		case "pause":
			_, err = f.svc.PauseMailboxSync(ctx, ana, own)
		case "resume":
			_, err = f.svc.ResumeMailboxSync(ctx, ana, own, service.SyncResumeRequest{})
		}
		if err != nil {
			t.Fatal(err)
		}
		if n := reconciledOf(engine, own) - before; n != want {
			t.Errorf("a %s told the engine %d times, want %d", what, n, want)
		}
	}
	told("pause", 1)
	told("pause", 0)
	told("resume", 1)
	told("resume", 0)
}

func TestSyncNowIsRefusedOnAPausedMailbox(t *testing.T) {
	engine := newFakeSync()
	f := newFixtureWith(t, fixtureOptions{sync: engine})
	ctx := t.Context()
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	own := f.ownSynced(t, ana, "ana@mail.example")
	engine.running(own, service.SyncStatus{Running: true, State: "live"})
	if err := f.svc.TriggerSync(ctx, ana, own); err != nil {
		t.Fatalf("sync now before the pause: %v", err)
	}
	f.pause(t, ana, own)
	err := f.svc.TriggerSync(ctx, ana, own)
	wantCode(t, "sync now while paused", err, service.CodeConflict)
	if !strings.Contains(service.MessageOf(err), "paused") {
		t.Errorf("sync now while paused says %q", service.MessageOf(err))
	}
}

func TestAHeldOrSwitchedOffMailboxHasNothingToPause(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	held := f.ownSynced(t, tm.ana, "ana@mail.example")
	if _, err := f.svc.StopMailboxSync(ctx, admin(), held); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.PauseMailboxSync(ctx, tm.ana, held)
	wantCode(t, "pausing a mailbox the operator turned off", err, service.CodeConflict)
	if !strings.Contains(service.MessageOf(err), "held") {
		t.Errorf("the refusal says %q", service.MessageOf(err))
	}
	off := false
	if _, err := f.svc.SetMailboxSync(ctx, tm.ana, tm.shared, service.MailboxSyncRequest{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.PauseMailboxSync(ctx, tm.ana, tm.shared)
	wantCode(t, "pausing a team mailbox switched off", err, service.CodeConflict)
	bea := f.ownSynced(t, tm.bea, "bea@mail.example")
	if _, err := f.svc.WithdrawSyncConsent(ctx, tm.bea); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.PauseMailboxSync(ctx, tm.bea, bea)
	wantCode(t, "pausing a mailbox whose person's sync is off", err, service.CodeConflict)
	for _, id := range []string{held, tm.shared, bea} {
		if f.paused(t, id) {
			t.Errorf("%s is paused", id)
		}
	}
}

func TestTurningAPausedPersonalMailboxOnLeavesItPaused(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	own := f.ownSynced(t, ana, "ana@mail.example")
	f.pause(t, ana, own)
	on := true
	st, err := f.svc.SetMailboxSync(t.Context(), ana, own, service.MailboxSyncRequest{Enabled: &on})
	if err != nil || !st.Paused || st.Held {
		t.Errorf("Ana turning her paused mailbox on: %+v (%v), want it still paused", st, err)
	}
}

func TestAClosedTeamsMailboxIsNeitherPausedNorResumed(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	f.pause(t, tm.ana, tm.shared)
	f.closeTeam(t, tm.id)
	_, err := f.svc.PauseMailboxSync(ctx, tm.ana, tm.shared)
	wantCode(t, "pausing it in the closed team", err, service.CodeNotFound)
	_, err = f.svc.ResumeMailboxSync(ctx, tm.ana, tm.shared, service.SyncResumeRequest{Version: f.consent().Sync})
	wantCode(t, "resuming it in the closed team", err, service.CodeNotFound)
	f.reopenTeam(t, tm.id)
	if !f.paused(t, tm.shared) {
		t.Fatal("the team's closure ended the pause")
	}
	if _, err := f.svc.ResumeMailboxSync(ctx, tm.ana, tm.shared, service.SyncResumeRequest{Version: f.consent().Sync}); err != nil {
		t.Errorf("resuming it once reopened: %v", err)
	}
}

func TestAWithheldTeamsMailboxIsNeitherPausedNorResumed(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	f.pause(t, tm.ana, tm.shared)
	f.withholdTeams(t)
	_, err := f.svc.PauseMailboxSync(ctx, tm.ana, tm.shared)
	wantWithheld(t, "pausing it where teams are withheld", err)
	_, err = f.svc.ResumeMailboxSync(ctx, tm.ana, tm.shared, service.SyncResumeRequest{Version: f.consent().Sync})
	wantWithheld(t, "resuming it where teams are withheld", err)
	if !f.paused(t, tm.shared) {
		t.Error("withholding teams ended the pause")
	}
}

func TestARemovedMailboxIsNeitherPausedNorResumed(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	own := f.ownSynced(t, ana, "ana@mail.example")
	f.removed(t, ana, own)
	_, err := f.svc.PauseMailboxSync(ctx, ana, own)
	wantCode(t, "pausing a removed mailbox", err, service.CodeNotFound)
	_, err = f.svc.ResumeMailboxSync(ctx, ana, own, service.SyncResumeRequest{})
	wantCode(t, "resuming a removed mailbox", err, service.CodeNotFound)
}

func TestATeamMailboxsPauseAndResumeAskTheFreshnessHookAsItsSyncSwitched(t *testing.T) {
	hook := &freshHook{}
	f := newFixtureWith(t, fixtureOptions{freshness: hook})
	ctx := t.Context()
	tm := syncedTeam(t, f)
	own := f.ownSynced(t, tm.ana, "ana@mail.example")
	ops := f.opsSynced(t, "ops@mail.example")
	hook.take()
	want := service.FreshAct{Kind: service.FreshTeamSync, ActorID: tm.ana.UserID, WorkspaceID: tm.id, AccountID: tm.shared}
	f.pause(t, tm.ana, tm.shared)
	if asked := hook.take(); len(asked) != 1 || !sameAct(asked[0], want) {
		t.Errorf("the team pause asked %+v, want %+v", asked, want)
	}
	f.pause(t, tm.ana, tm.shared)
	if asked := hook.take(); len(asked) != 0 {
		t.Errorf("pausing a paused mailbox asked %+v", asked)
	}
	// A refusal stops the resume, which writes nothing.
	hook.answerWith(func(context.Context, service.FreshAct) error { return errors.New("the directory is away") })
	_, err := f.svc.ResumeMailboxSync(ctx, tm.ana, tm.shared, service.SyncResumeRequest{Version: f.consent().Sync})
	if err == nil || !f.paused(t, tm.shared) {
		t.Errorf("a refused freshness: %v, paused %v", err, f.paused(t, tm.shared))
	}
	if n := f.count(t, `SELECT count(*) FROM mailbox_events WHERE kind = 'resumed'`); n != 0 {
		t.Error("the refused resume was journaled")
	}
	hook.answerWith(nil)
	hook.take()
	if _, err := f.svc.ResumeMailboxSync(ctx, tm.ana, tm.shared, service.SyncResumeRequest{Version: f.consent().Sync}); err != nil {
		t.Fatal(err)
	}
	if asked := hook.take(); len(asked) != 1 || !sameAct(asked[0], want) {
		t.Errorf("the team resume asked %+v, want %+v", asked, want)
	}
	// A personal and an operator mailbox ask nothing, nor does a recovery.
	f.pause(t, tm.ana, own)
	f.pause(t, admin(), ops)
	for p, id := range map[*service.Principal]string{&tm.ana: own, new(service.Principal): ops} {
		who := *p
		if who.UserID == "" {
			who = admin()
		}
		if _, err := f.svc.ResumeMailboxSync(ctx, who, id, service.SyncResumeRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	f.removed(t, tm.ana, own)
	if _, err := f.svc.RecoverAccount(ctx, tm.ana, own); err != nil {
		t.Fatal(err)
	}
	if asked := hook.take(); len(asked) != 0 {
		t.Errorf("a personal and an operator pause and resume, and a recovery, asked %+v", asked)
	}
}

func TestTheAccountSaysItIsPausedAndSinceWhenWithItsConsentStillStanding(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	own := f.ownSynced(t, ana, "ana@mail.example")
	held := f.ownSynced(t, ana, "ana@other.example")
	f.pause(t, ana, own)
	a, err := f.svc.GetAccount(ctx, ana, own)
	if err != nil || !a.Sync.Paused || a.Sync.PausedSince == 0 || !a.Sync.Enabled || a.Sync.Held {
		t.Errorf("the paused account: %+v (%v)", a.Sync, err)
	}
	if _, err := f.svc.StopMailboxSync(ctx, admin(), held); err != nil {
		t.Fatal(err)
	}
	if a, err := f.svc.GetAccount(ctx, ana, held); err != nil || !a.Sync.Held || a.Sync.Paused || a.Sync.Enabled {
		t.Errorf("the account the operator turned off: %+v (%v)", a.Sync, err)
	}
}

func TestAPauseNoticeTellsTheOtherReadersAsLiveAndAResumeWithdrawsIt(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	f.pause(t, tm.ana, tm.shared)
	got := f.notices(t, tm.bea)
	if len(got) != 1 || got[0].Kind != "paused" || got[0].State != "live" || got[0].Mailbox != "support@mail.example" ||
		got[0].AccountID != tm.shared || got[0].WorkspaceName != "Support" {
		t.Errorf("Bea's notices: %+v", got)
	}
	if got := f.notices(t, tm.ana); len(got) != 0 {
		t.Errorf("Ana is told of her own pause: %+v", got)
	}
	if _, err := f.svc.ResumeMailboxSync(ctx, tm.carol, tm.shared, service.SyncResumeRequest{Version: f.consent().Sync}); err != nil {
		t.Fatal(err)
	}
	if got := f.notices(t, tm.bea); len(got) != 0 {
		t.Errorf("Bea's notices once resumed: %+v", got)
	}
}

func TestAnEditionThatDoesNotOfferThePauseRefusesItAndNeverARecoveryOrAResume(t *testing.T) {
	f := newFixtureWith(t, fixtureOptions{pauseWithheld: true})
	ctx := t.Context()
	tm := syncedTeam(t, f)
	own := f.ownSynced(t, tm.ana, "ana@mail.example")
	ops := f.opsSynced(t, "ops@mail.example")
	for _, c := range []struct {
		p  service.Principal
		id string
	}{{tm.ana, own}, {tm.ana, tm.shared}, {admin(), ops}} {
		_, err := f.svc.PauseMailboxSync(ctx, c.p, c.id)
		wantCode(t, "pausing "+c.id, err, service.CodeNotAuthorized)
		if !strings.Contains(service.MessageOf(err), "not offered") {
			t.Errorf("pausing %s is refused as %q", c.id, service.MessageOf(err))
		}
	}
	for _, c := range []struct {
		p       service.Principal
		id      string
		version string
	}{{tm.ana, own, ""}, {tm.ana, tm.shared, f.consent().Sync}, {admin(), ops, ""}} {
		f.removed(t, c.p, c.id)
		recovered, err := f.svc.RecoverAccount(ctx, c.p, c.id)
		if err != nil || !recovered.Sync.Paused {
			t.Fatalf("recovering %s: %+v (%v), want it paused", c.id, recovered.Sync, err)
		}
		if st, err := f.svc.ResumeMailboxSync(ctx, c.p, c.id, service.SyncResumeRequest{Version: c.version}); err != nil || st.Paused {
			t.Errorf("resuming %s: %+v (%v)", c.id, st, err)
		}
	}
}

func TestAFolderBehindTheServerOnAPausedMailboxSaysResumeSyncToCatchUp(t *testing.T) {
	b := genericBox(t)
	ctx := t.Context()
	a, other := b.row(t, "INBOX", "a"), b.row(t, "INBOX", "b")
	b.m.exec(t, `UPDATE messages SET stale = 1 WHERE id = ?`, a)
	b.box.ChangeUIDValidity("INBOX")
	busy := func(what, want string) {
		t.Helper()
		_, err := b.m.svc.GetMessage(ctx, b.owner, service.GetMessageRequest{ID: a})
		checks := map[string]error{"opening a stale row": err}
		_, err = b.m.svc.SetFlags(ctx, b.owner, service.SetFlagsRequest{IDs: []int64{a}, Seen: yes()})
		checks["acting on a stale row"] = err
		_, err = b.m.svc.GetMessage(ctx, b.owner, service.GetMessageRequest{ID: other})
		checks["opening a message of a renumbered folder"] = err
		_, err = b.m.svc.SetFlags(ctx, b.owner, service.SetFlagsRequest{IDs: []int64{other}, Seen: yes()})
		checks["acting on a message of a renumbered folder"] = err
		for name, err := range checks {
			wantCode(t, what+", "+name, err, service.CodeConflict)
			if !strings.Contains(service.MessageOf(err), want) {
				t.Errorf("%s, %s says %q, want %q", what, name, service.MessageOf(err), want)
			}
		}
	}
	if _, err := b.m.svc.PauseMailboxSync(ctx, b.owner, b.id); err != nil {
		t.Fatal(err)
	}
	busy("paused", "resume sync to catch up")
	if _, err := b.m.svc.ResumeMailboxSync(ctx, b.owner, b.id, service.SyncResumeRequest{}); err != nil {
		t.Fatal(err)
	}
	busy("syncing", "try again shortly")
}

func TestEveryWayOfEndingAConsentEndsAPause(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	own := f.ownSynced(t, tm.ana, "ana@mail.example")
	ops := f.opsSynced(t, "ops@mail.example")
	ended := func(what, id string) {
		t.Helper()
		if f.paused(t, id) {
			t.Errorf("%s left %s paused", what, id)
		}
	}
	f.pause(t, tm.ana, own)
	if _, err := f.svc.WithdrawSyncConsent(ctx, tm.ana); err != nil {
		t.Fatal(err)
	}
	ended("Ana's withdrawal", own)
	f.pause(t, tm.ana, tm.shared)
	off := false
	if _, err := f.svc.SetMailboxSync(ctx, tm.ana, tm.shared, service.MailboxSyncRequest{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	ended("the team's turn-off", tm.shared)
	f.pause(t, admin(), ops)
	if _, err := f.svc.StopMailboxSync(ctx, admin(), ops); err != nil {
		t.Fatal(err)
	}
	ended("the operator's turn-off", ops)
	// A team consent migration 0011 bound to Bea, who is disabled.
	tm.syncOn(t, f, tm.shared)
	f.exec(t, `UPDATE accounts SET sync_enabled_via = 'migration', sync_enabled_by = ? WHERE id = ?`,
		tm.bea.UserID, tm.shared)
	f.pause(t, tm.ana, tm.shared)
	if _, err := f.svc.DisableUser(ctx, admin(), service.CloseUserRequest{Email: "bea@example.com", Force: true}); err != nil {
		t.Fatal(err)
	}
	ended("disabling the person a bound consent is tied to", tm.shared)
}

func TestAPauseChangesNoProtection(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	f.exec(t, `DELETE FROM mailbox_access WHERE account_id = ? AND user_id = ?`, tm.shared, tm.ana.UserID)
	f.pause(t, tm.ana, tm.shared)
	_, err := f.svc.DisableUser(ctx, admin(), service.CloseUserRequest{Email: "bea@example.com"})
	wantCode(t, "disabling Bea, the last reader of the paused mailbox", err, service.CodeConflict)
	if _, err := f.svc.DisableUser(ctx, admin(), service.CloseUserRequest{Email: "bea@example.com", Force: true}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM mailbox_events WHERE kind = 'stranded' AND account_id = ?`, tm.shared); n != 1 {
		t.Errorf("%d strandings journaled of the paused mailbox, want one", n)
	}
}

func TestEveryCauseThePauseTableAnswersIsASnapshotPause(t *testing.T) {
	answered := map[string]bool{}
	for _, m := range regexp.MustCompile(`THEN '([a-z_]+)'`).FindAllStringSubmatch(store.PauseSQL(true), -1) {
		if m[1] != store.PauseRemoved {
			answered[m[1]] = true
		}
	}
	var known []string
	for _, p := range service.SnapshotPauses() {
		known = append(known, string(p))
	}
	var want []string
	for cause := range answered {
		want = append(want, cause)
	}
	slices.Sort(want)
	slices.Sort(known)
	if !slices.Equal(known, want) || len(known) != 10 {
		t.Errorf("the snapshot's causes are %v, the pause table answers %v", known, want)
	}
}

// queryStrings reads one text column of every row a query returns.
func queryStrings(t *testing.T, f *fixture, query string, args ...any) []string {
	t.Helper()
	rows, err := f.db.Reader().QueryContext(t.Context(), query, args...)
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
