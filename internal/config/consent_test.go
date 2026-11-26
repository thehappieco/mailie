package config_test

import (
	"slices"
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

func TestEveryConsentVersionDefaultsToTheRevisionsTheOpenConsoleLastNamed(t *testing.T) {
	// The defaults name the deleted open console's last texts, which the
	// repository's history keeps (consent.go). They are pinned here, in Go
	// alone: a default that moved would ask everybody who agreed under it to
	// agree again to a text nobody wrote, and the core's console specs
	// (web/test/edition.ts) and the contract fixtures name these same
	// revisions.
	setenv(t, minimal(t, nil))
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := config.ConsentVersions{
		Sync: "2026-10-open-sync-4", Actions: "2026-10-open-actions-3", Send: "2026-10-open-sending", Keys: "2026-10-open-api-keys-3",
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

func TestTheRetiredMCPVariablesAreNamedOnlyWhereTheyAskForMCP(t *testing.T) {
	// This server serves no MCP of its own: what an earlier release read
	// to serve it refuses nothing now, and is named only where it would
	// have asked for MCP, so that the daemon can say so.
	for env, want := range map[string]struct {
		vars  map[string]string
		named []string
	}{
		"none set":            {nil, nil},
		"MCP over HTTP off":   {map[string]string{"MAIL_MCP_HTTP": "false"}, nil},
		"every one false":     {map[string]string{"MAIL_MCP_HTTP": "0", "MAIL_MCP_ELICIT_SEND": "FALSE"}, nil},
		"every one empty":     {map[string]string{"MAIL_MCP_HTTP": "", "MAIL_MCP_KEY": "", "MAIL_MCP_ELICIT_SEND": ""}, nil},
		"MCP over HTTP on":    {map[string]string{"MAIL_MCP_HTTP": "true"}, []string{"MAIL_MCP_HTTP"}},
		"no boolean":          {map[string]string{"MAIL_MCP_HTTP": "sometimes"}, []string{"MAIL_MCP_HTTP"}},
		"a key for stdio":     {map[string]string{"MAIL_MCP_KEY": "feedf00d.secret"}, []string{"MAIL_MCP_KEY"}},
		"elicitation":         {map[string]string{"MAIL_MCP_ELICIT_SEND": "1"}, []string{"MAIL_MCP_ELICIT_SEND"}},
		"all three, in order": {map[string]string{"MAIL_MCP_ELICIT_SEND": "true", "MAIL_MCP_KEY": "k", "MAIL_MCP_HTTP": "t"}, []string{"MAIL_MCP_HTTP", "MAIL_MCP_KEY", "MAIL_MCP_ELICIT_SEND"}},
	} {
		setenv(t, minimal(t, want.vars))
		cfg, err := config.Load()
		if err != nil {
			t.Errorf("%s: Load refused it: %v", env, err)
			continue
		}
		if !slices.Equal(cfg.RetiredMCP, want.named) {
			t.Errorf("%s: RetiredMCP = %v, want %v", env, cfg.RetiredMCP, want.named)
		}
		if strings.Contains(cfg.String(), "mcp") || strings.Contains(cfg.String(), "secret") {
			t.Errorf("%s: String() = %s", env, cfg.String())
		}
	}
}

func TestKeysSendByDefaultOnlyUnderTheDefaultKeyTermsAndAnotherEditionSaysWhetherTheyMay(t *testing.T) {
	// The default key terms (the deleted open console's last, consent.go)
	// say a key may send. Another edition's may not: a daemon serving them is
	// never left to a default that would let its keys send what its terms say
	// they cannot.
	setenv(t, minimal(t, nil))
	if cfg, err := config.Load(); err != nil || !cfg.KeysMaySend {
		t.Fatalf("the default key terms: KeysMaySend = %v, %v; want true", cfg.KeysMaySend, err)
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
	// key terms the same way: off under the default terms, said by the
	// edition under its own.
	setenv(t, minimal(t, map[string]string{"MAIL_CONSENT_VERSION_KEYS": "edition-keys-1", "MAIL_KEYS_MAY_SEND": "false"}))
	if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT") {
		t.Fatalf("other key terms without MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT: err = %v", err)
	}
	setenv(t, minimal(t, map[string]string{"MAIL_CONSENT_VERSION_KEYS": "edition-keys-1", "MAIL_KEYS_MAY_SEND": "false",
		"MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT": "true"}))
	if cfg, err := config.Load(); err != nil || !cfg.KeysActUnderCreatorConsent {
		t.Errorf("other key terms with MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT=true: %v, %v", cfg.KeysActUnderCreatorConsent, err)
	}
	// Set to the default terms by name, it is the default again.
	setenv(t, minimal(t, map[string]string{"MAIL_CONSENT_VERSION_KEYS": config.DefaultKeyTermsVersion}))
	if cfg, err := config.Load(); err != nil || !cfg.KeysMaySend {
		t.Errorf("the default key terms named: KeysMaySend = %v, %v; want true", cfg.KeysMaySend, err)
	}
}
