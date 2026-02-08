package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/Madhav-000-s/ledgerd/migrations"
)

// Migrate applies every pending migration.
//
// The schema is embedded rather than read from disk, so a container image cannot be
// built without its migrations and the integration suite cannot drift from what
// production runs.
//
// goose speaks database/sql, so the pool is adapted rather than opening a second
// connection with its own configuration — a migration that ran against different
// settings than the application uses would be a subtle way to get a schema the service
// cannot actually work with.
func Migrate(ctx context.Context, db *DB) error {
	goose.SetBaseFS(migrations.FS)
	defer goose.SetBaseFS(nil)

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("postgres: goose dialect: %w", err)
	}
	// goose logs to stdout by default, which would interleave with the service's
	// structured logs.
	goose.SetLogger(goose.NopLogger())

	sqlDB := stdlib.OpenDBFromPool(db.pool)
	defer sqlDB.Close()

	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		return fmt.Errorf("postgres: applying migrations: %w", err)
	}
	return nil
}

// MigrateDown rolls back a single migration. It exists for the integration suite and
// for local development; nothing in a deployment path calls it.
func MigrateDown(ctx context.Context, db *DB) error {
	goose.SetBaseFS(migrations.FS)
	defer goose.SetBaseFS(nil)

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("postgres: goose dialect: %w", err)
	}
	goose.SetLogger(goose.NopLogger())

	sqlDB := stdlib.OpenDBFromPool(db.pool)
	defer sqlDB.Close()

	if err := goose.DownContext(ctx, sqlDB, "."); err != nil {
		return fmt.Errorf("postgres: rolling back migration: %w", err)
	}
	return nil
}

// SchemaVersion reports the highest applied migration, for the readiness probe and for
// confirming a deploy actually migrated.
func SchemaVersion(ctx context.Context, db *DB) (int64, error) {
	goose.SetBaseFS(migrations.FS)
	defer goose.SetBaseFS(nil)

	if err := goose.SetDialect("postgres"); err != nil {
		return 0, fmt.Errorf("postgres: goose dialect: %w", err)
	}
	goose.SetLogger(goose.NopLogger())

	sqlDB := stdlib.OpenDBFromPool(db.pool)
	defer sqlDB.Close()

	v, err := goose.GetDBVersionContext(ctx, sqlDB)
	if err != nil {
		return 0, fmt.Errorf("postgres: reading schema version: %w", err)
	}
	return v, nil
}
