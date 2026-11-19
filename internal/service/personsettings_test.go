package service_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// A person's own settings as an extension reports them, and which people
// changed (settings.go; migration 0027's marks), on the directoryTeam of
// settings_test.go.

// personMark is a person's mark, 0 when they have none.
func (d directoryTeam) personMark(t *testing.T, userID string) int {
	t.Helper()
	return d.count(t, `SELECT coalesce((SELECT seq FROM person_changes WHERE user_id = ?), 0)`, userID)
}

// noticesOf is a person's notices as their settings report them.
func (d directoryTeam) noticesOf(t *testing.T, userID string) []service.PersonNoticeState {
	t.Helper()
	return d.personSettings(t, userID).Notices
}

func TestAConsentToAnEarlierTextIsOldTextForActionsAndSendAndStaysOnForSync(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	ctx := t.Context()
	texts := d.consent()
	if _, err := d.svc.GrantSyncConsent(ctx, d.ana, texts.Sync); err != nil {
		t.Fatal(err)
	}
	if _, err := d.svc.GrantActionsConsent(ctx, d.ana, texts.Actions); err != nil {
		t.Fatal(err)
	}
	if _, err := d.svc.GrantSendConsent(ctx, d.ana, texts.Send); err != nil {
		t.Fatal(err)
	}
	s := d.personSettings(t, d.ana.UserID)
	if s.Texts != (service.ConsentTexts{Sync: texts.Sync, Actions: texts.Actions, Send: texts.Send, Keys: texts.Keys}) ||
		s.Texts != d.svc.ConsentTexts() {
		t.Errorf("the texts accepted: %+v", s.Texts)
	}
	for name, c := range map[string]service.ConsentState{"sync": s.Sync, "actions": s.Actions, "send": s.Send} {
		if c.At.IsZero() || !c.Current || c.Revision == "" || !c.WithdrawnAt.IsZero() {
			t.Errorf("%s agreed to the current text: %+v", name, c)
		}
	}
	// The texts change: each consent was given to an earlier one.
	d.exec(t, `UPDATE users SET sync_consent_version = 'sync-0', actions_consent_version = 'actions-0',
		send_consent_version = 'send-0' WHERE id = ?`, d.ana.UserID)
	s = d.personSettings(t, d.ana.UserID)
	for name, c := range map[string]service.ConsentState{"sync": s.Sync, "actions": s.Actions, "send": s.Send} {
		if c.At.IsZero() || c.Current || c.Revision != name+"-0" {
			t.Errorf("%s agreed to an earlier text: %+v", name, c)
		}
	}
	// Bea never answered: every consent off.
	b := d.personSettings(t, d.bea.UserID)
	for name, c := range map[string]service.ConsentState{"sync": b.Sync, "actions": b.Actions, "send": b.Send} {
		if c != (service.ConsentState{}) {
			t.Errorf("Bea's %s: %+v", name, c)
		}
	}
	if !b.Enrolled {
		t.Error("Bea, who has an account key, is not enrolled")
	}
}

func TestAWithdrawalIsReportedOnlyForActionsAndSend(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	ctx := t.Context()
	texts := d.consent()
	for _, grant := range []func() error{
		func() error { _, err := d.svc.GrantSyncConsent(ctx, d.ana, texts.Sync); return err },
		func() error { _, err := d.svc.GrantActionsConsent(ctx, d.ana, texts.Actions); return err },
		func() error { _, err := d.svc.GrantSendConsent(ctx, d.ana, texts.Send); return err },
		func() error { _, err := d.svc.WithdrawSyncConsent(ctx, d.ana); return err },
		func() error { _, err := d.svc.WithdrawActionsConsent(ctx, d.ana); return err },
		func() error { _, err := d.svc.WithdrawSendConsent(ctx, d.ana); return err },
	} {
		if err := grant(); err != nil {
			t.Fatal(err)
		}
	}
	s := d.personSettings(t, d.ana.UserID)
	if s.Sync != (service.ConsentState{}) {
		t.Errorf("sync withdrawn: %+v, want off with no moment", s.Sync)
	}
	for name, c := range map[string]service.ConsentState{"actions": s.Actions, "send": s.Send} {
		if !c.At.IsZero() || c.Revision != "" || c.Current || c.WithdrawnAt.IsZero() {
			t.Errorf("%s withdrawn: %+v", name, c)
		}
	}
	// Given again, the withdrawal goes.
	if _, err := d.svc.GrantActionsConsent(ctx, d.ana, texts.Actions); err != nil {
		t.Fatal(err)
	}
	if c := d.personSettings(t, d.ana.UserID).Actions; !c.WithdrawnAt.IsZero() || c.At.IsZero() {
		t.Errorf("actions given again: %+v", c)
	}
}

