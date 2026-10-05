package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
)

func TestExternalSignInOnlyRefusesEveryPasswordRoute(t *testing.T) {
	// The same requests to two daemons: one whose people sign in only
	// through an extension, where every route that signs in with a password,
	// signs up or accepts an invitation, changes a password or creates an
	// invitation answers 403 with the one refusal, whoever asks; and one as
	// `serve` runs it, where each of them does what it does.
	for _, only := range []bool{true, false} {
		h := newHarnessWith(t, nil, serviceOptions{publicURL: "http://localhost:5174", externalSignInOnly: only})
		owner := h.person(t, "owner@example.com", auth.RoleOwner)
		member := h.person(t, "member@example.com", auth.RoleMember)
		key := h.key(t, auth.ScopeAdmin)
		resp := h.do(t, http.MethodPost, "/v1/workspaces", owner, `{"name":"Support"}`)
		var team struct{ ID string }
		decodeInto(t, resp, &team)
		if resp.StatusCode != http.StatusCreated || team.ID == "" {
			t.Fatalf("creating a team: %d %+v", resp.StatusCode, team)
		}
		// Invitations made before, from the command line: one that signs
		// new@ up, and one that joins member@ to the team.
		signUpCode, _, err := h.users.CreateInvite(t.Context(), auth.NewInvite{Email: "new@example.com", Role: auth.RoleMember, CreatedBy: "cli"})
		if err != nil {
			t.Fatal(err)
		}
		joinCode, _, err := h.users.CreateInvite(t.Context(), auth.NewInvite{
			Email: "member@example.com", WorkspaceID: team.ID, WorkspaceRole: "member", CreatedBy: "cli",
		})
		if err != nil {
			t.Fatal(err)
		}

		// Changing the owner's password ends her sessions, so it goes last.
		for _, route := range []struct{ name, method, path, token, body string }{
			{"sign in", http.MethodPost, "/v1/auth/login", "",
				fmt.Sprintf(`{"email":"owner@example.com","password":%q}`, authtest.Password)},
			{"sign up", http.MethodPost, "/v1/auth/signup", "",
				fmt.Sprintf(`{"invite":%q,"email":"new@example.com","name":"New","password":"a long enough password"}`, signUpCode)},
			{"accept a team invitation", http.MethodPost, "/v1/auth/invites/accept", member, fmt.Sprintf(`{"invite":%q}`, joinCode)},
			{"invite to the instance", http.MethodPost, "/v1/users/invites", owner, `{"email":"x@example.com"}`},
			{"invite to the instance with a key", http.MethodPost, "/v1/users/invites", key, `{"email":"y@example.com"}`},
			{"invite into a team", http.MethodPost, "/v1/workspaces/" + team.ID + "/invites", owner, `{"email":"z@example.com"}`},
			{"invite into a team with a key", http.MethodPost, "/v1/workspaces/" + team.ID + "/invites", key, `{"email":"w@example.com"}`},
			{"change a password", http.MethodPost, "/v1/auth/password", owner,
				fmt.Sprintf(`{"current":%q,"next":"a brand new password"}`, authtest.Password)},
		} {
			resp := h.do(t, route.method, route.path, route.token, route.body)
			status := resp.StatusCode
			code, message := "", ""
			if status >= 400 {
				code, message = decodeError(t, resp)
			}
			refused := status == http.StatusForbidden && code == "not_authorized" && strings.Contains(message, "another way")
			switch {
			case only && !refused:
				t.Errorf("passwords off: %s: %s %s answered %d %s %q, want 403 not_authorized", route.name, route.method, route.path, status, code, message)
			case !only && status >= 300:
				t.Errorf("passwords on: %s: %s %s answered %d %s %q", route.name, route.method, route.path, status, code, message)
			}
		}
		if !only {
			continue
		}

		// The person's own account works as anywhere.
		for _, route := range []struct {
			method, path, body string
			want               int
		}{
			{http.MethodGet, "/v1/auth/me", "", http.StatusOK},
			{http.MethodPut, "/v1/auth/profile", `{"name":"Owner"}`, http.StatusOK},
			{http.MethodGet, "/v1/workspaces/" + team.ID + "/invites", "", http.StatusOK},
			{http.MethodPost, "/v1/auth/logout", "", http.StatusNoContent},
		} {
			if resp := h.do(t, route.method, route.path, owner, route.body); resp.StatusCode != route.want {
				t.Errorf("%s %s answered %d, want %d", route.method, route.path, resp.StatusCode, route.want)
			}
		}
	}
}

func TestAPersonsOwnProfileSaysWhetherTheyHaveAPassword(t *testing.T) {
	h := newHarness(t, false)
	authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)
	external, _, _, err := h.users.SignInExternal(t.Context(), auth.ExternalSignIn{
		Issuer: "https://accounts.example.com", Subject: "subject-of-cy", Email: "cy@example.com",
		EmailVerified: true, TTL: auth.SessionTTL,
	})
	if err != nil {
		t.Fatal(err)
	}
	for token, want := range map[string]bool{h.signIn(t, "ana@example.com", authtest.Password).Token: true, external: false} {
		var me struct {
			User struct {
				Email       string `json:"email"`
				HasPassword *bool  `json:"has_password"`
			} `json:"user"`
		}
		resp := h.do(t, http.MethodGet, "/v1/auth/me", token, "")
		decodeInto(t, resp, &me)
		if resp.StatusCode != http.StatusOK || me.User.HasPassword == nil || *me.User.HasPassword != want {
			t.Errorf("%s: %d, has_password %v, want %v", me.User.Email, resp.StatusCode, me.User.HasPassword, want)
		}
	}
}
