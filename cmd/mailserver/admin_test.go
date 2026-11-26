package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/config"
)

func TestTheKeyCommandsNameMailAdminAPIWhenTheDaemonHasNoKeyRoutes(t *testing.T) {
	// Without MAIL_ADMIN_API the daemon mounts no /v1/apikeys: it answers
	// the API's JSON not_found, and an earlier release with no console the
	// router's plain-text 404. Either way the operator is told why.
	for name, reply := range map[string]func(http.ResponseWriter){
		"this release": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"not_found","message":"no such endpoint"}`))
		},
		"an earlier release without the console": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("404 page not found\n"))
		},
	} {
		cfg, _ := fakeDaemon(t, reply)
		if _, err := adminGet(t.Context(), cfg, "/v1/apikeys"); err == nil || !strings.Contains(err.Error(), "MAIL_ADMIN_API=true") {
			t.Errorf("%s: apikey list failed with %v, which does not name MAIL_ADMIN_API", name, err)
		}
	}
}

func TestAKeyNobodyHoldsDoesNotBlameMailAdminAPI(t *testing.T) {
	// A mounted key route's own not_found, a prefix no key has, is the
	// operator's mistake, not the daemon's configuration.
	cfg, _ := fakeDaemon(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","message":"no such api key"}`))
	})
	_, err := adminDo(t.Context(), cfg, http.MethodDelete, "/v1/apikeys/0a0b0c99", nil)
	if err == nil || strings.Contains(err.Error(), "MAIL_ADMIN_API") {
		t.Errorf("revoking a key nobody holds failed with %v", err)
	}
}

func TestAMissingAccountDoesNotBlameMailAdminAPI(t *testing.T) {
	cfg, _ := fakeDaemon(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","message":"not found"}`))
	})
	_, err := adminGet(t.Context(), cfg, "/v1/accounts/acc_gone")
	if err == nil || strings.Contains(err.Error(), "MAIL_ADMIN_API") {
		t.Errorf("a missing account failed with %v", err)
	}
}

func TestTheKeyListSaysWhyAKeyStopped(t *testing.T) {
	cfg, _ := fakeDaemon(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`[
		  {"prefix":"0a0b0c01","name":"Live","scope":"read","workspace_id":"wsp_1","created_at":1,"expires_at":0},
		  {"prefix":"0a0b0c02","name":"Left","scope":"read","workspace_id":"wsp_1","created_at":1,"revoked_at":2,
		   "stop_reason":"creator_left"},
		  {"prefix":"0a0b0c03","name":"Old","scope":"read","workspace_id":"wsp_1","created_at":1,"revoked_at":2}
		]`))
	})
	out, err := captureStdout(t, func() error { return apikeyList(t.Context(), cfg, nil) })
	if err != nil {
		t.Fatal(err)
	}
	for prefix, state := range map[string]string{
		"0a0b0c01": "active", "0a0b0c02": "revoked (creator_left)", "0a0b0c03": "revoked",
	} {
		var line string
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, prefix) {
				line = strings.TrimSpace(l)
			}
		}
		if !strings.HasSuffix(line, "  "+state) {
			t.Errorf("key %s is listed %q, want it to end %q", prefix, line, state)
		}
	}
}

func TestTheHoldCommandsHoldAndReleaseAKeyItsIssuerMadeBySelector(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer deadbeef.secret" {
			t.Errorf("asked without the admin key: %q", r.Header.Get("Authorization"))
		}
		asked = append(asked, r.Method+" "+r.URL.EscapedPath())
		if strings.Contains(r.URL.Path, "ffff") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"not_found","message":"this server holds no live key under that selector"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	cfg := config.Config{HTTPAddr: strings.TrimPrefix(srv.URL, "http://"), AdminKey: "deadbeef.secret"}
	for _, args := range [][]string{{"hold", "0123456789abcdef"}, {"release", "0123456789abcdef"}} {
		if err := apikeyCommand(t.Context(), cfg, args); err != nil {
			t.Fatalf("apikey %s: %v", args[0], err)
		}
	}
	if want := []string{"PUT /v1/issued-keys/0123456789abcdef/hold", "DELETE /v1/issued-keys/0123456789abcdef/hold"}; !slices.Equal(asked, want) {
		t.Errorf("asked %q, want %q", asked, want)
	}
	if err := apikeyCommand(t.Context(), cfg, []string{"hold", "ffffffffffffffff"}); err == nil ||
		!strings.Contains(err.Error(), "no live key") {
		t.Errorf("a selector not held: %v", err)
	}
	if err := apikeyCommand(t.Context(), cfg, []string{"hold"}); !errors.Is(err, errUsage) {
		t.Errorf("no selector: %v", err)
	}
}