func TestAPersonsSettingsSayWhenTheyLastSignedOutEverywhere(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	if s := d.personSettings(t, d.ana.UserID); !s.SignedOutEverywhereAt.IsZero() || s.MeasuredAt.IsZero() {
		t.Fatalf("Ana never signed out everywhere: %v (measured %v)", s.SignedOutEverywhereAt, s.MeasuredAt)
	}
	if err := d.svc.SignOut(t.Context(), d.ana, service.SignOutRequest{}); err != nil {
		t.Fatal(err)
	}
	if s := d.personSettings(t, d.ana.UserID); !s.SignedOutEverywhereAt.IsZero() {
		t.Errorf("signing out of one session recorded a sign-out everywhere at %v", s.SignedOutEverywhereAt)
	}
	ana := d.signInAgain(t, "ana@example.com")
	if err := d.svc.SignOut(t.Context(), ana, service.SignOutRequest{Everywhere: true}); err != nil {
		t.Fatal(err)
	}
	want := d.at(t, `SELECT signed_out_everywhere_at FROM users WHERE id = ?`, d.ana.UserID)
	if s := d.personSettings(t, d.ana.UserID); want.IsZero() || !s.SignedOutEverywhereAt.Equal(want) {
		t.Errorf("signed out everywhere at %v, the row says %v", s.SignedOutEverywhereAt, want)
	}
}

func TestAPersonsNoticesComeFromTheJournalsLabelAndHintNeverFromTheAddress(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	ctx := t.Context()
	d.removed(t, d.ana, d.box)
	// The console's own list names a kept mailbox by its address.
	listed, err := d.svc.Notices(ctx, d.bea)
	if err != nil || len(listed) != 1 || listed[0].Mailbox != "support@team.example.com" {
		t.Fatalf("Bea's notices in the console: %+v (%v)", listed, err)
	}
	s := d.personSettings(t, d.bea.UserID)
	if len(s.Notices) != 1 {
		t.Fatalf("Bea's notices: %+v", s.Notices)
	}
	n := s.Notices[0]
	if n.ID != listed[0].ID || n.Kind != "removed" || n.Label != "Help desk" || n.Hint != "s…@team.example.com" ||
		n.WorkspaceID != dirTeamID || n.AccountID != d.box || !n.Team || n.At.IsZero() {
		t.Errorf("Bea's notice: %+v", n)
	}
	for _, v := range stringsOf(reflect.ValueOf(s)) {
		if strings.Contains(v, "support@") || strings.Contains(v, "support") {
			t.Errorf("a person's settings carry %q", v)
		}
	}
	// A name that holds the address is no label.
	if _, err := d.svc.RecoverAccount(ctx, d.ana, d.box); err != nil {
		t.Fatal(err)
	}
	d.exec(t, `UPDATE accounts SET display_name = 'Support desk' WHERE id = ?`, d.box)
	d.removed(t, d.ana, d.box)
	if n := d.noticesOf(t, d.bea.UserID); len(n) != 1 || n[0].Label != "" || n[0].Hint != "s…@team.example.com" {
		t.Errorf("a notice of a mailbox whose name holds its address: %+v", n)
	}
}

// stringsOf lists every string a value holds, its fields' and its elements'.
func stringsOf(v reflect.Value) []string {
	switch v.Kind() {
	case reflect.String:
		return []string{v.String()}
	case reflect.Struct:
		var out []string
		for i := range v.NumField() {
			out = append(out, stringsOf(v.Field(i))...)
		}
		return out
	case reflect.Slice:
		var out []string
		for i := range v.Len() {
			out = append(out, stringsOf(v.Index(i))...)
		}
		return out
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return stringsOf(v.Elem())
	}
	return nil
}

