package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/mcpbridge"
)

// `mcp install` configures a client to reach a Mailie server:
//
//   - claude-desktop: a stdio server in claude_desktop_config.json that
//     launches this binary's `mcp connect`, with the key in its environment;
//   - cursor: a remote server in ~/.cursor/mcp.json, the endpoint and an
//     Authorization header, which Cursor sends itself;
//   - claude-code: nothing is written. Claude Code keeps its user-scoped
//     servers in ~/.claude.json, its own state file, which it rewrites while
//     it runs and whose format its documentation does not offer to other
//     programs; and `claude mcp add` takes a header only on its command line,
//     where any process on the machine can read it. So the command is
//     printed, with a placeholder where the key goes, for the person to run.
//
// The key is typed at a terminal without echo, or read as one line from
// standard input, and never taken as an argument. It is checked by opening
// a session with it before anything is written. Every write keeps whatever
// else the file holds, refuses to replace a server of the same name without
// --force, and goes through a temporary file renamed over the original (mode
// 0600, since the file now holds a key). An install refuses a symbolic link
// anywhere below the home directory on the way to the file, and keeps the
// file as it was before mailserver first changed it in <file>.bak-mailie,
// once: left empty when there was no file, so that no later write ever
// copies a key mailserver wrote. --uninstall removes the named server and
// nothing else, and keeps no copy.

// The clients mcp install knows.
const (
	clientClaudeDesktop = "claude-desktop"
	clientCursor        = "cursor"
	clientClaudeCode    = "claude-code"
)

// backupSuffix names the copy of a client's file as it was before
// mailserver first changed it.
const backupSuffix = ".bak-mailie"

// keyPlaceholder stands for the key in a command printed for the person to
// run.
const keyPlaceholder = "<your key>"

var (
	// mcpHome is where the clients keep their configuration. A variable so a
	// test points it at a temporary home: a test never writes a person's own.
	mcpHome = func() (clientHome, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return clientHome{}, err
		}
		return clientHome{
			home: home, appData: os.Getenv("APPDATA"), xdgConfig: os.Getenv("XDG_CONFIG_HOME"), goos: runtime.GOOS,
		}, nil
	}
	// mcpExecutable is this binary, which Claude Desktop will launch.
	mcpExecutable = os.Executable
	// mcpLookPath finds Claude Code's claude command.
	mcpLookPath = exec.LookPath
)

// clientHome is what decides where a client keeps its configuration.
type clientHome struct {
	home, appData, xdgConfig, goos string
}

// configPath is the file client reads its MCP servers from.
func (h clientHome) configPath(client string) (string, error) {
	switch client {
	case clientClaudeDesktop:
		switch h.goos {
		case "darwin":
			return filepath.Join(h.home, "Library", "Application Support", "Claude", "claude_desktop_config.json"), nil
		case "windows":
			if h.appData == "" {
				return "", errors.New("mcp install: APPDATA is not set, and Claude Desktop keeps its configuration there")
			}
			return filepath.Join(h.appData, "Claude", "claude_desktop_config.json"), nil
		default:
			dir := h.xdgConfig
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(h.home, ".config")
			}
			return filepath.Join(dir, "Claude", "claude_desktop_config.json"), nil
		}
	case clientCursor:
		return filepath.Join(h.home, ".cursor", "mcp.json"), nil
	}
	return "", fmt.Errorf("mcp install: %s keeps no configuration mailserver writes", client)
}

// linkRoot is the directory below which an install refuses a symbolic link
// on the way to path: the home directory when path is in it, otherwise the
// directory the client's location comes from (APPDATA, XDG_CONFIG_HOME), and
// failing those the file's own directory.
func (h clientHome) linkRoot(path string) string {
	for _, root := range []string{h.home, h.appData, h.xdgConfig} {
		if !filepath.IsAbs(root) {
			continue
		}
		if rel, err := filepath.Rel(root, path); err == nil && filepath.IsLocal(rel) {
			return root
		}
	}
	return filepath.Dir(path)
}

