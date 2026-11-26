// SPDX-License-Identifier: FSL-1.1-ALv2

// Package db opens the Postgres pool, applies the embedded goose migrations
// and runs transactions.
//
// Migration tool (recorded decision in docs/servers-sandrun-api-foundation.md
// Storage): goose SQL migrations embedded from migrations/. Each file is
// named `<version>_<name>.sql` with a five-digit zero-padded version and has
// a `-- +goose Up` section and no down section. goose applies each pending
// file once, in version order, inside its own transaction while it holds a
// Postgres session advisory lock, so two instances that start together
// apply each migration exactly once. goose records the applied versions in
// `goose_db_version`. Migrations are forward-only and never edited after
// they reach `main`; a later change adds a new file.
//
// Numbering: 00001 creates the core tables (organizations, memberships,
// invitations, personal API keys, audit events and the lease). Each later
// layer (OwnSand links, GitHub installations, agent integrations, runner
// profiles, requests, runners, webhook deliveries) adds its own file with
// the next free version, for example 00002_ownsand_links.sql. goose refuses
// a pending version that is lower than an applied one, so a branch whose
// number another change took first renumbers its file before it merges.
// New tables that belong to an organization reference
// organizations(organization_id) with ON DELETE CASCADE, so
// DeleteOrganization removes them with the organization row.
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
// that another goose user of the same database cannot block sandrun-api.
const migrationLockID int64 = 0x746964652d617069 // "sandrun-api"

// migrationTimeZone is the session time zone of the migration connections.
// goose stores the application time in a `timestamp` column without a time
// zone, from now() in the session time zone. UTC keeps those times
// comparable if a later migration reads them.
const migrationTimeZone = "UTC"

// DB is the Postgres pool of the control plane.
type DB struct {
	Pool *pgxpool.Pool
}

// Open connects to Postgres and checks the connection.
func Open(ctx context.Context, url string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// The parser error can contain secrets from the connection string.
		return nil, errors.New("parse database url: invalid connection string")
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

// Migrate applies every embedded migration that the database has not
// applied yet.
func (d *DB) Migrate(ctx context.Context, logger *slog.Logger) error {
	return d.MigrateUpTo(ctx, logger, 0)
}

// MigrateUpTo applies the embedded migrations that the database has not
// applied yet, up to and including maxVersion; 0 means all of them. Upgrade
// tests use it to build a database at an older schema version and then
// apply the later migrations through the normal runner.
func (d *DB) MigrateUpTo(ctx context.Context, logger *slog.Logger, maxVersion int64) error {
	// The migration connections are separate from the pool, so the session
	// time zone does not change the sessions that serve requests.
	connConfig := d.Pool.Config().ConnConfig.Copy()
	connConfig.RuntimeParams["timezone"] = migrationTimeZone
	sqlDB := stdlib.OpenDB(*connConfig)
	defer func() { _ = sqlDB.Close() }()

	locker, err := lock.NewPostgresSessionLocker(lock.WithLockID(migrationLockID))
	if err != nil {
		return fmt.Errorf("migration lock: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, Migrations(),
		goose.WithSessionLocker(locker),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	var results []*goose.MigrationResult
	if maxVersion == 0 {
		results, err = provider.Up(ctx)
	} else {
		results, err = provider.UpTo(ctx, maxVersion)
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
