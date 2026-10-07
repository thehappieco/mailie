package api_test

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/api"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// keyBody is a key of the current terms, holding what mailboxes name.
func keyBody(scope string, mailboxes ...string) string {
	return fmt.Sprintf(`{"name":"assistant","scope":%q,"terms_version":%q,"mailboxes":[%s]}`,
		scope, service.DefaultKeyTermsVersion, strings.Join(mailboxes, ","))
}

// holding is one mailbox of a key's request.
func holding(accountID string, read, act, send bool) string {
	return fmt.Sprintf(`{"account_id":%q,"read":%t,"act":%t,"send":%t}`, accountID, read, act, send)
}

type createdKey struct {
	Key    string `json:"key"`
	Prefix string `json:"prefix"`
}

// createKey creates a key of a workspace through its route, as token.
func (h *harness) createKey(t *testing.T, token, workspaceID, body string) createdKey {
	t.Helper()
	resp := h.do(t, http.MethodPost, "/v1/workspaces/"+workspaceID+"/apikeys", token, body)
	if resp.StatusCode != http.StatusCreated {
		code, msg := decodeError(t, resp)
		t.Fatalf("creating a key: %d %s %s", resp.StatusCode, code, msg)
	}
	var created createdKey
	decodeInto(t, resp, &created)
	return created
}

func TestOnlyAnOwnerOrAdminSignedInManagesAWorkspacesKeys(t *testing.T) {
	h := newHarness(t, true)
	tm := newSupportTeam(t, h)
	keys := "/v1/workspaces/" + tm.id + "/apikeys"
	resp := h.do(t, http.MethodPost, keys, tm.ana, keyBody("write", holding(tm.shared, true, true, false)))
	if resp.StatusCode != http.StatusCreated {
		code, msg := decodeError(t, resp)
		t.Fatalf("Ana creating a key: %d %s %s", resp.StatusCode, code, msg)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("the answer carrying a new key may be cached")
	}
	var created createdKey
	decodeInto(t, resp, &created)

	// Bea, a member; the key it made; an instance admin key: none mints,
	// lists, changes or revokes a key of the team.
	for name, token := range map[string]string{"Bea": tm.bea, "the key": created.Key, "an instance key": h.key(t, auth.ScopeAdmin)} {
		for _, c := range []struct{ method, path, body string }{
			{http.MethodGet, keys, ""},
			{http.MethodPost, keys, keyBody("read")},
			{http.MethodDelete, keys + "/" + created.Prefix, ""},
			{http.MethodPut, keys + "/" + created.Prefix + "/accounts/" + tm.shared, `{"read":true,"act":true,"send":true}`},
			{http.MethodDelete, keys + "/" + created.Prefix + "/accounts/" + tm.shared, ""},
			{http.MethodGet, keys + "/" + created.Prefix + "/sends", ""},
		} {
			if resp := h.do(t, c.method, c.path, token, c.body); resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s: %s %s answered %d, want 403", name, c.method, c.path, resp.StatusCode)
			}
		}
	}
	// Nor does a key reach the operator's key routes.
	if resp := h.do(t, http.MethodPost, "/v1/apikeys", created.Key, `{"name":"x","scope":"read"}`); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a workspace key on POST /v1/apikeys answered %d", resp.StatusCode)
	}
	var listed []service.WorkspaceKey
	decodeInto(t, h.do(t, http.MethodGet, keys, tm.ana, ""), &listed)
	if len(listed) != 1 || listed[0].Prefix != created.Prefix || len(listed[0].Mailboxes) != 1 ||
		!listed[0].Mailboxes[0].Act || listed[0].CreatedBy != tm.anaID {
		t.Errorf("Ana lists %+v", listed)
	}
	if resp := h.do(t, http.MethodDelete, keys+"/"+created.Prefix, tm.ana, ""); resp.StatusCode != http.StatusNoContent {
		t.Errorf("revoking with her session answered %d", resp.StatusCode)
	}
	if resp := h.do(t, http.MethodGet, "/v1/accounts", created.Key, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the revoked key answered %d", resp.StatusCode)
	}
}

