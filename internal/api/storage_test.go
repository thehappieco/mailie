package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

func TestOnlyTheOperatorsKeyIsToldTheDatabaseSizeAlongsideTheStorage(t *testing.T) {
	sync := newLendingEngine()
	h := newHarnessWith(t, nil, serviceOptions{sync: sync})
	ana := authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)
	authtest.NewUser(t, h.store, "olga@example.com", auth.RoleOwner)
	anaToken := authtest.SignIn(t, h.users, "ana@example.com")
	olgaToken := authtest.SignIn(t, h.users, "olga@example.com")
	if _, _, err := h.store.GrantSyncConsent(t.Context(), ana.ID, service.DefaultSyncConsentVersion); err != nil {
		t.Fatal(err)
	}
	const work = "acc_00000000000000d1"
	box := h.fakeMailbox(t, sync, work, ana.ID, "ana@work.example")
	box.Deliver("INBOX", providertest.FakeMessage{
		MessageID: "a@example.org", Subject: "Lunch", From: "bea@example.org", To: []string{"ana@work.example"},
		InternalDate: time.Now().Add(-time.Hour), Size: 4321,
	})
	storetest.IndexMailbox(t, h.store, work, box)
	usage, err := h.store.Usage(t.Context(), []string{work})
	if err != nil {
		t.Fatal(err)
	}
	held := usage[work].Bytes

	get := func(token string) map[string]any {
		t.Helper()
		resp := h.do(t, http.MethodGet, "/v1/me/storage", token, "")
		if resp.StatusCode != http.StatusOK {
			code, msg := decodeError(t, resp)
			t.Fatalf("GET /v1/me/storage answered %d %s %s", resp.StatusCode, code, msg)
		}
		var body map[string]any
		decodeInto(t, resp, &body)
		if body["rule_version"] != float64(store.RuleVersion) {
			t.Errorf("rule_version = %v, want %d", body["rule_version"], store.RuleVersion)
		}
		return body
	}

	anas := get(anaToken)
	if _, told := anas["database_bytes"]; told {
		t.Errorf("a member is told the database's size: %v", anas)
	}
	listed, _ := anas["mailboxes"].([]any)
	if len(listed) != 1 {
		t.Fatalf("ana's storage lists %v", anas["mailboxes"])
	}
	if m, _ := listed[0].(map[string]any); m["account_id"] != work || m["email"] != "ana@work.example" ||
		m["messages"] != float64(1) || m["bytes"] != float64(held) || m["provider_bytes"] != float64(4321) ||
		m["removed"] != false {
		t.Errorf("ana's mailbox = %v, want %d bytes under the rule", m, held)
	}
	if total, _ := anas["total"].(map[string]any); total["messages"] != float64(1) || total["bytes"] != float64(held) ||
		total["provider_bytes"] != float64(4321) {
		t.Errorf("ana's total = %v", anas["total"])
	}

	// An owner of the instance, signed in, is told nothing of the database
	// any more; the operator's key is, and a key that only reads is not.
	olgas := get(olgaToken)
	if _, told := olgas["database_bytes"]; told {
		t.Errorf("an owner signed in is told the database's size: %v", olgas)
	}
	if listed, _ := olgas["mailboxes"].([]any); len(listed) != 0 {
		t.Errorf("an owner sees another person's mailbox: %v", listed)
	}
	if size, _ := get(h.key(t, auth.ScopeAdmin))["database_bytes"].(float64); size <= 0 {
		t.Errorf("the operator's key is not told the database's size")
	}
	if _, told := get(h.key(t, auth.ScopeRead))["database_bytes"]; told {
		t.Errorf("an instance key that reads is told the database's size")
	}

	// Removed, the mailbox still counts, marked removed, until it is
	// deleted for good.
	if err := account.NewRepository(h.store, testKeyring(t)).SavePassword(t.Context(), work, "app-password"); err != nil {
		t.Fatal(err)
	}
	if resp := h.do(t, http.MethodDelete, "/v1/accounts/"+work+"?confirm="+work, anaToken, ""); resp.StatusCode != http.StatusOK {
		code, msg := decodeError(t, resp)
		t.Fatalf("removing ana's mailbox answered %d %s %s", resp.StatusCode, code, msg)
	}
	anas = get(anaToken)
	if listed, _ := anas["mailboxes"].([]any); len(listed) != 1 {
		t.Fatalf("after removing it, ana's storage lists %v", anas["mailboxes"])
	} else if m, _ := listed[0].(map[string]any); m["account_id"] != work || m["removed"] != true || m["bytes"] != float64(held) {
		t.Errorf("after removing it, ana's mailbox = %v", m)
	}

	if resp := h.do(t, http.MethodGet, "/v1/me/storage", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("nobody signed in: %d", resp.StatusCode)
	}
}