func TestANoticesStateFollowsItsMailboxFromLiveToRemovedToDeleted(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	ctx := t.Context()
	d.turnSync(t, d.ana, d.box, true)
	d.turnSync(t, d.ana, d.box, false)
	off := func() service.PersonNoticeState {
		t.Helper()
		for _, n := range d.noticesOf(t, d.bea.UserID) {
			if n.Kind == "sync_paused" {
				return n
			}
		}
		t.Fatal("Bea holds no notice of the turn-off")
		return service.PersonNoticeState{}
	}
	if n := off(); n.State != service.NoticeLive || !n.RecoverableUntil.IsZero() || n.Label != "Help desk" {
		t.Errorf("the turn-off of a live mailbox: %+v", n)
	}
	removal := d.removed(t, d.ana, d.box)
	if n := off(); n.State != service.NoticeRemoved || n.RecoverableUntil.Unix() != removal.PurgeAfter {
		t.Errorf("the turn-off of a removed mailbox: %+v, want removed until %d", n, removal.PurgeAfter)
	}
	if err := d.svc.PurgeAccount(ctx, d.ana, d.box, service.PurgeAccountRequest{Confirm: d.box}); err != nil {
		t.Fatal(err)
	}
	if n := off(); n.State != service.NoticeDeleted || !n.RecoverableUntil.IsZero() || n.Label != "" ||
		n.Hint != "s…@team.example.com" {
		t.Errorf("the turn-off of a mailbox deleted for good: %+v", n)
	}
}

func TestARemovalNoticeOfAMailboxDeletedForGoodSaysWhenItWasDeleted(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	removal := d.removed(t, d.ana, d.box)
	n := d.noticesOf(t, d.bea.UserID)
	if len(n) != 1 || n[0].State != service.NoticeRemoved || n[0].RecoverableUntil.Unix() != removal.PurgeAfter {
		t.Fatalf("the removal: %+v, want removed until %d", n, removal.PurgeAfter)
	}
	if err := d.svc.PurgeAccount(t.Context(), d.ana, d.box, service.PurgeAccountRequest{Confirm: d.box}); err != nil {
		t.Fatal(err)
	}
	purged := d.at(t, `SELECT created_at FROM mailbox_events WHERE account_id = ? AND kind = 'purged'`, d.box)
	n = d.noticesOf(t, d.bea.UserID)
	if len(n) != 1 || n[0].State != service.NoticeDeleted || !n[0].RecoverableUntil.Equal(purged) {
		t.Errorf("the removal of a mailbox deleted for good: %+v, want deleted at %v", n, purged)
	}
	// Once the journal no longer holds when, the end the removal recorded.
	d.exec(t, `DELETE FROM mailbox_events WHERE account_id = ? AND kind = 'purged'`, d.box)
	if n = d.noticesOf(t, d.bea.UserID); len(n) != 1 || n[0].RecoverableUntil.Unix() != removal.PurgeAfter {
		t.Errorf("the removal without its deletion's entry: %+v", n)
	}
}

func TestANoticesActorIsKnownOnlyWhileTheyAreAMemberOfItsWorkspace(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	carol := d.followed(t, "carol@example.com")
	d.apply(t, document("carol@example.com", 1, inTeam(workspace.RoleAdmin, 1)))
	d.turnSync(t, carol, d.box, true)
	d.pause(t, carol, d.box)
	n := d.noticesOf(t, d.bea.UserID)
	if len(n) != 1 || n[0].Kind != "paused" || n[0].Actor != carol.UserID || n[0].ActorUserID != carol.UserID ||
		n[0].Cause != "" || !n[0].Team {
		t.Fatalf("Bea's notice of Carol's pause: %+v", n)
	}
	// The directory ends Carol's membership: the notice no longer knows
	// her, though its entry still names her.
	d.apply(t, document("carol@example.com", 2))
	n = d.noticesOf(t, d.bea.UserID)
	if len(n) != 1 || n[0].Actor != carol.UserID || n[0].ActorUserID != "" {
		t.Errorf("Bea's notice once Carol left: %+v", n)
	}
	d.apply(t, document("carol@example.com", 3, inTeam(workspace.RoleAdmin, 1)))
	if n = d.noticesOf(t, d.bea.UserID); len(n) != 1 || n[0].ActorUserID != carol.UserID {
		t.Errorf("Bea's notice once Carol is back: %+v", n)
	}
}

