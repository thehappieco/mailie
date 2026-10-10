package mcp_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
)

func TestAKeysListAccountsCarriesNoMailboxKey(t *testing.T) {
	// API keys are unchanged in phase 3 (docs/key-scheme.md section 12.15):
	// a tool reads a mailbox that has a key by what its key holds, and is
	// handed none of the key's material, which only a person's console
	// reads.
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	const work = "acc_00000000000000a1"
	h.mailbox(work, ana.user.ID, "ana@work.example")
	pair := authtest.KeyMailbox(t, h.store, work, ana.user.ID)
	cs := h.connect(h.key(ana, "read"), "", nil)

	res := call(t, cs, "list_accounts", nil)
	if res.IsError {
		t.Fatalf("list_accounts failed: %s", text(res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var accounts struct {
		Accounts []service.Account `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &accounts); err != nil {
		t.Fatal(err)
	}
	if len(accounts.Accounts) != 1 || !accounts.Accounts[0].Access.Read {
		t.Fatalf("the key lists %+v, want the mailbox it reads", accounts.Accounts)
	}
	for _, needle := range []string{"mailbox_key", "waiting_key", pair.Namespace,
		base64.RawURLEncoding.EncodeToString(pair.PublicKey)} {
		if strings.Contains(string(raw), needle) || strings.Contains(text(res), needle) {
			t.Errorf("list_accounts carries %q", needle)
		}
	}
}
