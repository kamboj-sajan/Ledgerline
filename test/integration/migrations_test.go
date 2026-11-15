//go:build integration

package integration

import (
	"io/fs"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kamboj-sajan/ledgerline/internal/store"
	"github.com/kamboj-sajan/ledgerline/migrations"
)

// TestMigrationsUpDownUp applies every migration, rolls every one back, checks
// that nothing is left behind, and applies them all again. This catches Down
// sections that forget a table, function, trigger or type.
func TestMigrationsUpDownUp(t *testing.T) {
	ctx := t.Context()
	dsn := pg.NewEmptyDatabase(t)

	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no embedded migrations found (err=%v)", err)
	}

	m, err := store.NewMigrator(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("closing migrator: %v", err)
		}
	})

	pool, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	up, err := m.Up(ctx)
	if err != nil {
		t.Fatalf("first up: %v", err)
	}
	if len(up) != len(files) {
		t.Fatalf("first up applied %d migrations, want %d", len(up), len(files))
	}
	if n := countUserObjects(t, pool); n == 0 {
		t.Fatal("after up: schema is empty")
	}

	if _, err := m.DownTo(ctx, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
	if n := countUserObjects(t, pool); n != 0 {
		t.Fatalf("after full down: %d objects remain in schema public; a Down section is incomplete", n)
	}

	again, err := m.Up(ctx)
	if err != nil {
		t.Fatalf("second up: %v", err)
	}
	if len(again) != len(files) {
		t.Fatalf("second up applied %d migrations, want %d", len(again), len(files))
	}

	statuses, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range statuses {
		if s.AppliedAt.IsZero() {
			t.Errorf("migration %s not applied after second up", s.Source.Path)
		}
	}
}

// countUserObjects counts tables, views, sequences, functions and types in
// schema public, excluding goose's own version table.
func countUserObjects(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	const q = `
		SELECT
		  (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		     WHERE n.nspname = 'public' AND c.relkind IN ('r','v','m','S','p')
		       AND c.relname NOT LIKE 'goose_db_version%')
		+ (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		     WHERE n.nspname = 'public')
		+ (SELECT count(*) FROM pg_type ty JOIN pg_namespace n ON n.oid = ty.typnamespace
		     WHERE n.nspname = 'public' AND ty.typtype IN ('e','d','c')
		       AND NOT EXISTS (SELECT 1 FROM pg_class c WHERE c.reltype = ty.oid))`
	var n int
	if err := pool.QueryRow(t.Context(), q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
