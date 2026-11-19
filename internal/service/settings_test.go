package service_test

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// What an extension reports of a followed workspace's settings and of a
// person's own (settings.go). The directoryTeam: Ana owns the directory's
// team and Bea is a member of it, each signed in by an identity; its
// mailbox, "Help desk" (support@team.example.com), connected once, Ana
// linked and both read.

// report reads a workspace's report and fails the test on a refusal.
func (d directoryTeam) report(t *testing.T, workspaceID string) service.WorkspaceReport {
	t.Helper()
	r, err := d.svc.WorkspaceReport(t.Context(), workspaceID)
	if err != nil {
		t.Fatalf("the report of %s: %v", workspaceID, err)
	}
	return r
}

// mailboxIn is a mailbox of a report's settings.
func mailboxIn(t *testing.T, r service.WorkspaceReport, accountID string) service.MailboxSettings {
	t.Helper()
	for _, mb := range r.Settings.Mailboxes {
		if mb.ID == accountID {
			return mb
		}
	}
	t.Fatalf("%s is not in the report of %s", accountID, r.Settings.WorkspaceID)
	return service.MailboxSettings{}
}

// personSettings reads a person's settings and fails the test on a refusal.
func (d directoryTeam) personSettings(t *testing.T, userID string) service.PersonSettings {
	t.Helper()
	s, err := d.svc.PersonSettings(t.Context(), userID, service.MaxPersonNotices)
	if err != nil {
		t.Fatalf("the settings of %s: %v", userID, err)
	}
	return s
}

// turnSync switches the team's mailbox's sync on, to the current text, or
// off, as p.
func (d directoryTeam) turnSync(t *testing.T, p service.Principal, accountID string, on bool) {
	t.Helper()
	req := service.MailboxSyncRequest{Enabled: &on}
	if on {
		req.Version = d.consent().Sync
	}
	if _, err := d.svc.SetMailboxSync(t.Context(), p, accountID, req); err != nil {
		t.Fatalf("switching %s's sync %v: %v", accountID, on, err)
	}
}

// at is a column of a row as a moment, unix seconds.
func (d directoryTeam) at(t *testing.T, query string, args ...any) time.Time {
	t.Helper()
	return time.Unix(int64(d.count(t, query, args...)), 0).UTC()
}

func TestAWorkspaceReportReadsItsSnapshotAndItsSettingsAtOneMoment(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	r := d.report(t, dirTeamID)
	if r.SnapshotTooLarge || !r.Snapshot.MeasuredAt.Equal(r.MeasuredAt) || r.MeasuredAt.IsZero() {
		t.Fatalf("the report measured at %v, its snapshot at %v (too large %v)", r.MeasuredAt, r.Snapshot.MeasuredAt,
			r.SnapshotTooLarge)
	}
	if len(r.Snapshot.Mailboxes) != 1 || len(r.Settings.Mailboxes) != 1 || r.Snapshot.Mailboxes[0].ID != d.box {
		t.Fatalf("the snapshot's mailboxes %+v, the settings' %+v", r.Snapshot.Mailboxes, r.Settings.Mailboxes)
	}
	s := r.Settings
	if s.WorkspaceID != dirTeamID || s.Kind != workspace.KindTeam || s.Availability != service.AvailabilityUsable ||
		s.LinkMinRole != workspace.RoleAdmin || s.SyncText != d.consent().Sync || s.MailboxesOver || s.MembersOver {
		t.Errorf("the team's settings: %+v", s)
	}
	mb := s.Mailboxes[0]
	connected := d.at(t, `SELECT updated_at FROM credentials WHERE account_id = ?`, d.box)
	want := service.MailboxSettings{
		ID: d.box, Provider: "imap", AuthKind: "password", State: "active", ConnectedAt: connected,
		LinkedBy: d.ana.UserID, Team: true, Indexed: service.IndexNone,
	}
	if !reflect.DeepEqual(mb, want) {
		t.Errorf("the mailbox:\n%+v\nwant\n%+v", mb, want)
	}
	var members []string
	for _, m := range s.Members {
		members = append(members, fmt.Sprintf("%s %s %s %v", m.UserID, m.Role, m.Status, m.Enrolled))
	}
	wantMembers := []string{d.ana.UserID + " owner active true", d.bea.UserID + " member active true"}
	slices.Sort(wantMembers)
	if !slices.Equal(members, wantMembers) {
		t.Errorf("the members: %v, want %v", members, wantMembers)
	}
}

