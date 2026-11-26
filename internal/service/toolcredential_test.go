package service_test

import (
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
)

func TestTheRefusalsOfAToolsCredentialSendNobodyToAConsole(t *testing.T) {
	// A self-hosted server may have no console, and where an issuer makes
	// the keys they are made outside this server: a refusal that names a
	// place to create a key is wrong somewhere.
	f := newFixture(t)
	ana := authtest.NewUser(t, f.db, "ana@example.com", auth.RoleMember)
	session := authtest.SignIn(t, f.users, "ana@example.com")
	unagreed := authtest.NewWorkspaceKey(t, f.db, auth.ScopeRead, authtest.Personal(t, f.db, ana.ID), ana.ID)
	if _, err := f.db.Writer().ExecContext(t.Context(), `UPDATE api_keys SET terms_version = '' WHERE prefix = ?`,
		authtest.Prefix(unagreed)); err != nil {
		t.Fatal(err)
	}

	_, err := f.svc.AuthenticateTool(t.Context(), session, nil)
	if service.CodeOf(err) != service.CodeUnauthorized ||
		!strings.Contains(service.MessageOf(err), "the MCP server takes an API key") {
		t.Errorf("a session as a tool's credential: %v", err)
	}
	refusals := []string{service.MessageOf(err)}
	for name, authenticate := range map[string]func() error{
		"as a tool's": func() error { _, err := f.svc.AuthenticateTool(t.Context(), unagreed, nil); return err },
		"over REST":   func() error { _, err := f.svc.Authenticate(t.Context(), unagreed, nil); return err },
	} {
		err := authenticate()
		if service.CodeOf(err) != service.CodeNotAuthorized ||
			!strings.Contains(service.MessageOf(err), "agreed to the key terms") {
			t.Errorf("a key nobody agreed to the terms of, %s: %v", name, err)
		}
		refusals = append(refusals, service.MessageOf(err))
	}
	for _, msg := range refusals {
		if strings.Contains(strings.ToLower(msg), "console") {
			t.Errorf("the refusal names a console: %q", msg)
		}
	}
}
