package service_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/service/issuertest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Acts an extension runs for a person (delegated.go; docs/workspaces.md,
// "Acts an extension runs for a person"): the one way in, a person acting
// through an extension refused everything else, each request run once and
// recorded in its own write, and narrowings that never undo a newer
// widening.

// delegated is a request an extension carries for p, first asked at first,
// kept a day.
func delegated(p service.Principal, id, workspaceID string, first time.Time) service.Delegation {
	return service.Delegation{Extension: "ext", ID: id, UserID: p.UserID, WorkspaceID: workspaceID, FirstIssued: first,
		KeepUntil: time.Now().Add(24 * time.Hour)}
}

// actsFor is what an extension may run for d, and fails the test if it may
// run nothing.
func (f *fixture) actsFor(t *testing.T, d service.Delegation) *service.PersonActs {
	t.Helper()
	acts, err := f.svc.ActFor(t.Context(), d)
	if err != nil {
		t.Fatalf("ActFor(%s): %v", d.ID, err)
	}
	return acts
}

// wantRefusal fails the test unless err names refusal.
func wantRefusal(t *testing.T, what string, err error, refusal service.Refusal) {
	t.Helper()
	if got := service.RefusalOf(err); got != refusal {
		t.Errorf("%s: %v, refused %q, want %q", what, err, got, refusal)
	}
}

// ago is now less d, to the second a stored moment has.
func ago(d time.Duration) time.Time { return time.Now().Add(-d) }

func TestEveryExportedMethodRefusesAPersonActingThroughAnExtension(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleOwner)
	person := service.Principal{Kind: auth.KindPerson, UserID: ana.UserID, UserRole: auth.RoleOwner, Scope: auth.ScopeAdmin}
	principal := reflect.TypeFor[service.Principal]()
	ctxType := reflect.TypeFor[context.Context]()
	svc := reflect.ValueOf(f.svc)
	called := 0
	for i := range svc.NumMethod() {
		m := svc.Type().Method(i)
		takes := false
		args := []reflect.Value{}
		for j := 1; j < m.Type.NumIn(); j++ {
			switch in := m.Type.In(j); in {
			case principal:
				takes = true
				args = append(args, reflect.ValueOf(person))
			case ctxType:
				args = append(args, reflect.ValueOf(t.Context()))
			default:
				args = append(args, reflect.Zero(in))
			}
		}
		if !takes {
			continue
		}
		called++
		method := svc.Method(i)
		var out []reflect.Value
		if m.Type.IsVariadic() {
			out = method.CallSlice(args)
		} else {
			out = method.Call(args)
		}
		err, _ := out[len(out)-1].Interface().(error)
		if !errors.Is(err, service.ErrNeedsOwnSession) || service.CodeOf(err) != service.CodeNotAuthorized {
			t.Errorf("%s with a person acting through an extension: %v, want not_authorized (ErrNeedsOwnSession)", m.Name, err)
		}
	}
	if called < 90 {
		t.Errorf("only %d methods take a principal; the test would prove little", called)
	}
}

func TestEveryKeySideOfADelegatedUseCaseRefusesAPersonActingThroughAnExtension(t *testing.T) {
	f := newFixture(t)
	tm := syncedTeam(t, f)
	person := service.Principal{Kind: auth.KindPerson, UserID: tm.ana.UserID, Scope: auth.ScopeAdmin}
	for side, err := range service.KeySidesForTest(t.Context(), f.svc, person, tm.shared, tm.bea.UserID) {
		if !errors.Is(err, service.ErrNeedsOwnSession) {
			t.Errorf("%s, for a person acting through an extension: %v, want ErrNeedsOwnSession", side, err)
		}
	}
}

func TestAPersonActingThroughAnExtensionNeverPassesAStepUpNorWritesAKeyOrAGrant(t *testing.T) {
	f := newFixture(t)
	tm := syncedTeam(t, f)
	f.stepUp(t, tm.ana)
	acts := f.actsFor(t, delegated(tm.ana, "r1", tm.id, time.Now()))
	before := f.count(t, `SELECT count(*) FROM mailbox_grants`) + f.count(t, `SELECT count(*) FROM mailbox_keys`)
	// Read is given with a grant, which only a session's browser seals.
	_, err := acts.RaiseFlags(t.Context(), tm.shared, tm.carol.UserID, service.GrantFlags{}, service.GrantFlags{Read: true})
	wantCode(t, "a raise that gives read", err, service.CodeBadRequest)
	_, err = acts.RaiseFlags(t.Context(), tm.shared, tm.bea.UserID, service.GrantFlags{Read: true}, service.GrantFlags{})
	wantCode(t, "a raise that takes read", err, service.CodeBadRequest)
	if after := f.count(t, `SELECT count(*) FROM mailbox_grants`) + f.count(t, `SELECT count(*) FROM mailbox_keys`); after != before {
		t.Errorf("a person acting through an extension changed the keys and grants: %d, were %d", after, before)
	}
	// The step-up a session holds is not theirs: every act that asks one is
	// a session's method, refused first.
	_, err = f.svc.MailboxKey(t.Context(), service.Principal{Kind: auth.KindPerson, UserID: tm.ana.UserID,
		Scope: auth.ScopeAdmin}, tm.shared)
	wantRefusal(t, "a mailbox key read through an extension", err, service.RefusalNeedsOwnSession)
}

