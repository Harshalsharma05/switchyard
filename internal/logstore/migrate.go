// Package logstore is the Postgres request-log writer and query layer (Part 2,
// Phase 1). This file is the schema half: a small forward-only migration
// runner that applies migrations/*.sql in order and records what it has done.
//
// It is a hand-rolled runner rather than a migration library on purpose — the
// need is "apply a handful of numbered SQL files once, in order, idempotently",
// and that is about forty lines. See DECISIONS.md.
package logstore

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// advisoryLockKey guards the whole migration run so two gateway or migrate
// processes starting at once cannot apply the same file twice. The value is
// arbitrary but must stay stable — it is the identity of "the SwitchYard
// migration lock", chosen once and never changed.
const advisoryLockKey int64 = 0x5359_4152_4400 // "SYARD\0"

// Migrate applies every pending migration from src, in filename order, each in
// its own transaction. It is safe to call on every boot: already-applied
// versions are skipped, so a re-run against an up-to-date database is a no-op.
//
// A migration filename must start with a zero-padded integer version followed
// by an underscore, e.g. 0001_requests.sql. The number is the version; the
// rest is a human label.
//
// The returned slice names the files applied by this call, in order — empty
// when the database was already current.
func Migrate(ctx context.Context, conn *pgx.Conn, src fs.FS) ([]string, error) {
	// Serialize concurrent runners. The lock is released when this session
	// ends; unlocking explicitly on the happy path just returns it sooner.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		return nil, fmt.Errorf("acquiring migration lock: %w", err)
	}
	defer conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey)

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    integer     PRIMARY KEY,
			name       text        NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return nil, fmt.Errorf("creating schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return nil, err
	}

	files, err := migrationFiles(src)
	if err != nil {
		return nil, err
	}

	var ran []string
	for _, f := range files {
		if applied[f.version] {
			continue
		}
		if err := applyOne(ctx, conn, src, f); err != nil {
			return ran, err
		}
		ran = append(ran, f.name)
	}
	return ran, nil
}

// MigrateDSN opens one connection, runs Migrate against it, and closes it. It
// is the whole job of cmd/migrate, and the request-log setup path in
// cmd/gateway calls it too.
func MigrateDSN(ctx context.Context, dsn string, src fs.FS) ([]string, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	defer conn.Close(ctx)

	return Migrate(ctx, conn, src)
}

type migrationFile struct {
	version int
	name    string // full filename, e.g. "0001_requests.sql"
}

func migrationFiles(src fs.FS) ([]migrationFile, error) {
	entries, err := fs.Glob(src, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("listing migrations: %w", err)
	}
	sort.Strings(entries)

	files := make([]migrationFile, 0, len(entries))
	for _, name := range entries {
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			return nil, fmt.Errorf("migration %q has no version_ prefix", name)
		}
		v, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("migration %q: parsing version %q: %w", name, prefix, err)
		}
		files = append(files, migrationFile{version: v, name: name})
	}
	return files, nil
}

func appliedVersions(ctx context.Context, conn *pgx.Conn) (map[int]bool, error) {
	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("reading applied migrations: %w", err)
	}
	defer rows.Close()

	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scanning applied migration: %w", err)
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// applyOne runs a single migration and records it in the same transaction, so
// a failure part-way through leaves neither the schema change nor the
// bookkeeping row behind.
func applyOne(ctx context.Context, conn *pgx.Conn, src fs.FS, f migrationFile) error {
	body, err := fs.ReadFile(src, f.name)
	if err != nil {
		return fmt.Errorf("reading migration %q: %w", f.name, err)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning migration %q: %w", f.name, err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, string(body)); err != nil {
		return fmt.Errorf("applying migration %q: %w", f.name, err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO schema_migrations (version, name) VALUES ($1, $2)",
		f.version, f.name,
	); err != nil {
		return fmt.Errorf("recording migration %q: %w", f.name, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing migration %q: %w", f.name, err)
	}
	return nil
}
