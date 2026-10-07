package api_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store/storetest"
	"github.com/thehappieco/mailie/internal/workspace"
)

// actionsHarness is ana, signed in, with a mailbox on a fake server that
// sync has indexed: an inbox with one message, an archive, a trash and a
// folder of hers.
type actionsHarness struct {
	*harness
	token   string
	userID  string
	account string
	box     *providertest.FakeMailbox
	msg     int64
}

func newActionsHarness(t *testing.T) *actionsHarness {
	t.Helper()
	e := newLendingEngine()
	h := newHarnessWith(t, nil, serviceOptions{sync: e})
	ana := authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)
	token := authtest.SignIn(t, h.users, "ana@example.com")
	if resp := h.do(t, http.MethodPost, "/v1/me/sync-consent", token,
		`{"version":"`+service.DefaultSyncConsentVersion+`"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("sync consent: %d", resp.StatusCode)
	}
	const id = "acc_00000000000000e2"
	box := h.fakeMailbox(t, e, id, ana.ID, "ana@mail.example")
	box.CreateFolder("Archive", imap.MailboxAttrArchive)
	box.CreateFolder("Trash", imap.MailboxAttrTrash)
	box.CreateFolder("Work")
	uid := box.Deliver("INBOX", providertest.FakeMessage{
		MessageID: "lunch@example.org", Subject: "Lunch", From: "Bea <bea@example.org>", To: []string{"ana@mail.example"},
	})
	storetest.IndexMailbox(t, h.store, id, box)
	return &actionsHarness{harness: h, token: token, userID: ana.ID, account: id, box: box, msg: h.messageRow(t, id, uid)}
}

func (h *actionsHarness) allow(t *testing.T) {
	t.Helper()
	resp := h.do(t, http.MethodPost, "/v1/me/actions-consent", h.token, `{"version":"`+service.DefaultActionsConsentVersion+`"}`)
	if resp.StatusCode != http.StatusOK {
		code, msg := decodeError(t, resp)
		t.Fatalf("actions consent: %d %s %s", resp.StatusCode, code, msg)
	}
}

func (h *actionsHarness) folderID(t *testing.T, name string) int64 {
	t.Helper()
	var id int64
	if err := h.store.Reader().QueryRowContext(t.Context(), `SELECT id FROM folders WHERE account_id = ? AND name = ?`,
		h.account, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func actionResult(t *testing.T, resp *http.Response) service.ActionResult {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		code, msg := decodeError(t, resp)
		t.Fatalf("status %d: %s %s", resp.StatusCode, code, msg)
	}
	var out service.ActionResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTheActionRoutesNeedTheWriteScope(t *testing.T) {
	h := newActionsHarness(t)
	h.allow(t)
	readKey := authtest.NewWorkspaceKey(t, h.store, auth.ScopeRead, authtest.Personal(t, h.store, h.userID), h.userID,
		workspace.KeyGrant{AccountID: h.account, Flags: workspace.Flags{Read: true}})
	for _, route := range []struct{ method, path, body string }{
		{http.MethodPatch, fmt.Sprintf("/v1/messages/%d", h.msg), `{"seen":true}`},
		{http.MethodPost, "/v1/messages/flags", fmt.Sprintf(`{"ids":[%d],"seen":true}`, h.msg)},
		{http.MethodPost, "/v1/messages/move", fmt.Sprintf(`{"ids":[%d],"to":"archive"}`, h.msg)},
		{http.MethodPost, "/v1/messages/trash", fmt.Sprintf(`{"ids":[%d]}`, h.msg)},
	} {
		resp := h.do(t, route.method, route.path, readKey, route.body)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s with a read key answered %d", route.method, route.path, resp.StatusCode)
		}
		if code, _ := decodeError(t, resp); code != "not_authorized" {
			t.Errorf("%s %s: code %q", route.method, route.path, code)
		}
	}
	if n := h.box.Opens("interactive"); n != 0 {
		t.Fatalf("read-key requests opened %d connections", n)
	}
}

func TestWithoutTheOwnersConsentAnActionIsAConflict(t *testing.T) {
	h := newActionsHarness(t)
	resp := h.do(t, http.MethodPatch, fmt.Sprintf("/v1/messages/%d", h.msg), h.token, `{"seen":true}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code, _ := decodeError(t, resp); code != "conflict" {
		t.Fatalf("code = %q", code)
	}
}

