package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/config"
)

const validKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// setenv points the whole environment at a known state: Load reads os.Getenv
// directly, so a leftover variable from the developer's shell would otherwise
// decide what the test asserts.
func setenv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, e := range os.Environ() {
		if k, _, ok := strings.Cut(e, "="); ok && strings.HasPrefix(k, "MAIL_") {
			t.Setenv(k, "")
			os.Unsetenv(k)
		}
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	setenv(t, map[string]string{
		"MAIL_ENV":                "staging",
		"MAIL_CREDENTIAL_KEY_HEX": "not-hex",
		"MAIL_LOG_LEVEL":          "chatty",
		"MAIL_DATA_DIR":           t.TempDir(),
	})

	_, err := config.Load()
	if err == nil {
		t.Fatal("want an error, got none")
	}
	// One restart per typo is the failure mode this guards against.
	for _, want := range []string{"MAIL_ENV", "MAIL_CREDENTIAL_KEY_HEX", "MAIL_LOG_LEVEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}

func TestAMissingCredentialKeyIsFatal(t *testing.T) {
	setenv(t, map[string]string{"MAIL_DATA_DIR": t.TempDir()})

	_, err := config.Load()
	if err == nil {
		t.Fatal("want an error, got none")
	}
	if !strings.Contains(err.Error(), "MAIL_CREDENTIAL_KEY_HEX is required") {
		t.Fatalf("want the key to be reported as required, got:\n%v", err)
	}
}

func TestACredentialKeyOfTheWrongLengthIsRejected(t *testing.T) {
	setenv(t, map[string]string{
		"MAIL_CREDENTIAL_KEY_HEX": "abcd",
		"MAIL_DATA_DIR":           t.TempDir(),
	})

	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "want 32 bytes") {
		t.Fatalf("want a length complaint, got: %v", err)
	}
}

func TestPreviousKeysStayAvailableForDecryption(t *testing.T) {
	old := strings.Repeat("11", 32)
	setenv(t, map[string]string{
		"MAIL_CREDENTIAL_KEY_HEX":       validKey,
		"MAIL_CREDENTIAL_KEY_ID":        "2",
		"MAIL_CREDENTIAL_PREVIOUS_KEYS": "1:" + old,
		"MAIL_DATA_DIR":                 t.TempDir(),
	})

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Credentials.ActiveKeyID != 2 {
		t.Errorf("active key id = %d, want 2", cfg.Credentials.ActiveKeyID)
	}
	if len(cfg.Credentials.Keys) != 2 {
		t.Fatalf("keyring holds %d keys, want 2 (rows sealed under key 1 must stay readable)", len(cfg.Credentials.Keys))
	}
	if _, ok := cfg.Credentials.Keys[1]; !ok {
		t.Error("key 1 is missing from the keyring")
	}
}

func TestAPreviousKeyCannotReuseTheActiveKeyID(t *testing.T) {
	setenv(t, map[string]string{
		"MAIL_CREDENTIAL_KEY_HEX":       validKey,
		"MAIL_CREDENTIAL_PREVIOUS_KEYS": "1:" + strings.Repeat("22", 32),
		"MAIL_DATA_DIR":                 t.TempDir(),
	})

	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "also the active key id") {
		t.Fatalf("want a collision complaint, got: %v", err)
	}
}

func TestProdRefusesTheIMAPWireTrace(t *testing.T) {
	setenv(t, map[string]string{
		"MAIL_ENV":                "prod",
		"MAIL_CREDENTIAL_KEY_HEX": validKey,
		"MAIL_IMAP_DEBUG":         "true",
		"MAIL_DATA_DIR":           t.TempDir(),
	})

	_, err := config.Load()
	// The trace contains the AUTHENTICATE XOAUTH2 line, which is a live
	// bearer token in base64.
	if err == nil || !strings.Contains(err.Error(), "MAIL_IMAP_DEBUG") {
		t.Fatalf("want the wire trace refused in prod, got: %v", err)
	}
}

func TestStringNeverPrintsKeyMaterial(t *testing.T) {
	setenv(t, map[string]string{
		"MAIL_CREDENTIAL_KEY_HEX":   validKey,
		"MAIL_GOOGLE_CLIENT_ID":     "client.apps.googleusercontent.com",
		"MAIL_GOOGLE_CLIENT_SECRET": "GOCSPX-super-secret",
		"MAIL_ADMIN_KEY":            "deadbeef.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"MAIL_MCP_KEY":              "feedf00d.bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"MAIL_DATA_DIR":             t.TempDir(),
	})

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out := cfg.String()
	for _, secret := range []string{validKey, "GOCSPX-super-secret", "deadbeef.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"feedf00d.bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"} {
		if strings.Contains(out, secret) {
			t.Errorf("String() leaked %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "credential_keys=1*") {
		t.Errorf("String() should say which key is active, got:\n%s", out)
	}
	if !strings.Contains(out, "mcp_key=set") {
		t.Errorf("String() should say whether an MCP key is set, got:\n%s", out)
	}
}

func TestTheRealEnvironmentBeatsTheDotEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	// A stale file on a laptop must not be able to point a deployment at the
	// wrong credential key: every stored refresh token would stop decrypting.
	body := "MAIL_CREDENTIAL_KEY_HEX=" + strings.Repeat("99", 32) + "\n" +
		"export MAIL_HTTP_ADDR=\"127.0.0.1:9999\"\n" +
		"# a comment\n\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	setenv(t, map[string]string{
		"MAIL_CREDENTIAL_KEY_HEX": validKey,
		"MAIL_DATA_DIR":           dir,
	})

	if err := config.LoadDotEnv(path); err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Credentials.Keys[1]; string(got) == strings.Repeat("\x99", 32) {
		t.Error("the .env file overrode the real environment")
	}
	if cfg.HTTPAddr != "127.0.0.1:9999" {
		t.Errorf("HTTPAddr = %q, want the value from .env (it was unset in the environment)", cfg.HTTPAddr)
	}
}

func TestAMissingDotEnvFileIsNotAnError(t *testing.T) {
	if err := config.LoadDotEnv(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("want nil for a missing file, got %v", err)
	}
}

func TestDataDirDefaultsUnderTheUserConfigDirectory(t *testing.T) {
	setenv(t, map[string]string{"MAIL_CREDENTIAL_KEY_HEX": validKey})

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if filepath.Base(cfg.DataDir) != "mailserver" {
		t.Errorf("DataDir = %q, want it to end in mailserver", cfg.DataDir)
	}
	if got := cfg.DatabasePath(); filepath.Base(got) != "mail.db" {
		t.Errorf("DatabasePath = %q", got)
	}
}

func TestTheDownloadSpoolBudgetDefaultsToAGibibyteAndCanBeSet(t *testing.T) {
	setenv(t, map[string]string{"MAIL_CREDENTIAL_KEY_HEX": validKey, "MAIL_DATA_DIR": t.TempDir()})
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DownloadSpoolBytes != 1<<30 {
		t.Errorf("default = %d, want 1 GiB", cfg.DownloadSpoolBytes)
	}

	t.Setenv("MAIL_DOWNLOAD_SPOOL_MAX_BYTES", "256M")
	if cfg, err = config.Load(); err != nil || cfg.DownloadSpoolBytes != 256<<20 {
		t.Errorf("256M = %d, %v", cfg.DownloadSpoolBytes, err)
	}

	t.Setenv("MAIL_DOWNLOAD_SPOOL_MAX_BYTES", "lots")
	if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "MAIL_DOWNLOAD_SPOOL_MAX_BYTES") {
		t.Errorf("an unreadable budget: %v", err)
	}
}
