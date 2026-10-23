package service_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Freshness (freshness.go): what asks the extension that follows a directory
// before an act that widens access or touches a team's shared data, what
// never asks, and how an answer that ended the actor's place stops the act.

// freshHook records every question, and answers with answer when set.
type freshHook struct {
	mu     sync.Mutex
	asked  []service.FreshAct
	answer func(ctx context.Context, act service.FreshAct) error
}

func (h *freshHook) Fresh(ctx context.Context, act service.FreshAct) error {
	h.mu.Lock()
	h.asked = append(h.asked, act)
	answer := h.answer
	h.mu.Unlock()
	if answer == nil {
		return nil
	}
	return answer(ctx, act)
}

// take returns what was asked since the last take.
func (h *freshHook) take() []service.FreshAct {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.asked
	h.asked = nil
	return out
}

func (h *freshHook) answerWith(fn func(ctx context.Context, act service.FreshAct) error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.answer = fn
}

// freshWorld is a daemon whose workspaces come from a directory, with a
// freshness hook: Ana owns the directory's team, Bea is a member of it.
type freshWorld struct {
	*mailFixture
	hook     *freshHook
	ana, bea service.Principal
}

func newFreshWorld(t *testing.T) *freshWorld {
	t.Helper()
	hook := &freshHook{}
	m := newMailFixtureWith(t, fixtureOptions{source: workspace.Platform(), freshness: hook,
		registry: account.RegistryOptions{
			Google:    account.OAuthClient{ClientID: "google-client"},
			Microsoft: account.OAuthClient{ClientID: "ms-client", Tenant: "common"},
		}})
	w := &freshWorld{mailFixture: m, hook: hook}
	w.ana = m.followed(t, "ana@example.com")
	m.apply(t, document("ana@example.com", 1, inTeam(workspace.RoleOwner, 1)))
	w.bea = m.followed(t, "bea@example.com")
	m.apply(t, document("bea@example.com", 1, inTeam(workspace.RoleMember, 1)))
	hook.take()
	return w
}

// endAna is an answer that applies Ana's next document, which no longer
// lists the team: her membership ends, or is held, before the act goes on.
func (w *freshWorld) endAna(t *testing.T) func(ctx context.Context, act service.FreshAct) error {
	t.Helper()
	return func(ctx context.Context, _ service.FreshAct) error {
		_, err := w.svc.ApplyDirectoryDocument(ctx, document("ana@example.com", 2), service.ApplyOptions{})
		return err
	}
}

// freshSite is one act that asks: how to make it ready, the act, what it
// asks, and that a refused act changed nothing.
type freshSite struct {
	// ready prepares the act, its own questions taken after it.
	ready func(t *testing.T, w *freshWorld) freshCase
}

type freshCase struct {
	do        func() error
	want      service.FreshAct
	unchanged func(t *testing.T)
	// suspends: the act is on Ana's own mailbox, whose standing a
	// directory ends by disabling her rather than by ending a membership.
	suspends bool
}

// teamBox links a keyless team mailbox Ana reads, and returns its id.
func (w *freshWorld) box(t *testing.T, email string) string {
	t.Helper()
	return w.teamBox(t, dirTeamID, w.ana.UserID, email)
}

// removedBox is a team mailbox Ana reads, which connected once, removed by
// her and kept: with the team's consent to sync and an index, when synced.
func (w *freshWorld) removedBox(t *testing.T, email string, synced bool) string {
	t.Helper()
	box := w.box(t, email)
	w.connect(t, box)
	if synced {
		on := true
		if _, err := w.svc.SetMailboxSync(t.Context(), w.ana, box,
			service.MailboxSyncRequest{Enabled: &on, Version: w.consent().Sync}); err != nil {
			t.Fatal(err)
		}
		w.seedIndex(t, box, "Kept")
	}
	if _, err := w.svc.RemoveAccount(t.Context(), w.ana, box, service.RemoveAccountRequest{Confirm: box}); err != nil {
		t.Fatal(err)
	}
	return box
}

