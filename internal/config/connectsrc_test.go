package config_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/config"
)

func TestWithoutConnectSrcTheConsoleConnectsOnlyToItsOwnOrigin(t *testing.T) {
	for _, unset := range []string{"", "  "} {
		setenv(t, map[string]string{"MAIL_CREDENTIAL_KEY_HEX": validKey, "MAIL_DATA_DIR": t.TempDir(), "MAIL_CONNECT_SRC": unset})
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ConnectSrc != nil {
			t.Errorf("MAIL_CONNECT_SRC=%q: ConnectSrc = %q, want none", unset, cfg.ConnectSrc)
		}
		// The startup log says what it always said.
		if strings.Contains(cfg.String(), "connect_src") {
			t.Errorf("String() names origins nobody configured:\n%s", cfg.String())
		}
	}
}

func TestConnectSrcIsAListOfOriginsAsABrowserWritesThem(t *testing.T) {
	setenv(t, map[string]string{
		"MAIL_CREDENTIAL_KEY_HEX": validKey, "MAIL_DATA_DIR": t.TempDir(),
		"MAIL_CONNECT_SRC": " https://id.example.com\thttps://API.Example.com:443/ https://id.example.com:8443 " +
			"http://localhost:9000 http://app.localhost:5173 http://127.0.0.1:8081 " +
			"https://203.0.113.7 https://id.example.com\n",
	})
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://id.example.com", "https://api.example.com", "https://id.example.com:8443",
		"http://localhost:9000", "http://app.localhost:5173", "http://127.0.0.1:8081", "https://203.0.113.7",
	}
	if !slices.Equal(cfg.ConnectSrc, want) {
		t.Errorf("ConnectSrc = %q\nwant %q", cfg.ConnectSrc, want)
	}
	if !strings.Contains(cfg.String(), "connect_src="+strings.Join(want, ",")) {
		t.Errorf("String() does not list the origins:\n%s", cfg.String())
	}
}

func TestConnectSrcRefusesWhatIsNotExactlyOneOrigin(t *testing.T) {
	for raw, complaint := range map[string]string{
		"*":                                 "wildcards",
		"https://*.example.com":             "wildcards",
		"https://id.example.com:*":          "wildcards",
		"id.example.com":                    "want https://",
		"'self'":                            "want https://",
		"data:":                             "want https://",
		"wss://id.example.com":              "want https://",
		"https:id.example.com":              "no host",
		"https://":                          "no host",
		"https://id.example.com:":           "empty port",
		"https://id.example.com:99999":      "1-65535",
		"https://user:pass@id.example.com":  "credentials",
		"https://id.example.com/api":        "without a path",
		"https://id.example.com/?next=/":    "without a query",
		"https://id.example.com#frag":       "without a query or fragment",
		"https://id.example.com;script-src": "a DNS name or an IPv4 address",
		"https://id_example.com":            "a DNS name or an IPv4 address",
		"https://-id.example.com":           "a DNS name or an IPv4 address",
		"https://id..example.com":           "a DNS name or an IPv4 address",
		"https://[fe80::1%25en0]":           "IPv6",
		"http://[::1]:8081":                 "an IPv6 address cannot be a CSP source",
		"https://[2001:db8::1]:8443":        "an IPv6 address cannot be a CSP source",
		"https://[::ffff:203.0.113.7]":      "an IPv6 address cannot be a CSP source",
		"http://id.example.com":             "http is only allowed",
		"http://localhost.example.com":      "http is only allowed",
		"http://10.0.0.1:8080":              "http is only allowed",
	} {
		setenv(t, map[string]string{
			"MAIL_CREDENTIAL_KEY_HEX": validKey, "MAIL_DATA_DIR": t.TempDir(),
			"MAIL_CONNECT_SRC": "https://fine.example.com " + raw,
		})
		_, err := config.Load()
		if err == nil || !strings.Contains(err.Error(), "MAIL_CONNECT_SRC") || !strings.Contains(err.Error(), complaint) {
			t.Errorf("%s: want a complaint about %q, got %v", raw, complaint, err)
		}
		if err != nil && strings.Contains(err.Error(), "fine.example.com") {
			t.Errorf("%s: the good origin beside it was reported too: %v", raw, err)
		}
	}
}

func TestConnectSrcReportsEveryBadOriginAtOnceAndProdRequiresHTTPS(t *testing.T) {
	setenv(t, map[string]string{
		"MAIL_ENV": "prod", "MAIL_CREDENTIAL_KEY_HEX": validKey, "MAIL_DATA_DIR": t.TempDir(),
		"MAIL_CONNECT_SRC": "http://localhost:9000 https://*.example.com https://id.example.com/api",
	})
	_, err := config.Load()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{
		`MAIL_CONNECT_SRC: "http://localhost:9000": prod requires https`,
		`MAIL_CONNECT_SRC: "https://*.example.com": wildcards`,
		`MAIL_CONNECT_SRC: "https://id.example.com/api": must be an origin, without a path`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not say %q:\n%v", want, err)
		}
	}
}
