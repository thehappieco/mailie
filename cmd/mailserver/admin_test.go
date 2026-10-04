package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestTheKeyCommandsNameMailAdminAPIWhenTheDaemonHasNoKeyRoutes(t *testing.T) {
	// Without MAIL_ADMIN_API the daemon mounts no /v1/apikeys: with the
	// console it answers the console's JSON not_found, without one the
	// router's plain-text 404. Either way the operator is told why.
	for name, reply := range map[string]func(http.ResponseWriter){
		"with the console": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"not_found","message":"not found"}`))
		},
		"without the console": func(w http.ResponseWriter) {
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