func TestNoTransportEverAuthenticatesAPersonWithoutASession(t *testing.T) {
	w := newIssuedWorld(t, fixtureOptions{})
	ctx := t.Context()
	issued, _ := w.issue(t, issuertest.Key{Grants: []workspace.KeyGrant{{AccountID: w.box, Flags: workspace.Flags{Read: true}}}})
	presented := map[string]string{
		"a session":       authtest.SignIn(t, w.users, "ana@example.com"),
		"an instance key": authtest.NewKey(t, w.db, auth.ScopeAdmin),
		"a workspace key": authtest.NewWorkspaceKey(t, w.db, auth.ScopeRead, w.ws, w.ana.UserID,
			workspace.KeyGrant{AccountID: w.box, Flags: workspace.Flags{Read: true}}),
		"an issued key": issued,
	}
	authenticated := 0
	for what, raw := range presented {
		for tool, authenticate := range map[bool]func(context.Context, string, auth.Gate) (service.Principal, error){
			false: w.svc.Authenticate, true: w.svc.AuthenticateTool,
		} {
			p, err := authenticate(ctx, raw, nil)
			if err != nil {
				continue
			}
			authenticated++
			if p.IsDelegated() || p.Kind == auth.KindPerson || (p.IsPerson() && !p.IsSession()) {
				t.Errorf("%s (tool %v) authenticates as a person acting through an extension: %+v", what, tool, p)
			}
		}
	}
	if authenticated < 6 {
		t.Errorf("only %d of the credentials authenticated; the test would prove little", authenticated)
	}
	// The session check never takes one for a session.
	err := w.svc.Recheck(ctx, service.Principal{Kind: auth.KindPerson, UserID: w.ana.UserID, Scope: auth.ScopeAdmin})
	wantRefusal(t, "a recheck of a person acting through an extension", err, service.RefusalNeedsOwnSession)
}

// transportNever is what no transport names.
var transportNever = regexp.MustCompile(`\b(ActFor|KindPerson|PersonActs|Delegation|IsDelegated)\b`)

func TestTheTransportsNeverNameActForNorKindPerson(t *testing.T) {
	for _, dir := range []string{"../api", "../mcp"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no sources in %s: %v", dir, err)
		}
		for _, file := range files {
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if name := transportNever.FindString(string(src)); name != "" {
				t.Errorf("%s names %s: no transport reaches a person acting through an extension", file, name)
			}
		}
	}
}

func TestAnActThatCommittedAlwaysAnswersRanWhateverFollowsItsCommit(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	acts := f.actsFor(t, delegated(ana, "r1", "", time.Now()))
	out, err := service.RunForTest(t.Context(), acts, "withdraw_consent", "", func(req *store.Request) (store.RequestOutcome, error) {
		if _, _, err := f.db.WithdrawActionsConsentFor(t.Context(), req, ana.UserID); err != nil {
			return store.RequestOutcome{}, err
		}
		return store.RequestOutcome{Changed: true}, errors.New("something after the commit failed")
	})
	if err != nil || !out.Ran {
		t.Fatalf("an act whose write committed and whose aftermath failed: %+v, %v; want what it recorded", out, err)
	}
	if out, ran, err := f.svc.RequestOutcome(t.Context(), "ext", "r1"); err != nil || !ran || out.Changed {
		t.Errorf("its record: %+v, ran %v, %v (a consent that was off: nothing changed)", out, ran, err)
	}
}

func TestADisabledSuspendedOrDeletedPersonActsOnNothingThroughAnExtension(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	own := f.ownSynced(t, tm.bea, "bea@mail.example")

	// Suspended: nothing but their own narrowings.
	f.suspend(t, tm.bea.UserID)
	acts := f.actsFor(t, delegated(tm.bea, "s1", authtest.Personal(t, f.db, tm.bea.UserID), time.Now()))
	_, err := acts.PauseSync(ctx, own)
	wantRefusal(t, "a suspended person pausing their mailbox", err, service.RefusalActorInactive)
	f.resume(t, tm.bea.UserID)

	// Disabled here, or deleted: nothing at all.
	if _, err := f.svc.DisableUser(ctx, admin(), service.CloseUserRequest{Email: "carol@example.com"}); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.ActFor(ctx, delegated(tm.carol, "d1", tm.id, time.Now()))
	wantRefusal(t, "a person disabled here", err, service.RefusalActorInactive)
	_, err = f.svc.ActFor(ctx, service.Delegation{Extension: "ext", ID: "g1", UserID: "usr_0000000000009999",
		FirstIssued: time.Now(), KeepUntil: time.Now().Add(time.Hour)})
	wantRefusal(t, "a person nobody knows", err, service.RefusalActorInactive)

	// Suspended after the extension asked, before the write: refused in the
	// write, which changes nothing.
	hooked := newFixtureWith(t, fixtureOptions{freshness: &freshHook{}})
	team := syncedTeam(t, hooked)
	hook := hooked.opts.freshness.(*freshHook)
	hook.answerWith(func(ctx context.Context, _ service.FreshAct) error {
		_, err := hooked.svc.SuspendPerson(ctx, team.ana.UserID)
		return err
	})
	acts = hooked.actsFor(t, delegated(team.ana, "w1", team.id, time.Now()))
	_, err = acts.PauseSync(ctx, team.shared)
	wantRefusal(t, "a person suspended while the hook was asked", err, service.RefusalActorInactive)
	if hooked.paused(t, team.shared) {
		t.Error("the refused pause paused the mailbox")
	}
	if _, ran, _ := hooked.svc.RequestOutcome(ctx, "ext", "w1"); ran {
		t.Error("the refused request was recorded")
	}
}

func TestAnItemOfAnotherWorkspaceIsNotFoundToAPersonActingThroughAnExtension(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	own := f.ownSynced(t, tm.ana, "ana@mail.example")
	personal := authtest.Personal(t, f.db, tm.ana.UserID)
	for what, d := range map[string]service.Delegation{
		"the team's mailbox named in her personal workspace": delegated(tm.ana, "r1", personal, time.Now()),
		"her own mailbox named in the team":                  delegated(tm.ana, "r2", tm.id, time.Now()),
	} {
		box := tm.shared
		if d.WorkspaceID == tm.id {
			box = own
		}
		_, err := f.actsFor(t, d).PauseSync(ctx, box)
		wantRefusal(t, what, err, service.RefusalNotFound)
	}
	if f.paused(t, tm.shared) || f.paused(t, own) {
		t.Error("an act named in another workspace paused a mailbox")
	}
	// A person's own act names no workspace, and an act on a mailbox one.
	_, err := f.actsFor(t, delegated(tm.ana, "r3", tm.id, time.Now())).WithdrawConsent(ctx, service.ConsentSend)
	wantCode(t, "a person's own act naming a workspace", err, service.CodeBadRequest)
	_, err = f.actsFor(t, delegated(tm.ana, "r4", "", time.Now())).PauseSync(ctx, tm.shared)
	wantCode(t, "an act on a mailbox naming none", err, service.CodeBadRequest)
}