var freshSites = map[string]freshSite{
	"SetAccess": {func(t *testing.T, w *freshWorld) freshCase {
		box := w.box(t, "support@team.example.com")
		return freshCase{
			do: func() error {
				_, err := w.svc.SetAccess(t.Context(), w.ana, box, w.bea.UserID, grantRequest(true, false, false, false))
				return err
			},
			want: service.FreshAct{Kind: service.FreshRaiseFlags, ActorID: w.ana.UserID, RecipientIDs: []string{w.bea.UserID},
				WorkspaceID: dirTeamID, AccountID: box},
			unchanged: func(t *testing.T) {
				if n := w.count(t, `SELECT count(*) FROM mailbox_access WHERE account_id = ? AND user_id = ?`, box, w.bea.UserID); n != 0 {
					t.Error("Bea was given read")
				}
			},
		}
	}},
	"SupplyKey": {func(t *testing.T, w *freshWorld) freshCase {
		box := w.box(t, "support@team.example.com")
		w.giveRead(t, box, w.bea.UserID)
		key := newBrowserKey(t, "", 1)
		if _, err := w.svc.WriteFirstKey(t.Context(), w.ana, box, service.FirstKeyRequest{
			PublicKey: b64(key.public), Namespace: key.namespace,
			Grants: []service.GrantToRequest{key.grantTo(t, w.fixture, w.ana.UserID), key.grantTo(t, w.fixture, w.bea.UserID)},
		}); err != nil {
			t.Fatal(err)
		}
		// Bea's grant goes, as her reset would take it: she waits for the key.
		w.exec(t, `DELETE FROM mailbox_grants WHERE account_id = ? AND user_id = ?`, box, w.bea.UserID)
		return freshCase{
			do: func() error {
				_, err := w.svc.SupplyKey(t.Context(), w.ana, box, w.bea.UserID, key.supply(t, w.fixture, w.bea.UserID))
				return err
			},
			want: service.FreshAct{Kind: service.FreshShareKey, ActorID: w.ana.UserID, RecipientIDs: []string{w.bea.UserID},
				WorkspaceID: dirTeamID, AccountID: box},
			unchanged: func(t *testing.T) {
				if n := w.count(t, `SELECT count(*) FROM mailbox_grants WHERE account_id = ? AND user_id = ?`, box, w.bea.UserID); n != 0 {
					t.Error("the key was handed to Bea")
				}
			},
		}
	}},
	"WriteFirstKey": {func(t *testing.T, w *freshWorld) freshCase {
		box := w.box(t, "support@team.example.com")
		w.giveRead(t, box, w.bea.UserID)
		key := newBrowserKey(t, "", 1)
		return freshCase{
			do: func() error {
				_, err := w.svc.WriteFirstKey(t.Context(), w.ana, box, service.FirstKeyRequest{
					PublicKey: b64(key.public), Namespace: key.namespace,
					Grants: []service.GrantToRequest{key.grantTo(t, w.fixture, w.ana.UserID), key.grantTo(t, w.fixture, w.bea.UserID)},
				})
				return err
			},
			want: service.FreshAct{Kind: service.FreshShareKey, ActorID: w.ana.UserID, RecipientIDs: []string{w.bea.UserID},
				WorkspaceID: dirTeamID, AccountID: box},
			unchanged: func(t *testing.T) {
				if n := w.count(t, `SELECT count(*) FROM mailbox_keys WHERE account_id = ?`, box); n != 0 {
					t.Error("the mailbox was keyed")
				}
			},
		}
	}},
	"WriteNewKey": {func(t *testing.T, w *freshWorld) freshCase {
		key := newBrowserKey(t, "", 1)
		added, err := w.svc.AddAccount(t.Context(), w.ana, key.link(t, w.fixture, w.ana, w.passwordAccount(t, "ana@mail.example")))
		if err != nil {
			t.Fatal(err)
		}
		mine := added.Account.ID
		next := newBrowserKey(t, key.namespace, 2)
		return freshCase{
			do: func() error {
				_, err := w.svc.WriteNewKey(t.Context(), w.ana, mine, service.NewKeyRequest{
					Epoch: 2, PublicKey: b64(next.public), Grant: next.sealTo(t, w.fixture, w.ana.UserID),
				})
				return err
			},
			want: service.FreshAct{Kind: service.FreshShareKey, ActorID: w.ana.UserID, RecipientIDs: []string{w.ana.UserID},
				WorkspaceID: personalOf("ana@example.com"), AccountID: mine},
			unchanged: func(t *testing.T) {
				if n := w.count(t, `SELECT max(epoch) FROM mailbox_keys WHERE account_id = ?`, mine); n != 1 {
					t.Errorf("the mailbox's key is at epoch %d", n)
				}
			},
			suspends: true,
		}
	}},
	"SetKeyAccess": {func(t *testing.T, w *freshWorld) freshCase {
		box := w.box(t, "support@team.example.com")
		created, err := w.svc.CreateWorkspaceKey(t.Context(), w.ana, dirTeamID, keyRequest("tool", "write", reads(box)))
		if err != nil {
			t.Fatal(err)
		}
		prefix := created.Prefix
		return freshCase{
			do: func() error {
				_, err := w.svc.SetKeyAccess(t.Context(), w.ana, dirTeamID, prefix, box, keyAccess(true, true, false))
				return err
			},
			want: service.FreshAct{Kind: service.FreshRaiseFlags, ActorID: w.ana.UserID, WorkspaceID: dirTeamID, AccountID: box},
			unchanged: func(t *testing.T) {
				if n := w.count(t, `SELECT act FROM key_access WHERE key_prefix = ? AND account_id = ?`, prefix, box); n != 0 {
					t.Error("the key was given act")
				}
			},
		}
	}},
	"CreateWorkspaceKey": {func(t *testing.T, w *freshWorld) freshCase {
		box := w.box(t, "support@team.example.com")
		return freshCase{
			do: func() error {
				_, err := w.svc.CreateWorkspaceKey(t.Context(), w.ana, dirTeamID, keyRequest("tool", "read", reads(box)))
				return err
			},
			want: service.FreshAct{Kind: service.FreshCreateKey, ActorID: w.ana.UserID, WorkspaceID: dirTeamID},
			unchanged: func(t *testing.T) {
				if n := w.count(t, `SELECT count(*) FROM api_keys WHERE workspace_id = ?`, dirTeamID); n != 0 {
					t.Error("a key was created")
				}
			},
		}
	}},
	"AddAccount": {func(t *testing.T, w *freshWorld) freshCase {
		req := w.passwordAccount(t, "new@team.example.com")
		req.WorkspaceID = dirTeamID
		return freshCase{
			do: func() error {
				_, err := w.svc.AddAccount(t.Context(), w.ana, keyed(w.ana, req))
				return err
			},
			want: service.FreshAct{Kind: service.FreshLink, ActorID: w.ana.UserID, WorkspaceID: dirTeamID},
			unchanged: func(t *testing.T) {
				if n := w.count(t, `SELECT count(*) FROM accounts WHERE email = 'new@team.example.com'`); n != 0 {
					t.Error("the mailbox was linked")
				}
			},
		}
	}},
	"StartOAuth": {func(t *testing.T, w *freshWorld) freshCase {
		const id = "acc_fresh0000000oauth"
		if _, err := w.repo.Create(t.Context(), account.Account{
			ID: id, WorkspaceID: dirTeamID, Email: "team@gmail.com", Provider: provider.KindGmail, AuthKind: "oauth2",
			IMAPHost: "imap.gmail.com", IMAPPort: 993, SMTPHost: "smtp.gmail.com", SMTPPort: 465, SMTPTLS: "implicit",
			LoginUser: "team@gmail.com", State: account.StateNeedsReauth,
		}, w.ana.UserID); err != nil {
			t.Fatal(err)
		}
		return freshCase{
			do: func() error {
				_, err := w.svc.StartOAuth(t.Context(), w.ana, id, "loopback")
				return err
			},
			want: service.FreshAct{Kind: service.FreshReauthorize, ActorID: w.ana.UserID, WorkspaceID: dirTeamID, AccountID: id},
			unchanged: func(t *testing.T) {
				if n := w.count(t, `SELECT count(*) FROM oauth_pending WHERE account_id = ?`, id); n != 0 {
					t.Error("a consent attempt was started")
				}
				if a := w.stateOfAccount(t, id); a.State != account.StateNeedsReauth {
					t.Errorf("the mailbox's state is %s", a.State)
				}
			},
		}
	}},
	"SetMailboxSync": {func(t *testing.T, w *freshWorld) freshCase {
		box := w.box(t, "support@team.example.com")
		return freshCase{
			do: func() error {
				on := true
				_, err := w.svc.SetMailboxSync(t.Context(), w.ana, box, service.MailboxSyncRequest{Enabled: &on, Version: w.consent().Sync})
				return err
			},
			want: service.FreshAct{Kind: service.FreshTeamSync, ActorID: w.ana.UserID, WorkspaceID: dirTeamID, AccountID: box},
			unchanged: func(t *testing.T) {
				if n := w.count(t, `SELECT sync_enabled_at FROM accounts WHERE id = ?`, box); n != 0 {
					t.Error("the team's consent to sync was given")
				}
			},
		}
	}},
	"PurgeAccount": {func(t *testing.T, w *freshWorld) freshCase {
		box := w.removedBox(t, "support@team.example.com", false)
		return freshCase{
			do: func() error {
				return w.svc.PurgeAccount(t.Context(), w.ana, box, service.PurgeAccountRequest{Confirm: box})
			},
			want: service.FreshAct{Kind: service.FreshPurge, ActorID: w.ana.UserID, WorkspaceID: dirTeamID, AccountID: box},
			unchanged: func(t *testing.T) {
				if n := w.count(t, `SELECT count(*) FROM accounts WHERE id = ? AND removed_at <> 0`, box); n != 1 {
					t.Error("the removed mailbox was deleted for good")
				}
			},
		}
	}},
	"SyncOffRemoved": {func(t *testing.T, w *freshWorld) freshCase {
		box := w.removedBox(t, "support@team.example.com", true)
		return freshCase{
			do: func() error {
				off := false
				_, err := w.svc.SetMailboxSync(t.Context(), w.ana, box, service.MailboxSyncRequest{Enabled: &off})
				return err
			},
			want: service.FreshAct{Kind: service.FreshRemovedSyncOff, ActorID: w.ana.UserID, WorkspaceID: dirTeamID, AccountID: box},
			unchanged: func(t *testing.T) {
				if n := w.count(t, `SELECT count(*) FROM accounts WHERE id = ? AND sync_enabled_at <> 0`, box); n != 1 {
					t.Error("the removed mailbox's consent to sync was withdrawn")
				}
				if n := w.count(t, `SELECT count(*) FROM messages WHERE account_id = ?`, box); n == 0 {
					t.Error("the index the removed mailbox keeps was deleted")
				}
			},
		}
	}},
	"SetFlags": {func(t *testing.T, w *freshWorld) freshCase {
		id, box := w.ownedBoxIn(t, w.ana, dirTeamID, "desk@team.example.com", providertest.FakeOptions{Caps: providertest.GmailCaps()})
		box.Deliver("INBOX", message("a", "Lunch on Friday"))
		w.index(t, id, box)
		if _, err := w.svc.GrantActionsConsent(t.Context(), w.ana, w.consent().Actions); err != nil {
			t.Fatal(err)
		}
		row := w.messageID(t, id, "INBOX", 1)
		return freshCase{
			do: func() error {
				_, err := w.svc.SetFlags(t.Context(), w.ana, service.SetFlagsRequest{IDs: []int64{row}, Seen: yes()})
				return err
			},
			want: service.FreshAct{Kind: service.FreshActions, ActorID: w.ana.UserID, WorkspaceID: dirTeamID, AccountID: id},
			unchanged: func(t *testing.T) {
				if n := box.CallCount(providertest.MethodStoreFlags); n != 0 {
					t.Errorf("the server was asked to store flags %d times", n)
				}
			},
		}
	}},
	"SendMessage": {func(t *testing.T, w *freshWorld) freshCase {
		b := w.sendingBoxIn(t, w.ana, dirTeamID, "out@team.example.com", providertest.FakeOptions{Caps: providertest.GmailCaps()})
		return freshCase{
			do: func() error {
				_, err := b.send(t, w.ana, "fresh-1", b.compose("cid@example.org"))
				return err
			},
			want: service.FreshAct{Kind: service.FreshSend, ActorID: w.ana.UserID, WorkspaceID: dirTeamID, AccountID: b.id},
			unchanged: func(t *testing.T) {
				if n := len(b.smtp.Messages()); n != 0 {
					t.Errorf("%d messages were submitted", n)
				}
				if n := w.count(t, `SELECT count(*) FROM sends WHERE account_id = ?`, b.id); n != 0 {
					t.Errorf("%d sends were recorded", n)
				}
			},
		}
	}},
}

