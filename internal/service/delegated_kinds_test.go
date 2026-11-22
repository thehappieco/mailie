package service_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Each act an extension runs for a person against the use case a session
// runs (delegated.go): the same fixture, the session's method on one copy
// and the request on another, and the same database after, the same
// journal, the same notices, the same freshness asked. So the two paths
// cannot drift.

// kindWorld is the fixture every kind starts from: the Support team, its
// mailbox synced and indexed, Ana its owner and Bea a member who read it,
// Carol an admin who reads nothing; and Ana's own mailbox, synced and
// indexed, in her personal workspace.
type kindWorld struct {
	f        *fixture
	hook     *freshHook
	tm       keysTeam
	own      string
	personal string
}

func newKindWorld(t *testing.T) kindWorld {
	t.Helper()
	hook := &freshHook{}
	f := newFixtureWith(t, fixtureOptions{freshness: hook})
	tm := syncedTeam(t, f)
	own := f.ownSynced(t, tm.ana, "ana@mail.example")
	f.connect(t, own)
	f.seedIndex(t, tm.shared, "Zebra")
	f.seedIndex(t, own, "Okapi")
	return kindWorld{f: f, hook: hook, tm: tm, own: own, personal: authtest.Personal(t, f.db, tm.ana.UserID)}
}

// settled moves every widening the fixture made, and every removal, a while
// back, so that no narrowing asked now meets one made in its own second.
func (w kindWorld) settled(t *testing.T) {
	t.Helper()
	w.f.exec(t, `UPDATE accounts SET sync_enabled_at = sync_enabled_at - 600 WHERE sync_enabled_at > 600`)
	w.f.exec(t, `UPDATE users SET sync_consent_at = sync_consent_at - 600 WHERE sync_consent_at > 600`)
	w.f.exec(t, `UPDATE users SET actions_consent_at = actions_consent_at - 600 WHERE actions_consent_at > 600`)
	w.f.exec(t, `UPDATE users SET send_consent_at = send_consent_at - 600 WHERE send_consent_at > 600`)
	w.f.exec(t, `UPDATE mailbox_access SET raised_at = raised_at - 600 WHERE raised_at > 600`)
	w.f.removalsMovedBack(t, 10*time.Minute)
	w.hook.take()
}