func TestTheFreshnessHookIsAskedOfAPersonActingThroughAnExtensionAsOfASession(t *testing.T) {
	hook := &freshHook{}
	f := newFixtureWith(t, fixtureOptions{freshness: hook})
	ctx := t.Context()
	tm := syncedTeam(t, f)
	hook.take()
	if _, err := f.actsFor(t, delegated(tm.ana, "r1", tm.id, time.Now())).PauseSync(ctx, tm.shared); err != nil {
		t.Fatal(err)
	}
	asked := hook.take()
	if len(asked) != 1 || asked[0].Kind != service.FreshTeamSync || asked[0].ActorID != tm.ana.UserID ||
		asked[0].AccountID != tm.shared {
		t.Errorf("a team pause through an extension asked %+v", asked)
	}
	if _, err := f.actsFor(t, delegated(tm.carol, "r2", tm.id, time.Now())).RaiseFlags(ctx, tm.shared, tm.bea.UserID,
		service.GrantFlags{Read: true}, service.GrantFlags{Read: true, Send: true}); err != nil {
		t.Fatal(err)
	}
	asked = hook.take()
	if len(asked) != 1 || asked[0].Kind != service.FreshRaiseFlags || asked[0].ActorID != tm.carol.UserID ||
		len(asked[0].RecipientIDs) != 1 || asked[0].RecipientIDs[0] != tm.bea.UserID {
		t.Errorf("a raise through an extension asked %+v", asked)
	}
	f.exec(t, `UPDATE mailbox_access SET raised_at = raised_at - 60 WHERE account_id = ?`, tm.shared)
	// Taking away widens nothing, and asks nothing.
	if _, err := f.actsFor(t, delegated(tm.carol, "r3", tm.id, time.Now())).DropFlags(ctx, tm.shared, tm.bea.UserID,
		service.GrantFlags{Send: true}); err != nil {
		t.Fatal(err)
	}
	if asked := hook.take(); len(asked) != 0 {
		t.Errorf("a drop through an extension asked %+v", asked)
	}
	// Nor a drop of read, nor an access removal: the last reader's refusal
	// is this server's own.
	if _, err := f.actsFor(t, delegated(tm.carol, "r4", tm.id, time.Now())).DropFlags(ctx, tm.shared, tm.bea.UserID,
		service.GrantFlags{Read: true}); err != nil {
		t.Fatal(err)
	}
	tm.grant(t, tm.shared, workspace.Flags{Read: true})
	f.exec(t, `UPDATE mailbox_access SET raised_at = raised_at - 60 WHERE account_id = ?`, tm.shared)
	if out, err := f.actsFor(t, delegated(tm.carol, "r5", tm.id, time.Now())).RemoveAccess(ctx, tm.shared, tm.bea.UserID); err != nil ||
		!out.Changed {
		t.Fatalf("an access removal: %+v, %v", out, err)
	}
	if asked := hook.take(); len(asked) != 0 {
		t.Errorf("a drop of read and an access removal through an extension asked %+v", asked)
	}
}

func TestAHooksRefusalKeepsItsCause(t *testing.T) {
	hook := &freshHook{}
	f := newFixtureWith(t, fixtureOptions{freshness: hook})
	ctx := t.Context()
	tm := syncedTeam(t, f)
	inactive := &service.Error{Code: service.CodeUnauthorized, Message: "the session has ended", Err: service.ErrActorInactive}
	unconfirmed := &service.Error{Code: service.CodeInternal, Message: "upstream: not now", Retry: time.Minute,
		Err: service.ErrFreshUnconfirmed}
	for i, c := range []struct {
		answer  error
		refusal service.Refusal
		code    service.Code
	}{
		{inactive, service.RefusalActorInactive, service.CodeUnauthorized},
		{unconfirmed, service.RefusalFreshUnconfirmed, service.CodeInternal},
		{errors.New("the directory did not answer"), service.RefusalFreshUnconfirmed, service.CodeInternal},
	} {
		hook.answerWith(func(context.Context, service.FreshAct) error { return c.answer })
		_, err := f.actsFor(t, delegated(tm.ana, "r"+string(rune('1'+i)), tm.id, time.Now())).PauseSync(ctx, tm.shared)
		wantRefusal(t, "the hook answering "+c.answer.Error(), err, c.refusal)
		wantCode(t, "the hook answering "+c.answer.Error(), err, c.code)
		// A session is answered as before.
		_, err = f.svc.PauseMailboxSync(ctx, tm.ana, tm.shared)
		wantCode(t, "a session, the hook answering "+c.answer.Error(), err, c.code)
	}
	if got := service.RetryAfterOf(func() error {
		_, err := f.svc.PauseMailboxSync(ctx, tm.ana, tm.shared)
		return err
	}()); got != 30*time.Second {
		t.Errorf("a hook that could not be asked says to wait %v", got)
	}
}

