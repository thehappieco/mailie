package main

import (
	"context"
	"flag"
	"log/slog"

	"github.com/thehappieco/mailie/internal/app"
	"github.com/thehappieco/mailie/internal/config"
)

// serve runs the daemon, which internal/app assembles.
func serve(ctx context.Context, cfg config.Config, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	mcpStdio := fs.Bool("mcp-stdio", false, "also speak MCP over stdin and stdout, for a client that launches this binary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return app.Run(ctx, cfg, logger, serveOptions(version, *mcpStdio))
}

// serveOptions are the self-hosted edition's: what `serve` runs the daemon with.
func serveOptions(version string, mcpStdio bool) app.Options {
	return app.Options{
		Version:  version,
		MCPStdio: mcpStdio,
		// The self-hosted edition offers pausing one mailbox's sync: its
		// operator pauses and resumes an operator mailbox from the command
		// line (`account sync ID pause|resume`), and no console shows a sync
		// text a pause could break (platform decision 0063).
		SyncPauseOffered: true,
	}
}
