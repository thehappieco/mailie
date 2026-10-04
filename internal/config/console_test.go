package config_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/config"
)

func TestThePublicURLIsAnOriginAndNothingElse(t *testing.T) {
	for raw, want := range map[string]string{
		"http://localhost:5174":           "http://localhost:5174",
		"http://LOCALHOST:5174/":          "http://localhost:5174",
		"http://127.0.0.1:8080":           "http://127.0.0.1:8080",
		"http://[::1]:8080":               "http://[::1]:8080",
		"https://console.mailie.example":  "https://console.mailie.example",
		"https://console.mailie.example/": "https://console.mailie.example",
		// A browser never writes the default port, and neither does the
		// redirect_uri registered with a provider.
		"https://console.example:443":  "https://console.example",
		"http://localhost:80":          "http://localhost",
		"https://console.example:8443": "https://console.example:8443",
	} {
		setenv(t, map[string]string{
			"MAIL_CREDENTIAL_KEY_HEX": validKey, "MAIL_DATA_DIR": t.TempDir(), "MAIL_PUBLIC_URL": raw,
		})
		cfg, err := config.Load()
		if err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		if cfg.PublicURL != want {
			t.Errorf("%s: PublicURL = %q, want %q", raw, cfg.PublicURL, want)
		}
	}

	for raw, complaint := range map[string]string{
		"console.mailie.example":                    "http:// or https://",
		"ftp://console.example":                     "http:// or https://",
		"http://console.example":                    "only allowed for localhost",
		"https://console.example/console":           "without a path",
		"https://console.example/?next=/":           "without a query",
		"https://console.example/#frag":             "without a query or fragment",
		"https://user:pass@console.example":         "credentials",
		"https://":                                  "no host",
		"http://localhost.evil.example:5174":        "only allowed for localhost",
		"http://127.0.0.1.nip.io:5174/oauth/return": "without a path",
		"https://console.example:":                  "empty port",
		"https://console.example:/":                 "empty port",
		"https://console.example:99999":             "1-65535",
		"http://localhost:0":                        "1-65535",
	} {
		setenv(t, map[string]string{
			"MAIL_CREDENTIAL_KEY_HEX": validKey, "MAIL_DATA_DIR": t.TempDir(), "MAIL_PUBLIC_URL": raw,
		})
		_, err := config.Load()
		if err == nil || !strings.Contains(err.Error(), "MAIL_PUBLIC_URL") || !strings.Contains(err.Error(), complaint) {
			t.Errorf("%s: want a complaint about %q, got %v", raw, complaint, err)
		}
	}
}

func TestAMalformedPublicURLIsNotAlsoReportedAsMissing(t *testing.T) {
	// The operator set it, wrongly; telling them a web client needs it as
	// well sends them looking for a variable that is right there.
	setenv(t, map[string]string{
		"MAIL_CREDENTIAL_KEY_HEX": validKey, "MAIL_DATA_DIR": t.TempDir(),
		"MAIL_PUBLIC_URL":               "https://console.example:",
		"MAIL_GOOGLE_WEB_CLIENT_ID":     "id.apps.googleusercontent.com",
		"MAIL_GOOGLE_WEB_CLIENT_SECRET": "not-a-real-secret",
	})
	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "empty port") {
		t.Fatalf("want the malformed URL reported, got %v", err)
	}
	if strings.Contains(err.Error(), "a web client needs MAIL_PUBLIC_URL") {
		t.Errorf("a malformed MAIL_PUBLIC_URL was also reported as missing: %v", err)
	}
}

func TestProdRequiresAnHTTPSPublicURL(t *testing.T) {
	setenv(t, map[string]string{
		"MAIL_ENV": "prod", "MAIL_CREDENTIAL_KEY_HEX": validKey, "MAIL_DATA_DIR": t.TempDir(),
		"MAIL_PUBLIC_URL": "http://localhost:8080",
	})
	if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "prod requires https") {
		t.Fatalf("want https required in prod, got %v", err)
	}
}