func TestARequestRunsOnceWhateverItsRetries(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	pauses := func() int {
		return f.count(t, `SELECT count(*) FROM mailbox_events WHERE kind = 'paused' AND account_id = ?`, tm.shared)
	}
	d := delegated(tm.ana, "r1", tm.id, time.Now())
	var wg sync.WaitGroup
	outcomes := make([]service.ActOutcome, 8)
	errs := make([]error, 8)
	for i := range outcomes {
		wg.Go(func() {
			acts, err := f.svc.ActFor(ctx, d)
			if err != nil {
				errs[i] = err
				return
			}
			outcomes[i], errs[i] = acts.PauseSync(ctx, tm.shared)
		})
	}
	wg.Wait()
	first := 0
	for i := range outcomes {
		switch {
		case errs[i] != nil:
			t.Errorf("run %d: %v", i, errs[i])
		case !outcomes[i].Changed:
			t.Errorf("run %d answered %+v, want the first outcome: paused", i, outcomes[i])
		case !outcomes[i].Ran:
			first++
		}
	}
	if first != 1 || pauses() != 1 {
		t.Errorf("%d runs paused it, and it was paused %d times; want one", first, pauses())
	}
	// Resumed since: the request asked again answers what it did, and
	// pauses nothing again.
	if _, err := f.svc.ResumeMailboxSync(ctx, tm.ana, tm.shared, service.SyncResumeRequest{Version: f.consent().Sync}); err != nil {
		t.Fatal(err)
	}
	out, err := f.actsFor(t, d).PauseSync(ctx, tm.shared)
	if err != nil || !out.Ran || !out.Changed || f.paused(t, tm.shared) || pauses() != 1 {
		t.Errorf("the request again after a resume: %+v, %v; paused %v", out, err, f.paused(t, tm.shared))
	}
}

func TestARequestThatRanAnswersItsFirstOutcomeBeforeAnyCheck(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	f.exec(t, `UPDATE mailbox_access SET raised_at = raised_at - 60 WHERE account_id = ?`, tm.shared)
	d := delegated(tm.carol, "r1", tm.id, time.Now())
	if out, err := f.actsFor(t, d).DropFlags(ctx, tm.shared, tm.bea.UserID, service.GrantFlags{Read: true}); err != nil || !out.Changed {
		t.Fatalf("Carol taking Bea's read: %+v, %v", out, err)
	}
	// Carol is no longer anyone here, and the mailbox is gone.
	if _, err := f.svc.DeleteUser(ctx, admin(), service.CloseUserRequest{Email: "carol@example.com", Force: true}); err != nil {
		t.Fatal(err)
	}
	f.removed(t, tm.ana, tm.shared)
	acts := f.actsFor(t, d)
	for what, act := range map[string]func() (service.ActOutcome, error){
		"the same act": func() (service.ActOutcome, error) {
			return acts.DropFlags(ctx, tm.shared, tm.bea.UserID, service.GrantFlags{Read: true})
		},
		"another act":    func() (service.ActOutcome, error) { return acts.PauseSync(ctx, tm.shared) },
		"a person's act": func() (service.ActOutcome, error) { return acts.WithdrawConsent(ctx, service.ConsentSync) },
	} {
		if out, err := act(); err != nil || !out.Ran || !out.Changed {
			t.Errorf("%s of a request that ran: %+v, %v, want its first outcome", what, out, err)
		}
	}
}

func TestARefusedRequestRecordsNothing(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	_, err := f.actsFor(t, delegated(tm.bea, "r1", tm.id, time.Now())).PauseSync(ctx, tm.shared)
	wantRefusal(t, "a member pausing a team mailbox", err, service.RefusalNotAuthorized)
	if _, ran, err := f.svc.RequestOutcome(ctx, "ext", "r1"); err != nil || ran {
		t.Errorf("the refused request was recorded: %v, %v", ran, err)
	}
	// Made an admin, the same request runs.
	f.exec(t, `UPDATE workspace_members SET role = 'admin' WHERE workspace_id = ? AND user_id = ?`, tm.id, tm.bea.UserID)
	if out, err := f.actsFor(t, delegated(tm.bea, "r1", tm.id, time.Now())).PauseSync(ctx, tm.shared); err != nil ||
		!out.Changed || out.Ran {
		t.Errorf("the request again, allowed now: %+v, %v", out, err)
	}
}

func TestANoOpRequestIsRecordedAndAnswersItsFirstOutcomeAfterACrash(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	resume := func() {
		t.Helper()
		if _, err := f.svc.ResumeMailboxSync(ctx, tm.ana, tm.shared, service.SyncResumeRequest{Version: f.consent().Sync}); err != nil {
			t.Fatal(err)
		}
	}
	once := func(what, id string, act func(*service.PersonActs) (service.ActOutcome, error), change func(), unchanged func() bool) {
		t.Helper()
		d := delegated(tm.ana, id, tm.id, time.Now())
		if out, err := act(f.actsFor(t, d)); err != nil || out.Changed || out.Ran {
			t.Errorf("%s: %+v, %v, want done with nothing changed", what, out, err)
		}
		change()
		if out, err := act(f.actsFor(t, d)); err != nil || out.Changed || !out.Ran {
			t.Errorf("%s again, after what it found changed: %+v, %v, want its first outcome", what, out, err)
		}
		if !unchanged() {
			t.Errorf("%s again ran a second time", what)
		}
	}
	f.pause(t, tm.ana, tm.shared)
	once("a pause of a paused mailbox", "r1",
		func(a *service.PersonActs) (service.ActOutcome, error) { return a.PauseSync(ctx, tm.shared) },
		resume, func() bool { return !f.paused(t, tm.shared) })
	once("a resume of a mailbox not paused", "r2",
		func(a *service.PersonActs) (service.ActOutcome, error) {
			return a.ResumeSync(ctx, tm.shared, f.consent().Sync)
		},
		func() { f.pause(t, tm.ana, tm.shared) }, func() bool { return f.paused(t, tm.shared) })
	resume()
	if _, err := f.svc.WithdrawSendConsent(ctx, tm.ana); err != nil {
		t.Fatal(err)
	}
	personal := func(id string) service.Delegation { return delegated(tm.ana, id, "", time.Now()) }
	d := personal("r3")
	if out, err := f.actsFor(t, d).WithdrawConsent(ctx, service.ConsentSend); err != nil || out.Changed {
		t.Errorf("a withdrawal of a consent off: %+v, %v", out, err)
	}
	if _, err := f.svc.GrantSendConsent(ctx, tm.ana, f.consent().Send); err != nil {
		t.Fatal(err)
	}
	if out, err := f.actsFor(t, d).WithdrawConsent(ctx, service.ConsentSend); err != nil || !out.Ran || out.Changed {
		t.Errorf("the withdrawal again, agreed since: %+v, %v", out, err)
	}
	if f.count(t, `SELECT send_consent_at FROM users WHERE id = ?`, tm.ana.UserID) == 0 {
		t.Error("the withdrawal asked again took the agreement given since")
	}
	if out, err := f.actsFor(t, delegated(tm.ana, "r4", tm.id, time.Now())).DropFlags(ctx, tm.shared, tm.bea.UserID,
		service.GrantFlags{Send: true}); err != nil || out.Changed {
		t.Errorf("a drop of a flag not held: %+v, %v", out, err)
	}
}

