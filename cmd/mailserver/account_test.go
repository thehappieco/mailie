package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/config"
)

// fakeDaemon answers GET /v1/accounts/{id} with whatever the test scripts,
// one reply per poll.
func fakeDaemon(t *testing.T, replies ...func(http.ResponseWriter)) (config.Config, *atomic.Int32) {
	t.Helper()
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer deadbeef.secret" {
			t.Errorf("poll without the admin key: %q", r.Header.Get("Authorization"))
		}
		n := int(polls.Add(1)) - 1
		if n >= len(replies) {
			n = len(replies) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		replies[n](w)
	}))
	t.Cleanup(srv.Close)
	return config.Config{HTTPAddr: strings.TrimPrefix(srv.URL, "http://"), AdminKey: "deadbeef.secret"}, &polls
}

func state(s, reason string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"id":"acc_1","state":"` + s + `","state_reason":"` + reason + `"}`))
	}
}

func TestWaitingForConsentRidesOutARateLimit(t *testing.T) {
	// The person is halfway through a consent screen; a 429 on the poll is
	// no reason to abandon them.
	throttled := func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":"rate_limited","message":"too many requests"}`))
	}
	cfg, polls := fakeDaemon(t, throttled, state("active", ""))

	started := time.Now()
	if err := waitForState(t.Context(), cfg, "acc_1", "active", time.Minute); err != nil {
		t.Fatalf("waitForState: %v", err)
	}
	if polls.Load() != 2 {
		t.Errorf("polled %d times, want 2", polls.Load())
	}
	// Never faster than the poll interval, even when Retry-After says less.
	if waited := time.Since(started); waited < pollInterval {
		t.Errorf("retried after %v, want at least %v", waited, pollInterval)
	}
}

func TestWaitingForConsentStopsWhenTheAccountFails(t *testing.T) {
	cfg, polls := fakeDaemon(t, state("error", "consent was declined"))
	err := waitForState(t.Context(), cfg, "acc_1", "active", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "consent was declined") {
		t.Fatalf("err = %v, want the reason the account failed", err)
	}
	if polls.Load() != 1 {
		t.Errorf("polled %d times after the account failed", polls.Load())
	}
}

func TestWaitingForConsentGivesUpOnARealError(t *testing.T) {
	cfg, _ := fakeDaemon(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","message":"no such account"}`))
	})
	if err := waitForState(t.Context(), cfg, "acc_1", "active", time.Minute); err == nil ||
		!strings.Contains(err.Error(), "no such account") {
		t.Fatalf("err = %v", err)
	}
}

func TestACustomDomainOnICloudCanNameItsSignInFromTheCLI(t *testing.T) {
	// Apple refuses an iCloud+ custom-domain address as the login; the
	// account's iCloud address has to travel with it.
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/accounts" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Errorf("decoding the request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"account":{"id":"acc_1","email":"ana@lima.example","provider":"icloud","auth_kind":"password","state":"active"}}`))
	}))
	t.Cleanup(srv.Close)
	cfg := config.Config{HTTPAddr: strings.TrimPrefix(srv.URL, "http://"), AdminKey: "deadbeef.secret"}

	err := accountAdd(t.Context(), cfg, []string{
		"--email", "ana@lima.example", "--provider", "icloud", "--password", "abcd-efgh-ijkl-mnop",
		"--login-user", "ana@icloud.com",
	})
	if err != nil {
		t.Fatalf("account add: %v", err)
	}
	if sent["login_user"] != "ana@icloud.com" || sent["email"] != "ana@lima.example" || sent["provider"] != "icloud" {
		t.Errorf("sent %v", sent)
	}
	for _, key := range []string{"imap_host", "imap_port", "smtp_host", "smtp_port", "smtp_tls"} {
		if _, ok := sent[key]; ok {
			t.Errorf("sent %s, which the daemon refuses for iCloud", key)
		}
	}
}
