package app

import (
	"net/http"
	"strings"
	"testing"
)

func TestTheRetiredMCPVariablesAreSaidOnceAtStartAndMCPIsTheAPIs404(t *testing.T) {
	for _, retired := range [][]string{nil, {"MAIL_MCP_HTTP", "MAIL_MCP_KEY"}} {
		cfg := localConfig(t)
		cfg.RetiredMCP = retired
		d := startDaemon(t, cfg, Options{})
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
			for _, path := range []string{"/mcp", "/mcp/sessions/1"} {
				if a := fetch(t, method, d.base+path); a.status != http.StatusNotFound || a.body != apiNotFound ||
					a.header.Get("Content-Type") != "application/json" {
					t.Errorf("%v: %s %s answered %d %q", retired, method, path, a.status, a.body)
				}
			}
		}
		d.stop()
		if err := d.wait(); err != nil {
			t.Fatal(err)
		}
		var said []string
		for line := range strings.Lines(d.logs.String()) {
			if strings.Contains(line, "serves no MCP of its own") {
				said = append(said, line)
			}
		}
		switch {
		case retired == nil && len(said) != 0:
			t.Errorf("no retired variable set, and the start says %q", said)
		case retired != nil && (len(said) != 1 || !strings.Contains(said[0], "level=WARN") ||
			!strings.Contains(said[0], `variables="[MAIL_MCP_HTTP MAIL_MCP_KEY]"`) ||
			!strings.Contains(said[0], "connected")):
			t.Errorf("the start says %q", said)
		}
		if line := logLine(d.logs.String(), "msg=starting"); line == "" || strings.Contains(line, "mcp") {
			t.Errorf("the start line names MCP: %s", line)
		}
	}
}
