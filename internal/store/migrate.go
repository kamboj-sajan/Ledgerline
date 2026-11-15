package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/kamboj-sajan/ledgerline/migrations"
)

// Migrator applies the embedded migrations in migrations.FS.
//
// goose works on database/sql, so the Migrator opens its own short-lived
// *sql.DB through pgx's stdlib adapter. The application itself uses pgxpool.
type Migrator struct {
	db       *sql.DB
	provider *goose.Provider
}

// NewMigrator connects to dsn. Call Close when done.
//
// It takes a Postgres session-level advisory lock while migrating, so two
// processes that start at the same moment (two containers, say) cannot apply
// the same migration twice.
func NewMigrator(dsn string) (*Migrator, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: opening database: %w", err)
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("store: creating migration lock: %w", err), db.Close())
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS,
		goose.WithSessionLocker(locker),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("store: loading migrations: %w", err), db.Close())
	}
	return &Migrator{db: db, provider: provider}, nil
}

// Up applies every pending migration.
func (m *Migrator) Up(ctx context.Context) ([]*goose.MigrationResult, error) {
	return m.provider.Up(ctx)
}

// Down rolls back the most recently applied migration.
func (m *Migrator) Down(ctx context.Context) (*goose.MigrationResult, error) {
	return m.provider.Down(ctx)
}

// DownTo rolls back migrations until version is the latest applied one.
// DownTo(ctx, 0) rolls back everything.
func (m *Migrator) DownTo(ctx context.Context, version int64) ([]*goose.MigrationResult, error) {
	return m.provider.DownTo(ctx, version)
}

// Status reports every known migration and whether it has been applied.
func (m *Migrator) Status(ctx context.Context) ([]*goose.MigrationStatus, error) {
	return m.provider.Status(ctx)
}

// Close releases the database connection.
func (m *Migrator) Close() error {
	return m.db.Close()
}
