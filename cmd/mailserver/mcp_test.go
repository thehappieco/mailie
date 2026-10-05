package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thehappieco/mailie/internal/mcpbridge"
)

// The mcp commands run against a real daemon on a loopback port, and write
// client configurations only under a temporary home: never a person's own.

// mcpDaemon runs a daemon, serving MCP over HTTP when on is set, and returns
// its address, a read key for it, and its log.
func mcpDaemon(t *testing.T, on bool) (string, string, *lockedBuffer) {
	t.Helper()
	cfg := localConfig(t)
	cfg.MCPHTTP = on
	key := readKey(t, cfg)
	base, logs := runDaemon(t, cfg)
	return base, key, logs
}

// mcpOutput captures what the mcp commands write.
func mcpOutput(t *testing.T) (stdout, stderr *lockedBuffer) {
	t.Helper()
	prevOut, prevErr := mcpStdout, mcpStderr
	stdout, stderr = &lockedBuffer{}, &lockedBuffer{}
	mcpStdout, mcpStderr = stdout, stderr
	t.Cleanup(func() { mcpStdout, mcpStderr = prevOut, prevErr })
	return stdout, stderr
}

// launchedBinary is where the tests say this binary is.
const launchedBinary = "/opt/mailie/bin/mailserver"

// tempHome points mcp install at an empty home of this system's kind, and
// at a binary that is not a temporary build.
func tempHome(t *testing.T) clientHome {
	t.Helper()
	h := clientHome{home: t.TempDir(), goos: runtime.GOOS}
	if h.goos == "windows" {
		h.appData = filepath.Join(h.home, "AppData", "Roaming")
	}
	prevHome, prevExe := mcpHome, mcpExecutable
	mcpHome = func() (clientHome, error) { return h, nil }
	mcpExecutable = func() (string, error) { return launchedBinary, nil }
	t.Cleanup(func() { mcpHome, mcpExecutable = prevHome, prevExe })
	return h
}

func configPathOf(t *testing.T, h clientHome, client string) string {
	t.Helper()
	path, err := h.configPath(client)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// writeFile writes a client's configuration as the client would have.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// decoded is a JSON file as plain values: what it means, not how it is laid
// out.
func decoded(t *testing.T, content string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(content), &v); err != nil {
		t.Fatalf("%v in %s", err, content)
	}
	return v
}

// serverIn is the server name of the configuration at path, and the rest of
// the file without it.
func serverIn(t *testing.T, path, name string) (entry map[string]any, rest map[string]any) {
	t.Helper()
	rest = decoded(t, readFile(t, path))
	servers, _ := rest["mcpServers"].(map[string]any)
	entry, _ = servers[name].(map[string]any)
	delete(servers, name)
	return entry, rest
}