func TestALateWithdrawalNeverUndoesANewerAgreement(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	for _, c := range []struct {
		which  service.Consent
		column string
		agree  func() error
	}{
		{service.ConsentSync, "sync_consent_at", func() error {
			_, err := f.svc.GrantSyncConsent(ctx, tm.ana, f.consent().Sync)
			return err
		}},
		{service.ConsentActions, "actions_consent_at", func() error {
			_, err := f.svc.GrantActionsConsent(ctx, tm.ana, f.consent().Actions)
			return err
		}},
		{service.ConsentSend, "send_consent_at", func() error {
			_, err := f.svc.GrantSendConsent(ctx, tm.ana, f.consent().Send)
			return err
		}},
	} {
		if err := c.agree(); err != nil {
			t.Fatal(err)
		}
		// Asked a minute before the agreement: it takes nothing.
		_, err := f.actsFor(t, delegated(tm.ana, "early-"+string(c.which), "", ago(time.Minute))).WithdrawConsent(ctx, c.which)
		wantRefusal(t, "a "+string(c.which)+" withdrawal asked before the agreement", err, service.RefusalSetSince)
		wantCode(t, "a "+string(c.which)+" withdrawal asked before the agreement", err, service.CodeConflict)
		if f.count(t, `SELECT `+c.column+` FROM users WHERE id = ?`, tm.ana.UserID) == 0 {
			t.Errorf("the late %s withdrawal took the newer agreement", c.which)
		}
		// Asked after it: it runs.
		f.exec(t, `UPDATE users SET `+c.column+` = `+c.column+` - 300 WHERE id = ?`, tm.ana.UserID)
		out, err := f.actsFor(t, delegated(tm.ana, "late-"+string(c.which), "", ago(time.Minute))).WithdrawConsent(ctx, c.which)
		if err != nil || !out.Changed {
			t.Errorf("a %s withdrawal asked after the agreement: %+v, %v", c.which, out, err)
		}
	}
	// A team's agreement, the same.
	_, err := f.actsFor(t, delegated(tm.ana, "early-team", tm.id, ago(time.Minute))).TeamSync(ctx, tm.shared, false, "")
	wantRefusal(t, "a team's turn-off asked before its agreement", err, service.RefusalSetSince)
	if f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, tm.shared) == 0 {
		t.Error("the late turn-off took the team's newer agreement")
	}
	f.exec(t, `UPDATE accounts SET sync_enabled_at = sync_enabled_at - 300 WHERE id = ?`, tm.shared)
	if out, err := f.actsFor(t, delegated(tm.ana, "late-team", tm.id, ago(time.Minute))).TeamSync(ctx, tm.shared, false, ""); err != nil ||
		!out.Changed {
		t.Errorf("a team's turn-off asked after its agreement: %+v, %v", out, err)
	}
}

func TestAWideningThroughAnExtensionIsStampedWithItsIntentsFirstMoment(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	f.pause(t, tm.ana, tm.shared)
	// The team agreed an hour ago.
	f.exec(t, `UPDATE accounts SET sync_enabled_at = sync_enabled_at - 3600 WHERE id = ?`, tm.shared)
	// Ana's resume is asked first; Carol's turn-off after it; the resume
	// runs last. The order they were asked in decides: the turn-off runs.
	resumeAsked, offAsked := ago(10*time.Minute), ago(5*time.Minute)
	if _, err := f.actsFor(t, delegated(tm.ana, "resume", tm.id, resumeAsked)).ResumeSync(ctx, tm.shared, f.consent().Sync); err != nil {
		t.Fatal(err)
	}
	if got := int64(f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, tm.shared)); got != resumeAsked.Unix() {
		t.Errorf("the resume's agreement is recorded at %d, want when it was asked, %d", got, resumeAsked.Unix())
	}
	if out, err := f.actsFor(t, delegated(tm.carol, "off", tm.id, offAsked)).TeamSync(ctx, tm.shared, false, ""); err != nil || !out.Changed {
		t.Errorf("a turn-off asked after a resume that ran before it: %+v, %v", out, err)
	}
	// An agreement records when the person asked.
	asked := ago(3 * time.Minute)
	if _, err := f.actsFor(t, delegated(tm.bea, "agree", "", asked)).AgreeConsent(ctx, service.ConsentActions, f.consent().Actions); err != nil {
		t.Fatal(err)
	}
	if got := int64(f.count(t, `SELECT actions_consent_at FROM users WHERE id = ?`, tm.bea.UserID)); got != asked.Unix() {
		t.Errorf("the agreement is recorded at %d, want when it was asked, %d", got, asked.Unix())
	}
	_, err := f.actsFor(t, delegated(tm.bea, "agree-old", "", asked)).AgreeConsent(ctx, service.ConsentSend, "an-older-text")
	wantRefusal(t, "an agreement to another text", err, service.RefusalNotCurrentText)
}

