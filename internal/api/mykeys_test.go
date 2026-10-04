package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
)

func createKeyBody(scope, terms string) string {
	return fmt.Sprintf(`{"name":"assistant","scope":%q,"terms_version":%q}`, scope, terms)
}

func TestOnlyThePersonSignedInManagesTheirKeys(t *testing.T) {
	h := newHarness(t, true)
	token := h.person(t, "ana@example.com", auth.RoleOwner)
	resp := h.do(t, http.MethodPost, "/v1/me/apikeys", token, createKeyBody("write", service.DefaultKeyTermsVersion))
	if resp.StatusCode != http.StatusCreated {
		code, msg := decodeError(t, resp)
		t.Fatalf("creating a key answered %d %s %s", resp.StatusCode, code, msg)
	}
	var created struct {
		Key    string `json:"key"`
		Prefix string `json:"prefix"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("the answer carrying a new key may be cached")
	}
	// The key it made, and an instance admin key: neither mints, lists or
	// revokes a person's keys.
	for _, key := range []string{created.Key, h.key(t, auth.ScopeAdmin)} {
		for _, c := range []struct{ method, path, body string }{
			{http.MethodGet, "/v1/me/apikeys", ""},
			{http.MethodPost, "/v1/me/apikeys", createKeyBody("read", service.DefaultKeyTermsVersion)},
			{http.MethodDelete, "/v1/me/apikeys/" + created.Prefix, ""},
		} {
			resp := h.do(t, c.method, c.path, key, c.body)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s with a key answered %d, want 403", c.method, c.path, resp.StatusCode)
			}
		}
	}
	// Nor does a person's key reach the operator's key routes.
	if resp := h.do(t, http.MethodPost, "/v1/apikeys", created.Key, `{"name":"x","scope":"read"}`); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a person's key on POST /v1/apikeys answered %d", resp.StatusCode)
	}
	if resp := h.do(t, http.MethodDelete, "/v1/me/apikeys/"+created.Prefix, token, ""); resp.StatusCode != http.StatusNoContent {
		t.Errorf("revoking with the session answered %d", resp.StatusCode)
	}
}

func TestAKeyForKeyTermsThePersonWasNotShownIsAConflict(t *testing.T) {
	h := newHarness(t, false)
	token := h.person(t, "ana@example.com", auth.RoleMember)
	resp := h.do(t, http.MethodPost, "/v1/me/apikeys", token, createKeyBody("read", "2025-01-api-keys"))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("old terms answered %d, want 409", resp.StatusCode)
	}
	if code, _ := decodeError(t, resp); code != string(service.CodeConflict) {
		t.Errorf("code = %s", code)
	}
}

func TestSomebodyElsesKeyIsNotFound(t *testing.T) {
	h := newHarness(t, false)
	ana := h.person(t, "ana@example.com", auth.RoleMember)
	bob := h.person(t, "bob@example.com", auth.RoleMember)
	resp := h.do(t, http.MethodPost, "/v1/me/apikeys", bob, createKeyBody("read", service.DefaultKeyTermsVersion))
	var created struct {
		Prefix string `json:"prefix"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if resp := h.do(t, http.MethodDelete, "/v1/me/apikeys/"+created.Prefix, ana, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("revoking bob's key as ana answered %d, want 404", resp.StatusCode)
	}
	resp = h.do(t, http.MethodGet, "/v1/me/apikeys", ana, "")
	var listed []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Errorf("ana lists %v", listed)
	}
}

func TestAKeyAnAdministratorMadeForAPersonCannotReadThroughREST(t *testing.T) {
	// The API hands a person's mail only to their own session or to a key
	// they created in the console, agreeing to the key terms: on every
	// route, not only on /mcp.
	h := newHarness(t, true)
	ana := h.person(t, "ana@example.com", auth.RoleMember)
	h.mailbox(t, ana, "ana@mail.example")
	var anaID string
	if err := h.store.Reader().QueryRowContext(t.Context(),
		`SELECT id FROM users WHERE email = 'ana@example.com'`).Scan(&anaID); err != nil {
		t.Fatal(err)
	}
	minted := authtest.NewKey(t, h.store, auth.ScopeRead, anaID)
	resp := h.do(t, http.MethodPost, "/v1/me/apikeys", ana, createKeyBody("read", service.DefaultKeyTermsVersion))
	var hers struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&hers); err != nil || hers.Key == "" {
		t.Fatalf("creating her key: %d %v", resp.StatusCode, err)
	}
	for _, path := range []string{"/v1/accounts", "/v1/messages", "/v1/events/wait?timeout=1"} {
		resp := h.do(t, http.MethodGet, path, minted, "")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s with a key made for her answered %d, want 403", path, resp.StatusCode)
		} else if code, _ := decodeError(t, resp); code != string(service.CodeNotAuthorized) {
			t.Errorf("GET %s: code %s", path, code)
		}
		if resp := h.do(t, http.MethodGet, path, hers.Key, ""); resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s with the key she created answered %d", path, resp.StatusCode)
		}
	}
	// Nor does an administrator make such a key any more.
	resp = h.do(t, http.MethodPost, "/v1/apikeys", h.key(t, auth.ScopeAdmin),
		`{"name":"for ana","scope":"read","user_id":"`+anaID+`"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an administrator issuing a key for a person answered %d, want 400", resp.StatusCode)
	}
}
