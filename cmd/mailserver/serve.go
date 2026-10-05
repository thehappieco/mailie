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
	return app.Run(ctx, cfg, logger, app.Options{Version: version, MCPStdio: *mcpStdio})
}
