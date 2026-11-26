package main

import (
	"context"
	"flag"
	"log/slog"

	"github.com/thehappieco/mailie/internal/app"
	"github.com/thehappieco/mailie/internal/config"
)

// serve runs the daemon, which internal/app assembles. It takes no flag:
// what the daemon does is its configuration's.
func serve(ctx context.Context, cfg config.Config, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return app.Run(ctx, cfg, logger, serveOptions(version))
}

// serveOptions are the self-hosted edition's: what `serve` runs the daemon with.
func serveOptions(version string) app.Options {
	return app.Options{
		Version: version,
		// The self-hosted edition offers pausing one mailbox's sync: its
		// operator pauses and resumes an operator mailbox from the command
		// line (`account sync ID pause|resume`), and no console shows a sync
		// text a pause could break (platform decision 0063).
		SyncPauseOffered: true,
	}
}
