package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/mcpbridge"
	"github.com/thehappieco/mailie/internal/obs"
)

// `mcp connect` and `mcp install` run on a person's machine, beside an MCP
// client, and talk to a Mailie server somewhere else over HTTPS: they need
// none of the daemon's variables, and read no .env.
//
// connect is a local MCP server on standard input and output for a client
// that launches one (Claude Desktop, for one), relaying every message to the
// server's /mcp with the key from MAILIE_API_KEY (internal/mcpbridge). The
// key is never an argument: a command line is visible to every process on
// the machine, and lands in shell histories and process accounting.
//
// install writes a client's configuration so that it reaches the server:
// through connect for Claude Desktop, over HTTP with the key in a header for
// Cursor, and for Claude Code it prints the command to run (mcp_install.go).

// keyVariable is the environment variable connect reads the key from.
const keyVariable = "MAILIE_API_KEY"

var (
	// mcpLocal is connect's side towards the client: standard input and
	// output. A variable so a test can be the client.
	mcpLocal = func() sdk.Transport { return &sdk.StdioTransport{} }
	// mcpStdout and mcpStderr are where mcp writes: install's report, and
	// connect's log and install's questions. Under connect, standard output
	// belongs to the client and carries nothing else.
	mcpStdout io.Writer = os.Stdout
	mcpStderr io.Writer = os.Stderr
)

func mcpCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: mcp needs a subcommand: connect or install", errUsage)
	}
	switch args[0] {
	case "connect":
		return mcpConnect(ctx, args[1:])
	case "install":
		return mcpInstall(ctx, args[1:])
	default:
		return fmt.Errorf("%w: unknown mcp subcommand %q", errUsage, args[0])
	}
}

func mcpConnect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp connect", flag.ContinueOnError)
	fs.SetOutput(mcpStderr)
	rawURL := fs.String("url", "", "the Mailie server's address, such as https://mail.example.com "+
		"(plain http only for this machine)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		// Not quoted back: a stray argument here may well be the key.
		return errors.New("mcp connect: unexpected argument; the key is never an argument, it comes from " + keyVariable)
	}
	if *rawURL == "" {
		return errors.New("mcp connect: --url is required: the Mailie server's address")
	}
	endpoint, err := mcpbridge.Endpoint(*rawURL)
	if err != nil {
		return fmt.Errorf("mcp connect: --url: %w", err)
	}
	key := strings.TrimSpace(os.Getenv(keyVariable))
	if key == "" {
		return errors.New("mcp connect: " + keyVariable + " is not set: the API key comes from that environment " +
			"variable, never from an argument; an owner or an admin of a workspace creates one in the console, under " +
			"the workspace's API keys")
	}
	if _, _, ok := auth.SplitKey(key); !ok {
		return errors.New("mcp connect: " + keyVariable + " does not hold an API key (one looks like " +
			"0123abcd.<secret>); a console session token is not one")
	}
	err = mcpbridge.Run(ctx, mcpLocal(), mcpbridge.Options{
		Endpoint: endpoint, Key: key, Log: obs.NewLoggerTo(mcpStderr, "info", "text"), UserAgent: userAgent("connect"),
	})
	if err != nil {
		return fmt.Errorf("mcp connect: %w%s", err, keyHint(err))
	}
	return nil
}

// keyHint is what to do about a refused key, said after the refusal.
func keyHint(err error) string {
	switch {
	case errors.Is(err, mcpbridge.ErrKeyRefused):
		return "; an owner or an admin of a workspace creates a key in the console, under the workspace's API keys"
	case errors.Is(err, mcpbridge.ErrKeyNotAllowed):
		return "; use a key an owner or an admin of a workspace created in the console, under the workspace's API keys"
	}
	return ""
}

func userAgent(command string) string { return "mailserver/" + version + " (mcp " + command + ")" }
