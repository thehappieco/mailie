package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

// Inside the package: the guarantee is about the decorator every route is
// wrapped in, and the way to test it is a handler that does what a sign-out in
// another tab does while a request is running.

func TestARevokedSessionIsRejectedBeforeTheResponseIsWritten(t *testing.T) {
	db := storetest.New(t)
	keyring, err := secrets.NewKeyring(1, map[uint8][]byte{1: make([]byte, secrets.KeyLen)})
	if err != nil {
		t.Fatal(err)
	}
	registry := account.NewRegistry(t.Context(), account.NewRepository(db, keyring), account.RegistryOptions{})
	t.Cleanup(func() { _ = registry.Close() })
	users := auth.NewUsers(db)
	h := &Handler{Service: service.New(service.Deps{Accounts: registry, Keys: auth.NewKeys(db), Users: users, Store: db})}

	authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	token := authtest.SignIn(t, users, "ana@example.com")

	handler := h.authenticated(auth.ScopeRead, opts(), func(q *request) {
		// The answer was computed while the session was live...
		if err := users.EndAllSessions(q.ctx(), q.principal.UserID); err != nil {
			t.Errorf("EndAllSessions: %v", err)
		}
		// ...and must not be published now that it is not.
		q.finish(http.StatusOK, map[string]string{"subject": "the quarterly numbers"})
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/accounts", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "quarterly") {
		t.Fatalf("the answer was written after the session ended: %s", rec.Body.String())
	}
}

func TestAClientThatLeavesIsNotLoggedAsAFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		leave     bool
		wantError bool
	}{
		{name: "the client left", leave: true, wantError: false},
		{name: "the mail server failed", leave: false, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			ctx, leave := context.WithCancel(obs.WithLogger(t.Context(), obs.NewLoggerTo(&logs, "debug", "text")))
			defer leave()
			h := &Handler{}
			handler := h.public(opts(), func(q *request) {
				if tc.leave {
					// What a browser does when a person opens the next
					// message before this one arrived.
					leave()
				}
				q.fail(service.E(service.CodeInternal, "upstream: the mail server could not be reached", context.Canceled))
			})
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/messages/1", nil))

			if got := strings.Contains(logs.String(), "level=ERROR"); got != tc.wantError {
				t.Fatalf("logged an error: %v, want %v; log:\n%s", got, tc.wantError, logs.String())
			}
		})
	}
}
