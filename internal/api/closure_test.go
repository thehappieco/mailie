package api_test

import (
	"net/http"
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
)

func TestClosingAnAccountOverRESTIsForAnInstanceAdminKeyOnly(t *testing.T) {
	h := newHarness(t, false)
	authtest.NewUser(t, h.store, "owner@example.com", auth.RoleOwner)
	ana := authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)
	owner := h.signIn(t, "owner@example.com", authtest.Password).Token
	anas := h.signIn(t, "ana@example.com", authtest.Password).Token
	body := `{"email":"ana@example.com"}`
	admin := h.key(t, auth.ScopeAdmin)
	added := h.do(t, http.MethodPost, "/v1/accounts", admin, h.passwordAccount(t, "shared@mail.example"))
	var created struct {
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	decodeInto(t, added, &created)

	for name, token := range map[string]string{
		"an owner's session": owner,
		"a read key":         h.key(t, auth.ScopeRead),
		"a restricted key":   h.key(t, auth.ScopeAdmin, created.Account.ID),
	} {
		for _, path := range []string{"/v1/users/disable", "/v1/users/delete"} {
			if resp := h.do(t, http.MethodPost, path, token, body); resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s on %s: %d, want 403", name, path, resp.StatusCode)
			}
		}
	}

	resp := h.do(t, http.MethodPost, "/v1/users/disable", admin, body)
	if resp.StatusCode != http.StatusOK {
		code, message := decodeError(t, resp)
		t.Fatalf("disable: %d %s %s", resp.StatusCode, code, message)
	}
	var disabled struct {
		ID            string `json:"id"`
		Email         string `json:"email"`
		SessionsEnded int    `json:"sessions_ended"`
		KeysRevoked   int    `json:"keys_revoked"`
	}
	decodeInto(t, resp, &disabled)
	if disabled.ID != ana.ID || disabled.Email != "ana@example.com" || disabled.SessionsEnded != 1 {
		t.Errorf("disable answered %+v", disabled)
	}
	if resp := h.do(t, http.MethodGet, "/v1/auth/me", anas, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a disabled person's session still answers %d", resp.StatusCode)
	}

	// The body is checked like every other: a misspelt flag is an error, not
	// a deletion without it.
	if resp := h.do(t, http.MethodPost, "/v1/users/delete", admin, `{"email":"ana@example.com","forse":true}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown field: %d, want 400", resp.StatusCode)
	}
	resp = h.do(t, http.MethodPost, "/v1/users/delete", admin, body)
	if resp.StatusCode != http.StatusOK {
		code, message := decodeError(t, resp)
		t.Fatalf("delete: %d %s %s", resp.StatusCode, code, message)
	}
	var deleted struct {
		ID              string `json:"id"`
		SessionsDeleted int    `json:"sessions_deleted"`
		AccountsRemoved int    `json:"accounts_removed"`
	}
	decodeInto(t, resp, &deleted)
	if deleted.ID != ana.ID || deleted.SessionsDeleted != 1 {
		t.Errorf("delete answered %+v", deleted)
	}
	if resp := h.do(t, http.MethodPost, "/v1/users/delete", admin, body); resp.StatusCode != http.StatusNotFound {
		t.Errorf("deleting again: %d, want 404", resp.StatusCode)
	}
}