func TestAWorkspaceReportIsNotFoundOutsideTheDirectorysSource(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	for _, id := range []string{workspace.OperatorID, "wsp_nobodyknows"} {
		if _, err := d.svc.WorkspaceReport(t.Context(), id); service.CodeOf(err) != service.CodeNotFound {
			t.Errorf("the report of %s: %v, want not_found", id, err)
		}
	}
	local := newFixture(t)
	if _, err := local.svc.WorkspaceReport(t.Context(), workspace.OperatorID); service.CodeOf(err) != service.CodeConflict {
		t.Errorf("a report where workspaces are local: %v, want conflict", err)
	}
}

func TestEveryStateReasonMapsToOneReasonCodeAndOnlyOnAnErrorOrANeedsReauth(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	reasons := map[string]service.MailboxReason{
		account.ReasonScopeMissing:   service.ReasonScopeMissing,
		account.ReasonMailboxRefused: service.ReasonMailboxRefused,
		account.ReasonTokenRejected:  service.ReasonTokenRejected,
		account.ReasonUnproved:       service.ReasonUnproved,
		account.ReasonDeclined:       service.ReasonOther,
		account.ReasonFailed:         service.ReasonOther,
		account.ReasonNoAddress:      service.ReasonOther,
		// A dead grant's, an older text's, none: never matched loosely.
		"the refresh token is no longer valid; authorize the account again": service.ReasonOther,
		account.ReasonScopeMissing + ".":                                    service.ReasonOther,
		"":                                                                  service.ReasonOther,
	}
	seen := map[service.MailboxReason]bool{}
	for _, state := range service.MailboxStates() {
		for text, code := range reasons {
			d.exec(t, `UPDATE accounts SET state = ?, state_reason = ? WHERE id = ?`, state, text, d.box)
			mb := mailboxIn(t, d.report(t, dirTeamID), d.box)
			want := service.MailboxReason("")
			switch account.State(state) {
			case account.StateError, account.StateDisabled, account.StateNeedsReauth:
				want = code
			}
			if mb.State != state || mb.Reason != want {
				t.Errorf("%s with %q: %s, reason %q; want %q", state, text, mb.State, mb.Reason, want)
			}
			seen[mb.Reason] = true
		}
	}
	for _, code := range service.MailboxReasons() {
		if !seen[code] {
			t.Errorf("no state reason gives %q", code)
		}
	}
	// A removed mailbox says its removal, and no reason.
	d.exec(t, `UPDATE accounts SET state = 'error', state_reason = ? WHERE id = ?`, account.ReasonUnproved, d.box)
	d.removed(t, d.ana, d.box)
	if mb := mailboxIn(t, d.report(t, dirTeamID), d.box); !mb.Removed || mb.Reason != "" {
		t.Errorf("the removed mailbox in error: removed %v, reason %q", mb.Removed, mb.Reason)
	}
}