func TestOnlyAnOwnerOrAnAdminIsTheActorOfATeamMailboxsNoticeThatNamesOne(t *testing.T) {
	// Every way a person pauses or turns off a team mailbox's sync,
	// journaling an entry with no cause: a member is refused, even one who
	// manages the mailbox, and an owner and an admin act.
	d := newDirectoryTeam(t, fixtureOptions{})
	ctx := t.Context()
	if _, err := workspace.NewRepository(d.db, nil).SetGrant(ctx, d.box, d.bea.UserID,
		workspace.Flags{Read: true, Manage: true}, "usr_test", nil); err != nil {
		t.Fatal(err)
	}
	d.turnSync(t, d.ana, d.box, true)
	off := false
	for what, act := range map[string]func() error{
		"turning it off": func() error {
			_, err := d.svc.SetMailboxSync(ctx, d.bea, d.box, service.MailboxSyncRequest{Enabled: &off})
			return err
		},
		"pausing it": func() error { _, err := d.svc.PauseMailboxSync(ctx, d.bea, d.box); return err },
	} {
		if err := act(); service.CodeOf(err) != service.CodeNotAuthorized {
			t.Errorf("a member %s: %v, want not_authorized", what, err)
		}
	}
	d.removed(t, d.ana, d.box)
	if _, err := d.svc.SetMailboxSync(ctx, d.bea, d.box, service.MailboxSyncRequest{Enabled: &off}); err == nil {
		t.Error("a member turned a removed team mailbox's sync off")
	}
	if n := d.count(t, `SELECT count(*) FROM mailbox_events WHERE kind IN ('paused', 'sync_paused') AND actor = ?`,
		d.bea.UserID); n != 0 {
		t.Errorf("%d entries name a member as who paused or turned off", n)
	}
}

func TestAPersonsNoticesAreTheNewestFirstAndSayWhenMoreWereLeft(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	ctx := t.Context()
	for range 3 {
		d.turnSync(t, d.ana, d.box, true)
		d.turnSync(t, d.ana, d.box, false)
	}
	all := d.noticesOf(t, d.bea.UserID)
	if len(all) != 3 {
		t.Fatalf("Bea holds %d notices, want three", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].ID == all[i].ID || all[i-1].At.Before(all[i].At) {
			t.Errorf("notice %d is not newer than notice %d", i-1, i)
		}
	}
	two, err := d.svc.PersonSettings(ctx, d.bea.UserID, 2)
	if err != nil || len(two.Notices) != 2 || !two.MoreNotices || two.Notices[0].ID != all[0].ID ||
		two.Notices[1].ID != all[1].ID {
		t.Errorf("the newest two: %+v, more %v (%v)", two.Notices, two.MoreNotices, err)
	}
	if three, err := d.svc.PersonSettings(ctx, d.bea.UserID, 3); err != nil || three.MoreNotices {
		t.Errorf("all three: more %v (%v)", three.MoreNotices, err)
	}
	for _, limit := range []int{0, service.MaxPersonNotices + 1} {
		if _, err := d.svc.PersonSettings(ctx, d.bea.UserID, limit); service.CodeOf(err) != service.CodeBadRequest {
			t.Errorf("a read of %d notices: %v, want bad_request", limit, err)
		}
	}
	if _, err := d.svc.PersonSettings(ctx, "usr_nobodyknows", 10); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("the settings of nobody: %v, want not_found", err)
	}
}

