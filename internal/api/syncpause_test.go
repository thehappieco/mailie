package api_test

import (
	"net/http"
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
)

func TestTheSyncPauseRoutesAnswerTheMailboxsSync(t *testing.T) {
	h := newHarness(t, false)
	tm := newSupportTeam(t, h)
	tm.grant(t, h, `{"read":true,"act":false,"send":false,"manage":false}`)
	if resp := h.do(t, http.MethodPost, "/v1/me/sync-consent", tm.bea,
		`{"version":"`+service.DefaultSyncConsentVersion+`"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("Bea agreeing to sync: %d", resp.StatusCode)
	}
	type accountSync struct {
		Enabled     bool  `json:"enabled"`
		Paused      bool  `json:"paused"`
		PausedSince int64 `json:"paused_since"`
		Held        bool  `json:"held"`
	}
	answer := func(method, path, token, body string, want int) accountSync {
		t.Helper()
		resp := h.do(t, method, path, token, body)
		if resp.StatusCode != want {
			code, msg := decodeError(t, resp)
			t.Fatalf("%s %s %s: %d %s %s, want %d", method, path, body, resp.StatusCode, code, msg, want)
		}
		var st accountSync
		if want == http.StatusOK {
			decodeInto(t, resp, &st)
		}
		return st
	}
	pause := func(id string) string { return "/v1/accounts/" + id + "/sync/pause" }
	resume := func(id string) string { return "/v1/accounts/" + id + "/sync/resume" }
	current := `{"version":"` + service.DefaultSyncConsentVersion + `"}`

	// The team's mailbox: no body or an empty one pauses it, and answers
	// its sync; its resume names the sync text, and nothing else.
	if st := answer(http.MethodPost, pause(tm.shared), tm.ana, "", http.StatusOK); !st.Paused || st.PausedSince == 0 || !st.Enabled {
		t.Errorf("the pause answered %+v", st)
	}
	answer(http.MethodPost, pause(tm.shared), tm.ana, "{}", http.StatusOK)
	answer(http.MethodPost, pause(tm.shared), tm.bea, "", http.StatusForbidden)
	answer(http.MethodPost, resume(tm.shared), tm.ana, "", http.StatusBadRequest)
	answer(http.MethodPost, resume(tm.shared), tm.ana, `{"version":"an-older-text"}`, http.StatusBadRequest)
	answer(http.MethodPost, resume(tm.shared), tm.ana, `{"version":"`+service.DefaultSyncConsentVersion+`","more":1}`,
		http.StatusBadRequest)
	if st := answer(http.MethodPost, resume(tm.shared), tm.ana, current, http.StatusOK); st.Paused || !st.Enabled {
		t.Errorf("the resume answered %+v", st)
	}

	// Bea's own: no version, with no body or an empty one.
	answer(http.MethodPost, pause(tm.own), tm.bea, "{}", http.StatusOK)
	answer(http.MethodPost, resume(tm.own), tm.bea, current, http.StatusBadRequest)
	if st := answer(http.MethodPost, resume(tm.own), tm.bea, "", http.StatusOK); st.Paused {
		t.Errorf("Bea's resume answered %+v", st)
	}

	// A key never pauses: an instance key does not see the team's mailbox,
	// and a workspace key holding Bea's is refused.
	answer(http.MethodPost, pause(tm.shared), h.key(t, auth.ScopeAdmin), "", http.StatusNotFound)
	created := h.createKey(t, tm.bea, authtest.Personal(t, h.store, tm.beaID), keyBody("write", holding(tm.own, true, true, false)))
	answer(http.MethodPost, pause(tm.own), created.Key, "", http.StatusForbidden)

	// A removed mailbox is not found.
	answer(http.MethodDelete, "/v1/accounts/"+tm.own+"?confirm="+tm.own, tm.bea, "", http.StatusOK)
	answer(http.MethodPost, pause(tm.own), tm.bea, "", http.StatusNotFound)
	answer(http.MethodPost, resume(tm.own), tm.bea, "", http.StatusNotFound)
}
