package config_test

import (
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/config"
)

// minimal is the least the daemon starts with, plus extra.
func minimal(t *testing.T, extra map[string]string) map[string]string {
	t.Helper()
	env := map[string]string{"MAIL_CREDENTIAL_KEY_HEX": validKey, "MAIL_DATA_DIR": t.TempDir()}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

func TestEveryConsentVersionDefaultsToTheOpenConsolesText(t *testing.T) {
	// The open console's texts (web/src/open) name these revisions, and its
	// contract spec holds them to the fixtures the handlers write; a default
	// that moved without them would ask everybody to agree to a text nobody
	// wrote.
	setenv(t, minimal(t, nil))
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := config.ConsentVersions{
		Sync: "2026-10-open-sync-3", Actions: "2026-10-open-actions-3", Send: "2026-10-open-sending", Keys: "2026-10-open-api-keys-2",
	}
	if cfg.Consent != want || config.DefaultConsentVersions() != want {
		t.Fatalf("consent versions %+v, defaults %+v; want %+v", cfg.Consent, config.DefaultConsentVersions(), want)
	}
	if (config.ConsentVersions{Send: "s2"}).OrDefaults() != (config.ConsentVersions{
		Sync: want.Sync, Actions: want.Actions, Send: "s2", Keys: want.Keys,
	}) {
		t.Error("OrDefaults does not keep a configured revision and default the rest")
	}
}

func TestEachConsentVersionIsReadFromItsOwnVariable(t *testing.T) {
	setenv(t, minimal(t, map[string]string{
		"MAIL_CONSENT_VERSION_SYNC":           "sync-2",
		"MAIL_CONSENT_VERSION_ACTIONS":        "actions-2",
		"MAIL_CONSENT_VERSION_SEND":           "send-2",
		"MAIL_CONSENT_VERSION_KEYS":           "keys-2",
		"MAIL_KEYS_MAY_SEND":                  "false",
		"MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT": "false",
	}))
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := (config.ConsentVersions{Sync: "sync-2", Actions: "actions-2", Send: "send-2", Keys: "keys-2"}); cfg.Consent != want {
		t.Fatalf("consent versions %+v, want %+v", cfg.Consent, want)
	}
	if out := cfg.String(); !strings.Contains(out, "consent_versions=sync:sync-2,actions:actions-2,send:send-2,keys:keys-2") {
		t.Errorf("String() does not show the revisions asked for: %s", out)
	}
}

func TestAConsentVersionIsPrintableASCIIWithoutSpacesAndAtMost64Bytes(t *testing.T) {
	// A revision is compared byte for byte with what a console sends back,
	// so nothing that could be spelt two ways is accepted.
	for _, bad := range []string{"two words", "tab\there", "versão-2", "line\nbreak", strings.Repeat("v", 65)} {
		setenv(t, minimal(t, map[string]string{"MAIL_CONSENT_VERSION_ACTIONS": bad}))
		if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "MAIL_CONSENT_VERSION_ACTIONS") {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
	longest := strings.Repeat("v", 64)
	setenv(t, minimal(t, map[string]string{"MAIL_CONSENT_VERSION_KEYS": longest, "MAIL_KEYS_MAY_SEND": "false",
		"MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT": "false"}))
	if cfg, err := config.Load(); err != nil || cfg.Consent.Keys != longest {
		t.Fatalf("64 bytes: %q, %v", cfg.Consent.Keys, err)
	}
}

func TestTheMCPHTTPEndpointIsOnUnlessSwitchedOff(t *testing.T) {
	setenv(t, minimal(t, nil))
	cfg, err := config.Load()
	if err != nil || !cfg.MCPHTTP {
		t.Fatalf("by default: MCPHTTP = %t, %v", cfg.MCPHTTP, err)
	}
	if !strings.Contains(cfg.String(), "mcp_http=true") {
		t.Errorf("String() = %s", cfg.String())
	}

	setenv(t, minimal(t, map[string]string{"MAIL_MCP_HTTP": "false"}))
	cfg, err = config.Load()
	if err != nil || cfg.MCPHTTP {
		t.Fatalf("MAIL_MCP_HTTP=false: MCPHTTP = %t, %v", cfg.MCPHTTP, err)
	}
	if !strings.Contains(cfg.String(), "mcp_http=false") {
		t.Errorf("String() = %s", cfg.String())
	}

	setenv(t, minimal(t, map[string]string{"MAIL_MCP_HTTP": "sometimes"}))
	if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "MAIL_MCP_HTTP") {
		t.Errorf("MAIL_MCP_HTTP=sometimes: err = %v", err)
	}
}

func TestKeysSendByDefaultOnlyUnderTheOpenKeyTermsAndAnotherEditionSaysWhetherTheyMay(t *testing.T) {
	// The open console's key terms say a key may send. Another edition's
	// may not: a daemon serving them is never left to a default that would
	// let its keys send what its terms say they cannot.
	setenv(t, minimal(t, nil))
	if cfg, err := config.Load(); err != nil || !cfg.KeysMaySend {
		t.Fatalf("the open key terms: KeysMaySend = %v, %v; want true", cfg.KeysMaySend, err)
	}
	setenv(t, minimal(t, map[string]string{"MAIL_CONSENT_VERSION_KEYS": "edition-keys-1"}))
	if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "MAIL_KEYS_MAY_SEND") {
		t.Fatalf("other key terms without MAIL_KEYS_MAY_SEND: err = %v", err)
	}
	for value, want := range map[string]bool{"false": false, "true": true} {
		setenv(t, minimal(t, map[string]string{"MAIL_CONSENT_VERSION_KEYS": "edition-keys-1", "MAIL_KEYS_MAY_SEND": value,
			"MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT": "false"}))
		if cfg, err := config.Load(); err != nil || cfg.KeysMaySend != want {
			t.Errorf("other key terms with MAIL_KEYS_MAY_SEND=%s: KeysMaySend = %v, %v", value, cfg.KeysMaySend, err)
		}
	}
	// Whether a key acts under its creator's actions consent goes with the
	// key terms the same way: off under the open terms, said by the edition
	// under its own.
	setenv(t, minimal(t, map[string]string{"MAIL_CONSENT_VERSION_KEYS": "edition-keys-1", "MAIL_KEYS_MAY_SEND": "false"}))
	if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT") {
		t.Fatalf("other key terms without MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT: err = %v", err)
	}
	setenv(t, minimal(t, map[string]string{"MAIL_CONSENT_VERSION_KEYS": "edition-keys-1", "MAIL_KEYS_MAY_SEND": "false",
		"MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT": "true"}))
	if cfg, err := config.Load(); err != nil || !cfg.KeysActUnderCreatorConsent {
		t.Errorf("other key terms with MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT=true: %v, %v", cfg.KeysActUnderCreatorConsent, err)
	}
	// Set to the open terms by name, it is the open default again.
	setenv(t, minimal(t, map[string]string{"MAIL_CONSENT_VERSION_KEYS": config.DefaultKeyTermsVersion}))
	if cfg, err := config.Load(); err != nil || !cfg.KeysMaySend {
		t.Errorf("the open key terms named: KeysMaySend = %v, %v; want true", cfg.KeysMaySend, err)
	}
}
