package service_test

import (
	"slices"
	"testing"

	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// A removed mailbox (migration 0020) is kept for a recovery and used by
// nothing meanwhile: to everyone, its owners and the operator included, it
// does not exist.

// removeBySQL removes a mailbox as the repository will, for 30 days, by who.
func (f *fixture) removeBySQL(t *testing.T, accountID, who string) {
	t.Helper()
	f.exec(t, `UPDATE accounts SET removed_at = unixepoch(), removed_by = ?, purge_after = unixepoch() + 2592000
		WHERE id = ?`, who, accountID)
}

func TestARemovedMailboxIsGoneForEveryUse(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()
	tm.grant(t, tm.shared, workspace.Flags{Read: true, Act: true, Send: true})
	created, err := f.svc.CreateWorkspaceKey(ctx, tm.ana, tm.id, keyRequest("assistant", "write",
		service.KeyMailboxRequest{AccountID: tm.shared, Read: true, Act: true}))
	if err != nil {
		t.Fatal(err)
	}
	k := f.authenticate(t, created.Key)
	own := f.mailbox(t, tm.ana, "ana@mail.example")
	ops := f.mailbox(t, admin(), "ops@mail.example")
	f.seedIndex(t, tm.shared, "Zebra")
	f.seedIndex(t, own, "Okapi")
	type use struct {
		who string
		p   service.Principal
		id  string
	}
	uses := []use{
		{"Ana, the team's owner", tm.ana, tm.shared}, {"Bea, who reads it", tm.bea, tm.shared},
		{"Carol, an admin", tm.carol, tm.shared}, {"the team's key", k, tm.shared},
		{"Ana, her own", tm.ana, own}, {"the operator, its own", admin(), ops},
	}
	for _, u := range uses {
		if !slices.Contains(ids(t, f, u.p), u.id) {
			t.Fatalf("before: %s does not see %s", u.who, u.id)
		}
	}

	f.removeBySQL(t, tm.shared, tm.ana.UserID)
	f.removeBySQL(t, own, tm.ana.UserID)
	f.removeBySQL(t, ops, "key:aaaaaaaa")

	for _, u := range uses {
		if slices.Contains(ids(t, f, u.p), u.id) {
			t.Errorf("%s lists the removed mailbox", u.who)
		}
		_, err := f.svc.GetAccount(ctx, u.p, u.id)
		wantCode(t, u.who+" reading its card", err, service.CodeNotFound)
		_, err = f.svc.ListFolders(ctx, u.p, u.id)
		wantCode(t, u.who+" listing its folders", err, service.CodeNotFound)
		_, err = f.svc.SyncStatus(ctx, u.p, u.id)
		wantCode(t, u.who+" reading its sync", err, service.CodeNotFound)
		err = f.svc.TriggerSync(ctx, u.p, u.id)
		wantCode(t, u.who+" asking it to sync", err, service.CodeNotFound)
		_, err = f.svc.SearchMessages(ctx, u.p, service.SearchRequest{AccountID: u.id})
		wantCode(t, u.who+" searching it", err, service.CodeNotFound)
		_, err = f.svc.Subscribe(ctx, u.p, 0, service.EventFilter{AccountIDs: []string{u.id}})
		wantCode(t, u.who+" following its events", err, service.CodeNotFound)
		if !u.p.IsSession() && !u.p.IsInstance() {
			continue
		}
		_, err = f.svc.StartOAuth(ctx, u.p, u.id, "")
		wantCode(t, u.who+" authorizing it again", err, service.CodeNotFound)
		_, err = f.svc.RemoveAccount(ctx, u.p, u.id, service.RemoveAccountRequest{Confirm: u.id})
		wantCode(t, u.who+" removing it again", err, service.CodeNotFound)
	}
	// Nothing of it is found by a search of everything either.
	for _, word := range []string{"Zebra", "Okapi"} {
		page, err := f.svc.SearchMessages(ctx, tm.ana, service.SearchRequest{Query: word})
		if err != nil || len(page.Messages) != 0 {
			t.Errorf("searching everything for %s finds %+v (%v)", word, page.Messages, err)
		}
	}
	// Who holds what on it, its key and its sync are nobody's to change, nor
	// to see.
	on, off := true, false
	_, err = f.svc.SetAccess(ctx, tm.ana, tm.shared, tm.bea.UserID,
		service.GrantRequest{Read: &on, Act: &off, Send: &off, Manage: &off})
	wantCode(t, "Ana changing Bea's grant", err, service.CodeNotFound)
	err = f.svc.RevokeAccess(ctx, tm.ana, tm.shared, tm.bea.UserID, workspace.Flags{})
	wantCode(t, "Ana revoking Bea's grant", err, service.CodeNotFound)
	_, err = f.svc.SetKeyAccess(ctx, tm.ana, tm.id, created.Prefix, tm.shared, keyAccess(true, false, false))
	wantCode(t, "Ana changing what the key holds", err, service.CodeNotFound)
	_, err = f.svc.MailboxKey(ctx, tm.ana, tm.shared)
	wantCode(t, "Ana reading its key", err, service.CodeNotFound)
	_, err = f.svc.SetMailboxSync(ctx, tm.ana, tm.shared, service.MailboxSyncRequest{Enabled: &on, Version: f.consent().Sync})
	wantCode(t, "Ana turning its sync on", err, service.CodeNotFound)
	_, err = f.svc.SetMailboxSync(ctx, admin(), ops, switchSync(true))
	wantCode(t, "the operator turning its own sync on", err, service.CodeNotFound)
	for name, p := range map[string]service.Principal{"Ana": tm.ana, "the operator": admin()} {
		directory, err := f.svc.AccessDirectory(ctx, p, tm.id)
		if err != nil {
			t.Fatal(err)
		}
		if len(directory) != 0 {
			t.Errorf("%s's directory of the team lists %+v", name, directory)
		}
	}
	listed, err := f.svc.ListWorkspaces(ctx, admin())
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range listed {
		if w.Mailboxes != nil && *w.Mailboxes != 0 {
			t.Errorf("the operator's listing counts %d mailboxes in %s", *w.Mailboxes, w.ID)
		}
	}

	// Recovered, it is used again, by those who still qualify.
	f.exec(t, `UPDATE accounts SET removed_at = 0, removed_by = '', purge_after = 0, sync_held_at = unixepoch()
		WHERE id = ?`, tm.shared)
	if a := accessOf(t, f, tm.bea, tm.shared); !a.Access.Read {
		t.Errorf("recovered, Bea holds %+v on it", a.Access)
	}
}

// removeErr is what removing a mailbox, its id repeated, answers p.
func (f *fixture) removeErr(t *testing.T, p service.Principal, id string) error {
	t.Helper()
	_, err := f.svc.RemoveAccount(t.Context(), p, id, service.RemoveAccountRequest{Confirm: id})
	return err
}