func TestAPersonIsMarkedWhenAConsentTheirKeyOrTheirSignOutEverywhereChanges(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	ctx := t.Context()
	texts := d.consent()
	ana := d.ana
	for _, step := range []struct {
		what string
		act  func() error
	}{
		{"a consent to sync", func() error { _, err := d.svc.GrantSyncConsent(ctx, ana, texts.Sync); return err }},
		{"a consent to actions", func() error { _, err := d.svc.GrantActionsConsent(ctx, ana, texts.Actions); return err }},
		{"a consent to sending", func() error { _, err := d.svc.GrantSendConsent(ctx, ana, texts.Send); return err }},
		{"actions withdrawn", func() error { _, err := d.svc.WithdrawActionsConsent(ctx, ana); return err }},
		{"sending withdrawn", func() error { _, err := d.svc.WithdrawSendConsent(ctx, ana); return err }},
		{"sync withdrawn", func() error { _, err := d.svc.WithdrawSyncConsent(ctx, ana); return err }},
		{"a sign-out everywhere", func() error {
			return d.svc.SignOut(ctx, ana, service.SignOutRequest{Everywhere: true})
		}},
		{"a new account key", func() error { d.resetKey(t, ana.UserID, "ana@example.com"); return nil }},
	} {
		before, bea := d.personMark(t, ana.UserID), d.personMark(t, d.bea.UserID)
		if err := step.act(); err != nil {
			t.Fatalf("%s: %v", step.what, err)
		}
		if d.personMark(t, ana.UserID) <= before || d.personMark(t, d.bea.UserID) != bea {
			t.Errorf("%s moved Ana's mark from %d to %d and Bea's from %d to %d", step.what, before,
				d.personMark(t, ana.UserID), bea, d.personMark(t, d.bea.UserID))
		}
		if step.what == "a sign-out everywhere" {
			ana = d.signInAgain(t, "ana@example.com")
		}
	}
}

func TestEveryHolderOfANoticeIsMarkedWhenItsMailboxIsRemovedRecoveredOrDeletedForGood(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	ctx := t.Context()
	carol := d.followed(t, "carol@example.com")
	d.apply(t, document("carol@example.com", 1, inTeam(workspace.RoleMember, 1)))
	d.giveRead(t, d.box, carol.UserID)
	d.turnSync(t, d.ana, d.box, true)
	d.turnSync(t, d.ana, d.box, false)
	for _, step := range []struct {
		what string
		act  func() error
	}{
		{"its removal", func() error {
			_, err := d.svc.RemoveAccount(ctx, d.ana, d.box, service.RemoveAccountRequest{Confirm: d.box})
			return err
		}},
		{"its recovery", func() error { _, err := d.svc.RecoverAccount(ctx, d.ana, d.box); return err }},
		{"its removal again", func() error {
			_, err := d.svc.RemoveAccount(ctx, d.ana, d.box, service.RemoveAccountRequest{Confirm: d.box})
			return err
		}},
		{"its deletion for good", func() error {
			return d.svc.PurgeAccount(ctx, d.ana, d.box, service.PurgeAccountRequest{Confirm: d.box})
		}},
	} {
		bea, carolMark := d.personMark(t, d.bea.UserID), d.personMark(t, carol.UserID)
		if err := step.act(); err != nil {
			t.Fatalf("%s: %v", step.what, err)
		}
		if d.personMark(t, d.bea.UserID) <= bea || d.personMark(t, carol.UserID) <= carolMark {
			t.Errorf("%s left Bea's mark at %d (was %d) or Carol's at %d (was %d)", step.what,
				d.personMark(t, d.bea.UserID), bea, d.personMark(t, carol.UserID), carolMark)
		}
	}
}

