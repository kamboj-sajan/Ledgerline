// Package storetest runs a disposable PostgreSQL in Docker for integration
// tests, using testcontainers.
//
// A test binary starts one container (normally from TestMain). The embedded
// migrations are applied once to a template database, and every test then gets
// its own database cloned from that template with CREATE DATABASE … TEMPLATE.
// Cloning takes milliseconds, so tests stay isolated, even with t.Parallel(),
// without paying for a container or a migration run per test.
package storetest

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/kamboj-sajan/ledgerline/internal/store"
)

// Image is the PostgreSQL image used by every integration test. Keep it in
// step with compose.yaml.
const Image = "postgres:17-alpine"

const templateDB = "ledgerline_template"

// TB is the part of testing.TB that storetest needs. *testing.T and
// *testing.B satisfy it, and so does *rapid.T, which lets a property test get
// a fresh database for every generated case.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	Errorf(format string, args ...any)
	Cleanup(func())
	Context() context.Context
}

// Postgres is a running container plus an admin connection used to create
// and drop per-test databases.
type Postgres struct {
	container *tcpostgres.PostgresContainer
	baseURL   *url.URL      // connection URL; only the database name changes per test
	admin     *pgxpool.Pool // connected to the "postgres" maintenance database
	counter   atomic.Int64

	// Postgres refuses CREATE DATABASE … TEMPLATE while any other session is
	// connected to the template. Clones are serialised so that parallel tests
	// never race on the template; each clone takes milliseconds, so it is cheap.
	cloneMu sync.Mutex
}

// Start launches the container and migrates the template database.
func Start(ctx context.Context) (*Postgres, error) {
	c, err := tcpostgres.Run(ctx, Image,
		tcpostgres.WithDatabase(templateDB),
		tcpostgres.WithUsername("ledgerline"),
		tcpostgres.WithPassword("ledgerline"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		return nil, fmt.Errorf("storetest: starting postgres container: %w", err)
	}
	p := &Postgres{container: c}
	if err := p.init(ctx); err != nil {
		_ = testcontainers.TerminateContainer(c)
		return nil, err
	}
	return p, nil
}

func (p *Postgres) init(ctx context.Context) error {
	raw, err := p.container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return fmt.Errorf("storetest: connection string: %w", err)
	}
	if p.baseURL, err = url.Parse(raw); err != nil {
		return fmt.Errorf("storetest: parsing connection string: %w", err)
	}

	m, err := store.NewMigrator(p.urlFor(templateDB))
	if err != nil {
		return err
	}
	_, upErr := m.Up(ctx)
	// Close before cloning: the template must have no open sessions.
	if err := m.Close(); err != nil {
		return fmt.Errorf("storetest: closing migrator: %w", err)
	}
	if upErr != nil {
		return fmt.Errorf("storetest: migrating template: %w", upErr)
	}

	if p.admin, err = store.Open(ctx, p.urlFor("postgres")); err != nil {
		return err
	}
	return nil
}

// Close drops the admin connection and terminates the container.
func (p *Postgres) Close() error {
	if p.admin != nil {
		p.admin.Close()
	}
	return testcontainers.TerminateContainer(p.container)
}

func (p *Postgres) urlFor(db string) string {
	u := *p.baseURL
	u.Path = "/" + db
	return u.String()
}

// NewDatabase creates a migrated database for the calling test and returns
// its connection URL. The database is dropped when the test finishes.
func (p *Postgres) NewDatabase(t TB) string {
	t.Helper()
	return p.create(t, "TEMPLATE "+templateDB)
}

// NewEmptyDatabase creates a database with no migrations applied, for tests
// of the migrations themselves.
func (p *Postgres) NewEmptyDatabase(t TB) string {
	t.Helper()
	return p.create(t, "")
}

// NewPool is NewDatabase plus a connection pool that is closed when the test
// finishes.
func (p *Postgres) NewPool(t TB) *pgxpool.Pool {
	t.Helper()
	pool, err := store.Open(t.Context(), p.NewDatabase(t))
	if err != nil {
		t.Fatalf("storetest: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func (p *Postgres) create(t TB, clause string) string {
	t.Helper()
	name := fmt.Sprintf("test_%d", p.counter.Add(1))
	ident := pgx.Identifier{name}.Sanitize()

	p.cloneMu.Lock()
	_, err := p.admin.Exec(t.Context(), "CREATE DATABASE "+ident+" "+clause)
	p.cloneMu.Unlock()
	if err != nil {
		t.Fatalf("storetest: creating database %s: %v", name, err)
	}
	t.Cleanup(func() {
		// t.Context() is already cancelled during cleanup, so use a fresh one.
		// FORCE disconnects any pool the test forgot to close.
		if _, err := p.admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
			t.Errorf("storetest: dropping database %s: %v", name, err)
		}
	})
	return p.urlFor(name)
}