// refuseLinks refuses a symbolic link anywhere from below root down to path,
// path included: ~/.cursor, ~/.config or the file itself linked into a
// repository of dotfiles would take the key there, where it has no
// business. Only below root: a link above it (/home on some systems, /var
// on macOS) is the system's, not the person's. What does not exist yet,
// mailserver creates as plain directories.
func refuseLinks(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("mcp install: %s is not below %s; nothing was written", path, root)
	}
	at := root
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		at = filepath.Join(at, part)
		fi, err := os.Lstat(at)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil
		case err != nil:
			return fmt.Errorf("mcp install: %w", err)
		case fi.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("mcp install: %s is a symbolic link, and mailserver writes a key only into a file "+
				"of its own, never through a link (into a repository of dotfiles, for one); nothing was written", at)
		case at == path:
			// The file itself: loadClientConfig says what else is wrong with it.
		case fi.Mode().Type() != fs.ModeDir:
			// A junction on Windows, for one, which leads elsewhere as a link does.
			return fmt.Errorf("mcp install: %s is not a plain directory; nothing was written", at)
		}
	}
	return nil
}

// serverName is a name every client accepts for a server.
var serverName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func mcpInstall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp install", flag.ContinueOnError)
	fs.SetOutput(mcpStderr)
	client := fs.String("client", "", "the client to configure: claude-desktop, cursor or claude-code")
	rawURL := fs.String("url", "", "the Mailie server's address, such as https://mail.example.com "+
		"(plain http only for this machine)")
	name := fs.String("name", "mailie", "the server's name in the client's configuration")
	force := fs.Bool("force", false, "replace a server of the same name")
	uninstall := fs.Bool("uninstall", false, "remove the server of that name instead, and nothing else")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		// Not quoted back: a stray argument here may well be the key.
		return errors.New("mcp install: unexpected argument; the key is never an argument, " +
			"it is typed at a terminal or read from standard input")
	}
	if !serverName.MatchString(*name) {
		return errors.New("mcp install: --name takes 1 to 64 letters, digits, - and _")
	}
	switch *client {
	case clientClaudeCode:
		return installClaudeCode(ctx, *rawURL, *name, *uninstall)
	case clientClaudeDesktop, clientCursor:
	case "":
		return errors.New("mcp install: --client is required: claude-desktop, cursor or claude-code")
	default:
		return fmt.Errorf("mcp install: unknown --client %q: claude-desktop, cursor or claude-code", *client)
	}
	home, err := mcpHome()
	if err != nil {
		return fmt.Errorf("mcp install: finding your home directory: %w", err)
	}
	path, err := home.configPath(*client)
	if err != nil {
		return err
	}
	if *uninstall {
		return uninstallServer(path, *name)
	}

	if *rawURL == "" {
		return errors.New("mcp install: --url is required: the Mailie server's address")
	}
	endpoint, err := mcpbridge.Endpoint(*rawURL)
	if err != nil {
		return fmt.Errorf("mcp install: --url: %w", err)
	}
	var command string
	if *client == clientClaudeDesktop {
		if command, err = launchable(); err != nil {
			return err
		}
	}
	// What the file holds first: a file that cannot take the server is
	// refused before a key is typed for nothing.
	linkRoot := home.linkRoot(path)
	if err := refuseLinks(linkRoot, path); err != nil {
		return err
	}
	cfg, err := loadClientConfig(path)
	if err != nil {
		return err
	}
	if cfg.has(*name) && !*force {
		return fmt.Errorf("mcp install: %s already has a server named %q; nothing was written: "+
			"--force replaces it, --name picks another name", path, *name)
	}

	key, err := readInstallKey(ctx, endpoint)
	if err != nil {
		return err
	}
	if err := mcpbridge.Check(ctx, mcpbridge.Options{Endpoint: endpoint, Key: key, UserAgent: userAgent("install")}); err != nil {
		return fmt.Errorf("mcp install: the key does not open a session at %s: %w%s; nothing was written",
			endpoint, err, keyHint(err))
	}

	var entry []byte
	if *client == clientClaudeDesktop {
		entry, err = json.Marshal(stdioServer{
			Command: command, Args: []string{"mcp", "connect", "--url", mcpbridge.Base(endpoint)},
			Env: map[string]string{keyVariable: key},
		})
	} else {
		entry, err = json.Marshal(httpServer{URL: endpoint, Headers: map[string]string{"Authorization": "Bearer " + key}})
	}
	if err != nil {
		return err
	}
	// Read again: the file, or the way to it, may have changed while the key
	// was typed.
	if err := refuseLinks(linkRoot, path); err != nil {
		return err
	}
	if cfg, err = loadClientConfig(path); err != nil {
		return err
	}
	replaced := cfg.has(*name)
	if replaced && !*force {
		return fmt.Errorf("mcp install: %s gained a server named %q meanwhile; nothing was written", path, *name)
	}
	cfg.servers.set(*name, entry)
	if err := cfg.keepOriginal(); err != nil {
		return err
	}
	if err := cfg.save(); err != nil {
		return err
	}

	verb := "Added the MCP server %q to %s.\n"
	if replaced {
		verb = "Replaced the MCP server %q in %s.\n"
	}
	app := "Claude Desktop"
	if *client == clientCursor {
		app = "Cursor"
	}
	say(mcpStdout, verb, *name, path)
	say(mcpStdout, "The file holds the key now, so only you can read it (mode 0600).\n")
	sayBackup(path)
	if *client == clientClaudeDesktop {
		say(mcpStdout, "Claude Desktop will run %s mcp connect --url %s; if this binary moves, run mcp install --force again.\n",
			command, mcpbridge.Base(endpoint))
	}
	say(mcpStdout, "Restart %s to load it. To take the key back, revoke it in the console: the workspace's API keys, "+
		"or your keys under My account.\n", app)
	return nil
}