func TestMarkingAMessageReadAndStarringItAnswersWithTheRowAsItIsNow(t *testing.T) {
	h := newActionsHarness(t)
	h.allow(t)
	res := actionResult(t, h.do(t, http.MethodPatch, fmt.Sprintf("/v1/messages/%d", h.msg), h.token,
		`{"seen":true,"flagged":true}`))
	if len(res.Messages) != 1 || !res.Messages[0].Seen || !res.Messages[0].Flagged || len(res.Removed) != 0 {
		t.Fatalf("result = %+v", res)
	}
	res = actionResult(t, h.do(t, http.MethodPost, "/v1/messages/flags", h.token,
		fmt.Sprintf(`{"ids":[%d],"seen":false}`, h.msg)))
	if res.Messages[0].Seen || !res.Messages[0].Flagged {
		t.Fatalf("result = %+v", res)
	}
	// A body with a field the route does not know is refused, not ignored.
	resp := h.do(t, http.MethodPatch, fmt.Sprintf("/v1/messages/%d", h.msg), h.token, `{"read":true}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown field answered %d", resp.StatusCode)
	}
}

func TestAMoveTakesAFolderIDAsAStringOrANumberAndATrashIsItsOwnRoute(t *testing.T) {
	h := newActionsHarness(t)
	h.allow(t)
	work := h.folderID(t, "Work")
	res := actionResult(t, h.do(t, http.MethodPost, "/v1/messages/move", h.token,
		fmt.Sprintf(`{"ids":[%d],"to":"%d"}`, h.msg, work)))
	if len(res.Messages) != 1 || res.Messages[0].ID != h.msg || res.Messages[0].FolderID != work {
		t.Fatalf("result = %+v", res)
	}
	res = actionResult(t, h.do(t, http.MethodPost, "/v1/messages/move", h.token,
		fmt.Sprintf(`{"ids":[%d],"to":%d}`, h.msg, h.folderID(t, "INBOX"))))
	if res.Messages[0].FolderRole != "inbox" {
		t.Fatalf("result = %+v", res)
	}
	resp := h.do(t, http.MethodPost, "/v1/messages/move", h.token, fmt.Sprintf(`{"ids":[%d],"to":"trash"}`, h.msg))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a move to the trash answered %d", resp.StatusCode)
	}
	res = actionResult(t, h.do(t, http.MethodPost, "/v1/messages/trash", h.token, fmt.Sprintf(`{"ids":[%d]}`, h.msg)))
	if res.Messages[0].FolderRole != "trash" || res.Messages[0].ID != h.msg {
		t.Fatalf("result = %+v", res)
	}
	resp = h.do(t, http.MethodPost, "/v1/messages/trash", h.token, fmt.Sprintf(`{"ids":[%d]}`, h.msg))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("trashing a message in the trash answered %d", resp.StatusCode)
	}
}

func TestTheActionsConsentRoutesAreThePersonsLikeTheSyncConsents(t *testing.T) {
	h := newActionsHarness(t)
	// A key of her workspace acts as nobody: it neither reads her consent
	// nor gives it.
	key := authtest.NewWorkspaceKey(t, h.store, auth.ScopeSend, authtest.Personal(t, h.store, h.userID), h.userID)
	resp := h.do(t, http.MethodGet, "/v1/me/actions-consent", key, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a key of her workspace reading her consent: %d", resp.StatusCode)
	}
	resp = h.do(t, http.MethodPost, "/v1/me/actions-consent", key, `{"version":"`+service.DefaultActionsConsentVersion+`"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a key agreeing for her answered %d", resp.StatusCode)
	}
	resp = h.do(t, http.MethodPost, "/v1/me/actions-consent", h.token, `{"version":"2026-01-older"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an older text answered %d", resp.StatusCode)
	}
	h.allow(t)
	resp = h.do(t, http.MethodDelete, "/v1/me/actions-consent", h.token, "")
	body, _ := io.ReadAll(resp.Body)
	var c service.ActionsConsent
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &c) != nil || c.Consented ||
		c.CurrentVersion != service.DefaultActionsConsentVersion {
		t.Fatalf("withdrawing: %d %s", resp.StatusCode, body)
	}
}