func TestALateResumeThroughAnExtensionNeverMovesANewerAgreementBack(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	f.pause(t, tm.ana, tm.shared)
	// Ana's resume is asked first and Carol's turn-off after it; both wait.
	// Meanwhile Ana resumes and pauses again on her own screen: an agreement
	// newer than Carol's turn-off, which it must refuse.
	resumeAsked, offAsked := ago(10*time.Minute), ago(5*time.Minute)
	if _, err := f.svc.ResumeMailboxSync(ctx, tm.ana, tm.shared, service.SyncResumeRequest{Version: f.consent().Sync}); err != nil {
		t.Fatal(err)
	}
	agreed := f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, tm.shared)
	f.pause(t, tm.ana, tm.shared)
	if out, err := f.actsFor(t, delegated(tm.ana, "resume", tm.id, resumeAsked)).ResumeSync(ctx, tm.shared, f.consent().Sync); err != nil ||
		!out.Changed {
		t.Fatalf("the late resume: %+v, %v", out, err)
	}
	if got := f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, tm.shared); got != agreed {
		t.Errorf("the late resume moved the agreement from %d to %d", agreed, got)
	}
	_, err := f.actsFor(t, delegated(tm.carol, "off", tm.id, offAsked)).TeamSync(ctx, tm.shared, false, "")
	wantRefusal(t, "a turn-off asked before the newer agreement", err, service.RefusalSetSince)
	if f.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, tm.shared) == 0 {
		t.Error("the turn-off withdrew the newer agreement")
	}
}

func TestALateRaiseThroughAnExtensionNeverMovesANewerRaiseBack(t *testing.T) {
	for _, giveBack := range []string{"on a screen", "through a request asked after the removal"} {
		t.Run(giveBack, func(t *testing.T) {
			f := newFixture(t)
			ctx := t.Context()
			tm := syncedTeam(t, f)
			tm.grant(t, tm.shared, workspace.Flags{Read: true, Send: true})
			raisedAt := `SELECT raised_at FROM mailbox_access WHERE account_id = ? AND user_id = ?`
			f.exec(t, `UPDATE mailbox_access SET raised_at = raised_at - 3600 WHERE account_id = ? AND user_id = ?`,
				tm.shared, tm.bea.UserID)
			// Ana's raise of act, drawn from read and send, is asked first,
			// and Carol's removal of Bea's access after it; both wait.
			// Meanwhile send is taken away and given back: a flag given
			// after Carol asked, which her removal must not take.
			raiseAsked, removalAsked := ago(10*time.Minute), ago(5*time.Minute)
			if err := f.svc.RevokeAccess(ctx, tm.ana, tm.shared, tm.bea.UserID, workspace.Flags{Send: true}); err != nil {
				t.Fatal(err)
			}
			if giveBack == "on a screen" {
				if _, err := f.svc.SetAccess(ctx, tm.ana, tm.shared, tm.bea.UserID, grantRequest(true, false, true, false)); err != nil {
					t.Fatal(err)
				}
			} else if out, err := f.actsFor(t, delegated(tm.ana, "give-back", tm.id, ago(2*time.Minute))).RaiseFlags(ctx,
				tm.shared, tm.bea.UserID, service.GrantFlags{Read: true}, service.GrantFlags{Read: true, Send: true}); err != nil ||
				!out.Changed {
				t.Fatalf("the give-back: %+v, %v", out, err)
			}
			raised := f.count(t, raisedAt, tm.shared, tm.bea.UserID)
			if out, err := f.actsFor(t, delegated(tm.ana, "raise", tm.id, raiseAsked)).RaiseFlags(ctx, tm.shared, tm.bea.UserID,
				service.GrantFlags{Read: true, Send: true}, service.GrantFlags{Read: true, Act: true, Send: true}); err != nil ||
				!out.Changed {
				t.Fatalf("the late raise: %+v, %v", out, err)
			}
			if got := f.count(t, raisedAt, tm.shared, tm.bea.UserID); got != raised {
				t.Errorf("the late raise moved the grant's raise from %d back to %d", raised, got)
			}
			_, err := f.actsFor(t, delegated(tm.carol, "remove", tm.id, removalAsked)).RemoveAccess(ctx, tm.shared, tm.bea.UserID)
			wantRefusal(t, "a removal asked before send was given back", err, service.RefusalSetSince)
			if g := held(t, f, tm.ana, tm.id, tm.shared, tm.bea.UserID); !g.Read || !g.Act || !g.Send {
				t.Errorf("the removal asked before send was given back left Bea %+v", g)
			}
		})
	}
}

