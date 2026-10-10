package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
)

func TestClosingAnAccountOverRESTIsForTheOperatorOrAnInstanceOwnerSignedIn(t *testing.T) {
	h := newHarness(t, false)
	authtest.NewUser(t, h.store, "owner@example.com", auth.RoleOwner)
	authtest.NewUser(t, h.store, "mo@example.com", auth.RoleMember)
	ana := authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)
	owner := h.signIn(t, "owner@example.com").Token
	member := h.signIn(t, "mo@example.com").Token
	anas := h.signIn(t, "ana@example.com").Token
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
		"a member's session": member,
		"a read key":         h.key(t, auth.ScopeRead),
		"a restricted key":   h.key(t, auth.ScopeAdmin, created.Account.ID),
	} {
		for _, path := range []string{"/v1/users/disable", "/v1/users/delete"} {
			if resp := h.do(t, http.MethodPost, path, token, body); resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s on %s: %d, want 403", name, path, resp.StatusCode)
			}
		}
	}

	// An owner of the instance signed in administers its people.
	resp := h.do(t, http.MethodPost, "/v1/users/disable", owner, body)
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

func TestDisablingOrDeletingTheLastReaderOverRESTNeedsForce(t *testing.T) {
	h := newHarness(t, false)
	tm := newSupportTeam(t, h)
	authtest.NewUser(t, h.store, "keeper@example.com", auth.RoleOwner)
	admin := h.key(t, auth.ScopeAdmin)
	// Ana owns the team and is the only one who reads its mailbox; make
	// another owner, so that only the mailbox stands in the way.
	if resp := h.do(t, http.MethodPatch, "/v1/workspaces/"+tm.id+"/members/"+tm.beaID, tm.ana, `{"role":"owner"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("making Bea an owner: %d", resp.StatusCode)
	}
	for _, path := range []string{"/v1/users/disable", "/v1/users/delete"} {
		resp := h.do(t, http.MethodPost, path, admin, `{"email":"ana@example.com"}`)
		code, message := decodeError(t, resp)
		if resp.StatusCode != http.StatusConflict || code != "conflict" || !strings.Contains(message, tm.shared) {
			t.Errorf("%s of the last reader: %d %s %q", path, resp.StatusCode, code, message)
		}
	}
	// Once Bea reads it too, Ana goes; the team's mailbox stays the team's.
	if resp := h.do(t, http.MethodPut, "/v1/accounts/"+tm.shared+"/access/"+tm.beaID, tm.ana,
		sealedFor(t, h, tm.beaID, `{"read":true,"act":false,"send":false,"manage":false}`)); resp.StatusCode != http.StatusOK {
		t.Fatalf("granting Bea read: %d", resp.StatusCode)
	}
	if resp := h.do(t, http.MethodPost, "/v1/users/delete", admin, `{"email":"ana@example.com"}`); resp.StatusCode != http.StatusOK {
		code, message := decodeError(t, resp)
		t.Fatalf("deleting Ana once Bea reads it: %d %s %s", resp.StatusCode, code, message)
	}
	if resp := h.do(t, http.MethodGet, "/v1/accounts/"+tm.shared, tm.bea, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("the team's mailbox after its linker was deleted: %d", resp.StatusCode)
	}
}

func TestClosingTheLastReaderOfATeamThatOutlivesThemOverRESTNeedsForce(t *testing.T) {
	// Bea, the team's other member, has her membership disabled: the team
	// outlives Ana all the same, and nobody could read its mailbox again.
	h := newHarness(t, false)
	tm := newSupportTeam(t, h)
	authtest.NewUser(t, h.store, "keeper@example.com", auth.RoleOwner)
	admin := h.key(t, auth.ScopeAdmin)
	if resp := h.do(t, http.MethodPatch, "/v1/workspaces/"+tm.id+"/members/"+tm.beaID, tm.ana, `{"status":"disabled"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("disabling Bea's membership: %d", resp.StatusCode)
	}
	for _, path := range []string{"/v1/users/disable", "/v1/users/delete"} {
		resp := h.do(t, http.MethodPost, path, admin, `{"email":"ana@example.com"}`)
		code, message := decodeError(t, resp)
		if resp.StatusCode != http.StatusConflict || code != "conflict" || !strings.Contains(message, tm.shared) {
			t.Errorf("%s of the last reader of a team that outlives her: %d %s %q", path, resp.StatusCode, code, message)
		}
	}
	if resp := h.do(t, http.MethodPost, "/v1/users/delete", admin, `{"email":"ana@example.com","force":true}`); resp.StatusCode != http.StatusOK {
		code, message := decodeError(t, resp)
		t.Fatalf("deleting Ana with force: %d %s %s", resp.StatusCode, code, message)
	}
	var n int
	if err := h.store.Reader().QueryRowContext(t.Context(), `SELECT count(*) FROM accounts WHERE id = ?`, tm.shared).Scan(&n); err != nil || n != 1 {
		t.Errorf("the team's mailbox went with Ana: %d, %v", n, err)
	}
}