// stdioServer is a server a client launches: Claude Desktop's entry.
type stdioServer struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// httpServer is a server a client reaches over HTTP with a header: Cursor's
// entry.
type httpServer struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// launchable is the path of this binary, for Claude Desktop to launch: never
// a temporary one, which would be gone by the time it does.
func launchable() (string, error) {
	exe, err := mcpExecutable()
	if err != nil {
		return "", fmt.Errorf("mcp install: finding this binary: %w", err)
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return "", fmt.Errorf("mcp install: finding this binary: %w", err)
	}
	if strings.Contains(exe, string(filepath.Separator)+"go-build") {
		return "", errors.New("mcp install: this binary runs from a temporary build directory (go run), " +
			"which will be gone when Claude Desktop launches it; build it (make build) and run mcp install from the build")
	}
	return exe, nil
}

// readInstallKey reads the key: typed at a terminal without echo, or one
// line of standard input when that is not a terminal.
func readInstallKey(ctx context.Context, endpoint string) (string, error) {
	var key string
	if stdinIsTerminal() {
		say(mcpStderr, "API key for %s (not shown): ", endpoint)
		line, err := readHidden(ctx)
		say(mcpStderr, "\n") // the Return the terminal did not echo
		if err != nil {
			return "", fmt.Errorf("mcp install: reading the key: %w", err)
		}
		key = string(line)
	} else {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("mcp install: reading the key from standard input: %w", err)
		}
		key = line
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("mcp install: no key was given; nothing was written")
	}
	if _, _, ok := auth.SplitKey(key); !ok {
		// Not quoted back: it may be a key all the same, mistyped.
		return "", errors.New("mcp install: that is not an API key (one looks like 0123abcd.<secret>); nothing was written")
	}
	return key, nil
}

// installClaudeCode prints the command that configures Claude Code, which
// mailserver does not run nor write for (see the top of this file), after
// checking that the address answers MCP.
func installClaudeCode(ctx context.Context, rawURL, name string, uninstall bool) error {
	_, lookErr := mcpLookPath("claude")
	missing := ""
	if lookErr != nil {
		missing = "(Claude Code's claude command is not on this PATH: run it where Claude Code is installed.)\n"
	}
	if uninstall {
		say(mcpStdout, "Claude Code keeps its servers in its own state file, which mailserver does not edit. Remove this one with:\n\n"+
			"  claude mcp remove --scope user %s\n\n%s", name, missing)
		return nil
	}
	if rawURL == "" {
		return errors.New("mcp install: --url is required: the Mailie server's address")
	}
	endpoint, err := mcpbridge.Endpoint(rawURL)
	if err != nil {
		return fmt.Errorf("mcp install: --url: %w", err)
	}
	if err := mcpbridge.Reachable(ctx, endpoint, nil); err != nil {
		return fmt.Errorf("mcp install: %s: %w", endpoint, err)
	}
	say(mcpStdout, "Claude Code speaks MCP over HTTP with a header itself, and needs no bridge. mailserver writes nothing\n"+
		"for it: Claude Code keeps its servers in ~/.claude.json, its own state file, which it rewrites while it runs\n"+
		"and whose format is not documented for other programs to edit; and claude mcp add takes the key only on its\n"+
		"command line, where other programs on this machine can read it. Run this yourself, with your key in place\n"+
		"of %s (and clear the line from your shell's history afterwards):\n\n"+
		"  claude mcp add --transport http --scope user %s %s --header \"Authorization: Bearer %s\"\n\n%s",
		keyPlaceholder, name, endpoint, keyPlaceholder, missing)
	return nil
}

