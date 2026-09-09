// Package teamstore is the Postgres home for teams and organizations (Tier 1,
// Phase 2). It depends on internal/auth for the Team type but not the other way
// round: auth stays pure in-memory logic, so nothing that imports it for a Team
// picks up a database dependency.
//
// This file is the one-time import from configs/teams.yaml.
package teamstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Harshalsharma05/switchyard/internal/auth"
)

// The organisation every seeded team joins. teams.organization_id is NOT NULL,
// so a default org has to exist before any team does; one org for everyone
// makes the concept real without building multi-org behaviour.
const (
	DefaultOrgID   = "personal"
	DefaultOrgName = "Personal"
)

// Guards the emptiness check and the inserts that follow, so two migrate
// containers starting together cannot both see an empty table and both import.
// Distinct from logstore's migration lock, and stable forever.
const seedLockKey int64 = 0x5359_4152_4401 // "SYARD\1"

// Seed imports teams into an empty teams table and reports how many it wrote.
//
// It writes nothing and returns 0 when any team already exists: configs/teams.yaml
// is a first-boot seed for an empty database, never a second source of truth
// alongside it. Once a team is in Postgres, the file is dead — a limit edited
// through the admin API must not be reverted by the next deploy re-reading YAML.
func Seed(ctx context.Context, conn *pgx.Conn, teams []auth.Team) (int, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("beginning team seed: %w", err)
	}
	defer tx.Rollback(ctx)

	// Transaction-scoped: released on commit or rollback, so a crashed seeder
	// cannot leave the lock held.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", seedLockKey); err != nil {
		return 0, fmt.Errorf("acquiring team seed lock: %w", err)
	}

	var populated bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM teams)").Scan(&populated); err != nil {
		return 0, fmt.Errorf("checking for existing teams: %w", err)
	}
	if populated {
		return 0, nil
	}

	// ON CONFLICT because the org can outlive the teams that joined it — every
	// team soft-deleted still leaves the row here.
	if _, err := tx.Exec(ctx,
		`INSERT INTO organizations (id, name) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
		DefaultOrgID, DefaultOrgName,
	); err != nil {
		return 0, fmt.Errorf("creating default organization: %w", err)
	}

	for _, t := range teams {
		if err := insertTeam(ctx, tx, t); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("committing team seed: %w", err)
	}
	return len(teams), nil
}

// SeedDSN opens one connection, runs Seed against it, and closes it — the same
// shape as logstore.MigrateDSN, and what cmd/migrate calls.
func SeedDSN(ctx context.Context, dsn string, teams []auth.Team) (int, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return 0, fmt.Errorf("connecting to postgres: %w", err)
	}
	defer conn.Close(ctx)

	return Seed(ctx, conn, teams)
}

const insertTeamSQL = `
	INSERT INTO teams (
		id, organization_id, name, priority, rpm, tpm, monthly_budget_micros,
		allowed_providers, allowed_models, is_admin,
		key_hash, key_source, key_masked, key_created_at
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`

func insertTeam(ctx context.Context, tx pgx.Tx, t auth.Team) error {
	// A revoked key is NULL, not "" — the partial unique index on key_hash is
	// what lets several revoked teams coexist, and "" is not a valid digest.
	var keyHash *string
	if t.KeyHash != "" {
		keyHash = &t.KeyHash
	}

	if _, err := tx.Exec(ctx, insertTeamSQL,
		t.ID, DefaultOrgID, t.Name, string(t.Priority),
		t.RateLimits.RPM, t.RateLimits.TPM, t.MonthlyBudgetMicros,
		t.AllowedProviders, t.AllowedModels, t.IsAdmin,
		keyHash, t.KeySource, t.KeyMasked, t.KeyCreatedAt,
	); err != nil {
		return fmt.Errorf("seeding team %q: %w", t.ID, err)
	}
	return nil
}
