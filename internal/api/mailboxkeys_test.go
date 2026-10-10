package api_test

import (
	"database/sql"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/keyscheme"
	"github.com/thehappieco/mailie/internal/workspace"
)

func TestTheMailboxKeyRoutesAreThinOverTheService(t *testing.T) {
	h := newHarness(t, false)
	tm := newSupportTeam(t, h)
	status := func(method, path, token, body string, want int) *http.Response {
		t.Helper()
		resp := h.do(t, method, path, token, body)
		if resp.StatusCode != want {
			code, msg := decodeError(t, resp)
			t.Fatalf("%s %s: %d %s %s, want %d", method, path, resp.StatusCode, code, msg, want)
		}
		return resp
	}
	keyPath := "/v1/accounts/" + tm.shared + "/mailbox-key"

	// Ana linked the team's mailbox with its key, and reads it with her grant.
	var state struct {
		Epoch     int    `json:"epoch"`
		Namespace string `json:"namespace"`
		Grant     string `json:"grant"`
		Waiting   []struct {
			UserID string `json:"user_id"`
		} `json:"waiting"`
	}
	decodeInto(t, status(http.MethodGet, keyPath, tm.ana, "", http.StatusOK), &state)
	if state.Epoch != 1 || state.Grant == "" || !keyscheme.ValidNamespace(state.Namespace) {
		t.Fatalf("Ana's console reads %+v", state)
	}

	// No route takes a private key: the body is refused before anything.
	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/accounts", linkAs(t, tm.ana, `{"email":"x@gmail.com","flow":"loopback","private_key":"a"}`)},
		{http.MethodPut, "/v1/accounts/" + tm.shared + "/access/" + tm.beaID,
			`{"read":true,"act":false,"send":false,"manage":false,"private_key":"a"}`},
		{http.MethodPost, keyPath, `{"public_key":"a","namespace":"a","grants":[],"private_key":"a"}`},
		{http.MethodPut, keyPath, `{"epoch":2,"public_key":"a","grant":"a","private_key":"a"}`},
		{http.MethodPut, "/v1/accounts/" + tm.shared + "/grants/" + tm.beaID, `{"epoch":1,"grant":"a","private_key":"a"}`},
	} {
		status(route.method, route.path, tm.ana, route.body, http.StatusBadRequest)
	}

	// Read on it comes with Bea's grant, after a fresh step-up: Ana's sign-in
	// is eleven minutes old, and a step-up opens the window again.
	if _, err := h.store.Writer().ExecContext(t.Context(),
		`UPDATE sessions SET authenticated_at = authenticated_at - 660 WHERE user_id = ?`, tm.anaID); err != nil {
		t.Fatal(err)
	}
	give := sealedFor(t, `{"read":true,"act":false,"send":false,"manage":false}`)
	resp := status(http.MethodPut, "/v1/accounts/"+tm.shared+"/access/"+tm.beaID, tm.ana, give, http.StatusForbidden)
	if code, _ := decodeError(t, resp); code != "not_authorized" {
		t.Errorf("a stale step-up answered %s", code)
	}
	status(http.MethodPost, "/v1/auth/stepup", tm.ana, fmt.Sprintf(`{"auth_key":%q}`, authtest.AuthKey), http.StatusOK)
	var grant struct {
		Read   bool `json:"read"`
		Sealed bool `json:"sealed"`
	}
	decodeInto(t, status(http.MethodPut, "/v1/accounts/"+tm.shared+"/access/"+tm.beaID, tm.ana, give, http.StatusOK), &grant)
	if !grant.Read || !grant.Sealed {
		t.Errorf("Bea's grant answered %+v", grant)
	}

	// Carol is given read with her grant and reset: she waits for the key,
	// and Bea, a member who reads it, supplies it.
	h.person(t, "carol@example.com", auth.RoleMember)
	carolUser, err := h.users.GetByEmail(t.Context(), "carol@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.Write(t.Context(), func(tx *sql.Tx) error {
		return workspace.NewRepository(h.store, nil).AddMemberTx(t.Context(), tx, tm.id, carolUser.ID, workspace.RoleMember, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	status(http.MethodPut, "/v1/accounts/"+tm.shared+"/access/"+carolUser.ID, tm.ana, give, http.StatusOK)
	code, _, err := h.users.CreateReset(t.Context(), carolUser.ID, false, "cli")
	if err != nil {
		t.Fatal(err)
	}
	carol, _, _, err := h.users.CompleteReset(t.Context(), code, "carol@example.com", authtest.Enrolment(t), "authtest")
	if err != nil {
		t.Fatal(err)
	}
	var account struct {
		Access struct {
			Read       bool `json:"read"`
			WaitingKey bool `json:"waiting_key"`
		} `json:"access"`
	}
	decodeInto(t, status(http.MethodGet, "/v1/accounts/"+tm.shared, carol, "", http.StatusOK), &account)
	if account.Access.Read || !account.Access.WaitingKey {
		t.Errorf("Carol after her reset: %+v", account.Access)
	}
	decodeInto(t, status(http.MethodGet, keyPath, tm.bea, "", http.StatusOK), &state)
	if len(state.Waiting) != 1 || state.Waiting[0].UserID != carolUser.ID {
		t.Errorf("Bea's console offers %+v", state.Waiting)
	}
	status(http.MethodPut, "/v1/accounts/"+tm.shared+"/grants/"+carolUser.ID, tm.bea,
		fmt.Sprintf(`{"epoch":1,"grant":%q}`, grantAt(t, 1)), http.StatusOK)
	account.Access.Read, account.Access.WaitingKey = false, false
	decodeInto(t, status(http.MethodGet, "/v1/accounts/"+tm.shared, carol, "", http.StatusOK), &account)
	if !account.Access.Read || account.Access.WaitingKey {
		t.Errorf("Carol once supplied: %+v", account.Access)
	}

	// The mailbox has a key: no first key; and a team's is never renewed.
	status(http.MethodPost, keyPath, tm.ana, jsonOf(t, map[string]any{
		"public_key": mailboxPublicKey(t), "namespace": keyscheme.NewSealID(),
		"grants": []map[string]any{{"user_id": tm.anaID, "grant": grantAt(t, 1)}},
	}), http.StatusConflict)
	status(http.MethodPut, keyPath, tm.ana, jsonOf(t, map[string]any{
		"epoch": 2, "public_key": mailboxPublicKey(t), "grant": grantAt(t, 2),
	}), http.StatusForbidden)
	// Nobody outside the team learns the mailbox exists.
	outsider := h.person(t, "dan@example.com", auth.RoleMember)
	status(http.MethodGet, keyPath, outsider, "", http.StatusNotFound)
}