// rowsOf reads every row a query answers, each column as the driver gives
// it.
func (w kindWorld) rowsOf(t *testing.T, query string) [][]any {
	t.Helper()
	rows, err := w.f.db.Reader().QueryContext(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// text is a column's value as text.
func text(v any) string {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// names maps every id the fixture holds to what names it the same way in
// any copy of it: a person's address, a mailbox's, a workspace's name.
func (w kindWorld) names(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for query, prefix := range map[string]string{
		`SELECT id, email FROM users`:    "person:",
		`SELECT id, email FROM accounts`: "mailbox:",
		`SELECT w.id, coalesce(u.email, w.name) FROM workspaces w LEFT JOIN users u ON u.id = w.person_id`: "workspace:",
	} {
		for _, row := range w.rowsOf(t, query) {
			out[text(row[0])] = prefix + text(row[1])
		}
	}
	return out
}

// digest is what an act changed, every id named as names names it: each
// mailbox's removal, pause, hold, agreement and index; each person's
// consents, last sign-out everywhere and live sessions; who holds what; the
// journal, in order; and who is told what.
func (w kindWorld) digest(t *testing.T, names map[string]string) []string {
	t.Helper()
	name := func(v any) string {
		s := text(v)
		if n, ok := names[s]; ok {
			return n
		}
		return s
	}
	line := func(row []any) string {
		parts := make([]string, len(row))
		for i, v := range row {
			parts[i] = name(v)
		}
		return strings.Join(parts, " | ")
	}
	var out []string
	for _, query := range []string{
		`SELECT 'mailbox', id, removed_at <> 0, sync_paused_at <> 0, sync_paused_by, sync_held_at <> 0, sync_enabled_at <> 0,
		        sync_enabled_by, sync_consent_version, sync_enabled_via,
		        (SELECT count(*) FROM folders f WHERE f.account_id = a.id) + (SELECT count(*) FROM messages m WHERE m.account_id = a.id),
		        removed_by
		   FROM accounts a`,
		`SELECT 'person', id, sync_consent_at <> 0, sync_consent_version, actions_consent_at <> 0, actions_consent_version,
		        actions_consent_withdrawn_at <> 0, send_consent_at <> 0, send_consent_version, send_consent_withdrawn_at <> 0,
		        signed_out_everywhere_at <> 0,
		        (SELECT count(*) FROM sessions s WHERE s.user_id = u.id AND s.revoked_at = 0)
		   FROM users u`,
		`SELECT 'access', account_id, user_id, read, act, send, manage FROM mailbox_access`,
		`SELECT 'notice', n.user_id, e.kind, e.account_id, e.actor FROM notices n JOIN mailbox_events e ON e.seq = n.event_seq`,
	} {
		for _, row := range w.rowsOf(t, query) {
			out = append(out, line(row))
		}
	}
	slices.Sort(out)
	// The journal in its order, after the rest, with who read the mailbox
	// then.
	for _, row := range w.rowsOf(t, `SELECT 'journal', kind, account_id, workspace_id, actor, cause, lost_user,
		        (SELECT group_concat(user_id) FROM (SELECT user_id FROM mailbox_event_readers r WHERE r.event_seq = e.seq ORDER BY user_id))
		   FROM mailbox_events e ORDER BY seq`) {
		named := []string{}
		for _, id := range strings.Split(text(row[7]), ",") {
			if id != "" {
				named = append(named, name(id))
			}
		}
		slices.Sort(named)
		out = append(out, line(row[:7])+" | "+strings.Join(named, ","))
	}
	return out
}

// askedOf is what the freshness hook was asked, every id named.
func askedOf(asked []service.FreshAct, names map[string]string) []string {
	out := []string{}
	for _, a := range asked {
		recipients := []string{}
		for _, id := range a.RecipientIDs {
			recipients = append(recipients, names[id])
		}
		out = append(out, strings.Join([]string{string(a.Kind), names[a.ActorID], strings.Join(recipients, ","),
			names[a.WorkspaceID], names[a.AccountID]}, " | "))
	}
	return out
}

// kindCase is one kind: what its fixture needs first, the session's use
// case, and the request an extension carries for the same person.
type kindCase struct {
	kind string
	// prep readies the fixture, and returns what the act names beside it.
	prep func(t *testing.T, w kindWorld) string
	// actor is who acts, and where: the request's workspace, empty for a
	// person's own act.
	actor     func(w kindWorld) (service.Principal, string)
	session   func(ctx context.Context, w kindWorld, arg string) error
	request   func(ctx context.Context, w kindWorld, acts *service.PersonActs, arg string) (service.ActOutcome, error)
	unchanged bool
}

func kindCases() []kindCase {
	team := func(who func(keysTeam) service.Principal) func(w kindWorld) (service.Principal, string) {
		return func(w kindWorld) (service.Principal, string) { return who(w.tm), w.tm.id }
	}
	ana := func(tm keysTeam) service.Principal { return tm.ana }
	carol := func(tm keysTeam) service.Principal { return tm.carol }
	anaOwn := func(w kindWorld) (service.Principal, string) { return w.tm.ana, w.personal }
	anaPerson := func(w kindWorld) (service.Principal, string) { return w.tm.ana, "" }
	nothing := func(*testing.T, kindWorld) string { return "" }
	removeShared := func(t *testing.T, w kindWorld) string {
		w.f.removed(t, w.tm.ana, w.tm.shared)
		return ""
	}
	on, off := true, false
	return []kindCase{
		{kind: "item.sync_now", prep: nothing, actor: team(ana),
			session: func(ctx context.Context, w kindWorld, _ string) error {
				return w.f.svc.TriggerSync(ctx, w.tm.ana, w.tm.shared)
			},
			request: func(ctx context.Context, w kindWorld, a *service.PersonActs, _ string) (service.ActOutcome, error) {
				return a.SyncNow(ctx, w.tm.shared)
			}},
		{kind: "item.sync_stop", prep: nothing, actor: team(ana),
			session: func(ctx context.Context, w kindWorld, _ string) error {
				_, err := w.f.svc.PauseMailboxSync(ctx, w.tm.ana, w.tm.shared)
				return err
			},
			request: func(ctx context.Context, w kindWorld, a *service.PersonActs, _ string) (service.ActOutcome, error) {
				return a.PauseSync(ctx, w.tm.shared)
			}},
		{kind: "item.sync_resume", actor: team(carol),
			prep: func(t *testing.T, w kindWorld) string { w.f.pause(t, w.tm.ana, w.tm.shared); return "" },
			session: func(ctx context.Context, w kindWorld, _ string) error {
				_, err := w.f.svc.ResumeMailboxSync(ctx, w.tm.carol, w.tm.shared, service.SyncResumeRequest{Version: w.f.consent().Sync})
				return err
			},
			request: func(ctx context.Context, w kindWorld, a *service.PersonActs, _ string) (service.ActOutcome, error) {
				return a.ResumeSync(ctx, w.tm.shared, w.f.consent().Sync)
			}},
		{kind: "item.lift_hold", actor: anaOwn,
			prep: func(t *testing.T, w kindWorld) string { w.f.operatorStopped(t, w.own); return "" },
			session: func(ctx context.Context, w kindWorld, _ string) error {
				_, err := w.f.svc.SetMailboxSync(ctx, w.tm.ana, w.own, switchSync(true))
				return err
			},
			request: func(ctx context.Context, w kindWorld, a *service.PersonActs, _ string) (service.ActOutcome, error) {
				return a.LiftHold(ctx, w.own)
			}},
		{kind: "item.team_sync_on", actor: team(carol),
			prep: func(t *testing.T, w kindWorld) string {
				if _, err := w.f.svc.SetMailboxSync(t.Context(), w.tm.ana, w.tm.shared, switchSync(false)); err != nil {
					t.Fatal(err)
				}
				return ""
			},
			session: func(ctx context.Context, w kindWorld, _ string) error {
				_, err := w.f.svc.SetMailboxSync(ctx, w.tm.carol, w.tm.shared,
					service.MailboxSyncRequest{Enabled: &on, Version: w.f.consent().Sync})
				return err
			},
			request: func(ctx context.Context, w kindWorld, a *service.PersonActs, _ string) (service.ActOutcome, error) {
				return a.TeamSync(ctx, w.tm.shared, true, w.f.consent().Sync)
			}},
		{kind: "item.team_sync_off", prep: nothing, actor: team(carol),
			session: func(ctx context.Context, w kindWorld, _ string) error {
				_, err := w.f.svc.SetMailboxSync(ctx, w.tm.carol, w.tm.shared, service.MailboxSyncRequest{Enabled: &off})
				return err
			},
			request: func(ctx context.Context, w kindWorld, a *service.PersonActs, _ string) (service.ActOutcome, error) {
				return a.TeamSync(ctx, w.tm.shared, false, "")
			}},
		{kind: "item.remove", prep: nothing, actor: team(ana),
			session: func(ctx context.Context, w kindWorld, _ string) error {
				_, err := w.f.svc.RemoveAccount(ctx, w.tm.ana, w.tm.shared, service.RemoveAccountRequest{Confirm: w.tm.shared})
				return err
			},
			request: func(ctx context.Context, w kindWorld, a *service.PersonActs, _ string) (service.ActOutcome, error) {
				return a.Remove(ctx, w.tm.shared)
			}},
		{kind: "item.recover", prep: removeShared, actor: team(ana),
			session: func(ctx context.Context, w kindWorld, _ string) error {
				_, err := w.f.svc.RecoverAccount(ctx, w.tm.ana, w.tm.shared)
				return err
			},
			request: func(ctx context.Context, w kindWorld, a *service.PersonActs, _ string) (service.ActOutcome, error) {
				return a.Recover(ctx, w.tm.shared)
			}},
		{kind: "item.purge", prep: removeShared, actor: team(ana),
			session: func(ctx context.Context, w kindWorld, _ string) error {
				return w.f.svc.PurgeAccount(ctx, w.tm.ana, w.tm.shared, service.PurgeAccountRequest{Confirm: w.tm.shared})
			},
			request: func(ctx context.Context, w kindWorld, a *service.PersonActs, _ string) (service.ActOutcome, error) {
				return a.Purge(ctx, w.tm.shared)
			}},
		{kind: "item.removed_index_off", prep: removeShared, actor: team(ana),
			session: func(ctx context.Context, w kindWorld, _ string) error {
				_, err := w.f.svc.SetMailboxSync(ctx, w.tm.ana, w.tm.shared, switchSync(false))
				return err
			},
			request: func(ctx context.Context, w kindWorld, a *service.PersonActs, _ string) (service.ActOutcome, error) {
				return a.RemovedIndexOff(ctx, w.tm.shared)
			}},
		{kind: "item.removed_index_off (personal)", actor: anaOwn,
			prep: func(t *testing.T, w kindWorld) string { w.f.removed(t, w.tm.ana, w.own); return "" },
			session: func(ctx context.Context, w kindWorld, _ string) error {
				_, err := w.f.svc.SetMailboxSync(ctx, w.tm.ana, w.own, switchSync(false))
				return err
			},
			request: func(ctx context.Context, w kindWorld, a *service.PersonActs, _ string) (service.ActOutcome, error) {
				return a.RemovedIndexOff(ctx, w.own)
			}},
		{kind: "item.flags_raise", prep: nothing, actor: team(carol),
			session: func(ctx context.Context, w kindWorld, _ string) error {
				_, err := w.f.svc.SetAccess(ctx, w.tm.carol, w.tm.shared, w.tm.bea.UserID, grantRequest(true, true, true, false))
				return err
			},
			request: func(ctx context.Context, w kindWorld, a *service.PersonActs, _ string) (service.ActOutcome, error) {
				return a.RaiseFlags(ctx, w.tm.shared, w.tm.bea.UserID, service.GrantFlags{Read: true},
					service.GrantFlags{Read: true, Act: true, Send: true})
			}},
		{kind: "item.flags_drop", actor: team(carol),
			prep: func(t *testing.T, w kindWorld) string {
				w.tm.grant(t, w.tm.shared, workspace.Flags{Read: true, Send: true, Manage: true})
				return ""
			},
			session: func(ctx context.Context, w kindWorld, _ string) error {
				return w.f.svc.RevokeAccess(ctx, w.tm.carol, w.tm.shared, w.tm.bea.UserID, workspace.Flags{Send: true, Manage: true})
			},
			request: func(ctx context.Context, w kindWorld, a *service.PersonActs, _ string) (service.ActOutcome, error) {
				return a.DropFlags(ctx, w.tm.shared, w.tm.bea.UserID, service.GrantFlags{Send: true, Manage: true})
			}},
		{kind: "item.access_remove", prep: nothing, actor: team(carol),
			session: func(ctx context.Context, w kindWorld, _ string) error {
				return w.f.svc.RevokeAccess(ctx, w.tm.carol, w.tm.shared, w.tm.bea.UserID, workspace.Flags{})
			},
			request: func(ctx context.Context, w kindWorld, a *service.PersonActs, _ string) (service.ActOutcome, error) {
				return a.RemoveAccess(ctx, w.tm.shared, w.tm.bea.UserID)
			}},
		{kind: "consent.agree", prep: nothing, actor: anaPerson,
			session: func(ctx context.Context, w kindWorld, _ string) error {
				_, err := w.f.svc.GrantActionsConsent(ctx, w.tm.ana, w.f.consent().Actions)
				return err
			},
			request: func(ctx context.Context, w kindWorld, a *service.PersonActs, _ string) (service.ActOutcome, error) {
				return a.AgreeConsent(ctx, service.ConsentActions, w.f.consent().Actions)
			}},
		{kind: "consent.withdraw", prep: nothing, actor: anaPerson,
			session: func(ctx context.Context, w kindWorld, _ string) error {
				_, err := w.f.svc.WithdrawSyncConsent(ctx, w.tm.ana)
				return err
			},
			request: func(ctx context.Context, w kindWorld, a *service.PersonActs, _ string) (service.ActOutcome, error) {
				return a.WithdrawConsent(ctx, service.ConsentSync)
			}},
		{kind: "person.sign_out_everywhere", prep: nothing, actor: anaPerson,
			session: func(ctx context.Context, w kindWorld, _ string) error {
				return w.f.svc.SignOut(ctx, w.tm.ana, service.SignOutRequest{Everywhere: true})
			},
			request: func(ctx context.Context, w kindWorld, a *service.PersonActs, _ string) (service.ActOutcome, error) {
				return a.SignOutEverywhere(ctx, time.Now())
			}},
		{kind: "person.notices_dismiss", actor: func(w kindWorld) (service.Principal, string) { return w.tm.bea, "" },
			prep: func(t *testing.T, w kindWorld) string {
				w.f.pause(t, w.tm.ana, w.tm.shared)
				told := w.f.notices(t, w.tm.bea)
				if len(told) != 1 {
					t.Fatalf("Bea is told %+v", told)
				}
				return told[0].ID
			},
			session: func(ctx context.Context, w kindWorld, id string) error {
				return w.f.svc.DismissNotice(ctx, w.tm.bea, id)
			},
			request: func(ctx context.Context, w kindWorld, a *service.PersonActs, id string) (service.ActOutcome, error) {
				return a.DismissNotices(ctx, []string{id})
			}},
	}
}

func TestEachKindThroughAnExtensionDoesWhatItsSessionDoes(t *testing.T) {
	cases := kindCases()
	kinds := map[string]bool{}
	for _, c := range cases {
		kinds[strings.Fields(c.kind)[0]] = true
	}
	if len(kinds) != 17 {
		t.Fatalf("the cases cover %d kinds, want the 17 an extension carries", len(kinds))
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			ctx := t.Context()
			bySession, byRequest := newKindWorld(t), newKindWorld(t)
			argS, argR := c.prep(t, bySession), c.prep(t, byRequest)
			bySession.settled(t)
			byRequest.settled(t)
			namesS, namesR := bySession.names(t), byRequest.names(t)
			if before, other := bySession.digest(t, namesS), byRequest.digest(t, namesR); !slices.Equal(before, other) {
				t.Fatalf("the two copies differ before the act:\n%s\n---\n%s", strings.Join(before, "\n"), strings.Join(other, "\n"))
			}

			sessionErr := c.session(ctx, bySession, argS)
			p, ws := c.actor(byRequest)
			acts := byRequest.f.actsFor(t, service.Delegation{Extension: "ext", ID: "kind-1", UserID: p.UserID, WorkspaceID: ws,
				FirstIssued: time.Now(), KeepUntil: time.Now().Add(time.Hour)})
			out, requestErr := c.request(ctx, byRequest, acts, argR)

			if service.RefusalOf(sessionErr) != service.RefusalOf(requestErr) || (sessionErr == nil) != (requestErr == nil) {
				t.Fatalf("the session: %v; the request: %v", sessionErr, requestErr)
			}
			if requestErr == nil && out.Changed == c.unchanged {
				t.Errorf("the request's outcome: %+v", out)
			}
			got, want := byRequest.digest(t, namesR), bySession.digest(t, namesS)
			if !slices.Equal(got, want) {
				t.Errorf("the request left\n%s\nthe session left\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			if got, want := askedOf(byRequest.hook.take(), namesR), askedOf(bySession.hook.take(), namesS); !slices.Equal(got, want) {
				t.Errorf("the request asked the freshness hook %v, the session %v", got, want)
			}
			_, ran, err := byRequest.f.svc.RequestOutcome(ctx, "ext", "kind-1")
			if err != nil || ran != (requestErr == nil && c.kind != "item.sync_now") {
				t.Errorf("the request recorded: %v (%v)", ran, err)
			}
		})
	}
}
