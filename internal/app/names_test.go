package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
)

// namedSignIn is the extension a binary that embeds the daemon would mount to
// sign people in through its provider, here with the name the provider gives
// the person now taken from the request's body ({"name": …}): Cy's identity,
// her account key made at her first sign-in.
func namedSignIn(t *testing.T) Extension {
	enc := base64.RawURLEncoding.EncodeToString
	publicKey, wrap := enc(authtest.PublicKey(t)), enc(authtest.PlatformWrap(t))
	return func(_ context.Context, r *Router, d Deps) error {
		r.HandleFunc("POST /v1/auth/provider", func(w http.ResponseWriter, req *http.Request) {
			ctx := req.Context()
			var body struct{ Name string }
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			in := service.ExternalSignIn{
				Issuer: "https://accounts.example.com", Subject: "subject-of-cy", Email: "cy@example.com",
				EmailVerified: true, Name: body.Name, UserAgent: req.UserAgent(), TTL: 24 * time.Hour,
				WantsKey: true, ProductKeyID: authtest.ProductKeyID,
			}
			if _, _, err := d.Service.PinIdentityKey(ctx, in.Issuer, in.Subject, in.ProductKeyID,
				authtest.ProductKey(in.Subject)); err != nil {
				http.Error(w, service.MessageOf(err), http.StatusForbidden)
				return
			}
			signed, err := d.Service.SignInExternal(ctx, in)
			if err != nil {
				http.Error(w, service.MessageOf(err), http.StatusForbidden)
				return
			}
			if signed.Session == nil {
				session, err := d.Service.EnrolExternal(ctx, service.ExternalEnrolment{
					Ticket: signed.Enrolment.Ticket, PublicKey: publicKey, PlatformWrap: wrap,
					ProductKeyID: in.ProductKeyID, UserAgent: req.UserAgent(),
				})
				if err != nil {
					http.Error(w, service.MessageOf(err), http.StatusForbidden)
					return
				}
				signed.Session = &session
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(signed.Session)
		})
		return nil
	}
}

// signInNamed signs Cy in through the extension under the provider's name,
// and returns her session.
func signInNamed(t *testing.T, base, name string) service.Session {
	t.Helper()
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		t.Fatal(err)
	}
	a := request(t, http.MethodPost, base+"/v1/auth/provider", "", string(body))
	var session service.Session
	if err := json.Unmarshal([]byte(a.body), &session); a.status != http.StatusOK || err != nil || session.Token == "" {
		t.Fatalf("the extension answered %d %q", a.status, a.body)
	}
	return session
}

func TestABinaryThatEmbedsTheDaemonHasNamesFollowTheIdentityThroughItsOptions(t *testing.T) {
	cfg, _, _ := seeded(t)
	base := runDaemon(t, cfg, Options{Extensions: []Extension{namedSignIn(t)}, ExternalSignInOnly: true,
		NameFollowsIdentity: true})

	if s := signInNamed(t, base, "Cy Lima"); s.User.Name != "Cy Lima" {
		t.Fatalf("her first sign-in named her %q", s.User.Name)
	}
	// Renamed at the provider, then left without a name there: each sign-in
	// names her so.
	for _, name := range []string{"Cy Souza", ""} {
		s := signInNamed(t, base, name)
		if a := request(t, http.MethodGet, base+"/v1/auth/me", s.Token, ""); a.status != http.StatusOK ||
			!strings.Contains(a.body, `"name":"`+name+`"`) {
			t.Errorf("GET /v1/auth/me after the provider's %q answered %d %q", name, a.status, a.body)
		}
	}
	// And she does not rename herself here.
	s := signInNamed(t, base, "Cy Lima")
	if a := request(t, http.MethodPut, base+"/v1/auth/profile", s.Token, `{"name":"Cy Q"}`); a.status != http.StatusConflict ||
		!strings.Contains(a.body, `"code":"conflict"`) {
		t.Errorf("PUT /v1/auth/profile answered %d %q, want 409 conflict", a.status, a.body)
	}
	if a := request(t, http.MethodGet, base+"/v1/auth/me", s.Token, ""); !strings.Contains(a.body, `"name":"Cy Lima"`) {
		t.Errorf("GET /v1/auth/me answered %q, want her the provider's Cy Lima", a.body)
	}
}

func TestTheZeroOptionsKeepTheNameAPersonsFirstSignInGave(t *testing.T) {
	// What `serve` runs, with an extension's sign-in: the name the provider
	// later gives reaches nobody, and the person renames themselves.
	cfg, _, _ := seeded(t)
	base := runDaemon(t, cfg, Options{Extensions: []Extension{namedSignIn(t)}})
	signInNamed(t, base, "Cy Lima")
	s := signInNamed(t, base, "Cy Souza")
	if s.User.Name != "Cy Lima" {
		t.Errorf("a later sign-in renamed her %q", s.User.Name)
	}
	if a := request(t, http.MethodPut, base+"/v1/auth/profile", s.Token, `{"name":"Cy Q"}`); a.status != http.StatusOK ||
		!strings.Contains(a.body, `"name":"Cy Q"`) {
		t.Errorf("PUT /v1/auth/profile answered %d %q, want her renamed", a.status, a.body)
	}
}
