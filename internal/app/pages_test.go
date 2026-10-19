package app

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/webui"
)

func TestABinaryThatEmbedsTheDaemonAddsToTheConsolesPagesThroughItsOptions(t *testing.T) {
	cfg := localConfig(t)
	base := runDaemon(t, cfg, Options{Pages: webui.Pages{
		// Its own pages, under a path of its own, carry a header of its
		// own and are never stored; any other page is as ever.
		Document: func(r *http.Request) webui.Document {
			if !strings.HasPrefix(r.URL.Path, "/screens/") {
				return webui.Document{}
			}
			return webui.Document{Header: http.Header{"Speculation-Rules": {`"/rules.json"`}}, Whole: true}
		},
		Files: map[string]webui.File{"/rules.json": {ContentType: "application/speculationrules+json", Body: []byte(`{}`)}},
	}})

	a := request(t, http.MethodGet, base+"/screens/one?x=1", "", "")
	if a.status != http.StatusOK || a.body != consolePage || a.header.Get("Speculation-Rules") != `"/rules.json"` ||
		a.header.Get("Cache-Control") != "no-store" || a.header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("/screens/one: %d %q %v", a.status, a.body, a.header)
	}
	a = request(t, http.MethodGet, base+"/", "", "")
	if a.status != http.StatusOK || a.header.Get("Speculation-Rules") != "" || a.header.Get("Cache-Control") != "no-cache" {
		t.Errorf("/: %d %v", a.status, a.header)
	}
	a = request(t, http.MethodGet, base+"/rules.json", "", "")
	if a.status != http.StatusOK || a.body != `{}` || a.header.Get("Content-Type") != "application/speculationrules+json" ||
		a.header.Get("Cache-Control") != "no-store" {
		t.Errorf("/rules.json: %d %q %v", a.status, a.body, a.header)
	}
	// The core's routes are the core's: the API answers as ever.
	if a := request(t, http.MethodGet, base+"/v1/healthz", "", ""); a.status != http.StatusOK || a.header.Get("Speculation-Rules") != "" {
		t.Errorf("/v1/healthz: %d %v", a.status, a.header)
	}
}

func TestADaemonAskedForAPageFileTheConsoleWouldNotServeDoesNotStart(t *testing.T) {
	cfg := localConfig(t)
	err := Run(t.Context(), cfg, obs.NewLoggerTo(io.Discard, "info", "text"), Options{Pages: webui.Pages{
		Files: map[string]webui.File{"/v1/rules.json": {ContentType: "application/json"}},
	}})
	if !errors.Is(err, webui.ErrPages) {
		t.Fatalf("Run = %v, want webui.ErrPages", err)
	}
	// Refused before anything was opened: no database, no lock.
	if entries, err := os.ReadDir(cfg.DataDir); err != nil || len(entries) != 0 {
		t.Errorf("the data directory holds %v (%v), want nothing", entries, err)
	}
}
