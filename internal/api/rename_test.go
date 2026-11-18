package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
)

func TestRenamingAMailboxIsAPatchOfItsDisplayNameAnsweredAsAGet(t *testing.T) {
	h := newHarness(t, false)
	ana := h.person(t, "ana@example.com", auth.RoleMember)
	id := h.mailbox(t, ana, "ana@mail.example")
	var renamed, read map[string]any
	decodeInto(t, h.do(t, http.MethodPatch, "/v1/accounts/"+id, ana, `{"display_name":"  Work "}`), &renamed)
	decodeInto(t, h.do(t, http.MethodGet, "/v1/accounts/"+id, ana, ""), &read)
	if renamed["display_name"] != "Work" {
		t.Errorf("the rename answered the name %v", renamed["display_name"])
	}
	// The same presentation as the card read right after, but for the sync
	// figures, which the engine may have moved.
	delete(renamed, "sync")
	delete(read, "sync")
	a, _ := json.Marshal(renamed)
	b, _ := json.Marshal(read)
	if string(a) != string(b) {
		t.Errorf("the rename answered\n%s\nwhere a GET answers\n%s", a, b)
	}

	for body, want := range map[string]int{
		`{}`:                      http.StatusBadRequest,
		`{"display_name":null}`:   http.StatusBadRequest,
		`{"display_name":"a\nb"}`: http.StatusBadRequest,
		`{"name":"Work"}`:         http.StatusBadRequest,
		``:                        http.StatusBadRequest,
	} {
		if resp := h.do(t, http.MethodPatch, "/v1/accounts/"+id, ana, body); resp.StatusCode != want {
			t.Errorf("PATCH %q: %d, want %d", body, resp.StatusCode, want)
		}
	}
	bob := h.person(t, "bob@example.com", auth.RoleMember)
	if resp := h.do(t, http.MethodPatch, "/v1/accounts/"+id, bob, `{"display_name":"Mine"}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("another person renaming it: %d, want 404", resp.StatusCode)
	}
	key := authtest.NewKey(t, h.store, auth.ScopeAdmin)
	if resp := h.do(t, http.MethodPatch, "/v1/accounts/"+id, key, `{"display_name":"Ops"}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("the operator renaming a person's mailbox: %d, want 404", resp.StatusCode)
	}
	if resp := h.do(t, http.MethodPatch, "/v1/accounts/"+id, ana, `{"display_name":""}`); resp.StatusCode != http.StatusOK {
		t.Errorf("clearing the name: %d", resp.StatusCode)
	}
}
