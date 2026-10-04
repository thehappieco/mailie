package obs_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/obs"
)

func TestAnAddressKeepsOnlyItsFirstLetterAndDomain(t *testing.T) {
	got := obs.Redact("fetching for alice.smith+work@example.com now")
	if strings.Contains(got, "alice.smith") {
		t.Fatalf("the local part survived: %q", got)
	}
	if !strings.Contains(got, "a…@example.com") {
		t.Fatalf("want a…@example.com, got %q", got)
	}
}

func TestAnAddressInAnyScriptKeepsOnlyItsFirstLetterToo(t *testing.T) {
	for in, want := range map[string]string{
		"refused joão@exemplo.com.br":   "refused j…@exemplo.com.br",
		"refused müller@beispiel.de":    "refused m…@beispiel.de",
		"refused o'brien@example.ie":    "refused o…@example.ie",
		"refused josé@correo.españa.es": "refused j…@correo.españa.es",
	} {
		if got := obs.Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAnAPIKeyKeepsItsPrefixAndLosesItsSecret(t *testing.T) {
	// The prefix is a lookup selector, so keeping it makes a log line
	// traceable to a caller; the rest authenticates and must not survive.
	got := obs.Redact("key deadbeef.Zm9vYmFyYmF6cXV4Zm9vYmFyYmF6cXV4Zm9vYmFyYmF6 rejected")
	if strings.Contains(got, "Zm9vYmFy") {
		t.Fatalf("the secret survived: %q", got)
	}
	if !strings.Contains(got, "deadbeef.") {
		t.Fatalf("the prefix should survive for correlation: %q", got)
	}
}

func TestAnAuthorizationHeaderNeverSurvives(t *testing.T) {
	got := obs.Redact("Authorization: Bearer ya29.a0AfH6SMBx-very-long-google-token")
	if strings.Contains(got, "ya29") {
		t.Fatalf("the bearer token survived: %q", got)
	}
}

func TestTheXOAUTH2LineNeverSurvives(t *testing.T) {
	// The IMAP wire trace prints this verbatim, and the base64 blob is a
	// live access token.
	got := obs.Redact("C: a3 AUTHENTICATE XOAUTH2 dXNlcj1tZUBleGFtcGxlLmNvbQFhdXRoPUJlYXJlciB0b2sxMjMBAQ==")
	if strings.Contains(got, "dXNlcj1t") {
		t.Fatalf("the SASL initial response survived: %q", got)
	}
	if !strings.Contains(got, "AUTHENTICATE XOAUTH2") {
		t.Fatalf("the command itself should stay legible: %q", got)
	}
}

func TestAnAttributeNamedLikeACredentialIsDroppedWhole(t *testing.T) {
	var buf bytes.Buffer
	lg := obs.NewLoggerTo(&buf, "debug", "text")
	lg.Info("refreshed", "refresh_token", "1//0eXaMpLe-refresh-token", "account", "acc_1")

	if strings.Contains(buf.String(), "0eXaMpLe") {
		t.Fatalf("a token attribute reached the log: %s", buf.String())
	}
}

// errFromProvider is what a provider's refusal looks like on the way to a log
// line: an error value, not a string, quoting the account it refused.
var errFromProvider = errors.New("oauth: invalid_grant: AADSTS50020: User account 'alice.smith@contoso.com' " +
	"does not exist in tenant; retried with Authorization: Bearer ya29.a0AfH6SMBx-live-access-token")

func TestTheProcessLoggerMasksAddressesAtEveryLevel(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		for _, level := range []string{"debug", "info", "warn", "error"} {
			var buf bytes.Buffer
			lg := obs.NewLoggerTo(&buf, level, format)
			lg.Log(context.Background(), levelOf(t, level), "signing in bob@contoso.onmicrosoft.com",
				"login", "carol.jones@example.org")

			out := buf.String()
			for _, leaked := range []string{"bob@", "carol.jones"} {
				if strings.Contains(out, leaked) {
					t.Errorf("%s/%s: %q reached the log: %s", format, level, leaked, out)
				}
			}
			if !strings.Contains(out, "b…@contoso.onmicrosoft.com") || !strings.Contains(out, "c…@example.org") {
				t.Errorf("%s/%s: the masked forms are missing: %s", format, level, out)
			}
		}
	}
}

func TestAnAddressOrATokenInsideAnErrorIsMaskedToo(t *testing.T) {
	// The attribute every failure is logged under is "err", and slog hands
	// an error to the handler as a value, not as a string.
	for _, format := range []string{"json", "text"} {
		var buf bytes.Buffer
		lg := obs.NewLoggerTo(&buf, "info", format)
		lg.Warn("authorization failed", "account", "acc_1", "err", fmt.Errorf("finishing consent: %w", errFromProvider))

		out := buf.String()
		for _, leaked := range []string{"alice.smith", "ya29", "live-access-token"} {
			if strings.Contains(out, leaked) {
				t.Errorf("%s: %q reached the log: %s", format, leaked, out)
			}
		}
		if !strings.Contains(out, "a…@contoso.com") || !strings.Contains(out, "AADSTS50020") {
			t.Errorf("%s: the error should stay legible, masked: %s", format, out)
		}
	}
}

func TestAnyOtherValueIsMaskedTooAndANilErrorIsHarmless(t *testing.T) {
	var buf bytes.Buffer
	lg := obs.NewLoggerTo(&buf, "info", "json")
	var missing *os.PathError
	lg.Info("values", "hosts", []string{"dave@example.net"}, "cause", error(missing), "none", nil)

	out := buf.String()
	if strings.Contains(out, "dave@") {
		t.Errorf("an address inside a slice reached the log: %s", out)
	}
	if !strings.Contains(out, `"none":"<nil>"`) {
		t.Errorf("a nil value should log as <nil>: %s", out)
	}
}

func levelOf(t *testing.T, name string) slog.Level {
	t.Helper()
	var l slog.Level
	if err := l.UnmarshalText([]byte(name)); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestLoggerFromNeverReturnsNil(t *testing.T) {
	if obs.LoggerFrom(context.Background()) == nil {
		t.Fatal("a handler that must nil-check before logging is a handler that stops logging")
	}
	lg := obs.NewLogger("info", "text")
	if got := obs.LoggerFrom(obs.WithLogger(context.Background(), lg)); got != lg {
		t.Fatal("WithLogger/LoggerFrom did not round-trip")
	}
}

func TestMetricsRegisterOnAnIsolatedRegistry(t *testing.T) {
	m := obs.NewMetrics()
	m.SyncPasses.WithLabelValues("acc_1", "INBOX", "ok").Inc()

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == "mailserver_sync_passes_total" {
			return
		}
	}
	t.Fatal("mailserver_sync_passes_total was not registered")
}