func TestAWebClientNeedsBothHalvesAndAnOrigin(t *testing.T) {
	setenv(t, map[string]string{
		"MAIL_CREDENTIAL_KEY_HEX":          validKey,
		"MAIL_DATA_DIR":                    t.TempDir(),
		"MAIL_GOOGLE_WEB_CLIENT_ID":        "web.apps.googleusercontent.com",
		"MAIL_MICROSOFT_WEB_CLIENT_SECRET": "secret-without-an-id",
	})
	_, err := config.Load()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{
		"MAIL_GOOGLE_WEB_CLIENT_ID and MAIL_GOOGLE_WEB_CLIENT_SECRET: set both or neither",
		"MAIL_MICROSOFT_WEB_CLIENT_ID and MAIL_MICROSOFT_WEB_CLIENT_SECRET: set both or neither",
		"MAIL_GOOGLE_WEB_CLIENT_ID: a web client needs MAIL_PUBLIC_URL",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not say %q:\n%v", want, err)
		}
	}
}

func TestTheConsoleSettingsAreReadAndItsSecretsNeverPrinted(t *testing.T) {
	web := t.TempDir()
	setenv(t, map[string]string{
		"MAIL_CREDENTIAL_KEY_HEX":          validKey,
		"MAIL_DATA_DIR":                    t.TempDir(),
		"MAIL_PUBLIC_URL":                  "https://console.mailie.example",
		"MAIL_WEB_DIR":                     web,
		"MAIL_GOOGLE_WEB_CLIENT_ID":        "web.apps.googleusercontent.com",
		"MAIL_GOOGLE_WEB_CLIENT_SECRET":    "GOCSPX-web-secret",
		"MAIL_MICROSOFT_WEB_CLIENT_ID":     "00000000-0000-0000-0000-000000000000",
		"MAIL_MICROSOFT_WEB_CLIENT_SECRET": "entra~web~secret",
		"MAIL_MICROSOFT_TENANT":            "organizations",
		"MAIL_ACCOUNT_ALLOW_PRIVATE":       "true",
		"MAIL_MICROSOFT_DEVICE_CODE":       "true",
	})
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.GoogleWeb.Configured() || cfg.MicrosoftWeb.ClientSecret != "entra~web~secret" || cfg.MicrosoftWeb.Tenant != "organizations" {
		t.Errorf("web clients = %+v / %+v", cfg.GoogleWeb, cfg.MicrosoftWeb)
	}
	if !cfg.AccountAllowPrivate || !cfg.MicrosoftDeviceCode {
		t.Errorf("flags not read: allow_private=%t device_code=%t", cfg.AccountAllowPrivate, cfg.MicrosoftDeviceCode)
	}
	if !filepath.IsAbs(cfg.WebDir) {
		t.Errorf("WebDir = %q, want an absolute path", cfg.WebDir)
	}

	out := cfg.String()
	for _, secret := range []string{"GOCSPX-web-secret", "entra~web~secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("String() leaked %q:\n%s", secret, out)
		}
	}
	for _, want := range []string{"google_web=set", "microsoft_web=set", "public_url=https://console.mailie.example"} {
		if !strings.Contains(out, want) {
			t.Errorf("String() lacks %q:\n%s", want, out)
		}
	}
}

func TestTheConsoleIsOffAndPrivateHostsRefusedByDefault(t *testing.T) {
	setenv(t, map[string]string{"MAIL_CREDENTIAL_KEY_HEX": validKey, "MAIL_DATA_DIR": t.TempDir()})
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebDir != "" || cfg.PublicURL != "" || cfg.AccountAllowPrivate || cfg.MicrosoftDeviceCode {
		t.Errorf("defaults = web %q, public %q, allow_private %t, device_code %t",
			cfg.WebDir, cfg.PublicURL, cfg.AccountAllowPrivate, cfg.MicrosoftDeviceCode)
	}
}

func TestLoopbackHostsAreRecognisedByNameAndAddress(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "LOCALHOST": true, "127.0.0.1": true, "127.0.0.2": true, "::1": true, "[::1]": true,
		"localhost.evil.example": false, "10.0.0.1": false, "console.mailie.example": false, "": false,
	} {
		if got := config.IsLoopbackHost(host); got != want {
			t.Errorf("IsLoopbackHost(%q) = %t, want %t", host, got, want)
		}
	}
}
