package api_test

import (
	"net/http"
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
)

func TestEveryCallerIsToldWhetherThisServerAnswersMCPOverHTTP(t *testing.T) {
	for _, served := range []bool{true, false} {
		h := newHarnessWith(t, nil, serviceOptions{mcpHTTP: served})
		authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)
		session := authtest.SignIn(t, h.users, "ana@example.com")
		for name, token := range map[string]string{"a session": session, "a read key": h.key(t, auth.ScopeRead)} {
			resp := h.do(t, http.MethodGet, "/v1/me/mcp", token, "")
			if resp.StatusCode != http.StatusOK {
				code, msg := decodeError(t, resp)
				t.Fatalf("served=%t, %s: GET /v1/me/mcp answered %d %s %s", served, name, resp.StatusCode, code, msg)
			}
			var body map[string]any
			decodeInto(t, resp, &body)
			if len(body) != 1 || body["http"] != served {
				t.Errorf("served=%t, %s: %v", served, name, body)
			}
		}
		if resp := h.do(t, http.MethodGet, "/v1/me/mcp", "", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("served=%t, nobody signed in: %d", served, resp.StatusCode)
		}
	}
}