func TestCreatingAKeyOfOnesOwnSaysKeysAreCreatedInAWorkspace(t *testing.T) {
	h := newHarness(t, false)
	token := h.person(t, "ana@example.com", auth.RoleOwner)
	resp := h.do(t, http.MethodPost, "/v1/me/apikeys", token,
		`{"name":"assistant","scope":"read","terms_version":"`+service.DefaultKeyTermsVersion+`"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /v1/me/apikeys answered %d, want 400", resp.StatusCode)
	}
	if code, msg := decodeError(t, resp); code != string(service.CodeBadRequest) ||
		!strings.Contains(msg, "API keys are created in a workspace by its owners and admins") {
		t.Errorf("answer %s %q", code, msg)
	}
}

func TestAKeyForKeyTermsThePersonWasNotShownIsAConflict(t *testing.T) {
	h := newHarness(t, false)
	tm := newSupportTeam(t, h)
	resp := h.do(t, http.MethodPost, "/v1/workspaces/"+tm.id+"/apikeys", tm.ana,
		`{"name":"assistant","scope":"read","terms_version":"2026-10-open-api-keys"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("old terms answered %d, want 409", resp.StatusCode)
	}
}

func TestAPersonListsAndRevokesOnlyTheKeysTheyCreated(t *testing.T) {
	h := newHarness(t, false)
	tm := newSupportTeam(t, h)
	anas := h.createKey(t, tm.ana, tm.id, keyBody("read"))
	var mine []service.WorkspaceKey
	decodeInto(t, h.do(t, http.MethodGet, "/v1/me/apikeys", tm.ana, ""), &mine)
	if len(mine) != 1 || mine[0].Prefix != anas.Prefix || mine[0].WorkspaceID != tm.id {
		t.Errorf("Ana lists %+v", mine)
	}
	decodeInto(t, h.do(t, http.MethodGet, "/v1/me/apikeys", tm.bea, ""), &mine)
	if len(mine) != 0 {
		t.Errorf("Bea lists %+v", mine)
	}
	if resp := h.do(t, http.MethodDelete, "/v1/me/apikeys/"+anas.Prefix, tm.bea, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("Bea revoking Ana's key answered %d, want 404", resp.StatusCode)
	}
	if resp := h.do(t, http.MethodDelete, "/v1/me/apikeys/"+anas.Prefix, tm.ana, ""); resp.StatusCode != http.StatusNoContent {
		t.Errorf("Ana revoking her key answered %d", resp.StatusCode)
	}
}

func TestAnAdminWhoReadsNothingCannotReadThroughAKeyOverREST(t *testing.T) {
	h := newHarness(t, false)
	tm := newSupportTeam(t, h)
	carol := h.person(t, "carol@example.com", auth.RoleMember)
	carolUser, err := h.users.GetByEmail(t.Context(), "carol@example.com")
	if err != nil {
		t.Fatal(err)
	}
	carolID := carolUser.ID
	if err := h.store.Write(t.Context(), func(tx *sql.Tx) error {
		return workspace.NewRepository(h.store, nil).AddMemberTx(t.Context(), tx, tm.id, carolID, workspace.RoleAdmin, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	keys := "/v1/workspaces/" + tm.id + "/apikeys"
	if resp := h.do(t, http.MethodPost, keys, carol, keyBody("send", holding(tm.shared, true, false, false))); resp.StatusCode != http.StatusForbidden {
		t.Errorf("Carol giving a key read answered %d, want 403", resp.StatusCode)
	}
	created := h.createKey(t, carol, tm.id, keyBody("send", holding(tm.shared, false, false, true)))
	if resp := h.do(t, http.MethodPut, keys+"/"+created.Prefix+"/accounts/"+tm.shared, carol,
		`{"read":true,"act":false,"send":true}`); resp.StatusCode != http.StatusForbidden {
		t.Errorf("Carol adding read to her key answered %d, want 403", resp.StatusCode)
	}
	for _, path := range []string{
		"/v1/messages?account=" + tm.shared,
		fmt.Sprintf("/v1/messages/%d", tm.message),
		"/v1/accounts/" + tm.shared + "/folders",
		"/v1/events/wait?timeout=1&account=" + tm.shared,
		"/v1/events?account=" + tm.shared,
	} {
		resp := h.do(t, http.MethodGet, path, created.Key, "")
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s with Carol's key answered %d", path, resp.StatusCode)
		}
	}
	var storage service.Storage
	decodeInto(t, h.do(t, http.MethodGet, "/v1/me/storage", created.Key, ""), &storage)
	if len(storage.Mailboxes) != 0 {
		t.Errorf("Carol's key's storage lists %+v", storage.Mailboxes)
	}
	var page service.MessagePage
	decodeInto(t, h.do(t, http.MethodGet, "/v1/messages", created.Key, ""), &page)
	if len(page.Messages) != 0 {
		t.Errorf("Carol's key searching everything found %d messages", len(page.Messages))
	}
	// Ana reads it, and gives the key read: it reads.
	if resp := h.do(t, http.MethodPut, keys+"/"+created.Prefix+"/accounts/"+tm.shared, tm.ana,
		`{"read":true,"act":false,"send":true}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("Ana giving the key read answered %d", resp.StatusCode)
	}
	decodeInto(t, h.do(t, http.MethodGet, "/v1/messages?account="+tm.shared, created.Key, ""), &page)
	if len(page.Messages) != 1 {
		t.Errorf("the key once a reader gave it read found %d messages", len(page.Messages))
	}
}

func TestAKeysStreamStopsCarryingAMailboxTakenOutOfIt(t *testing.T) {
	// What a key holds is read at every event: a stream held open with it
	// is told when a mailbox is taken out, carries none of its events
	// after, and ends when the key is revoked.
	h := newHarnessWith(t, func(h *api.Handler) { h.EventPing = 30 * time.Millisecond }, serviceOptions{})
	tm := newSupportTeam(t, h)
	created := h.createKey(t, tm.ana, tm.id, keyBody("read", holding(tm.shared, true, false, false)))
	held := h.openStream(t, "/v1/events", created.Key, "")
	h.publish(t, mailEvent(t, tm.shared, "Refund"))
	if f, ok := held.next(t); !ok || f.event != "message.new" {
		t.Fatalf("before: %+v", f)
	}
	keys := "/v1/workspaces/" + tm.id + "/apikeys/" + created.Prefix
	if resp := h.do(t, http.MethodDelete, keys+"/accounts/"+tm.shared, tm.ana, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("taking the mailbox out: %d", resp.StatusCode)
	}
	f, ok := held.next(t)
	if !ok || f.event != "access" || !strings.Contains(f.data, `"read":false`) || !strings.Contains(f.data, tm.shared) {
		t.Fatalf("after the mailbox was taken out the stream sent %+v, want the access it lost", f)
	}
	h.publish(t, mailEvent(t, tm.shared, "Second refund"))
	if resp := h.do(t, http.MethodDelete, keys, tm.ana, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoking: %d", resp.StatusCode)
	}
	f, ok = held.next(t)
	if !ok || f.event != "error" || !strings.Contains(f.data, `"code":"unauthorized"`) {
		t.Fatalf("the stream sent %+v, want only its end once the key was revoked", f)
	}
}

func TestTheOperatorListsEveryKeyWithItsWorkspace(t *testing.T) {
	h := newHarness(t, true)
	tm := newSupportTeam(t, h)
	created := h.createKey(t, tm.ana, tm.id, keyBody("read", holding(tm.shared, true, false, false)))
	admin := h.key(t, auth.ScopeAdmin)
	var listed []service.APIKey
	decodeInto(t, h.do(t, http.MethodGet, "/v1/apikeys", admin, ""), &listed)
	found := map[string]service.APIKey{}
	for _, k := range listed {
		found[k.Prefix] = k
	}
	if k := found[created.Prefix]; k.WorkspaceID != tm.id || len(k.AccountIDs) != 1 || k.AccountIDs[0] != tm.shared {
		t.Errorf("the team's key is listed as %+v", k)
	}
	if k := found[authtest.Prefix(admin)]; k.WorkspaceID != workspace.OperatorID {
		t.Errorf("the instance key is listed as %+v", k)
	}
	if resp := h.do(t, http.MethodDelete, "/v1/apikeys/"+created.Prefix, admin, ""); resp.StatusCode != http.StatusNoContent {
		t.Errorf("the operator revoking a team's key answered %d", resp.StatusCode)
	}
}
