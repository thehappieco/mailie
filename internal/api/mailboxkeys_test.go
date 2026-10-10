package api_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/keyscheme"
	"github.com/thehappieco/mailie/internal/provider"
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

func TestNoRouteTakesAMailboxPrivateKey(t *testing.T) {
	// The server never receives a mailbox's private key (docs/key-scheme.md
	// section 8). Each route that carries a mailbox key or a grant refuses a
	// body that adds one, for that field alone: the same body without it is
	// accepted.
	h := newHarness(t, false)
	tm := newSupportTeam(t, h)
	// Ana's own mailbox, to give a new key; a team mailbox from before the
	// key scheme, which she reads by the flag, to give a first one; and
	// Carol, a member who holds read on the team's keyed mailbox and waits
	// for its key, to supply it to.
	personal := h.mailbox(t, tm.ana, "ana@mail.example")
	orders, err := account.NewRepository(h.store, nil).Create(t.Context(), account.Account{
		ID: "acc_00000000000000e1", WorkspaceID: tm.id, Email: "orders@mail.example", Provider: provider.KindIMAP,
		AuthKind: "password", IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example", SMTPPort: 465,
		SMTPTLS: "implicit", LoginUser: "orders@mail.example", State: account.StateActive,
	}, tm.anaID)
	if err != nil {
		t.Fatal(err)
	}
	h.person(t, "carol@example.com", auth.RoleMember)
	carol, err := h.users.GetByEmail(t.Context(), "carol@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.Write(t.Context(), func(tx *sql.Tx) error {
		if err := workspace.NewRepository(h.store, nil).AddMemberTx(t.Context(), tx, tm.id, carol.ID, workspace.RoleMember, time.Now()); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act, send,
			manage, granted_by, created_at, updated_at) VALUES (?, ?, ?, 1, 0, 0, 0, ?, 0, 0)`, tm.shared, tm.id, carol.ID, tm.anaID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	for _, route := range []struct {
		what, method, path, body string
		accepted                 int
	}{
		{"linking a mailbox", http.MethodPost, "/v1/accounts",
			linkAs(t, tm.ana, h.passwordAccount(t, "ana.work@mail.example")), http.StatusCreated},
		{"giving read with a grant", http.MethodPut, "/v1/accounts/" + tm.shared + "/access/" + tm.beaID,
			sealedFor(t, `{"read":true,"act":false,"send":false,"manage":false}`), http.StatusOK},
		{"supplying the key", http.MethodPut, "/v1/accounts/" + tm.shared + "/grants/" + carol.ID,
			jsonOf(t, map[string]any{"epoch": 1, "grant": grantAt(t, 1)}), http.StatusOK},
		{"a first key", http.MethodPost, "/v1/accounts/" + orders.ID + "/mailbox-key", jsonOf(t, map[string]any{
			"public_key": mailboxPublicKey(t), "namespace": keyscheme.NewSealID(),
			"grants": []map[string]any{{"user_id": tm.anaID, "grant": grantAt(t, 1)}},
		}), http.StatusCreated},
		{"a new key", http.MethodPut, "/v1/accounts/" + personal + "/mailbox-key", jsonOf(t, map[string]any{
			"epoch": 2, "public_key": mailboxPublicKey(t), "grant": grantAt(t, 2),
		}), http.StatusOK},
	} {
		var fields map[string]any
		if err := json.Unmarshal([]byte(route.body), &fields); err != nil {
			t.Fatal(err)
		}
		fields["private_key"] = mailboxPublicKey(t)
		resp := h.do(t, route.method, route.path, tm.ana, jsonOf(t, fields))
		if code, msg := decodeError(t, resp); resp.StatusCode != http.StatusBadRequest || code != "bad_request" ||
			!strings.Contains(msg, `"private_key"`) {
			t.Errorf("%s with a private key: %d %s %q, want 400 for the field", route.what, resp.StatusCode, code, msg)
		}
		if resp := h.do(t, route.method, route.path, tm.ana, route.body); resp.StatusCode != route.accepted {
			code, msg := decodeError(t, resp)
			t.Errorf("%s without it: %d %s %s, want %d", route.what, resp.StatusCode, code, msg, route.accepted)
		}
	}
}
