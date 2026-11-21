// SPDX-License-Identifier: FSL-1.1-ALv2

// Package db opens the accounts-api Postgres pool, applies the embedded goose
// migrations and runs transactions.
//
// Migrations (recorded decision in docs/servers-accounts-api-foundation.md,
// Storage): goose SQL migrations embedded from migrations/. They are
// forward-only and never edited after they reach `main`; a change adds a new
// file. Every migration must stay compatible with the code of the previous
// release, because the old container keeps serving while the new one
// migrates.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockID is the Postgres advisory lock key that serializes
// migration runs of several instances. It differs from the goose default so
// that another goose user of the same database cannot block accounts-api.
const migrationLockID int64 = 0x6163636f756e7473 // "accounts"

// DB is the Postgres pool of accounts-api.
type DB struct {
	Pool *pgxpool.Pool
}

// Open creates the connection pool. It does not connect: pgxpool opens
// connections on first use, so a server started with RUN_MIGRATIONS=false
// serves /healthz before the database is reachable, and every query fails
// until it is. The error never contains the URL, which can hold a password.
func Open(ctx context.Context, url string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errors.New("parse database url: invalid connection string")
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
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

// Migrations returns the embedded migration files, rooted at the migrations
// directory.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		// The directory is embedded at build time, so this cannot happen.
		panic(err)
	}
	return sub
}

// Migrate applies every embedded migration that the database has not applied
// yet. A Postgres session advisory lock lets only one instance migrate at a
// time; the others wait and then find nothing to apply.
func (d *DB) Migrate(ctx context.Context, logger *slog.Logger) error {
	return d.MigrateTo(ctx, logger, 0)
}

// MigrateTo applies the embedded migrations up to and including version; 0
// applies all of them. Upgrade tests use it to build a database at an older
// schema version.
func (d *DB) MigrateTo(ctx context.Context, logger *slog.Logger, version int64) error {
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockID(migrationLockID))
	if err != nil {
		return fmt.Errorf("migration lock: %w", err)
	}
	sqlDB := stdlib.OpenDBFromPool(d.Pool)
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, Migrations(),
		goose.WithSessionLocker(locker),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	var results []*goose.MigrationResult
	if version == 0 {
		results, err = provider.Up(ctx)
	} else {
		results, err = provider.UpTo(ctx, version)
	}
	for _, r := range results {
		if r.Error != nil {
			continue
		}
		logger.Info("db_migration_applied",
			slog.Int64("version", r.Source.Version),
			slog.String("name", r.Source.Path),
			slog.Int64("duration_ms", r.Duration.Milliseconds()))
	}
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// IsUniqueViolation reports whether err is a unique constraint violation.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
