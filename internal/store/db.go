// Package store owns the Postgres schema and the webhook job queue.
package store

import (
	"context"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// DB is the connection pool plus the queries of this package.
type DB struct {
	pool *pgxpool.Pool
}

// Open connects to dsn and, when autoMigrate is set, applies the embedded
// migrations before returning.
//
// embeddingWidth is the width of the vectors memory holds, from the embedding
// provider in the configuration. Zero means memory is off, and then the column
// is left as it is.
func Open(ctx context.Context, dsn string, autoMigrate bool, embeddingWidth int) (*DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("database.dsn is required")
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}

	db := &DB{pool: pool}
	if autoMigrate {
		if err := db.Migrate(ctx); err != nil {
			pool.Close()
			return nil, err
		}
	}
	if embeddingWidth > 0 {
		if err := db.EnsureEmbeddingWidth(ctx, embeddingWidth); err != nil {
			pool.Close()
			return nil, err
		}
	}
	return db, nil
}

// Migrate applies the embedded migrations. It is safe to run on every start:
// goose records the applied versions in its own table.
func (d *DB) Migrate(ctx context.Context) error {
	// goose logs to stdout by default, which would interleave with the JSON
	// logs the rest of the process writes to stderr.
	goose.SetLogger(goose.NopLogger())
	goose.SetBaseFS(migrationFS)

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("selecting the goose dialect: %w", err)
	}

	sqlDB := stdlib.OpenDBFromPool(d.pool)
	defer func() { _ = sqlDB.Close() }()

	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		return fmt.Errorf("applying migrations: %w", err)
	}
	return nil
}

// Ping reports whether the database answers, which is what /healthz uses.
func (d *DB) Ping(ctx context.Context) error {
	return d.pool.Ping(ctx)
}

// Close releases the pool.
func (d *DB) Close() {
	d.pool.Close()
}
