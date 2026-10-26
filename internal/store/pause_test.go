package store

import (
	"context"
	"testing"
)

// Why a mailbox does not sync (PauseSQL): one table, held to the rule that
// decides whether it does (syncEligible).

// pauses reads PauseSQL for every account of the store, by id.
func pauses(ctx context.Context, t *testing.T, s *Store, withheld bool) map[string]string {
	t.Helper()
	rows, err := s.Reader().QueryContext(ctx, `SELECT a.id, `+PauseSQL(withheld)+` FROM accounts a`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var id, pause string
		if err := rows.Scan(&id, &pause); err != nil {
			t.Fatal(err)
		}
		out[id] = pause
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAPauseIsEmptyExactlyWhenTheMailboxMaySync(t *testing.T) {
	// The seed's mailboxes — personal, operator and team ones, under
	// consents given, withdrawn and bound to a disabled person — under
	// every change that stops or starts a sync: each is paused exactly
	// when the engine would not sync it, with teams withheld or not.
	f := newSchemaLatest(t)
	ctx := context.Background()
	paused, eligible := 0, 0
	check := func(step string) {
		t.Helper()
		for _, withheld := range []bool{false, true} {
			f.s.teamsWithheld = withheld
			got := pauses(ctx, t, f.s, withheld)
			for id, pause := range got {
				ok, err := f.s.SyncEligible(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if ok != (pause == "") {
					t.Errorf("%s (withheld %v): %s is eligible %v with the pause %q", step, withheld, id, ok, pause)
				}
				if ok {
					eligible++
				} else {
					paused++
				}
			}
		}
		f.s.teamsWithheld = false
	}
	// Every mailbox under its consent, so that some sync.
	f.accepted("consents", f.exec(`UPDATE users SET sync_consent_at = 900, sync_consent_version = 'sync-1'
		WHERE status = 'active'`))
	f.accepted("team and operator consents", f.exec(`UPDATE accounts SET sync_enabled_at = 900, sync_enabled_by = 'cli'
		WHERE owner_user_id IS NULL`))
	check("as seeded")
	for _, step := range []struct{ what, query string }{
		{"a mailbox waiting for consent", `UPDATE accounts SET state = 'pending_auth' WHERE id = 'acc_0000000000000001'`},
		{"one needing reauthorization", `UPDATE accounts SET state = 'needs_reauth' WHERE id = 'acc_0000000000000003'`},
		{"one in error", `UPDATE accounts SET state = 'error' WHERE id = 'acc_t3'`},
		{"one disabled", `UPDATE accounts SET state = 'disabled' WHERE id = 'acc_0000000000000002'`},
		{"every one active again", `UPDATE accounts SET state = 'active'`},
		// The operator's hold, of a personal mailbox: Owner's own.
		{"one held", `UPDATE accounts SET sync_held_at = 950 WHERE id = 'acc_0000000000000002'`},
		{"one paused", `UPDATE accounts SET sync_paused_at = 955, sync_paused_by = 'usr_0000000000000001'
			WHERE id = 'acc_t1'`},
		{"one removed", `UPDATE accounts SET removed_at = 960, removed_by = 'cli', purge_after = 960 + 2592000
			WHERE id = 'acc_t3'`},
		{"a team closed", `UPDATE workspaces SET closed_at = 970 WHERE id = 'wsp_t1'`},
		{"the team reopened", `UPDATE workspaces SET closed_at = 0 WHERE id = 'wsp_t1'`},
		{"a person's consent withdrawn", `UPDATE users SET sync_consent_at = 0, sync_consent_version = ''
			WHERE id = 'usr_0000000000000002'`},
		{"a person suspended", `UPDATE users SET status = 'disabled', suspended_at = 980 WHERE id = 'usr_0000000000000001'`},
		{"a team consent bound to them", `UPDATE accounts SET sync_enabled_via = 'migration',
			sync_enabled_by = 'usr_0000000000000001' WHERE id = 'acc_t1'`},
		{"a team consent withdrawn", `UPDATE accounts SET sync_enabled_at = 0, sync_enabled_by = '' WHERE id = 'acc_t2'`},
		{"the operator's switched off", `UPDATE accounts SET sync_enabled_at = 0, sync_enabled_by = ''
			WHERE workspace_id = 'wsp_operator'`},
		{"a team mailbox's readers losing read", `UPDATE mailbox_access SET read = 0, act = 0 WHERE account_id = 'acc_t1'`},
		{"the person resumed", `UPDATE users SET status = 'active', suspended_at = 0 WHERE id = 'usr_0000000000000001'`},
		{"its pause ended", `UPDATE accounts SET sync_paused_at = 0, sync_paused_by = '' WHERE id = 'acc_t1'`},
	} {
		f.accepted(step.what, f.exec(step.query))
		check(step.what)
	}
	if paused == 0 || eligible == 0 {
		t.Errorf("%d paused and %d eligible: the steps tried only one side", paused, eligible)
	}
}

func TestThePauseTableIsOneOrderForEveryCause(t *testing.T) {
	// A team mailbox of a team from another source, which every cause holds
	// at once; each lifted in turn shows the next, in the table's order,
	// until it syncs. Then a personal mailbox and an operator one, each with
	// their own consent's causes.
	f := newSchemaLatest(t)
	const bea, fay = "usr_0000000000000002", "usr_0000000000000006"
	seedExternalTeam(t, f, "wsp_px", bea, fay)
	externalMailbox(t, f, "acc_px", "wsp_px")
	f.accepted("Bea reads it", f.exec(`INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act, send,
		manage, granted_by, created_at, updated_at) VALUES ('acc_px', 'wsp_px', ?, 0, 0, 0, 1, 'cli', 800, 800)`, bea))
	f.accepted("Eve is disabled", f.exec(`UPDATE users SET status = 'disabled' WHERE id = 'usr_0000000000000005'`))
	pause := func(withheld bool, id string) string {
		t.Helper()
		return pauses(t.Context(), t, f.s, withheld)[id]
	}
	steps := []struct {
		pause, lift string
	}{
		// Each cause as it holds, and what lifts it.
		{PauseRemoved, `UPDATE accounts SET removed_at = 0, removed_by = '', purge_after = 0 WHERE id = 'acc_px'`},
		{PauseTeamClosed, `UPDATE workspaces SET closed_at = 0 WHERE id = 'wsp_px'`},
		{PausePendingAuth, `UPDATE accounts SET state = 'needs_reauth' WHERE id = 'acc_px'`},
		{PauseNeedsReauth, `UPDATE accounts SET state = 'error' WHERE id = 'acc_px'`},
		{PauseError, `UPDATE accounts SET state = 'active' WHERE id = 'acc_px'`},
		// A team mailbox is never held, and is paused only while its
		// consent stands: the pause comes with it.
		{PauseConsentOff, `UPDATE accounts SET sync_enabled_at = 900, sync_paused_at = 945, sync_paused_by = '` + bea + `'
			WHERE id = 'acc_px'`},
		{PausePersonOff, `UPDATE accounts SET sync_enabled_via = '', sync_enabled_by = 'usr_0000000000000001'
			WHERE id = 'acc_px'`},
		{PauseNoReader, `UPDATE mailbox_access SET read = 1 WHERE account_id = 'acc_px'`},
		{PausePaused, `UPDATE accounts SET sync_paused_at = 0, sync_paused_by = '' WHERE id = 'acc_px'`},
		{"", ""},
	}
	// Every cause at once: the state first, since a removal is recorded
	// whole and once.
	for _, q := range []string{
		`UPDATE accounts SET state = 'pending_auth', sync_enabled_at = 0,
			sync_enabled_via = 'migration', sync_enabled_by = 'usr_0000000000000005' WHERE id = 'acc_px'`,
		`UPDATE workspaces SET closed_at = 950 WHERE id = 'wsp_px'`,
		`UPDATE accounts SET removed_at = 960, removed_by = 'cli', purge_after = 960 + 2592000 WHERE id = 'acc_px'`,
	} {
		f.accepted("every cause", f.exec(q))
	}
	for _, step := range steps {
		if got := pause(false, "acc_px"); got != step.pause {
			t.Errorf("the pause is %q, want %q", got, step.pause)
		}
		// Withholding teams comes right after a closure, before the state.
		want := step.pause
		if want != PauseRemoved && want != PauseTeamClosed {
			want = PauseWithheld
		}
		if got := pause(true, "acc_px"); got != want {
			t.Errorf("with teams withheld, the pause is %q, want %q", got, want)
		}
		if step.lift != "" {
			f.accepted("lifting "+step.pause, f.exec(step.lift))
		}
	}
	// A disabled mailbox is in error too.
	f.accepted("disabled", f.exec(`UPDATE accounts SET state = 'disabled' WHERE id = 'acc_px'`))
	if got := pause(false, "acc_px"); got != PauseError {
		t.Errorf("a disabled mailbox's pause is %q, want %q", got, PauseError)
	}

	// A personal mailbox: held by the operator's turn-off, its person not
	// active, then not consenting, then paused. The seed: acc_3 is Bea's
	// own.
	const own = "acc_0000000000000003"
	f.accepted("Bea consents", f.exec(`UPDATE users SET sync_consent_at = 0 WHERE id = ?`, bea))
	f.accepted("Bea suspended", f.exec(`UPDATE users SET status = 'disabled', suspended_at = 990 WHERE id = ?`, bea))
	f.accepted("Bea's own held", f.exec(`UPDATE accounts SET sync_held_at = 985 WHERE id = ?`, own))
	for _, step := range []struct{ pause, lift string }{
		{PauseHeld, `UPDATE accounts SET sync_held_at = 0, sync_paused_at = 992, sync_paused_by = '` + bea + `'
			WHERE id = '` + own + `'`},
		{PausePersonOff, `UPDATE users SET status = 'active', suspended_at = 0 WHERE id = '` + bea + `'`},
		{PauseConsentOff, `UPDATE users SET sync_consent_at = 995, sync_consent_version = 'sync-1' WHERE id = '` + bea + `'`},
		{PausePaused, `UPDATE accounts SET sync_paused_at = 0, sync_paused_by = '' WHERE id = '` + own + `'`},
		{"", ""},
	} {
		if got := pause(false, own); got != step.pause {
			t.Errorf("Bea's own mailbox's pause is %q, want %q", got, step.pause)
		}
		// A personal mailbox is never withheld.
		if got := pause(true, own); got != step.pause {
			t.Errorf("with teams withheld, Bea's own mailbox's pause is %q, want %q", got, step.pause)
		}
		if step.lift != "" {
			f.accepted("lifting "+step.pause, f.exec(step.lift))
		}
	}

	// An operator mailbox: switched off, then on and paused, then resumed.
	// The seed: acc_5.
	const ops = "acc_0000000000000005"
	f.accepted("the operator's off", f.exec(`UPDATE accounts SET sync_enabled_at = 0, sync_enabled_by = '' WHERE id = ?`, ops))
	if got := pause(false, ops); got != PauseConsentOff {
		t.Errorf("the operator's mailbox, off: %q, want %q", got, PauseConsentOff)
	}
	f.accepted("the operator's on and paused", f.exec(`UPDATE accounts SET sync_enabled_at = 900, sync_enabled_by = 'cli',
		sync_paused_at = 905, sync_paused_by = 'cli' WHERE id = ?`, ops))
	if got := pause(true, ops); got != PausePaused {
		t.Errorf("the operator's mailbox, paused, with teams withheld: %q, want %q", got, PausePaused)
	}
	f.accepted("the operator's resumed", f.exec(`UPDATE accounts SET sync_paused_at = 0, sync_paused_by = '' WHERE id = ?`, ops))
	if got := pause(true, ops); got != "" {
		t.Errorf("the operator's mailbox, on, with teams withheld: %q, want none", got)
	}
}