func TestAPersonHoldingSeveralNoticesOfOneMailboxOrOneActorIsMarkedOnceAndEveryWriteSucceeds(t *testing.T) {
	// Bea holds two notices of the desk by Ana, then its removal's; every
	// write that marks the holders of a set of notices marks her once, and
	// commits.
	d := newDirectoryTeam(t, fixtureOptions{})
	ctx := t.Context()
	for range 2 {
		d.turnSync(t, d.ana, d.box, true)
		d.turnSync(t, d.ana, d.box, false)
	}
	steps := []struct {
		what string
		act  func() error
	}{
		{"the desk removed", func() error {
			_, err := d.svc.RemoveAccount(ctx, d.ana, d.box, service.RemoveAccountRequest{Confirm: d.box})
			return err
		}},
		{"the desk recovered", func() error { _, err := d.svc.RecoverAccount(ctx, d.ana, d.box); return err }},
		{"the desk removed again", func() error {
			_, err := d.svc.RemoveAccount(ctx, d.ana, d.box, service.RemoveAccountRequest{Confirm: d.box})
			return err
		}},
		{"the desk deleted for good", func() error {
			return d.svc.PurgeAccount(ctx, d.ana, d.box, service.PurgeAccountRequest{Confirm: d.box})
		}},
		// The directory ends Ana's membership, then gives it back.
		{"Ana's membership ended", func() error {
			_, err := d.svc.ApplyDirectoryDocument(ctx, document("ana@example.com", 2),
				service.ApplyOptions{CredentialsMargin: dirMargin})
			return err
		}},
		{"Ana's membership given back", func() error {
			_, err := d.svc.ApplyDirectoryDocument(ctx, document("ana@example.com", 3, inTeam(workspace.RoleOwner, 1)),
				service.ApplyOptions{CredentialsMargin: dirMargin})
			return err
		}},
	}
	if n := d.count(t, `SELECT count(*) FROM notices WHERE user_id = ?`, d.bea.UserID); n != 2 {
		t.Fatalf("Bea holds %d notices, want two by Ana", n)
	}
	for _, step := range steps {
		before := d.personMark(t, d.bea.UserID)
		if err := step.act(); err != nil {
			t.Fatalf("%s: %v", step.what, err)
		}
		if d.personMark(t, d.bea.UserID) <= before {
			t.Errorf("%s left Bea's mark at %d", step.what, before)
		}
		if n := d.count(t, `SELECT count(*) FROM person_changes WHERE user_id = ?`, d.bea.UserID); n != 1 {
			t.Errorf("%s: Bea has %d marks", step.what, n)
		}
	}
	if n := d.count(t, `SELECT count(*) FROM workspace_members WHERE workspace_id = ? AND user_id = ?`, dirTeamID,
		d.ana.UserID); n != 1 {
		t.Errorf("Ana is a member %d times", n)
	}
}

func TestAFirstSignInThroughAnIdentityMarksTheNewPerson(t *testing.T) {
	f := newDirectoryFixture(t)
	ctx := t.Context()
	_, head, err := f.svc.PersonChanges(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	doc := document("cy@example.com", 1)
	if _, err := f.signInWith(t, ctx, "cy@example.com", &doc); err != nil {
		t.Fatal(err)
	}
	cy, ok, err := f.svc.IdentityUser(ctx, dirIssuer, subjectOf("cy@example.com"))
	if err != nil || !ok {
		t.Fatalf("Cy's identity: %v (%v)", ok, err)
	}
	marks, newHead, err := f.svc.PersonChanges(ctx, head, 100)
	if err != nil || len(marks) != 1 || marks[0].UserID != cy || newHead <= head {
		t.Errorf("the marks after Cy's first sign-in: %+v, head %d (%v)", marks, newHead, err)
	}
	for _, limit := range []int{0, 1001} {
		if _, _, err := f.svc.PersonChanges(ctx, 0, limit); service.CodeOf(err) != service.CodeBadRequest {
			t.Errorf("a page of %d: %v, want bad_request", limit, err)
		}
	}
}

func TestANoticeThatDoesNotMatchWhereItsMailboxStandsIsLeftOutAndLogged(t *testing.T) {
	// A recovery withdraws its removal's notices; a row made live again
	// otherwise would leave one telling of a removal that is over.
	d := newDirectoryTeam(t, fixtureOptions{})
	d.removed(t, d.ana, d.box)
	notice := d.noticesOf(t, d.bea.UserID)[0]
	d.exec(t, `UPDATE accounts SET removed_at = 0, removed_by = '', purge_after = 0 WHERE id = ?`, d.box)
	if n := d.noticesOf(t, d.bea.UserID); len(n) != 0 {
		t.Errorf("a removal's notice of a live mailbox is reported: %+v", n)
	}
	logged := d.logs.String()
	if !strings.Contains(logged, notice.ID) || !strings.Contains(logged, "left out") ||
		strings.Contains(logged, "support@team.example.com") {
		t.Errorf("the log: %s", logged)
	}
}