func TestANarrowingNeverBlocksAnotherNarrowing(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	tm.grant(t, tm.shared, workspace.Flags{Read: true, Act: true, Send: true, Manage: true})
	f.exec(t, `UPDATE mailbox_access SET raised_at = raised_at - 600 WHERE account_id = ? AND user_id = ?`, tm.shared, tm.bea.UserID)
	asked := ago(5 * time.Minute)
	for i, step := range []struct {
		what    string
		act     func(*service.PersonActs) (service.ActOutcome, error)
		changed bool
	}{
		{"send dropped", func(a *service.PersonActs) (service.ActOutcome, error) {
			return a.DropFlags(ctx, tm.shared, tm.bea.UserID, service.GrantFlags{Send: true})
		}, true},
		{"manage dropped after it", func(a *service.PersonActs) (service.ActOutcome, error) {
			return a.DropFlags(ctx, tm.shared, tm.bea.UserID, service.GrantFlags{Manage: true})
		}, true},
		{"access removed after a drop", func(a *service.PersonActs) (service.ActOutcome, error) {
			return a.RemoveAccess(ctx, tm.shared, tm.bea.UserID)
		}, true},
		{"a drop after the removal", func(a *service.PersonActs) (service.ActOutcome, error) {
			return a.DropFlags(ctx, tm.shared, tm.bea.UserID, service.GrantFlags{Act: true})
		}, false},
	} {
		out, err := step.act(f.actsFor(t, delegated(tm.carol, "n"+string(rune('1'+i)), tm.id, asked)))
		if err != nil || out.Changed != step.changed {
			t.Errorf("%s, every one asked at the same moment: %+v, %v", step.what, out, err)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM mailbox_access WHERE account_id = ? AND user_id = ?`, tm.shared, tm.bea.UserID); n != 0 {
		t.Errorf("Bea still holds %d grants", n)
	}
}

func TestTakingAFlagAwayFromARowRaisedSinceIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	asked := ago(time.Minute)
	tm.grant(t, tm.shared, workspace.Flags{Read: true, Send: true})
	_, err := f.actsFor(t, delegated(tm.carol, "r1", tm.id, asked)).DropFlags(ctx, tm.shared, tm.bea.UserID, service.GrantFlags{Read: true})
	wantRefusal(t, "a drop of read given again since", err, service.RefusalSetSince)
	_, err = f.actsFor(t, delegated(tm.carol, "r2", tm.id, asked)).RemoveAccess(ctx, tm.shared, tm.bea.UserID)
	wantRefusal(t, "an access removal of a grant raised since", err, service.RefusalSetSince)
	if g := held(t, f, tm.ana, tm.id, tm.shared, tm.bea.UserID); !g.Read || !g.Send {
		t.Errorf("the refused narrowings left Bea %+v", g)
	}
	// What the grant does not hold is gone already: done.
	if out, err := f.actsFor(t, delegated(tm.carol, "r3", tm.id, asked)).DropFlags(ctx, tm.shared, tm.bea.UserID,
		service.GrantFlags{Manage: true}); err != nil || out.Changed {
		t.Errorf("a drop of a flag not held: %+v, %v", out, err)
	}
}

func TestANarrowingWithNothingLeftToRemoveIsDone(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	dan := tm.join(t, f, "dan@example.com", workspace.RoleMember)
	for what, act := range map[string]func(*service.PersonActs) (service.ActOutcome, error){
		"a drop from someone holding nothing": func(a *service.PersonActs) (service.ActOutcome, error) {
			return a.DropFlags(ctx, tm.shared, dan.UserID, service.GrantFlags{Send: true})
		},
		"an access removal of someone holding nothing": func(a *service.PersonActs) (service.ActOutcome, error) {
			return a.RemoveAccess(ctx, tm.shared, dan.UserID)
		},
	} {
		if out, err := act(f.actsFor(t, delegated(tm.ana, "n-"+strings.Fields(what)[1], tm.id, time.Now()))); err != nil || out.Changed {
			t.Errorf("%s: %+v, %v", what, out, err)
		}
	}
}

func TestRaisingFlagsFromAStaleValueIsRefusedInTheWrite(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	// Drawn while Bea held read; read was taken away since.
	if err := f.svc.RevokeAccess(ctx, tm.ana, tm.shared, tm.bea.UserID, workspace.Flags{}); err != nil {
		t.Fatal(err)
	}
	_, err := f.actsFor(t, delegated(tm.ana, "r1", tm.id, time.Now())).RaiseFlags(ctx, tm.shared, tm.bea.UserID,
		service.GrantFlags{Read: true}, service.GrantFlags{Read: true, Act: true})
	wantRefusal(t, "a raise drawn while Bea read", err, service.RefusalStateChanged)
	if n := f.count(t, `SELECT count(*) FROM mailbox_access WHERE account_id = ? AND user_id = ?`, tm.shared, tm.bea.UserID); n != 0 {
		t.Error("the stale raise gave Bea read back")
	}
	// Drawn from what she holds: it runs.
	if out, err := f.actsFor(t, delegated(tm.ana, "r2", tm.id, time.Now())).RaiseFlags(ctx, tm.shared, tm.bea.UserID,
		service.GrantFlags{}, service.GrantFlags{Manage: true}); err != nil || !out.Changed {
		t.Errorf("a raise drawn from what she holds: %+v, %v", out, err)
	}
}

func TestGivingReadFromAStaleValueIsRefusedInTheWrite(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	drawn := service.GrantFlags{Read: true, Send: true}
	// Send was taken away after the page drew Bea's flags.
	_, err := f.svc.SetAccess(ctx, tm.ana, tm.shared, tm.bea.UserID, service.GrantRequest{
		Read: new(true), Act: new(true), Send: new(true), Manage: new(false), From: &drawn,
	})
	wantCode(t, "a change drawn from flags she no longer holds", err, service.CodeConflict)
	wantRefusal(t, "a change drawn from flags she no longer holds", err, service.RefusalStateChanged)
	if g := held(t, f, tm.ana, tm.id, tm.shared, tm.bea.UserID); g.Send || g.Act {
		t.Errorf("the stale change gave Bea %+v", g)
	}
	now := service.GrantFlags{Read: true}
	if _, err := f.svc.SetAccess(ctx, tm.ana, tm.shared, tm.bea.UserID, service.GrantRequest{
		Read: new(true), Act: new(true), Send: new(false), Manage: new(false), From: &now,
	}); err != nil {
		t.Errorf("a change drawn from what she holds: %v", err)
	}
}

func TestAGrantChangeWithoutFromIsAsBefore(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	for _, flags := range []service.GrantRequest{grantRequest(true, true, true, false), grantRequest(true, false, false, true)} {
		g, err := f.svc.SetAccess(ctx, tm.ana, tm.shared, tm.bea.UserID, flags)
		if err != nil || g.Read != *flags.Read || g.Act != *flags.Act || g.Send != *flags.Send || g.Manage != *flags.Manage {
			t.Errorf("setting %v %v %v %v: %+v, %v", *flags.Read, *flags.Act, *flags.Send, *flags.Manage, g, err)
		}
	}
}

func TestEveryRefusalCauseHasOneRefusalAndBack(t *testing.T) {
	causes := map[service.Refusal]error{
		service.RefusalNotFound: service.ErrNotFound, service.RefusalNotAuthorized: service.ErrNotAuthorized,
		service.RefusalOwnerOnly: service.ErrOwnerOnly, service.RefusalOwnerAccess: service.ErrOwnerAccess,
		service.RefusalActorInactive: service.ErrActorInactive, service.RefusalTargetInactive: service.ErrTargetInactive,
		service.RefusalTeamClosed: service.ErrTeamClosed, service.RefusalTeamWithheld: service.ErrTeamWithheld,
		service.RefusalNotCurrentText: service.ErrNotCurrentText, service.RefusalNoReader: service.ErrNoReader,
		service.RefusalLastReader: service.ErrLastReader, service.RefusalRecoveryOver: service.ErrRecoveryOver,
		service.RefusalNotRemoved: service.ErrNotRemoved, service.RefusalAlreadyRemoved: service.ErrAlreadyRemoved,
		service.RefusalPaused: service.ErrPaused, service.RefusalSyncOff: service.ErrSyncOff, service.RefusalHeld: service.ErrHeld,
		service.RefusalNeedsReauth: service.ErrNeedsReauth, service.RefusalPendingAuth: service.ErrPendingAuth,
		service.RefusalEngineNotRunning: service.ErrEngineNotRunning, service.RefusalGrantNeeded: service.ErrGrantNeeded,
		service.RefusalStateChanged: service.ErrStateChanged, service.RefusalSetSince: service.ErrSetSince,
		service.RefusalManageByRole: service.ErrManageByRole, service.RefusalPersonalGrant: service.ErrPersonalGrant,
		service.RefusalNeedsOwnSession: service.ErrNeedsOwnSession, service.RefusalFreshUnconfirmed: service.ErrFreshUnconfirmed,
		service.RefusalRequestDone: service.ErrRequestDone,
	}
	for _, r := range service.Refusals() {
		cause, ok := causes[r]
		if !ok {
			t.Errorf("the refusal %q has no cause", r)
			continue
		}
		// Carried as an *Error's cause, as every refusal is.
		if got := service.RefusalOf(&service.Error{Code: service.CodeConflict, Message: "refused", Err: cause}); got != r {
			t.Errorf("the cause of %q is read as %q", r, got)
		}
	}
	if len(causes) != len(service.Refusals()) {
		t.Errorf("%d causes for %d refusals", len(causes), len(service.Refusals()))
	}
	// The repositories' own causes, as an *Error carries them.
	for cause, want := range map[error]service.Refusal{
		store.ErrSetSince: service.RefusalSetSince, store.ErrRequestDone: service.RefusalRequestDone,
		store.ErrActorInactive: service.RefusalActorInactive, store.ErrNotInWorkspace: service.RefusalNotFound,
		workspace.ErrGrantChanged: service.RefusalStateChanged, workspace.ErrManageByRole: service.RefusalManageByRole,
		workspace.ErrPersonal: service.RefusalPersonalGrant, workspace.ErrLastReader: service.RefusalLastReader,
		workspace.ErrNotMember: service.RefusalTargetInactive, workspace.ErrSealedGrantNeeded: service.RefusalGrantNeeded,
		workspace.ErrClosed: service.RefusalTeamClosed, account.ErrRemovalOver: service.RefusalRecoveryOver,
		account.ErrNotRemoved: service.RefusalNotRemoved, account.ErrNoReaderLeft: service.RefusalNoReader,
		store.ErrPauseHeld: service.RefusalHeld, store.ErrPauseSyncOff: service.RefusalSyncOff,
	} {
		if got := service.RefusalOf(&service.Error{Code: service.CodeConflict, Message: "refused", Err: cause}); got != want {
			t.Errorf("%v is read as %q, want %q", cause, got, want)
		}
	}
	// What carries no cause is read by its code alone.
	for code, want := range map[service.Code]service.Refusal{
		service.CodeNotFound: service.RefusalNotFound, service.CodeNotAuthorized: service.RefusalNotAuthorized,
		service.CodeUnauthorized: service.RefusalNotAuthorized, service.CodeConflict: service.RefusalNone,
		service.CodeInternal: service.RefusalNone,
	} {
		if got := service.RefusalOf(service.E(code, "refused", nil)); got != want {
			t.Errorf("a bare %s is read as %q, want %q", code, got, want)
		}
	}
	if service.RefusalOf(errors.New("a store fault")) != service.RefusalNone || service.RefusalOf(nil) != service.RefusalNone {
		t.Error("an error that is no refusal is read as one")
	}
}

func TestAClosedTeamIsTeamClosedToAPersonActingThroughAnExtension(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	f.closeTeam(t, tm.id)
	for i, c := range []struct {
		what string
		act  func(*service.PersonActs) (service.ActOutcome, error)
	}{
		{"a pause", func(a *service.PersonActs) (service.ActOutcome, error) { return a.PauseSync(ctx, tm.shared) }},
		{"a team's turn-off", func(a *service.PersonActs) (service.ActOutcome, error) { return a.TeamSync(ctx, tm.shared, false, "") }},
		{"a drop", func(a *service.PersonActs) (service.ActOutcome, error) {
			return a.DropFlags(ctx, tm.shared, tm.bea.UserID, service.GrantFlags{Read: true})
		}},
		{"a sync now", func(a *service.PersonActs) (service.ActOutcome, error) { return a.SyncNow(ctx, tm.shared) }},
	} {
		_, err := c.act(f.actsFor(t, delegated(tm.ana, "c"+string(rune('1'+i)), tm.id, time.Now())))
		wantRefusal(t, c.what+" in a closed team", err, service.RefusalTeamClosed)
	}
}

func TestRemovingARemovedMailboxIsAlreadyRemoved(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tm := syncedTeam(t, f)
	f.connect(t, tm.shared)
	if out, err := f.actsFor(t, delegated(tm.ana, "r1", tm.id, time.Now())).Remove(ctx, tm.shared); err != nil || !out.Changed || out.Purged {
		t.Fatalf("Ana removing the team's mailbox: %+v, %v", out, err)
	}
	_, err := f.actsFor(t, delegated(tm.ana, "r2", tm.id, time.Now())).Remove(ctx, tm.shared)
	wantRefusal(t, "an owner removing it again", err, service.RefusalAlreadyRemoved)
	// To anyone who may not recover it, it does not exist.
	_, err = f.actsFor(t, delegated(tm.carol, "r3", tm.id, time.Now())).Remove(ctx, tm.shared)
	wantRefusal(t, "an admin removing it again", err, service.RefusalNotFound)
	// An admin removing a live team mailbox: owners only.
	other := "acc_00000000000000bb"
	tm.link(t, f, other, "other@mail.example")
	f.connect(t, other)
	_, err = f.actsFor(t, delegated(tm.carol, "r4", tm.id, time.Now())).Remove(ctx, other)
	wantRefusal(t, "an admin removing a team mailbox", err, service.RefusalOwnerOnly)
}