// sameAct compares two questions, recipients in any order.
func sameAct(a, b service.FreshAct) bool {
	ra, rb := slices.Clone(a.RecipientIDs), slices.Clone(b.RecipientIDs)
	slices.Sort(ra)
	slices.Sort(rb)
	return a.Kind == b.Kind && a.ActorID == b.ActorID && a.WorkspaceID == b.WorkspaceID && a.AccountID == b.AccountID &&
		slices.Equal(ra, rb)
}

// sortedSites is the sites' names, in one order.
func sortedSites() []string {
	var names []string
	for name := range freshSites {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func TestEveryActThatWidensAccessAsksForFreshness(t *testing.T) {
	for _, name := range sortedSites() {
		t.Run(name, func(t *testing.T) {
			w := newFreshWorld(t)
			c := freshSites[name].ready(t, w)
			w.hook.take()
			if err := c.do(); err != nil {
				t.Fatalf("the act: %v", err)
			}
			asked := w.hook.take()
			if len(asked) != 1 || !sameAct(asked[0], c.want) {
				t.Errorf("asked %+v, want once %+v", asked, c.want)
			}
		})
	}
}

func TestFreshnessNamesTheRecipientOfAKeyOrOfRaisedFlags(t *testing.T) {
	// Who a key is sealed to, or whose flags rise; never the actor, but for
	// a new key of their own mailbox, whose one grant is theirs.
	for name, want := range map[string]func(w *freshWorld) []string{
		"SetAccess":     func(w *freshWorld) []string { return []string{w.bea.UserID} },
		"SupplyKey":     func(w *freshWorld) []string { return []string{w.bea.UserID} },
		"WriteFirstKey": func(w *freshWorld) []string { return []string{w.bea.UserID} },
		"WriteNewKey":   func(w *freshWorld) []string { return []string{w.ana.UserID} },
	} {
		t.Run(name, func(t *testing.T) {
			w := newFreshWorld(t)
			c := freshSites[name].ready(t, w)
			w.hook.take()
			if err := c.do(); err != nil {
				t.Fatal(err)
			}
			asked := w.hook.take()
			if len(asked) != 1 || !slices.Equal(asked[0].RecipientIDs, want(w)) {
				t.Errorf("asked %+v, want the recipients %v", asked, want(w))
			}
		})
	}
	// A key given read with its grant asks as a key shared.
	w := newFreshWorld(t)
	box := w.box(t, "support@team.example.com")
	w.giveRead(t, box, w.bea.UserID)
	key := newBrowserKey(t, "", 1)
	if _, err := w.svc.WriteFirstKey(t.Context(), w.ana, box, service.FirstKeyRequest{
		PublicKey: b64(key.public), Namespace: key.namespace,
		Grants: []service.GrantToRequest{key.grantTo(t, w.fixture, w.ana.UserID), key.grantTo(t, w.fixture, w.bea.UserID)},
	}); err != nil {
		t.Fatal(err)
	}
	cid := w.followed(t, "cid@example.com")
	w.apply(t, document("cid@example.com", 1, inTeam(workspace.RoleMember, 1)))
	w.hook.take()
	if _, err := w.svc.SetAccess(t.Context(), w.ana, box, cid.UserID, key.give(t, w.fixture, cid.UserID)); err != nil {
		t.Fatal(err)
	}
	if asked := w.hook.take(); len(asked) != 1 || asked[0].Kind != service.FreshShareKey ||
		!slices.Equal(asked[0].RecipientIDs, []string{cid.UserID}) {
		t.Errorf("giving read with a grant asked %+v", asked)
	}
}

func TestFreshnessIsNeverAskedToReadRemoveRecoverOrLowerAccess(t *testing.T) {
	w := newFreshWorld(t)
	ctx := t.Context()
	box := w.box(t, "support@team.example.com")
	// It connected once: a removal keeps it.
	if err := w.repo.SavePassword(ctx, box, "hunter2"); err != nil {
		t.Fatal(err)
	}
	w.giveRead(t, box, w.bea.UserID)
	on := true
	if _, err := w.svc.SetMailboxSync(ctx, w.ana, box, service.MailboxSyncRequest{Enabled: &on, Version: w.consent().Sync}); err != nil {
		t.Fatal(err)
	}
	created, err := w.svc.CreateWorkspaceKey(ctx, w.ana, dirTeamID, keyRequest("tool", "write", service.KeyMailboxRequest{
		AccountID: box, Read: true, Act: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	w.hook.take()
	for _, step := range []struct {
		what string
		do   func() error
	}{
		{"listing", func() error { _, err := w.svc.ListAccounts(ctx, w.ana, ""); return err }},
		{"reading one", func() error { _, err := w.svc.GetAccount(ctx, w.ana, box); return err }},
		{"the access directory", func() error { _, err := w.svc.AccessDirectory(ctx, w.ana, dirTeamID); return err }},
		{"setting flags already held", func() error {
			_, err := w.svc.SetAccess(ctx, w.ana, box, w.bea.UserID, grantRequest(true, false, false, false))
			return err
		}},
		{"lowering a key's flags", func() error {
			_, err := w.svc.SetKeyAccess(ctx, w.ana, dirTeamID, created.Prefix, box, keyAccess(true, false, false))
			return err
		}},
		{"taking a key's mailbox", func() error { return w.svc.RevokeKeyAccess(ctx, w.ana, dirTeamID, created.Prefix, box) }},
		{"revoking access", func() error { return w.svc.RevokeAccess(ctx, w.ana, box, w.bea.UserID, workspace.Flags{}) }},
		{"removing", func() error {
			_, err := w.svc.RemoveAccount(ctx, w.ana, box, service.RemoveAccountRequest{Confirm: box})
			return err
		}},
		{"recovering", func() error { _, err := w.svc.RecoverAccount(ctx, w.ana, box); return err }},
		{"removing again", func() error {
			_, err := w.svc.RemoveAccount(ctx, w.ana, box, service.RemoveAccountRequest{Confirm: box})
			return err
		}},
	} {
		if err := step.do(); err != nil {
			t.Fatalf("%s: %v", step.what, err)
		}
		if asked := w.hook.take(); len(asked) != 0 {
			t.Errorf("%s asked %+v", step.what, asked)
		}
	}
}

func TestFreshnessIsNeverAskedToDeleteForGoodOrTurnOffAnythingButARemovedTeamMailboxByItsOwner(t *testing.T) {
	// A team's mailbox asks, of an owner signed in (sites PurgeAccount and
	// SyncOffRemoved); a person's own, the operator's deletion of a team's
	// and the hourly sweep never do.
	w := newFreshWorld(t)
	ctx := t.Context()
	mine := w.mailbox(t, w.ana, "ana@mail.example")
	w.connect(t, mine)
	if _, err := w.svc.GrantSyncConsent(ctx, w.ana, w.consent().Sync); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.RemoveAccount(ctx, w.ana, mine, service.RemoveAccountRequest{Confirm: mine}); err != nil {
		t.Fatal(err)
	}
	shared := w.removedBox(t, "support@team.example.com", true)
	// Removed more than 30 days ago.
	swept := w.box(t, "desk@team.example.com")
	w.connect(t, swept)
	w.exec(t, `UPDATE accounts SET removed_at = 900, removed_by = ?, purge_after = 900 + 2592000 WHERE id = ?`,
		w.ana.UserID, swept)
	w.hook.take()
	for _, step := range []struct {
		what string
		do   func() error
	}{
		{"deleting Ana's own removed mailbox for good", func() error {
			return w.svc.PurgeAccount(ctx, w.ana, mine, service.PurgeAccountRequest{Confirm: mine})
		}},
		{"the operator turning a removed team mailbox's sync off", func() error {
			_, err := w.svc.StopMailboxSync(ctx, admin(), shared)
			return err
		}},
		{"the operator deleting a removed team mailbox for good", func() error {
			return w.svc.PurgeAccount(ctx, admin(), shared, service.PurgeAccountRequest{Confirm: shared})
		}},
		{"the sweep, past the 30 days", func() error {
			n, _, err := w.svc.SweepRemovedMailboxes(ctx, time.Hour)
			if err == nil && n != 1 {
				t.Errorf("the sweep deleted %d mailboxes, want the one past its 30 days", n)
			}
			return err
		}},
	} {
		if err := step.do(); err != nil {
			t.Fatalf("%s: %v", step.what, err)
		}
		if asked := w.hook.take(); len(asked) != 0 {
			t.Errorf("%s asked %+v", step.what, asked)
		}
	}
}

func TestFreshnessIsNeverAskedForAKeysOwnActs(t *testing.T) {
	w := newFreshWorld(t)
	ctx := t.Context()
	id, box := w.ownedBoxIn(t, w.ana, dirTeamID, "desk@team.example.com", providertest.FakeOptions{Caps: providertest.GmailCaps()})
	box.CreateFolder("Sent", imap.MailboxAttrSent)
	box.Deliver("INBOX", message("a", "Lunch on Friday"))
	w.index(t, id, box)
	if _, err := w.svc.GrantActionsConsent(ctx, w.ana, w.consent().Actions); err != nil {
		t.Fatal(err)
	}
	created, err := w.svc.CreateWorkspaceKey(ctx, w.ana, dirTeamID, keyRequest("tool", "write", service.KeyMailboxRequest{
		AccountID: id, Read: true, Act: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	tool := w.authenticate(t, created.Key)
	w.hook.take()
	row := w.messageID(t, id, "INBOX", 1)
	if _, err := w.svc.SetFlags(ctx, tool, service.SetFlagsRequest{IDs: []int64{row}, Seen: yes()}); err != nil {
		t.Fatalf("the key's action: %v", err)
	}
	if asked := w.hook.take(); len(asked) != 0 {
		t.Errorf("a key's own action asked %+v", asked)
	}
	// Nor the operator's instance key, which is nobody signed in.
	if _, err := w.svc.ListAccounts(ctx, admin(), ""); err != nil {
		t.Fatal(err)
	}
	if asked := w.hook.take(); len(asked) != 0 {
		t.Errorf("the operator asked %+v", asked)
	}
}

func TestFreshnessIsAskedOutsideEveryWriteTransaction(t *testing.T) {
	// The answer applies Ana's document again, a write: asked inside a
	// write, it would wait for the one writer forever.
	for _, name := range sortedSites() {
		t.Run(name, func(t *testing.T) {
			w := newFreshWorld(t)
			c := freshSites[name].ready(t, w)
			version := int64(10)
			w.hook.answerWith(func(ctx context.Context, _ service.FreshAct) error {
				version++
				_, err := w.svc.ApplyDirectoryDocument(ctx, document("ana@example.com", version,
					inTeam(workspace.RoleOwner, 1)), service.ApplyOptions{})
				return err
			})
			done := make(chan error, 1)
			go func() { done <- c.do() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the act, its answer a write: %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("the act never returned: its question was asked inside a write")
			}
		})
	}
}

// failsAt holds one site to its re-check: an answer that ends Ana's place
// fails the act, which changes nothing.
func failsAt(t *testing.T, name string) {
	t.Helper()
	w := newFreshWorld(t)
	c := freshSites[name].ready(t, w)
	w.hook.take()
	if c.suspends {
		w.hook.answerWith(func(ctx context.Context, _ service.FreshAct) error {
			doc := document("ana@example.com", 2, inTeam(workspace.RoleOwner, 1))
			doc.Status, doc.DisabledAt = service.DirectoryDisabled, time.Now()
			_, err := w.svc.ApplyDirectoryDocument(ctx, doc, service.ApplyOptions{})
			return err
		})
	} else {
		w.hook.answerWith(w.endAna(t))
	}
	err := c.do()
	if err == nil {
		t.Fatal("the act went on once the answer had ended Ana's place")
	}
	t.Logf("refused: %v", err)
	if code := service.CodeOf(err); code == service.CodeInternal {
		t.Errorf("the act failed as internal: %v", err)
	}
	c.unchanged(t)
	if len(w.hook.take()) != 1 {
		t.Error("the act was not asked about once")
	}
}

func TestAFreshnessThatEndsTheActorsMembershipFailsTheActAtSetAccess(t *testing.T) {
	failsAt(t, "SetAccess")
}

func TestAFreshnessThatEndsTheActorsMembershipFailsTheActAtSupplyKey(t *testing.T) {
	failsAt(t, "SupplyKey")
}

func TestAFreshnessThatEndsTheActorsMembershipFailsTheActAtWriteFirstKey(t *testing.T) {
	failsAt(t, "WriteFirstKey")
}

func TestAFreshnessThatEndsTheActorsMembershipFailsTheActAtWriteNewKey(t *testing.T) {
	// Her own mailbox: the directory disables her.
	failsAt(t, "WriteNewKey")
}

func TestAFreshnessThatEndsTheActorsMembershipFailsTheActAtSetKeyAccess(t *testing.T) {
	failsAt(t, "SetKeyAccess")
}

func TestAFreshnessThatEndsTheActorsMembershipFailsTheActAtCreateWorkspaceKey(t *testing.T) {
	failsAt(t, "CreateWorkspaceKey")
}

func TestAFreshnessThatEndsTheActorsMembershipFailsTheActAtAddAccount(t *testing.T) {
	failsAt(t, "AddAccount")
}

func TestAFreshnessThatEndsTheActorsMembershipFailsTheActAtStartOAuth(t *testing.T) {
	failsAt(t, "StartOAuth")
}

func TestAFreshnessThatEndsTheActorsMembershipFailsTheActAtSetMailboxSync(t *testing.T) {
	failsAt(t, "SetMailboxSync")
}

func TestAFreshnessThatEndsTheActorsMembershipFailsTheActAtPurgeAccount(t *testing.T) {
	failsAt(t, "PurgeAccount")
}

func TestAFreshnessThatEndsTheActorsMembershipFailsTheActAtSyncOffRemoved(t *testing.T) {
	failsAt(t, "SyncOffRemoved")
}

func TestAFreshnessThatEndsTheActorsMembershipFailsTheActAtSetFlags(t *testing.T) {
	failsAt(t, "SetFlags")
}

func TestAFreshnessThatEndsTheActorsMembershipFailsTheActAtSendMessage(t *testing.T) {
	failsAt(t, "SendMessage")
}

func TestARefusedFreshnessPassesItsOwnErrorAndMapsAnyOtherToUpstream(t *testing.T) {
	refusal := service.E(service.CodeNotAuthorized, "the directory says no", nil)
	down := errors.New("the directory did not answer")
	for _, c := range []struct {
		answer error
		check  func(t *testing.T, err error)
	}{
		{refusal, func(t *testing.T, err error) {
			if !errors.Is(err, refusal) || service.CodeOf(err) != service.CodeNotAuthorized ||
				service.MessageOf(err) != "the directory says no" {
				t.Errorf("a refusal of its own: %v", err)
			}
		}},
		{down, func(t *testing.T, err error) {
			if service.CodeOf(err) != service.CodeInternal || !strings.HasPrefix(service.MessageOf(err), "upstream: ") ||
				service.RetryAfterOf(err) != 30*time.Second || !errors.Is(err, down) || strings.Contains(service.MessageOf(err), "answer") {
				t.Errorf("a directory that did not answer: %v (retry %s)", err, service.RetryAfterOf(err))
			}
		}},
		{context.DeadlineExceeded, func(t *testing.T, err error) {
			if !errors.Is(err, context.DeadlineExceeded) || service.RetryAfterOf(err) != 0 {
				t.Errorf("a context that ended: %v", err)
			}
		}},
	} {
		w := newFreshWorld(t)
		box := w.box(t, "support@team.example.com")
		w.hook.answerWith(func(context.Context, service.FreshAct) error { return c.answer })
		_, err := w.svc.SetAccess(t.Context(), w.ana, box, w.bea.UserID, grantRequest(true, false, false, false))
		c.check(t, err)
		if n := w.count(t, `SELECT count(*) FROM mailbox_access WHERE account_id = ? AND user_id = ?`, box, w.bea.UserID); n != 0 {
			t.Error("Bea was given read past a refused answer")
		}
	}
}