func TestAWorkspaceReportTooLargeForItsSnapshotStillReadsItsSettings(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	d.exec(t, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO accounts(id, workspace_id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port,
		  smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at)
		SELECT printf('acc_many%07d', i), ?, printf('m%d@ops.example.org', i), 'imap', 'password', 'imap.example.org',
		  993, 'smtp.example.org', 587, 'starttls', 'm', 1, 'active', 1, 1, 1 FROM n`,
		service.SnapshotMaxMailboxes, dirTeamID)
	if _, err := d.svc.AccessSnapshot(t.Context(), dirTeamID); service.CodeOf(err) != service.CodeConflict {
		t.Fatalf("the snapshot of a team past its bound: %v, want conflict", err)
	}
	r := d.report(t, dirTeamID)
	s := r.Settings
	if !r.SnapshotTooLarge || len(r.Snapshot.Mailboxes) != 0 || r.MeasuredAt.IsZero() {
		t.Errorf("the report: too large %v, %d mailboxes in its snapshot, measured %v", r.SnapshotTooLarge,
			len(r.Snapshot.Mailboxes), r.MeasuredAt)
	}
	if !s.MailboxesOver || s.Mailboxes != nil || s.MembersOver || len(s.Members) != 2 ||
		s.Availability != service.AvailabilityUsable || s.LinkMinRole != workspace.RoleAdmin {
		t.Errorf("the settings: mailboxes over %v (%d), members over %v (%d), %s, %q", s.MailboxesOver, len(s.Mailboxes),
			s.MembersOver, len(s.Members), s.Availability, s.LinkMinRole)
	}
}

func TestAnICloudMailboxIsPresentedAsICloud(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	for _, c := range []struct{ provider, host, want string }{
		{"imap", "imap.mail.me.com", account.ICloud},
		{"imap", "IMAP.MAIL.ME.COM", account.ICloud},
		{"imap", "imap.mail.example", "imap"},
		{"gmail", "imap.gmail.com", "gmail"},
		{"microsoft", "outlook.office365.com", "microsoft"},
	} {
		d.exec(t, `UPDATE accounts SET provider = ?, imap_host = ? WHERE id = ?`, c.provider, c.host, d.box)
		got := mailboxIn(t, d.report(t, dirTeamID), d.box).Provider
		if got != c.want || !slices.Contains(service.MailboxProviders(), got) {
			t.Errorf("%s on %s is presented as %q, want %q", c.provider, c.host, got, c.want)
		}
	}
}

func TestATeamsAgreementSaysWhoWhenWhichTextAndWhetherItIsBound(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	if mb := mailboxIn(t, d.report(t, dirTeamID), d.box); mb.Agreement != nil || mb.SyncOn {
		t.Fatalf("a team mailbox nobody agreed to sync: %+v", mb)
	}
	d.turnSync(t, d.ana, d.box, true)
	mb := mailboxIn(t, d.report(t, dirTeamID), d.box)
	agreed := d.at(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, d.box)
	if !mb.SyncOn || mb.Agreement == nil || *mb.Agreement != (service.SyncAgreementState{
		ByUserID: d.ana.UserID, At: agreed, Revision: d.consent().Sync,
	}) {
		t.Fatalf("the agreement: on %v, %+v", mb.SyncOn, mb.Agreement)
	}
	// Still the one migration 0011 copied from its linker, and given by the
	// command line: bound, and by nobody.
	d.exec(t, `UPDATE accounts SET sync_enabled_via = 'migration', sync_enabled_by = 'cli' WHERE id = ?`, d.box)
	mb = mailboxIn(t, d.report(t, dirTeamID), d.box)
	if mb.Agreement == nil || !mb.Agreement.Bound || mb.Agreement.ByUserID != "" {
		t.Errorf("the bound agreement: %+v", mb.Agreement)
	}
	// Off, there is none.
	d.turnSync(t, d.ana, d.box, false)
	if mb := mailboxIn(t, d.report(t, dirTeamID), d.box); mb.Agreement != nil || mb.SyncOn {
		t.Errorf("a team mailbox turned off: on %v, %+v", mb.SyncOn, mb.Agreement)
	}
}

func TestAPauseSaysWhoAndWhenAndARecoverysPauseNamesTheRecoverer(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	ctx := t.Context()
	d.turnSync(t, d.ana, d.box, true)
	d.pause(t, d.ana, d.box)
	mb := mailboxIn(t, d.report(t, dirTeamID), d.box)
	paused := d.at(t, `SELECT sync_paused_at FROM accounts WHERE id = ?`, d.box)
	if mb.Paused == nil || *mb.Paused != (service.SyncPauseState{ByUserID: d.ana.UserID, At: paused}) || !mb.SyncOn {
		t.Fatalf("the pause: %+v, on %v", mb.Paused, mb.SyncOn)
	}
	if _, err := d.svc.ResumeMailboxSync(ctx, d.ana, d.box, service.SyncResumeRequest{Version: d.consent().Sync}); err != nil {
		t.Fatal(err)
	}
	if mb := mailboxIn(t, d.report(t, dirTeamID), d.box); mb.Paused != nil {
		t.Errorf("a resumed mailbox: %+v", mb.Paused)
	}
	d.removed(t, d.ana, d.box)
	if _, err := d.svc.RecoverAccount(ctx, d.ana, d.box); err != nil {
		t.Fatal(err)
	}
	if mb := mailboxIn(t, d.report(t, dirTeamID), d.box); mb.Paused == nil || mb.Paused.ByUserID != d.ana.UserID {
		t.Errorf("a recovered mailbox's pause: %+v, want Ana's, who recovered it", mb.Paused)
	}
	// Paused by the command line, or by a person forgotten since: nobody.
	d.exec(t, `UPDATE accounts SET sync_paused_by = '' WHERE id = ?`, d.box)
	if mb := mailboxIn(t, d.report(t, dirTeamID), d.box); mb.Paused == nil || mb.Paused.ByUserID != "" {
		t.Errorf("a pause by nobody: %+v", mb.Paused)
	}
}

func TestAHeldPersonalMailboxIsOffAndHeldWithNoIndex(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	ctx := t.Context()
	personal := personalOf("ana@example.com")
	own := d.teamBox(t, personal, d.ana.UserID, "ana@mail.example.com")
	d.connect(t, own)
	if _, err := d.svc.GrantSyncConsent(ctx, d.ana, d.consent().Sync); err != nil {
		t.Fatal(err)
	}
	d.seedIndex(t, own, "Quokka")
	r := d.report(t, personal)
	if mb := mailboxIn(t, r, own); !mb.SyncOn || mb.Held || mb.Team || mb.Indexed == service.IndexNone {
		t.Fatalf("Ana's own mailbox under her consent: %+v", mb)
	}
	if r.Settings.LinkMinRole != workspace.RoleOwner || r.Settings.Availability != service.AvailabilityUsable {
		t.Errorf("Ana's personal workspace: %q, %s", r.Settings.LinkMinRole, r.Settings.Availability)
	}
	d.operatorStopped(t, own)
	mb := mailboxIn(t, d.report(t, personal), own)
	if mb.SyncOn || !mb.Held || mb.Indexed != service.IndexNone || mb.Agreement != nil || mb.Paused != nil {
		t.Errorf("Ana's own mailbox the operator turned off: %+v", mb)
	}
}

func TestARemovedMailboxSaysItsEndAndWhetherItsIndexIsStillThere(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	ctx := t.Context()
	d.turnSync(t, d.ana, d.box, true)
	d.seedIndex(t, d.box, "Zanzibar")
	removal := d.removed(t, d.ana, d.box)
	mb := mailboxIn(t, d.report(t, dirTeamID), d.box)
	removedAt := d.at(t, `SELECT removed_at FROM accounts WHERE id = ?`, d.box)
	if !mb.Removed || !mb.RemovedAt.Equal(removedAt) || mb.PurgeAfter.Unix() != removal.PurgeAfter || !mb.IndexKept {
		t.Errorf("the removed mailbox: removed %v at %v until %v, index kept %v", mb.Removed, mb.RemovedAt, mb.PurgeAfter,
			mb.IndexKept)
	}
	// Its owners turn its sync off, which deletes what it kept.
	off := false
	if _, err := d.svc.SetMailboxSync(ctx, d.ana, d.box, service.MailboxSyncRequest{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if mb := mailboxIn(t, d.report(t, dirTeamID), d.box); !mb.Removed || mb.IndexKept {
		t.Errorf("the removed mailbox turned off: removed %v, index kept %v", mb.Removed, mb.IndexKept)
	}

	// A team mailbox migration 0011 left stopped and bound to its linker:
	// off, with the index it keeps, once removed too.
	bound := d.teamBox(t, dirTeamID, d.ana.UserID, "bound@team.example.com")
	d.connect(t, bound)
	d.giveRead(t, bound, d.bea.UserID)
	d.seedIndex(t, bound, "Wombat")
	d.exec(t, `UPDATE accounts SET sync_enabled_at = 0, sync_enabled_by = ?, sync_enabled_via = 'migration' WHERE id = ?`,
		d.ana.UserID, bound)
	if mb := mailboxIn(t, d.report(t, dirTeamID), bound); mb.SyncOn || mb.Agreement != nil || mb.Indexed == service.IndexNone {
		t.Errorf("the stopped bound mailbox: %+v", mb)
	}
	d.removed(t, d.ana, bound)
	if mb := mailboxIn(t, d.report(t, dirTeamID), bound); mb.SyncOn || !mb.IndexKept {
		t.Errorf("the stopped bound mailbox removed: on %v, index kept %v", mb.SyncOn, mb.IndexKept)
	}
}

func TestTheIndexIsNoneUntilAFolderFinishesAndCompleteOnceEveryFolderHas(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	indexed := func() service.IndexState { return mailboxIn(t, d.report(t, dirTeamID), d.box).Indexed }
	if got := indexed(); got != service.IndexNone {
		t.Fatalf("a mailbox with no folder: %s", got)
	}
	d.exec(t, `INSERT INTO folders(id, account_id, name, display_name, role, role_source, synced, selectable, uidvalidity,
		sync_state, backfill_cursor) VALUES (901, ?, 'INBOX', 'Inbox', 'inbox', 'inbox', 1, 1, 7, 'initial', 40),
		(902, ?, 'Archive', 'Archive', '', '', 1, 1, 7, 'initial', 40),
		(903, ?, 'Spam', 'Spam', 'junk', 'name-table', 0, 1, 0, 'disabled', 0)`, d.box, d.box, d.box)
	if got := indexed(); got != service.IndexNone {
		t.Errorf("folders that have not finished, no message: %s", got)
	}
	d.exec(t, `INSERT INTO messages(account_id, folder_id, uidvalidity, uid, message_id, group_key, subject, from_text,
		to_text, internal_date, size, first_seen_at, updated_at, body_text) VALUES (?, 901, 7, 1, 'm@x', 'mid:m@x', 'S', 'F',
		'T', 1, 1, 1, 1, 'b')`, d.box)
	if got := indexed(); got != service.IndexPartial {
		t.Errorf("a message, no folder finished: %s", got)
	}
	d.exec(t, `UPDATE folders SET sync_state = 'live', backfill_cursor = 0 WHERE id = 901`)
	if got := indexed(); got != service.IndexPartial {
		t.Errorf("one folder of two finished: %s", got)
	}
	// The folder that does not sync counts for nothing.
	d.exec(t, `UPDATE folders SET sync_state = 'error', backfill_cursor = 0 WHERE id = 902`)
	if got := indexed(); got != service.IndexComplete {
		t.Errorf("every folder finished: %s", got)
	}
	for _, s := range service.IndexStates() {
		if s != service.IndexNone && s != service.IndexPartial && s != service.IndexComplete {
			t.Errorf("an index state no test reaches: %s", s)
		}
	}
}

func TestAMailboxNobodyReadsOrWouldReadOnceRecoveredHasNoReader(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	// A mailbox of the team nobody holds anything on.
	d.exec(t, `INSERT INTO accounts(id, workspace_id, email, provider, auth_kind, imap_host, imap_port, smtp_host,
		smtp_port, smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at)
		VALUES ('acc_00000000000000e1', ?, 'orphan@team.example.com', 'imap', 'password', 'imap.mail.example', 993,
		'smtp.mail.example', 465, 'implicit', 'orphan', 0, 'active', 1, 1, 1)`, dirTeamID)
	r := d.report(t, dirTeamID)
	if !mailboxIn(t, r, "acc_00000000000000e1").NoReader || mailboxIn(t, r, d.box).NoReader {
		t.Fatalf("nobody reads the orphan (%v), Ana and Bea read the desk (%v)",
			mailboxIn(t, r, "acc_00000000000000e1").NoReader, mailboxIn(t, r, d.box).NoReader)
	}
	d.giveRead(t, "acc_00000000000000e1", d.bea.UserID)
	if mailboxIn(t, d.report(t, dirTeamID), "acc_00000000000000e1").NoReader {
		t.Error("a mailbox Bea reads has no reader")
	}
	// Removed, it is read by whoever would read it once recovered.
	d.removed(t, d.ana, d.box)
	if mailboxIn(t, d.report(t, dirTeamID), d.box).NoReader {
		t.Error("a removed mailbox its readers would read once recovered has no reader")
	}
	// Exactly when no snapshot line says it reads.
	r = d.report(t, dirTeamID)
	for _, mb := range r.Snapshot.Mailboxes {
		reads := false
		for _, p := range mb.People {
			reads = reads || p.Reads
		}
		if mailboxIn(t, r, mb.ID).NoReader == reads {
			t.Errorf("%s: no reader %v, and a snapshot line reads %v", mb.ID, mailboxIn(t, r, mb.ID).NoReader, reads)
		}
	}
}

func TestAMembersStatusIsDisabledSuspendedHeldThenActiveInThatOrder(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	people := map[string]string{}
	for _, name := range []string{"dan", "eve", "fay", "gil", "hal", "ivy"} {
		p := d.followed(t, name+"@example.com")
		d.apply(t, document(name+"@example.com", 1, inTeam(workspace.RoleMember, 1)))
		people[name] = p.UserID
	}
	for name, set := range map[string][]string{
		// Disabled here, over a hold.
		"dan": {`UPDATE users SET status = 'disabled' WHERE id = ?1`,
			`UPDATE workspace_members SET status = 'disabled', held_at = 900 WHERE user_id = ?1 AND workspace_id = ?2`},
		// Suspended, over a hold.
		"eve": {`UPDATE users SET status = 'disabled', suspended_at = 900 WHERE id = ?1`,
			`UPDATE workspace_members SET status = 'disabled', held_at = 900 WHERE user_id = ?1 AND workspace_id = ?2`},
		"fay": {`UPDATE workspace_members SET status = 'disabled', held_at = 900 WHERE user_id = ?1 AND workspace_id = ?2`},
		"gil": {`UPDATE workspace_members SET status = 'disabled' WHERE user_id = ?1 AND workspace_id = ?2`},
		"hal": {`UPDATE users SET status = 'disabled', suspended_at = 900 WHERE id = ?1`},
	} {
		for _, q := range set {
			d.exec(t, q, people[name], dirTeamID)
		}
	}
	want := map[string]service.MemberStatus{
		people["dan"]: service.MemberDisabled, people["eve"]: service.MemberSuspended, people["fay"]: service.MemberHeld,
		people["gil"]: service.MemberDisabled, people["hal"]: service.MemberSuspended, people["ivy"]: service.MemberActive,
		d.ana.UserID: service.MemberActive, d.bea.UserID: service.MemberActive,
	}
	seen := map[service.MemberStatus]bool{}
	for _, m := range d.report(t, dirTeamID).Settings.Members {
		if m.Status != want[m.UserID] {
			t.Errorf("%s: %s, want %s", m.UserID, m.Status, want[m.UserID])
		}
		seen[m.Status] = true
	}
	for _, s := range service.MemberStatuses() {
		if !seen[s] {
			t.Errorf("no member is %s", s)
		}
	}
}

func TestAClosedTeamIsClosedBeforeItIsWithheldAndNobodyLinksInEither(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	d.withholdTeams(t)
	r := d.report(t, dirTeamID)
	if r.Settings.Availability != service.AvailabilityWithheld || r.Settings.LinkMinRole != "" {
		t.Errorf("a team where teams are withheld: %s, %q", r.Settings.Availability, r.Settings.LinkMinRole)
	}
	if p := d.report(t, personalOf("ana@example.com")).Settings; p.Availability != service.AvailabilityUsable ||
		p.LinkMinRole != workspace.RoleOwner {
		t.Errorf("a personal workspace where teams are withheld: %s, %q", p.Availability, p.LinkMinRole)
	}
	d.exec(t, `UPDATE workspaces SET closed_at = 900 WHERE id = ?`, dirTeamID)
	r = d.report(t, dirTeamID)
	if r.Settings.Availability != service.AvailabilityClosed || r.Settings.LinkMinRole != "" {
		t.Errorf("a closed team where teams are withheld: %s, %q", r.Settings.Availability, r.Settings.LinkMinRole)
	}
	if !slices.Equal(service.Availabilities(), []service.Availability{service.AvailabilityClosed,
		service.AvailabilityWithheld, service.AvailabilityUsable}) {
		t.Errorf("the availabilities: %v", service.Availabilities())
	}
}

func TestWhoMayLinkIsTheOwnerInAPersonalWorkspaceAndAnAdminInATeam(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	if s := d.report(t, dirTeamID).Settings; s.LinkMinRole != workspace.RoleAdmin {
		t.Errorf("a team: %q", s.LinkMinRole)
	}
	if s := d.report(t, personalOf("bea@example.com")).Settings; s.LinkMinRole != workspace.RoleOwner {
		t.Errorf("Bea's personal workspace: %q", s.LinkMinRole)
	}
	// What the report says is what the service does: a member is refused a
	// link into the team, an admin is not.
	_, err := d.svc.AddAccount(t.Context(), d.bea, keyed(d.bea, service.AddAccountRequest{
		Email: "new@team.example.com", Provider: "gmail", Flow: "loopback", WorkspaceID: dirTeamID,
		SyncConsentVersion: d.consent().Sync,
	}))
	if service.CodeOf(err) != service.CodeNotAuthorized {
		t.Errorf("a member linking into the team: %v, want not_authorized", err)
	}
	d.exec(t, `UPDATE workspaces SET closed_at = 900 WHERE id = ?`, dirTeamID)
	if s := d.report(t, dirTeamID).Settings; s.LinkMinRole != "" || s.Availability != service.AvailabilityClosed {
		t.Errorf("a closed team: %q, %s", s.LinkMinRole, s.Availability)
	}
}

func TestAMailboxFilesItsSentCopyInTheReportExactlyWhereTheSendPathWould(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	for _, c := range []struct {
		what                  string
		provider, auth, email string
		host, kind            string
		save, want            bool
	}{
		{"another IMAP server that asks for it", "imap", "password", "support@team.example.com", "imap.mail.example",
			account.ServerOther, true, true},
		{"one that does not ask", "imap", "password", "support@team.example.com", "imap.mail.example",
			account.ServerOther, false, false},
		{"one no login reached yet", "imap", "password", "support@team.example.com", "imap.mail.example", "", true, false},
		{"Gmail's server under another name", "imap", "password", "support@team.example.com", "imap.mail.example",
			account.ServerGmail, true, false},
		{"Google's host", "imap", "password", "support@team.example.com", "imap.gmail.com", account.ServerOther, true, false},
		{"a Gmail address on a server that is not Gmail's", "imap", "password", "someone@gmail.com", "imap.mail.example",
			account.ServerOther, true, true},
		{"Gmail", "gmail", "oauth2", "support@team.example.com", "imap.gmail.com", account.ServerGmail, true, false},
		{"Microsoft", "microsoft", "oauth2", "support@team.example.com", "outlook.office365.com", account.ServerOther,
			true, false},
	} {
		d.exec(t, `UPDATE accounts SET provider = ?, auth_kind = ?, email = ?, imap_host = ?, server_kind = ?,
			save_sent_copy = ? WHERE id = ?`, c.provider, c.auth, c.email, c.host, c.kind, c.save, d.box)
		if got := mailboxIn(t, d.report(t, dirTeamID), d.box).FilesSentCopy; got != c.want {
			t.Errorf("%s: files its sent copy %v, want %v", c.what, got, c.want)
		}
	}
}

func TestAMailboxSaysWhenItFirstConnectedAndWhoLinkedIt(t *testing.T) {
	d := newDirectoryTeam(t, fixtureOptions{})
	mb := mailboxIn(t, d.report(t, dirTeamID), d.box)
	if mb.ConnectedAt.IsZero() || mb.LinkedBy != d.ana.UserID {
		t.Fatalf("the desk: connected at %v, linked by %q", mb.ConnectedAt, mb.LinkedBy)
	}
	// Its password stored again keeps the first moment.
	first := mb.ConnectedAt
	d.exec(t, `UPDATE credentials SET updated_at = updated_at + 3600 WHERE account_id = ?`, d.box)
	d.connect(t, d.box)
	if got := mailboxIn(t, d.report(t, dirTeamID), d.box).ConnectedAt; !got.Equal(first) {
		t.Errorf("connected at %v after its password was stored again, want %v", got, first)
	}
	// One that never connected, linked by a key and then the command line.
	never := d.teamBox(t, dirTeamID, d.ana.UserID, "never@team.example.com")
	for _, linker := range []string{"key:abcd0001", "cli", ""} {
		d.exec(t, `UPDATE accounts SET linked_by = ? WHERE id = ?`, linker, never)
		if mb := mailboxIn(t, d.report(t, dirTeamID), never); !mb.ConnectedAt.IsZero() || mb.LinkedBy != "" {
			t.Errorf("linked by %q, never connected: connected at %v, linked by %q", linker, mb.ConnectedAt, mb.LinkedBy)
		}
	}
}
