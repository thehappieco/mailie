package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPIsNoCommandAndSaysTheServerServesNoneOfItsOwn(t *testing.T) {
	// Where `mcp connect` and `mcp install` ran, beside an MCP client, there
	// is no daemon's configuration and maybe a .env nobody wrote for it: the
	// answer comes before either is read.
	onlyEnv(t, nil)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("not a variable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	for _, args := range [][]string{
		{"mcp"},
		{"mcp", "connect", "--url", "https://mail.example.com"},
		{"mcp", "install", "--client", "cursor", "--url", "https://mail.example.com"},
	} {
		err := run(args)
		if !errors.Is(err, errNoMCP) || errors.Is(err, errUsage) {
			t.Errorf("%v: %v, want the server's own MCP named gone", args, err)
		}
	}
	if !strings.Contains(errNoMCP.Error(), "serves no MCP of its own") || strings.Contains(errNoMCP.Error(), "://") {
		t.Errorf("the answer is %q", errNoMCP)
	}
}

func TestTheUsageNamesOnlyCommandsThatExist(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "usage")
	if err != nil {
		t.Fatal(err)
	}
	usage(f)
	raw, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	flat := strings.Join(strings.Fields(text), " ")
	for _, gone := range []string{
		"mcp", "MCP", "console", "user invite", "user password", "workspace create", "member role", "access grant",
	} {
		if strings.Contains(flat, gone) {
			t.Errorf("the usage still says %q:\n%s", gone, text)
		}
	}
	for _, says := range []string{
		"mailserver serve ", "account add --email ADDR [--password -]", "user disable --email ADDR",
		"user delete --email ADDR", "access revoke", "member remove", "apikey create --bootstrap",
	} {
		if !strings.Contains(flat, says) {
			t.Errorf("the usage does not say %q:\n%s", says, text)
		}
	}
}
