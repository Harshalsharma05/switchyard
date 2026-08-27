package logstore

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Harshalsharma05/switchyard/migrations"
)

// testDSN is where the logstore tests look for a real Postgres. It mirrors the
// integration harness's SWITCHYARD_TEST_REDIS_ADDR: an explicit override, or
// assembled from POSTGRES_PASSWORD plus the same defaults the gateway uses.
// With no password set there is nothing to connect with, so the tests skip.
func testDSN(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("SWITCHYARD_TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	pw := os.Getenv("POSTGRES_PASSWORD")
	if pw == "" {
		t.Skip("set POSTGRES_PASSWORD (and run Postgres) or SWITCHYARD_TEST_POSTGRES_DSN to run logstore tests")
	}
	return DBConfig{
		Host:     envOrDefault("SWITCHYARD_POSTGRES_HOST", "localhost:5432"),
		User:     envOrDefault("SWITCHYARD_POSTGRES_USER", "switchyard"),
		Password: pw,
		Database: envOrDefault("SWITCHYARD_POSTGRES_DB", "switchyard"),
		SSLMode:  envOrDefault("SWITCHYARD_POSTGRES_SSLMODE", "disable"),
	}.DSN()
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// newTestSchema connects to a real Postgres and hands back a connection scoped
// to a fresh, empty schema — skipping rather than failing when no database is
// reachable, the same convention internal/health and internal/resilience use
// for their Redis tests. Each test gets its own schema so migrations always
// run against a blank slate and never see another test's tables.
func newTestSchema(t *testing.T) (*pgx.Conn, context.Context) {
	t.Helper()

	dsn := testDSN(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("no Postgres reachable: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })

	schema := fmt.Sprintf("logstore_test_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("creating test schema: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn.Exec(c, "DROP SCHEMA "+schema+" CASCADE")
	})

	if _, err := conn.Exec(ctx, "SET search_path TO "+schema); err != nil {
		t.Fatalf("setting search_path: %v", err)
	}

	return conn, ctx
}

func TestMigrate_AppliesToEmptyDatabase(t *testing.T) {
	conn, ctx := newTestSchema(t)

	applied, err := Migrate(ctx, conn, migrations.FS)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// Derived from the embedded set rather than hardcoded, so adding a
	// migration does not break this test for the wrong reason.
	want := embeddedMigrations(t)
	if len(applied) != len(want) {
		t.Fatalf("applied = %v, want all of %v", applied, want)
	}
	for i := range want {
		if applied[i] != want[i] {
			t.Fatalf("applied = %v, want %v in order", applied, want)
		}
	}

	var recorded int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&recorded); err != nil {
		t.Fatalf("reading schema_migrations: %v", err)
	}
	if recorded != len(want) {
		t.Fatalf("schema_migrations has %d rows, want %d", recorded, len(want))
	}

	// The requests table is queryable and carries the Phase 1 columns.
	for _, col := range []string{
		"id", "ts", "team_id", "requested_model", "served_model", "provider",
		"status_code", "input_tokens", "output_tokens", "cost_micros",
		"latency_ms", "overhead_ms", "fallback", "cache_hit", "quality_score",
		"trace_id",
	} {
		var exists bool
		if err := conn.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = current_schema()
				  AND table_name = 'requests'
				  AND column_name = $1
			)`, col).Scan(&exists); err != nil {
			t.Fatalf("checking column %q: %v", col, err)
		}
		if !exists {
			t.Errorf("requests table is missing column %q", col)
		}
	}
}

func TestMigrate_IdempotentOnRerun(t *testing.T) {
	conn, ctx := newTestSchema(t)

	if _, err := Migrate(ctx, conn, migrations.FS); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}

	applied, err := Migrate(ctx, conn, migrations.FS)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("second run applied %v, want nothing", applied)
	}

	var count int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("counting schema_migrations: %v", err)
	}
	if want := len(embeddedMigrations(t)); count != want {
		t.Fatalf("schema_migrations has %d rows after two runs, want %d", count, want)
	}
}

// embeddedMigrations lists the shipped migration filenames, in apply order.
func embeddedMigrations(t *testing.T) []string {
	t.Helper()
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("listing embedded migrations: %v", err)
	}
	sort.Strings(names)
	return names
}

func TestMigrate_AppliesOnlyPendingVersions(t *testing.T) {
	conn, ctx := newTestSchema(t)

	if _, err := Migrate(ctx, conn, migrations.FS); err != nil {
		t.Fatalf("baseline Migrate: %v", err)
	}

	// A source with the real migration plus a new one: only the new file runs.
	// A version above everything shipped, so this stays the only pending file
	// however many real migrations exist.
	src := fstest.MapFS{
		"0001_requests.sql": mustReadEmbed(t, "0001_requests.sql"),
		"9999_extra.sql": &fstest.MapFile{
			Data: []byte("ALTER TABLE requests ADD COLUMN scratch integer"),
		},
	}

	applied, err := Migrate(ctx, conn, src)
	if err != nil {
		t.Fatalf("Migrate with new file: %v", err)
	}
	if want := []string{"9999_extra.sql"}; len(applied) != 1 || applied[0] != want[0] {
		t.Fatalf("applied = %v, want %v", applied, want)
	}
}

func TestMigrate_FailedMigrationRollsBack(t *testing.T) {
	conn, ctx := newTestSchema(t)

	src := fstest.MapFS{
		"0001_broken.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE ok (id int); THIS IS NOT SQL;"),
		},
	}

	if _, err := Migrate(ctx, conn, src); err == nil {
		t.Fatal("Migrate succeeded on broken SQL, want error")
	}

	// Neither the partial table nor a bookkeeping row survives.
	var exists bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = current_schema() AND table_name = 'ok'
		)`).Scan(&exists); err != nil {
		t.Fatalf("checking rolled-back table: %v", err)
	}
	if exists {
		t.Error("table 'ok' from a failed migration was left behind")
	}

	var count int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("counting schema_migrations: %v", err)
	}
	if count != 0 {
		t.Errorf("schema_migrations recorded %d rows for a failed migration, want 0", count)
	}
}

func mustReadEmbed(t *testing.T, name string) *fstest.MapFile {
	t.Helper()
	b, err := migrations.FS.ReadFile(name)
	if err != nil {
		t.Fatalf("reading embedded %q: %v", name, err)
	}
	return &fstest.MapFile{Data: b}
}
