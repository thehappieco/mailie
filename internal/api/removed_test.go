package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/api"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/workspace"
)

func TestRemovingRecoveringAndDeletingForGoodAreThinRoutesOverTheService(t *testing.T) {
	h := newHarness(t, false)
	tm := newSupportTeam(t, h)
	tm.grant(t, h, `{"read":true,"act":false,"send":false,"manage":false}`)
	operator := h.key(t, auth.ScopeAdmin)
	status := func(method, path, token, body string, want int) *http.Response {
		t.Helper()
		resp := h.do(t, method, path, token, body)
		if resp.StatusCode != want {
			code, msg := decodeError(t, resp)
			t.Fatalf("%s %s: %d %s %s, want %d", method, path, resp.StatusCode, code, msg, want)
		}
		return resp
	}
	type removedMailbox struct {
		ID         string `json:"id"`
		Email      string `json:"email"`
		RemovedAt  int64  `json:"removed_at"`
		RemovedBy  string `json:"removed_by"`
		PurgeAfter int64  `json:"purge_after"`
		IndexKept  bool   `json:"index_kept"`
		Readers    int    `json:"readers"`
	}
	listed := func(token string) []removedMailbox {
		t.Helper()
		var out []removedMailbox
		decodeInto(t, status(http.MethodGet, "/v1/workspaces/"+tm.id+"/removed-mailboxes", token, "", http.StatusOK), &out)
		return out
	}
	removeShared := "/v1/accounts/" + tm.shared + "?confirm=" + tm.shared

	// Removing answers what it did: kept 30 days, until when.
	var removal struct {
		AccountID  string `json:"account_id"`
		Purged     bool   `json:"purged"`
		PurgeAfter int64  `json:"purge_after"`
	}
	before := time.Now()
	decodeInto(t, status(http.MethodDelete, removeShared, tm.ana, "", http.StatusOK), &removal)
	if removal.AccountID != tm.shared || removal.Purged ||
		removal.PurgeAfter < before.Add(30*24*time.Hour).Unix()-1 || removal.PurgeAfter > time.Now().Add(31*24*time.Hour).Unix() {
		t.Errorf("the removal answered %+v", removal)
	}
	status(http.MethodGet, "/v1/accounts/"+tm.shared, tm.ana, "", http.StatusNotFound)

	// Its owner lists it with what a recovery brings back; a member who read
	// it is refused the list, and the operator sees any workspace's.
	got := listed(tm.ana)
	if len(got) != 1 || got[0].ID != tm.shared || got[0].Email != "support@mail.example" || got[0].RemovedBy != tm.anaID ||
		got[0].PurgeAfter != removal.PurgeAfter || got[0].RemovedAt == 0 || !got[0].IndexKept || got[0].Readers != 2 {
		t.Errorf("Ana lists %+v", got)
	}
	status(http.MethodGet, "/v1/workspaces/"+tm.id+"/removed-mailboxes", tm.bea, "", http.StatusForbidden)
	if got := listed(operator); len(got) != 1 || got[0].ID != tm.shared {
		t.Errorf("the operator lists %+v", got)
	}

	// A member is told it does not exist; its owner recovers it, its sync
	// held until someone who may turns it on.
	status(http.MethodPost, "/v1/accounts/"+tm.shared+"/recover", tm.bea, "", http.StatusNotFound)
	var recovered struct {
		ID   string `json:"id"`
		Sync struct {
			Enabled   bool  `json:"enabled"`
			Held      bool  `json:"held"`
			HeldSince int64 `json:"held_since"`
		} `json:"sync"`
	}
	decodeInto(t, status(http.MethodPost, "/v1/accounts/"+tm.shared+"/recover", tm.ana, "", http.StatusOK), &recovered)
	if recovered.ID != tm.shared || recovered.Sync.Enabled || !recovered.Sync.Held || recovered.Sync.HeldSince == 0 {
		t.Errorf("the recovery answered %+v", recovered)
	}
	if got := listed(tm.ana); len(got) != 0 {
		t.Errorf("a recovered mailbox is still listed removed: %+v", got)
	}

	// Deleting for good takes a removed mailbox, and its id repeated.
	purge := "/v1/accounts/" + tm.shared + "/purge"
	status(http.MethodPost, purge, tm.ana, `{"confirm":"`+tm.shared+`"}`, http.StatusConflict)
	status(http.MethodDelete, removeShared, tm.ana, "", http.StatusOK)
	status(http.MethodPost, purge, tm.ana, `{"confirm":"`+tm.own+`"}`, http.StatusBadRequest)
	status(http.MethodPost, purge, tm.ana, "", http.StatusBadRequest)
	status(http.MethodPost, purge, tm.bea, `{"confirm":"`+tm.shared+`"}`, http.StatusNotFound)
	status(http.MethodPost, purge, tm.ana, `{"confirm":"`+tm.shared+`"}`, http.StatusNoContent)
	if got := listed(tm.ana); len(got) != 0 {
		t.Errorf("a mailbox deleted for good is still listed: %+v", got)
	}
	status(http.MethodPost, "/v1/accounts/"+tm.shared+"/recover", tm.ana, "", http.StatusNotFound)

	// A link that never connected is deleted at once, and says so.
	added := status(http.MethodPost, "/v1/accounts", operator, `{"email":"ops@gmail.com","flow":"pasted"}`, http.StatusCreated)
	var link struct {
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	decodeInto(t, added, &link)
	var deleted map[string]any
	decodeInto(t, status(http.MethodDelete, "/v1/accounts/"+link.Account.ID+"?confirm="+link.Account.ID, operator, "",
		http.StatusOK), &deleted)
	if _, kept := deleted["purge_after"]; deleted["account_id"] != link.Account.ID || deleted["purged"] != true || kept {
		t.Errorf("removing a link that never connected answered %v", deleted)
	}
	var operators []removedMailbox
	decodeInto(t, status(http.MethodGet, "/v1/workspaces/"+workspace.OperatorID+"/removed-mailboxes", operator, "",
		http.StatusOK), &operators)
	if len(operators) != 0 {
		t.Errorf("a link that never connected is kept: %+v", operators)
	}
}

func TestANoticeEventTellsAReaderToReadTheirNotices(t *testing.T) {
	h := newHarnessWith(t, func(h *api.Handler) { h.EventPing = 30 * time.Millisecond }, serviceOptions{})
	tm := newSupportTeam(t, h)
	tm.grant(t, h, `{"read":true,"act":false,"send":false,"manage":false}`)
	status := func(method, path, token string, want int) *http.Response {
		t.Helper()
		resp := h.do(t, method, path, token, "")
		if resp.StatusCode != want {
			code, msg := decodeError(t, resp)
			t.Fatalf("%s %s: %d %s %s, want %d", method, path, resp.StatusCode, code, msg, want)
		}
		return resp
	}
	type notice struct {
		ID               string `json:"id"`
		Kind             string `json:"kind"`
		WorkspaceName    string `json:"workspace_name"`
		AccountID        string `json:"account_id"`
		Mailbox          string `json:"mailbox"`
		State            string `json:"state"`
		RecoverableUntil int64  `json:"recoverable_until"`
	}
	notices := func(token string) []notice {
		t.Helper()
		var out []notice
		decodeInto(t, status(http.MethodGet, "/v1/me/notices", token, http.StatusOK), &out)
		return out
	}
	bea := h.openStream(t, "/v1/events", tm.bea, "")
	ana := h.openStream(t, "/v1/events", tm.ana, "")

	status(http.MethodDelete, "/v1/accounts/"+tm.shared+"?confirm="+tm.shared, tm.ana, http.StatusOK)

	// Bea read it: her stream says she lost it, and that she has a notice,
	// with nothing of it and no id, since a notice is no journal entry.
	if f, ok := bea.next(t); !ok || f.event != "access" || f.data != `{"account_id":"`+tm.shared+`","read":false}` {
		t.Fatalf("Bea's stream sent %+v", f)
	}
	if f, ok := bea.next(t); !ok || f.event != "notice" || f.id != "" || f.data != `{}` {
		t.Fatalf("then %+v, want a notice", f)
	}
	got := notices(tm.bea)
	if len(got) != 1 || got[0].Kind != "removed" || got[0].AccountID != tm.shared || got[0].Mailbox != "support@mail.example" ||
		got[0].State != "removed" || got[0].RecoverableUntil == 0 || got[0].WorkspaceName != "Support" {
		t.Fatalf("Bea's notices: %+v", got)
	}

	// Ana removed it: she lost it too, and is told nothing.
	if f, ok := ana.next(t); !ok || f.event != "access" {
		t.Fatalf("Ana's stream sent %+v", f)
	}
	if f, ok := ana.within(t, 300*time.Millisecond); ok {
		t.Errorf("Ana's stream went on with %+v", f)
	}
	if got := notices(tm.ana); len(got) != 0 {
		t.Errorf("whoever removed it has notices: %+v", got)
	}

	// A stream opened now says nothing of a notice given before it: the
	// console reads them when it starts.
	again := h.openStream(t, "/v1/events", tm.bea, "")
	if f, ok := again.within(t, 300*time.Millisecond); ok {
		t.Errorf("a new stream of Bea's sent %+v", f)
	}

	// A notice is its person's to dismiss, once; a key has none.
	status(http.MethodGet, "/v1/me/notices", h.key(t, auth.ScopeAdmin), http.StatusForbidden)
	status(http.MethodDelete, "/v1/me/notices/"+got[0].ID, tm.ana, http.StatusNotFound)
	status(http.MethodDelete, "/v1/me/notices/"+got[0].ID, tm.bea, http.StatusNoContent)
	status(http.MethodDelete, "/v1/me/notices/"+got[0].ID, tm.bea, http.StatusNotFound)
	if got := notices(tm.bea); len(got) != 0 {
		t.Errorf("a dismissed notice is still listed: %+v", got)
	}
}

// within returns the next event to arrive within d, skipping comments; ok is
// false when none did, or the stream ended.
func (s *sseStream) within(t *testing.T, d time.Duration) (frame, bool) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case f, ok := <-s.frames:
			if !ok {
				return frame{}, false
			}
			if f.event == "" {
				continue
			}
			return f, true
		case <-deadline:
			return frame{}, false
		}
	}
}