func modeOf(t *testing.T, path string) fs.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func missing(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// installClients are the clients mcp install writes a file for.
var installClients = []string{clientClaudeDesktop, clientCursor}

// wantEntry checks the server mcp install wrote for client.
func wantEntry(t *testing.T, client string, entry map[string]any, base, key string) {
	t.Helper()
	var want map[string]any
	switch client {
	case clientClaudeDesktop:
		want = map[string]any{
			"command": launchedBinary,
			"args":    []any{"mcp", "connect", "--url", base},
			"env":     map[string]any{keyVariable: key},
		}
	case clientCursor:
		want = map[string]any{
			"url":     base + "/mcp",
			"headers": map[string]any{"Authorization": "Bearer " + key},
		}
	}
	if !reflect.DeepEqual(entry, want) {
		t.Errorf("%s's server is %v; want %v", client, entry, want)
	}
}

func TestInstallWritesAFreshConfigurationOnlyItsOwnerCanRead(t *testing.T) {
	base, key, logs := mcpDaemon(t, true)
	for _, client := range installClients {
		t.Run(client, func(t *testing.T) {
			home := tempHome(t)
			stdout, stderr := mcpOutput(t)
			pipeStdin(t, key+"\n")
			if err := mcpInstall(t.Context(), []string{"--client", client, "--url", base}); err != nil {
				t.Fatal(err)
			}
			path := configPathOf(t, home, client)
			entry, _ := serverIn(t, path, "mailie")
			wantEntry(t, client, entry, base, key)
			if mode := modeOf(t, path); runtime.GOOS != "windows" && mode != 0o600 {
				t.Errorf("the file holding the key has mode %v", mode)
			}
			// There was no file: the backup is empty, and says so.
			if got := readFile(t, path+backupSuffix); got != "" {
				t.Errorf("the backup of a file that did not exist holds %q", got)
			}
			if !strings.Contains(stdout.String(), path) || !strings.Contains(stdout.String(), "there was no such file") {
				t.Errorf("the report does not say which file changed, and that there was none before:\n%s", stdout)
			}
			for what, s := range map[string]string{"standard output": stdout.String(), "standard error": stderr.String()} {
				if strings.Contains(s, secretOf(key)) {
					t.Errorf("%s shows the key:\n%s", what, s)
				}
			}
		})
	}
	if strings.Contains(logs.String(), secretOf(key)) {
		t.Errorf("the daemon logged the key:\n%s", logs)
	}
}

// existing is a client's configuration with servers and settings of its
// own, in an order and a layout its own.
const existing = `{
  "globalShortcut": "Alt+Space",
  "mcpServers": {
    "files": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "/home/ana/notes"], "env": {"DEBUG": "1"}},
    "weather":    { "url": "https://weather.example/mcp", "headers": { "X-Api-Key": "w-123" } }
  },
  "preferences": {"theme": "dark", "nested": {"list": [1, 2.50, "três <&>", null, true, {"deep": []}]}}
}
`

func TestInstallKeepsEveryOtherServerAndSetting(t *testing.T) {
	base, key, _ := mcpDaemon(t, true)
	for _, client := range installClients {
		t.Run(client, func(t *testing.T) {
			home := tempHome(t)
			mcpOutput(t)
			path := configPathOf(t, home, client)
			writeFile(t, path, existing)
			pipeStdin(t, key+"\n")
			if err := mcpInstall(t.Context(), []string{"--client", client, "--url", base}); err != nil {
				t.Fatal(err)
			}
			entry, rest := serverIn(t, path, "mailie")
			wantEntry(t, client, entry, base, key)
			if want := decoded(t, existing); !reflect.DeepEqual(rest, want) {
				t.Errorf("the rest of the file changed:\n%v\nwas:\n%v", rest, want)
			}
			// In the order it was written in, too.
			after := readFile(t, path)
			if g, m, p := strings.Index(after, `"globalShortcut"`), strings.Index(after, `"mcpServers"`),
				strings.Index(after, `"preferences"`); g >= m || m >= p {
				t.Errorf("the file's members moved:\n%s", after)
			}
			if f, w, ours := strings.Index(after, `"files"`), strings.Index(after, `"weather"`),
				strings.Index(after, `"mailie"`); f >= w || w >= ours {
				t.Errorf("the servers moved:\n%s", after)
			}
			if got := readFile(t, path+backupSuffix); got != existing {
				t.Errorf("the backup is not the file as it was:\n%s", got)
			}
			if runtime.GOOS != "windows" {
				for _, p := range []string{path, path + backupSuffix} {
					if mode := modeOf(t, p); mode != 0o600 {
						t.Errorf("%s has mode %v", p, mode)
					}
				}
			}
			// No temporary file is left beside it.
			entries, err := os.ReadDir(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 2 {
				t.Errorf("the directory holds %v; want the file and its backup", entries)
			}
		})
	}
}

func TestInstallRefusesANameThatIsTakenUnlessForced(t *testing.T) {
	base, key, _ := mcpDaemon(t, true)
	for _, client := range installClients {
		t.Run(client, func(t *testing.T) {
			home := tempHome(t)
			mcpOutput(t)
			path := configPathOf(t, home, client)
			taken := strings.Replace(existing, `"files":`, `"mailie": {"command": "/somewhere/else"}, "files":`, 1)
			writeFile(t, path, taken)

			typed := pipeStdin(t, key+"\n")
			err := mcpInstall(t.Context(), []string{"--client", client, "--url", base})
			if err == nil || !strings.Contains(err.Error(), "already has a server named") || !strings.Contains(err.Error(), "nothing was written") {
				t.Fatalf("installing over a server of the same name: %v", err)
			}
			if readFile(t, path) != taken || !missing(path+backupSuffix) {
				t.Error("a refused install changed the file")
			}
			if typed.Len() != len(key)+1 {
				t.Error("the key was asked for although the name was taken")
			}

			pipeStdin(t, key+"\n")
			if err := mcpInstall(t.Context(), []string{"--client", client, "--url", base, "--force"}); err != nil {
				t.Fatal(err)
			}
			entry, rest := serverIn(t, path, "mailie")
			wantEntry(t, client, entry, base, key)
			if want := decoded(t, existing); !reflect.DeepEqual(rest, want) {
				t.Errorf("--force changed more than the server it replaced:\n%v", rest)
			}
			if readFile(t, path+backupSuffix) != taken {
				t.Error("the backup is not the file as it was before --force")
			}
		})
	}
}

func TestUninstallRemovesOnlyThatServer(t *testing.T) {
	base, key, _ := mcpDaemon(t, true)
	for _, client := range installClients {
		t.Run(client, func(t *testing.T) {
			home := tempHome(t)
			stdout, _ := mcpOutput(t)
			path := configPathOf(t, home, client)
			writeFile(t, path, existing)
			pipeStdin(t, key+"\n")
			if err := mcpInstall(t.Context(), []string{"--client", client, "--url", base}); err != nil {
				t.Fatal(err)
			}
			if err := mcpInstall(t.Context(), []string{"--client", client, "--uninstall"}); err != nil {
				t.Fatal(err)
			}
			if got, want := decoded(t, readFile(t, path)), decoded(t, existing); !reflect.DeepEqual(got, want) {
				t.Errorf("after uninstall the file is\n%v\nwas\n%v", got, want)
			}
			if strings.Contains(readFile(t, path), secretOf(key)) {
				t.Error("the key is still in the file")
			}
			if readFile(t, path+backupSuffix) != existing {
				t.Error("the backup is not the file as it was before mailserver first changed it")
			}
			if !strings.Contains(stdout.String(), "Removed the MCP server") || strings.Contains(stdout.String(), secretOf(key)) {
				t.Errorf("uninstall said:\n%s", stdout)
			}

			// Once more: there is nothing of ours left, and nothing changes.
			before := readFile(t, path)
			if err := mcpInstall(t.Context(), []string{"--client", client, "--uninstall"}); err != nil {
				t.Fatal(err)
			}
			if readFile(t, path) != before || !strings.Contains(stdout.String(), "nothing was changed") {
				t.Errorf("uninstalling what is not there changed the file, or said:\n%s", stdout)
			}
		})
	}
}

// holdingKey is every file under dir whose content holds the key's secret.
func holdingKey(t *testing.T, dir, key string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.Contains(readFile(t, path), secretOf(key)) {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestAfterAFreshInstallNoCopyEverHoldsTheKey(t *testing.T) {
	base, key, _ := mcpDaemon(t, true)
	for _, client := range installClients {
		t.Run(client, func(t *testing.T) {
			home := tempHome(t)
			stdout, _ := mcpOutput(t)
			path := configPathOf(t, home, client)
			pipeStdin(t, key+"\n")
			if err := mcpInstall(t.Context(), []string{"--client", client, "--url", base}); err != nil {
				t.Fatal(err)
			}

			// --force rewrites the file that holds the key: the backup still
			// records that there was no file, and copies nothing.
			pipeStdin(t, key+"\n")
			if err := mcpInstall(t.Context(), []string{"--client", client, "--url", base, "--force"}); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, path+backupSuffix); got != "" {
				t.Errorf("after --force the backup holds:\n%s", got)
			}

			said := len(stdout.String())
			if err := mcpInstall(t.Context(), []string{"--client", client, "--uninstall"}); err != nil {
				t.Fatal(err)
			}
			if found := holdingKey(t, home.home, key); len(found) != 0 {
				t.Errorf("after uninstall the key is still in %v", found)
			}
			if got := readFile(t, path+backupSuffix); got != "" {
				t.Errorf("after uninstall the backup holds:\n%s", got)
			}
			if s := stdout.String()[said:]; strings.Contains(s, "as it was before mailserver first changed it is in") ||
				!strings.Contains(s, "there was no such file") {
				t.Errorf("uninstall points at a backup as the file it replaced:\n%s", s)
			}
		})
	}
}

func TestUninstallKeepsNoCopyOfTheFileItChanges(t *testing.T) {
	for _, client := range installClients {
		t.Run(client, func(t *testing.T) {
			home := tempHome(t)
			stdout, _ := mcpOutput(t)
			path := configPathOf(t, home, client)
			// A server named mailie that mailserver did not write: its file was
			// never changed by mailserver, so there is no backup to begin with.
			taken := strings.Replace(existing, `"files":`,
				`"mailie": {"url": "https://mail.example.com/mcp", "headers": {"Authorization": "Bearer 0123abcd.byHand"}}, "files":`, 1)
			writeFile(t, path, taken)
			if err := mcpInstall(t.Context(), []string{"--client", client, "--uninstall"}); err != nil {
				t.Fatal(err)
			}
			if got, want := decoded(t, readFile(t, path)), decoded(t, existing); !reflect.DeepEqual(got, want) {
				t.Errorf("after uninstall the file is\n%v\nwant\n%v", got, want)
			}
			if !missing(path + backupSuffix) {
				t.Errorf("uninstall kept a copy, which holds the key it removed:\n%s", readFile(t, path+backupSuffix))
			}
			if strings.Contains(stdout.String(), backupSuffix) {
				t.Errorf("uninstall names a backup there is none of:\n%s", stdout)
			}
		})
	}
}

func TestAKeyThatDoesNotOpenASessionWritesNothing(t *testing.T) {
	base, key, _ := mcpDaemon(t, true)
	offBase, offKey, _ := mcpDaemon(t, false)
	prefix, _, _ := strings.Cut(key, ".")
	for _, client := range installClients {
		t.Run(client, func(t *testing.T) {
			for _, c := range []struct {
				name, url, key string
				want           error
				says           string
			}{
				{"a wrong key", base, prefix + ".notTheSecretOfThisKeyAtAllxxxxxxxxxxxxxxxxx", mcpbridge.ErrKeyRefused, "401"},
				{"not a key", base, "hello", nil, "not an API key"},
				{"no key", base, "", nil, "no key was given"},
				{"MCP off", offBase, offKey, mcpbridge.ErrNoMCP, "MAIL_MCP_HTTP=false"},
			} {
				home := tempHome(t)
				stdout, stderr := mcpOutput(t)
				path := configPathOf(t, home, client)
				writeFile(t, path, existing)
				pipeStdin(t, c.key+"\n")
				err := mcpInstall(t.Context(), []string{"--client", client, "--url", c.url})
				if err == nil || !strings.Contains(err.Error(), "nothing was written") || !strings.Contains(err.Error(), c.says) ||
					(c.want != nil && !errors.Is(err, c.want)) {
					t.Errorf("%s: %v", c.name, err)
				}
				if readFile(t, path) != existing || !missing(path+backupSuffix) {
					t.Errorf("%s: the file changed", c.name)
				}
				for _, s := range []string{err.Error(), stdout.String(), stderr.String()} {
					if secret := secretOf(c.key); secret != "" && strings.Contains(s, secret) {
						t.Errorf("%s: the key was repeated: %s", c.name, s)
					}
				}
			}
		})
	}
}

func TestAKeyTypedAtATerminalIsNeitherEchoedNorPrinted(t *testing.T) {
	base, key, _ := mcpDaemon(t, true)
	home := tempHome(t)
	stdout, stderr := mcpOutput(t)
	terminalStdin(t, "", key)
	if err := mcpInstall(t.Context(), []string{"--client", clientClaudeDesktop, "--url", base}); err != nil {
		t.Fatal(err)
	}
	entry, _ := serverIn(t, configPathOf(t, home, clientClaudeDesktop), "mailie")
	wantEntry(t, clientClaudeDesktop, entry, base, key)
	if !strings.Contains(stderr.String(), "API key for "+base+"/mcp (not shown): ") {
		t.Errorf("the prompt was %q", stderr)
	}
	if strings.Contains(stdout.String()+stderr.String(), secretOf(key)) {
		t.Errorf("the key was printed:\n%s\n%s", stdout, stderr)
	}
}

func TestInstallForClaudeCodeWritesNothingAndPrintsTheCommandWithAPlaceholder(t *testing.T) {
	base, key, _ := mcpDaemon(t, true)
	home := tempHome(t)
	stdout, _ := mcpOutput(t)
	typed := pipeStdin(t, key+"\n")
	prevLook := mcpLookPath
	mcpLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { mcpLookPath = prevLook })

	if err := mcpInstall(t.Context(), []string{"--client", clientClaudeCode, "--url", base}); err != nil {
		t.Fatal(err)
	}
	want := `claude mcp add --transport http --scope user mailie ` + base +
		`/mcp --header "Authorization: Bearer <your key>"`
	if !strings.Contains(stdout.String(), want) || !strings.Contains(stdout.String(), "not on this PATH") {
		t.Errorf("printed:\n%s\nwant the command %s", stdout, want)
	}
	if typed.Len() != len(key)+1 {
		t.Error("a key was read for a client nothing is written for")
	}
	if err := mcpInstall(t.Context(), []string{"--client", clientClaudeCode, "--uninstall", "--name", "work-mail"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "claude mcp remove --scope user work-mail") {
		t.Errorf("uninstall printed:\n%s", stdout)
	}
	entries, err := os.ReadDir(home.home)
	if err != nil || len(entries) != 0 {
		t.Errorf("the home holds %v (%v); claude-code writes nothing", entries, err)
	}

	// A wrong address is caught before the person runs anything.
	offBase, _, _ := mcpDaemon(t, false)
	err = mcpInstall(t.Context(), []string{"--client", clientClaudeCode, "--url", offBase})
	if !errors.Is(err, mcpbridge.ErrNoMCP) {
		t.Errorf("an address with no MCP: %v", err)
	}
}

func TestInstallRefusesWhatItCannotWriteSafely(t *testing.T) {
	base, key, _ := mcpDaemon(t, true)

	t.Run("a binary go run built", func(t *testing.T) {
		tempHome(t)
		mcpOutput(t)
		mcpExecutable = func() (string, error) {
			return filepath.Join(os.TempDir(), "go-build1234", "b001", "exe", "mailserver"), nil
		}
		typed := pipeStdin(t, key+"\n")
		err := mcpInstall(t.Context(), []string{"--client", clientClaudeDesktop, "--url", base})
		if err == nil || !strings.Contains(err.Error(), "temporary build directory") || typed.Len() != len(key)+1 {
			t.Errorf("installing a temporary binary: %v", err)
		}
	})
	t.Run("a symbolic link", func(t *testing.T) {
		home := tempHome(t)
		mcpOutput(t)
		path := configPathOf(t, home, clientCursor)
		target := filepath.Join(home.home, "dotfiles", "mcp.json")
		writeFile(t, target, existing)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Skip("no symbolic links here:", err)
		}
		pipeStdin(t, key+"\n")
		err := mcpInstall(t.Context(), []string{"--client", clientCursor, "--url", base})
		if err == nil || !strings.Contains(err.Error(), "symbolic link") || readFile(t, target) != existing {
			t.Errorf("installing through a symbolic link: %v", err)
		}
	})
	for name, content := range map[string]string{
		"not JSON":                       "{ this is not json",
		"not an object":                  `["a", "list"]`,
		"servers that are not an object": `{"mcpServers": ["x"]}`,
		"something after the object":     `{"mcpServers": {}} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			home := tempHome(t)
			mcpOutput(t)
			path := configPathOf(t, home, clientCursor)
			writeFile(t, path, content)
			pipeStdin(t, key+"\n")
			err := mcpInstall(t.Context(), []string{"--client", clientCursor, "--url", base})
			if err == nil || !strings.Contains(err.Error(), "nothing was written") || readFile(t, path) != content {
				t.Errorf("installing into %s: %v", name, err)
			}
		})
	}
	t.Run("an empty file", func(t *testing.T) {
		home := tempHome(t)
		mcpOutput(t)
		path := configPathOf(t, home, clientCursor)
		writeFile(t, path, "\n")
		pipeStdin(t, key+"\n")
		if err := mcpInstall(t.Context(), []string{"--client", clientCursor, "--url", base}); err != nil {
			t.Fatal(err)
		}
		entry, _ := serverIn(t, path, "mailie")
		wantEntry(t, clientCursor, entry, base, key)
	})
	t.Run("plain http to another machine", func(t *testing.T) {
		tempHome(t)
		mcpOutput(t)
		err := mcpInstall(t.Context(), []string{"--client", clientCursor, "--url", "http://mail.example.com"})
		if !errors.Is(err, mcpbridge.ErrInsecureURL) {
			t.Errorf("plain http: %v", err)
		}
	})
	t.Run("a name no client takes", func(t *testing.T) {
		tempHome(t)
		mcpOutput(t)
		err := mcpInstall(t.Context(), []string{"--client", clientCursor, "--url", base, "--name", "my mail"})
		if err == nil || !strings.Contains(err.Error(), "--name") {
			t.Errorf("a name with a space: %v", err)
		}
	})
	t.Run("the key as an argument", func(t *testing.T) {
		tempHome(t)
		mcpOutput(t)
		err := mcpInstall(t.Context(), []string{"--client", clientCursor, "--url", base, key})
		if err == nil || strings.Contains(err.Error(), secretOf(key)) {
			t.Errorf("a key given as an argument: %v", err)
		}
	})
}

// linkDir replaces dir with a symbolic link to a directory of the same name
// in a repository of dotfiles under home, as GNU Stow folds a tree, and
// returns where the link points.
func linkDir(t *testing.T, home, dir string) string {
	t.Helper()
	rel, err := filepath.Rel(home, dir)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "dotfiles", "pkg", rel)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dir); err != nil {
		t.Skip("no symbolic links here:", err)
	}
	return target
}

func TestInstallRefusesALinkedDirectoryOnTheWayToTheFile(t *testing.T) {
	base, key, _ := mcpDaemon(t, true)
	for _, client := range installClients {
		for _, where := range []string{"the file's directory", "the first directory below the home"} {
			for _, withFile := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s, %s, file there %v", client, where, withFile), func(t *testing.T) {
					home := tempHome(t)
					mcpOutput(t)
					path := configPathOf(t, home, client)
					dir := filepath.Dir(path)
					if where != "the file's directory" {
						rel, err := filepath.Rel(home.home, path)
						if err != nil {
							t.Fatal(err)
						}
						first, _, _ := strings.Cut(rel, string(filepath.Separator))
						dir = filepath.Join(home.home, first)
					}
					target := linkDir(t, home.home, dir)
					if withFile {
						writeFile(t, path, existing) // through the link: into the dotfiles
					}
					typed := pipeStdin(t, key+"\n")
					err := mcpInstall(t.Context(), []string{"--client", client, "--url", base})
					if err == nil || !strings.Contains(err.Error(), "symbolic link") || !strings.Contains(err.Error(), "nothing was written") {
						t.Errorf("installing through a linked directory: %v", err)
					}
					if typed.Len() != len(key)+1 {
						t.Error("the key was asked for although the way to the file is a link")
					}
					if found := holdingKey(t, target, key); len(found) != 0 {
						t.Errorf("the key was written into the dotfiles: %v", found)
					}
					if withFile && (readFile(t, path) != existing || !missing(path+backupSuffix)) {
						t.Error("the file behind the link changed")
					}
				})
			}
		}
	}
}

func TestALinkAboveTheHomeIsNoReasonToRefuse(t *testing.T) {
	base, key, _ := mcpDaemon(t, true)
	h := tempHome(t)
	// The home itself is a link, as /home is on some systems and /var, where
	// temporary homes live, is on macOS.
	realHome := filepath.Join(h.home, "real")
	if err := os.Mkdir(realHome, 0o700); err != nil {
		t.Fatal(err)
	}
	h.home = filepath.Join(h.home, "linked")
	if err := os.Symlink(realHome, h.home); err != nil {
		t.Skip("no symbolic links here:", err)
	}
	if h.goos == "windows" {
		h.appData = filepath.Join(h.home, "AppData", "Roaming")
	}
	mcpHome = func() (clientHome, error) { return h, nil }
	mcpOutput(t)
	pipeStdin(t, key+"\n")
	if err := mcpInstall(t.Context(), []string{"--client", clientCursor, "--url", base}); err != nil {
		t.Fatalf("installing into a home that is a link: %v", err)
	}
	entry, _ := serverIn(t, filepath.Join(realHome, ".cursor", "mcp.json"), "mailie")
	wantEntry(t, clientCursor, entry, base, key)
}

func TestUninstallGoesThroughALinkedDirectoryButNotALinkedFile(t *testing.T) {
	home := tempHome(t)
	mcpOutput(t)
	path := configPathOf(t, home, clientCursor)
	target := linkDir(t, home.home, filepath.Dir(path))
	taken := strings.Replace(existing, `"files":`, `"mailie": {"url": "https://mail.example.com/mcp"}, "files":`, 1)
	writeFile(t, path, taken)
	// Uninstalling writes no key, and takes one out of the dotfiles.
	if err := mcpInstall(t.Context(), []string{"--client", clientCursor, "--uninstall"}); err != nil {
		t.Fatal(err)
	}
	if got, want := decoded(t, readFile(t, filepath.Join(target, "mcp.json"))), decoded(t, existing); !reflect.DeepEqual(got, want) {
		t.Errorf("after uninstall the file is\n%v\nwant\n%v", got, want)
	}
	if !missing(path + backupSuffix) {
		t.Error("uninstall kept a copy")
	}

	// A linked file stays refused: the rename would replace the link.
	linked := filepath.Join(home.home, "dotfiles", "mcp.json")
	writeFile(t, linked, taken)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linked, path); err != nil {
		t.Fatal(err)
	}
	err := mcpInstall(t.Context(), []string{"--client", clientCursor, "--uninstall"})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") || readFile(t, linked) != taken {
		t.Errorf("uninstalling through a linked file: %v", err)
	}
}

func TestEachClientsConfigurationIsWhereTheClientReadsIt(t *testing.T) {
	for _, c := range []struct {
		home   clientHome
		client string
		want   string
	}{
		{clientHome{home: "/Volumes/home/ana", goos: "darwin"}, clientClaudeDesktop,
			"/Volumes/home/ana/Library/Application Support/Claude/claude_desktop_config.json"},
		{clientHome{home: `C:\Users\ana`, appData: `C:\Users\ana\AppData\Roaming`, goos: "windows"}, clientClaudeDesktop,
			filepath.Join(`C:\Users\ana\AppData\Roaming`, "Claude", "claude_desktop_config.json")},
		{clientHome{home: "/home/ana", goos: "linux"}, clientClaudeDesktop, "/home/ana/.config/Claude/claude_desktop_config.json"},
		{clientHome{home: "/home/ana", xdgConfig: "/srv/ana/config", goos: "linux"}, clientClaudeDesktop,
			"/srv/ana/config/Claude/claude_desktop_config.json"},
		{clientHome{home: "/home/ana", xdgConfig: "relative", goos: "linux"}, clientClaudeDesktop,
			"/home/ana/.config/Claude/claude_desktop_config.json"},
		{clientHome{home: "/Volumes/home/ana", goos: "darwin"}, clientCursor, "/Volumes/home/ana/.cursor/mcp.json"},
	} {
		got, err := c.home.configPath(c.client)
		if err != nil || got != filepath.FromSlash(c.want) && got != c.want {
			t.Errorf("%s on %s: %q, %v; want %q", c.client, c.home.goos, got, err, c.want)
		}
	}
	if _, err := (clientHome{home: `C:\Users\ana`, goos: "windows"}).configPath(clientClaudeDesktop); err == nil {
		t.Error("Claude Desktop's configuration on Windows without APPDATA")
	}
}

func TestConnectNeedsTheKeyFromTheEnvironmentAndNeverFromAnArgument(t *testing.T) {
	mcpOutput(t)
	t.Setenv(keyVariable, "")
	err := mcpConnect(t.Context(), []string{"--url", "https://mail.example.com"})
	if err == nil || !strings.Contains(err.Error(), keyVariable+" is not set") {
		t.Errorf("without a key: %v", err)
	}
	const key = "0123abcd.someSecretThatMustNotBeRepeatedxxxxxxxxxxx"
	err = mcpConnect(t.Context(), []string{"--url", "https://mail.example.com", key})
	if err == nil || strings.Contains(err.Error(), "someSecret") {
		t.Errorf("a key as an argument: %v", err)
	}
	t.Setenv(keyVariable, "aConsoleSessionTokenHasNoDot")
	err = mcpConnect(t.Context(), []string{"--url", "https://mail.example.com"})
	if err == nil || !strings.Contains(err.Error(), "does not hold an API key") || strings.Contains(err.Error(), "aConsoleSession") {
		t.Errorf("a session token: %v", err)
	}
	t.Setenv(keyVariable, key)
	if err := mcpConnect(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "--url is required") {
		t.Errorf("without --url: %v", err)
	}
	if err := mcpConnect(t.Context(), []string{"--url", "http://mail.example.com"}); !errors.Is(err, mcpbridge.ErrInsecureURL) {
		t.Errorf("plain http to another machine: %v", err)
	}
}

// connectClient runs mcp connect with a client on its standard input and
// output, and returns the client's session, what connect ends with, and why
// the client could not connect.
func connectClient(t *testing.T, base string) (*sdk.ClientSession, <-chan error, error) {
	t.Helper()
	client, local := sdk.NewInMemoryTransports()
	prev := mcpLocal
	mcpLocal = func() sdk.Transport { return local }
	t.Cleanup(func() { mcpLocal = prev })
	done := make(chan error, 1)
	go func() { done <- mcpConnect(t.Context(), []string{"--url", base}) }()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil).Connect(t.Context(), client, nil)
	return cs, done, err
}

func TestConnectRelaysTheDaemonsToolsUntilTheClientCloses(t *testing.T) {
	base, key, logs := mcpDaemon(t, true)
	t.Setenv(keyVariable, key)
	stdout, stderr := mcpOutput(t)
	cs, done, err := connectClient(t, base)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: "list_accounts", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("list_accounts through mcp connect: %v %v", err, res)
	}
	if err := cs.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Errorf("mcp connect ended with %v when its client closed", err)
	}
	if stdout.String() != "" {
		t.Errorf("mcp connect wrote to standard output, which is the client's:\n%s", stdout)
	}
	if !strings.Contains(stderr.String(), "relaying MCP") {
		t.Errorf("mcp connect logged:\n%s", stderr)
	}
	for what, s := range map[string]string{"mcp connect's log": stderr.String(), "the daemon's log": logs.String()} {
		if strings.Contains(s, secretOf(key)) {
			t.Errorf("%s holds the key:\n%s", what, s)
		}
	}
}

func TestConnectToADaemonWithMCPOffEndsSayingSo(t *testing.T) {
	base, key, _ := mcpDaemon(t, false)
	t.Setenv(keyVariable, key)
	mcpOutput(t)
	_, done, err := connectClient(t, base)
	if err == nil {
		t.Error("a client connected to a daemon with MCP over HTTP off")
	}
	err = <-done
	if !errors.Is(err, mcpbridge.ErrNoMCP) || !strings.Contains(err.Error(), "MAIL_MCP_HTTP=false") {
		t.Errorf("mcp connect ended with %v", err)
	}
}

func TestConnectWithAWrongKeyEndsSayingSoWithoutRepeatingIt(t *testing.T) {
	base, key, _ := mcpDaemon(t, true)
	prefix, _, _ := strings.Cut(key, ".")
	wrong := prefix + ".notTheSecretOfThisKeyAtAllxxxxxxxxxxxxxxxxx"
	t.Setenv(keyVariable, wrong)
	_, stderr := mcpOutput(t)
	_, done, _ := connectClient(t, base)
	err := <-done
	if !errors.Is(err, mcpbridge.ErrKeyRefused) || !strings.Contains(err.Error(), "create a key") {
		t.Errorf("mcp connect ended with %v", err)
	}
	if strings.Contains(err.Error()+stderr.String(), secretOf(wrong)) {
		t.Errorf("the key was repeated: %v\n%s", err, stderr)
	}
}

// secretOf is the part of a key that authenticates it.
func secretOf(key string) string {
	_, secret, _ := strings.Cut(key, ".")
	return secret
}
