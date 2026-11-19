package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/service/issuertest"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

// A key an issuer makes, through the REST decorator: the header exactly as
// written, which is why these drive ServeHTTP and never a connection (the
// server trims the spaces and tabs that end a header's value).

func TestARefusedHeaderCarryingAnIssuedKeyAnswersTheOneUnauthorizedBody(t *testing.T) {
	db := storetest.New(t)
	users := auth.NewUsers(db)
	h := &Handler{Service: service.New(service.Deps{Keys: auth.NewKeys(db), Users: users, Store: db,
		KeyIssuer: issuertest.New()})}
	ana := authtest.NewUser(t, db, "ana@example.com", auth.RoleMember)
	token, _ := issuertest.Store(t, db, issuertest.Key{WorkspaceID: authtest.Personal(t, db, ana.ID), CreatorID: ana.ID})
	own := authtest.NewKey(t, db, auth.ScopeRead)
	handler := h.authenticated(auth.ScopeRead, opts(), func(q *request) {
		q.finish(http.StatusOK, map[string]string{"key": q.principal.KeyPrefix})
	})
	call := func(values ...string) (int, wireError) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/accounts", nil)
		if values != nil {
			req.Header["Authorization"] = values
		}
		handler.ServeHTTP(rec, req)
		var e wireError
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		return rec.Code, e
	}
	const invalid, required = "invalid or expired api key", "a bearer api key or session token is required"
	for what, c := range map[string]struct {
		values []string
		want   string
	}{
		"a tab for the space":      {[]string{"Bearer\t" + token}, invalid},
		"two spaces":               {[]string{"Bearer  " + token}, invalid},
		"a trailing space":         {[]string{"Bearer " + token + " "}, invalid},
		"a trailing tab":           {[]string{"Bearer " + token + "\t"}, invalid},
		"a leading space":          {[]string{" Bearer " + token}, invalid},
		"a second header":          {[]string{"Bearer " + token, "Bearer " + token}, invalid},
		"Basic for Bearer":         {[]string{"Basic " + token}, invalid},
		"the token with no scheme": {[]string{token}, invalid},
		"two tokens in one value":  {[]string{"Bearer " + token + ", Bearer " + token}, invalid},
		"no header":                {nil, required},
		"Bearer alone":             {[]string{"Bearer"}, required},
		"Basic and a password":     {[]string{"Basic YWxhZGRpbjpvcGVuc2VzYW1l"}, required},
		"an own key after a tab":   {[]string{"Bearer\t" + own}, required},
		"two own keys":             {[]string{"Bearer " + own, "Bearer " + own}, required},
	} {
		code, e := call(c.values...)
		if code != http.StatusUnauthorized || e.Code != service.CodeUnauthorized || e.Message != c.want {
			t.Errorf("%s: %d %+v, want 401 %q", what, code, e, c.want)
		}
	}
	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "bEaReR"} {
		if code, e := call(scheme + " " + token); code != http.StatusOK {
			t.Errorf("%s and the token: %d %+v", scheme, code, e)
		}
	}
}
