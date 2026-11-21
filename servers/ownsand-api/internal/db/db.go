// SPDX-License-Identifier: FSL-1.1-ALv2

// Package db opens the Postgres pool, applies the embedded migrations and
// runs transactions.
//
// Migration tool (recorded decision in docs/servers-ownsand-api-foundation.md
// Storage): an embedded, forward-only runner in this package. Each file in
// migrations/ is named `<version>_<name>.sql` with a zero-padded version and
// runs once, in version order, inside its own transaction while the runner
// holds a Postgres advisory lock, so two instances that start together
// apply each migration exactly once. The `schema_migrations` table records
// the applied versions with the SHA-256 of each file, and the runner refuses
// to start when an applied file changed. Migrations are never edited after
// they reach `main`; a later change adds a new file.
package db

import (
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockID is the advisory lock key of the migration runner.
const migrationLockID int64 = 0x6f776e73616e64 // "ownsand"

// DB is the Postgres pool of the control plane.
type DB struct {
	Pool *pgxpool.Pool
}

// Open connects to Postgres and checks the connection.
func Open(ctx context.Context, url string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &DB{Pool: pool}, nil
}

// Close closes the pool.
func (d *DB) Close() { d.Pool.Close() }

// Querier is implemented by the pool and by transactions.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// InTx runs fn in a read-committed transaction and commits it when fn
// returns nil.
func (d *DB) InTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, d.Pool, pgx.TxOptions{}, fn)
}

type migration struct {
	version int
	name    string
	sql     string
	sum     [32]byte
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	seen := map[int]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			return nil, fmt.Errorf("migration %s: name must be <version>_<name>.sql", name)
		}
		version, err := strconv.Atoi(prefix)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("migration %s: invalid version", name)
		}
		if seen[version] {
			return nil, fmt.Errorf("migration %s: duplicate version %d", name, version)
		}
		seen[version] = true
		body, err := fs.ReadFile(migrationFiles, "migrations/"+name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: version, name: name, sql: string(body), sum: sha256.Sum256(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// Migrate applies every embedded migration that the database has not
// applied yet.
func (d *DB) Migrate(ctx context.Context, logger *slog.Logger) error {
	return d.migrate(ctx, logger, 0)
}

// migrate applies the pending embedded migrations with a version up to
// maxVersion, or all of them when maxVersion is 0. Only tests set a limit,
// to check a migration against data that an older schema wrote.
func (d *DB) migrate(ctx context.Context, logger *slog.Logger, maxVersion int) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	if maxVersion > 0 {
		kept := migrations[:0]
		for _, m := range migrations {
			if m.version <= maxVersion {
				kept = append(kept, m)
			}
		}
		migrations = kept
	}
	conn, err := d.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockID)
	}()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version integer PRIMARY KEY,
		name text NOT NULL,
		sha256 bytea NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	applied := map[int][]byte{}
	rows, err := conn.Query(ctx, "SELECT version, sha256 FROM schema_migrations")
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int
		var sum []byte
		if err := rows.Scan(&v, &sum); err != nil {
			rows.Close()
			return err
		}
		applied[v] = sum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, m := range migrations {
		if sum, ok := applied[m.version]; ok {
			if string(sum) != string(m.sum[:]) {
				return fmt.Errorf("migration %s changed after it was applied", m.name)
			}
			continue
		}
		err := pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{}, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name, sha256) VALUES ($1, $2, $3)", m.version, m.name, m.sum[:])
			return err
		})
		if err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		logger.Info("db_migration_applied", slog.Int("version", m.version), slog.String("name", m.name))
	}
	return nil
}

// IsUniqueViolation reports whether err is a unique constraint violation.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