// uninstallServer removes the server name from the file at path, and
// nothing else. It keeps no copy: the file it changes holds the key it
// removes. It writes no key either, so a linked directory on the way to the
// file is no reason to refuse; a linked file still is, since the rename
// would replace the link.
func uninstallServer(path, name string) error {
	cfg, err := loadClientConfig(path)
	if err != nil {
		return err
	}
	if !cfg.has(name) {
		say(mcpStdout, "%s has no server named %q; nothing was changed.\n", path, name)
		return nil
	}
	cfg.servers.remove(name)
	if err := cfg.save(); err != nil {
		return err
	}
	say(mcpStdout, "Removed the MCP server %q from %s.\n", name, path)
	sayBackup(path)
	say(mcpStdout, "The key it held works until it is revoked, in the console: the workspace's API keys, "+
		"or your keys under My account.\n")
	return nil
}

// sayBackup says what mailserver keeps of the file at path as it was before
// mailserver first changed it, when it keeps anything.
func sayBackup(path string) {
	backup := path + backupSuffix
	fi, err := os.Lstat(backup)
	switch {
	case err != nil || !fi.Mode().IsRegular():
	case fi.Size() == 0:
		say(mcpStdout, "Before mailserver first wrote %s there was no such file, or an empty one: %s, empty, records that.\n",
			path, backup)
	default:
		say(mcpStdout, "The file as it was before mailserver first changed it is in %s.\n", backup)
	}
}

// say writes to an output whose failure there is nothing to do about.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...) //nolint:errcheck // a terminal that is gone has nobody to tell
}

// clientConfig is a client's configuration file, read to be changed.
type clientConfig struct {
	path string
	// previous is the file's content as read: nil when there was no file.
	previous []byte
	doc      jsonObject
	servers  jsonObject
}

// loadClientConfig reads path. A file that is missing or empty holds
// nothing yet; one that is not a JSON object, or whose mcpServers is not, is
// refused rather than replaced. A symbolic link is refused too: it often
// leads into a repository of dotfiles, where a key has no business, and the
// rename that writes the file would replace the link.
func loadClientConfig(path string) (*clientConfig, error) {
	cfg := &clientConfig{path: path}
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return cfg, nil
	case err != nil:
		return nil, fmt.Errorf("mcp install: %w", err)
	case fi.Mode()&fs.ModeSymlink != 0:
		return nil, fmt.Errorf("mcp install: %s is a symbolic link, and mailserver writes a key only into a file "+
			"of its own; nothing was written", path)
	case !fi.Mode().IsRegular():
		return nil, fmt.Errorf("mcp install: %s is not a regular file; nothing was written", path)
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: the client's own configuration file
	if err != nil {
		return nil, fmt.Errorf("mcp install: %w", err)
	}
	cfg.previous = data
	if len(bytes.TrimSpace(data)) == 0 {
		return cfg, nil
	}
	if cfg.doc, err = parseObject(data); err != nil {
		return nil, fmt.Errorf("mcp install: %s is not a JSON object (%w); fix it or move it away; nothing was written", path, err)
	}
	if raw, ok := cfg.doc.get("mcpServers"); ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if cfg.servers, err = parseObject(raw); err != nil {
			return nil, fmt.Errorf("mcp install: the mcpServers of %s is not a JSON object (%w); nothing was written", path, err)
		}
	}
	return cfg, nil
}

