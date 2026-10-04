package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

// Migrate applies every migration newer than PRAGMA user_version.
//
// Each migration runs inside the transaction that also bumps user_version, so a
// crash halfway leaves the database at the previous version rather than in a
// shape no code knows how to read. There is no migrations table: user_version
// is a single integer SQLite maintains for exactly this, and a table would be
// one more thing to keep consistent with it.
func (s *Store) Migrate(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	current, err := s.userVersion(ctx)
	if err != nil {
		return err
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := s.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// PendingMigrations reports the migrations that Migrate would apply.
func (s *Store) PendingMigrations(ctx context.Context) ([]string, error) {
	migrations, err := loadMigrations()
	if err != nil {
		return nil, err
	}
	current, err := s.userVersion(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range migrations {
		if m.version > current {
			out = append(out, m.name)
		}
	}
	return out, nil
}

// SchemaVersion is the applied schema version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) { return s.userVersion(ctx) }

func (s *Store) userVersion(ctx context.Context) (int, error) {
	var v int
	if err := s.w.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("store: read user_version: %w", err)
	}
	return v, nil
}

func (s *Store) applyMigration(ctx context.Context, m migration) error {
	err := s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("store: migration %s: %w", m.name, err)
		}
		// PRAGMA does not accept a bound parameter, and the value is an int
		// parsed from a file name that ships inside the binary.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
			return fmt.Errorf("store: migration %s: set user_version: %w", m.name, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}
	out := make([]migration, 0, len(entries))
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		numStr, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			return nil, fmt.Errorf("store: migration %q: want NNNN_name.sql", e.Name())
		}
		version, err := strconv.Atoi(numStr)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("store: migration %q: %q is not a version number", e.Name(), numStr)
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("store: migrations %q and %q share version %d", other, e.Name(), version)
		}
		seen[version] = e.Name()

		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("store: read migration %q: %w", e.Name(), err)
		}
		out = append(out, migration{version: version, name: e.Name(), sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}
