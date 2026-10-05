package app

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
)

// sweepSpool removes the sections the IMAP adapter spooled and nobody
// closed, and the attachments of a send that never ended: what a crash in the
// middle of a download or a send leaves. It returns how many it removed.
func sweepSpool(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	var errs []error
	for _, e := range entries {
		if !e.Type().IsRegular() || (!strings.HasPrefix(e.Name(), "part-") && !strings.HasPrefix(e.Name(), "send-")) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// sweepFlows removes expired consent attempts.
func sweepFlows(ctx context.Context, accounts *account.Registry, logger *slog.Logger) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := accounts.SweepFlows(ctx); err != nil {
				logger.Debug("sweeping expired authorisation attempts failed", "err", err)
			}
		}
	}
}

// housekeepingInterval is how often the retention sweep runs.
const housekeepingInterval = time.Hour

// housekeep enforces retention: now, and then every interval until ctx ends.
// Today that is the unused invites, deleted within auth.InviteRetention of
// expiring, and the send records and their send.finished notices, deleted
// store.SendRetention after their last change. scrub then takes the deleted
// rows out of the write-ahead log too.
func housekeep(ctx context.Context, users *auth.Users, sweepSends func(context.Context, time.Duration) (int, int, error),
	scrub func(context.Context) error, logger *slog.Logger, every time.Duration,
) {
	sweep := func() {
		// Told when the next sweep is, so nothing outlives its retention
		// by waiting for it.
		deleted := 0
		n, err := users.SweepInvites(ctx, every)
		switch {
		case err != nil && ctx.Err() == nil:
			logger.Warn("sweeping expired invites failed", "err", err)
		case n > 0:
			logger.Info("deleted expired invites", "count", n)
			deleted += n
		}
		records, notices, err := sweepSends(ctx, every)
		switch {
		case err != nil && ctx.Err() == nil:
			logger.Warn("sweeping old send records failed", "err", err)
		case records+notices > 0:
			logger.Info("deleted old send records", "count", records, "notices", notices)
			deleted += records + notices
		}
		if deleted > 0 {
			if err := scrub(ctx); err != nil && ctx.Err() == nil {
				logger.Warn("emptying the write-ahead log after the sweep failed; a later checkpoint will", "err", err)
			}
		}
	}
	sweep()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
