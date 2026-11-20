// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationFiles holds the embedded schema migrations. The runner is the
// in-house migration tool that the control plane contract records: it adds no
// dependency.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockKey is the Postgres advisory lock that serializes migration
// runs of concurrently starting instances (a deployment briefly runs the new
// container next to the old one).
const migrationLockKey int64 = 0x6f776e73616e64 // "ownsand"

// Migration is one embedded schema file.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Migrations returns the embedded migrations in version order. A file name is
// "<version>_<name>.sql" with a positive decimal version.
func Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	var out []Migration
	seen := map[int]string{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, rest, ok := strings.Cut(strings.TrimSuffix(name, ".sql"), "_")
		if !ok || rest == "" {
			return nil, fmt.Errorf("migration %q: want <version>_<name>.sql", name)
		}
		version, err := strconv.Atoi(prefix)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("migration %q: invalid version", name)
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", other, name, version)
		}
		seen[version] = name
		body, err := fs.ReadFile(migrationFiles, "migrations/"+name)
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Version: version, Name: rest, SQL: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Migrate applies every embedded migration that the database has not applied
// yet. Each migration runs in its own transaction together with its row in
// schema_migrations, so a failed migration leaves no partial schema. It
// refuses to run when the database has applied a version that this binary
// does not know (a rollback to an older binary after a newer migration).
func Migrate(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	migrations, err := Migrations()
	if err != nil {
		return err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		// Use a fresh context so that the lock is released also after ctx ends.
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrationLockKey)
	}()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version integer PRIMARY KEY,
		name text NOT NULL,
		applied_at timestamptz NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	applied := map[int]bool{}
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		return err
	}
	known := map[int]bool{}
	for _, m := range migrations {
		known[m.Version] = true
	}
	for _, v := range versions {
		applied[int(v)] = true
		if !known[int(v)] {
			return fmt.Errorf("database has migration %d that this binary does not know", v)
		}
	}
	for _, m := range migrations {
		if applied[m.Version] {
			continue
		}
		start := time.Now()
		err := pgx.BeginFunc(ctx, conn.Conn(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, m.SQL); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name, applied_at) VALUES ($1, $2, now())`, m.Version, m.Name)
			return err
		})
		if err != nil {
			return fmt.Errorf("apply migration %d_%s: %w", m.Version, m.Name, err)
		}
		logger.Info("migration_applied", slog.Int("version", m.Version), slog.String("name", m.Name), slog.Duration("duration", time.Since(start)))
	}
	return nil
}