func (c *clientConfig) has(name string) bool {
	_, ok := c.servers.get(name)
	return ok
}

// keepOriginal keeps the file as it was before mailserver first changed it in
// path + backupSuffix, once: a copy already there, the first, is never
// replaced. When there was no file, the copy is left empty, which records
// that: a later write finds it there and copies nothing, so the copy never
// holds a key mailserver wrote. Only an install keeps one.
func (c *clientConfig) keepOriginal() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return fmt.Errorf("mcp install: %w", err)
	}
	// previous is nil when there was no file.
	return writeOnce(c.path+backupSuffix, c.previous)
}

// save writes the configuration back with its servers.
func (c *clientConfig) save() error {
	servers, err := c.servers.marshal()
	if err != nil {
		return err
	}
	c.doc.set("mcpServers", servers)
	data, err := c.doc.marshal()
	if err != nil {
		return err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, data, "", "  "); err != nil {
		return err
	}
	out.WriteByte('\n')
	return writeAtomically(c.path, out.Bytes())
}

// writeAtomically replaces the file at path with data, mode 0600, through a
// temporary file renamed over it, so that a crash leaves the old file or the
// new one and never half of either.
func writeAtomically(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mcp install: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".mailie-*")
	if err != nil {
		return fmt.Errorf("mcp install: %w", err)
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(name) //nolint:errcheck // the temporary file of a write that failed
		}
	}()
	if err = tmp.Chmod(0o600); err == nil {
		if _, err = tmp.Write(data); err == nil {
			err = tmp.Sync()
		}
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("mcp install: writing %s: %w; nothing was changed", path, err)
	}
	if err = os.Rename(name, path); err != nil {
		return fmt.Errorf("mcp install: replacing %s: %w; nothing was changed", path, err)
	}
	syncDir(dir)
	return nil
}

// writeOnce writes data to a new file at path, mode 0600, unless something
// is there already (a link included, which O_EXCL does not follow): the
// first backup is the one worth keeping.
func writeOnce(path string, data []byte) error {
	//nolint:gosec // G304: the backup beside the client's own configuration file
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("mcp install: keeping a backup: %w; nothing was changed", err)
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path) //nolint:errcheck // a backup that could not be written whole is no backup
		return fmt.Errorf("mcp install: keeping a backup: %w; nothing was changed", err)
	}
	return nil
}

// syncDir makes a rename in dir durable where the system allows it.
func syncDir(dir string) {
	d, err := os.Open(dir) //nolint:gosec // G304: the directory just written to
	if err != nil {
		return
	}
	_ = d.Sync()  //nolint:errcheck // not every system syncs a directory; the rename stands either way
	_ = d.Close() //nolint:errcheck // read-only handle
}

// jsonObject is a JSON object whose members keep their order and their
// values as written, so that a file mailserver changes one member of reads
// as it did, apart from that member and its layout.
type jsonObject []jsonMember

type jsonMember struct {
	key   string
	value json.RawMessage
}

// parseObject reads one JSON object, and nothing after it.
func parseObject(data []byte) (jsonObject, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if tok != json.Delim('{') {
		return nil, errors.New("not an object")
	}
	obj := jsonObject{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errors.New("a member without a name")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		obj = append(obj, jsonMember{key: key, value: value})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("something follows the object")
	}
	return obj, nil
}

func (o jsonObject) get(key string) (json.RawMessage, bool) {
	for _, m := range o {
		if m.key == key {
			return m.value, true
		}
	}
	return nil, false
}

// set replaces key's value where it is, or adds it at the end.
func (o *jsonObject) set(key string, value json.RawMessage) {
	for i, m := range *o {
		if m.key == key {
			(*o)[i].value = value
			return
		}
	}
	*o = append(*o, jsonMember{key: key, value: value})
}

// remove removes every member named key.
func (o *jsonObject) remove(key string) {
	kept := (*o)[:0]
	for _, m := range *o {
		if m.key != key {
			kept = append(kept, m)
		}
	}
	*o = kept
}

func (o jsonObject) marshal() ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		if err := enc.Encode(m.key); err != nil {
			return nil, err
		}
		b.Truncate(b.Len() - 1) // the encoder's newline
		b.WriteByte(':')
		b.Write(m.value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
