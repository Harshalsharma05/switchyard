package teamstore

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Harshalsharma05/switchyard/internal/auth"
	"github.com/Harshalsharma05/switchyard/internal/logstore"
	"github.com/Harshalsharma05/switchyard/migrations"
)

// Same convention as internal/logstore: an explicit DSN override, or one
// assembled from POSTGRES_PASSWORD, and a skip when there is no database to
// talk to.
func testDSN(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("SWITCHYARD_TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	pw := os.Getenv("POSTGRES_PASSWORD")
	if pw == "" {
		t.Skip("set POSTGRES_PASSWORD (and run Postgres) or SWITCHYARD_TEST_POSTGRES_DSN to run teamstore tests")
	}
	return logstore.DBConfig{
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

// newMigratedSchema hands back a connection scoped to a fresh schema with every
// migration applied, so each test seeds into a genuinely empty teams table.
func newMigratedSchema(t *testing.T) (*pgx.Conn, context.Context) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	conn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Skipf("no Postgres reachable: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })

	schema := fmt.Sprintf("teamstore_test_%d", time.Now().UnixNano())
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

	if _, err := logstore.Migrate(ctx, conn, migrations.FS); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return conn, ctx
}

func configTeams() []auth.Team {
	return []auth.Team{
		{
			ID:                  "acme",
			Name:                "Acme Corp",
			KeyHash:             "2ce25520782abab77e9a1fde48ff35112740f6bc078b28154dec3d32651b854b",
			AllowedProviders:    []string{"groq", "gemini", "ollama"},
			AllowedModels:       []string{"openai/gpt-oss-120b", "llama3.2:3b"},
			RateLimits:          auth.RateLimits{RPM: 60, TPM: 100000},
			MonthlyBudgetMicros: 50_000_000,
			Priority:            auth.PriorityRealtime,
			IsAdmin:             true,
			KeySource:           auth.KeySourceConfig,
		},
		{
			ID:                  "globex",
			Name:                "Globex Inc",
			KeyHash:             "dd06a2a0a097eb98b0bb25130211d5f748218c5b2c27397b2f853a11a3ff9577",
			AllowedProviders:    []string{"groq", "ollama"},
			AllowedModels:       []string{"openai/gpt-oss-20b"},
			RateLimits:          auth.RateLimits{RPM: 10, TPM: 20000},
			MonthlyBudgetMicros: 5_000_000,
			Priority:            auth.PriorityBatch,
			KeySource:           auth.KeySourceConfig,
		},
	}
}

// Every team must arrive with its limits, budget, and allowlists intact — the
// checklist's "identical limits, budget, and allowlists". The allowlists are the
// interesting half: they cross into JSONB and have to come back as the same
// ordered list.
func TestSeedImportsEveryTeam(t *testing.T) {
	conn, ctx := newMigratedSchema(t)

	want := configTeams()
	n, err := Seed(ctx, conn, want)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if n != len(want) {
		t.Fatalf("Seed wrote %d teams, want %d", n, len(want))
	}

	var orgName string
	if err := conn.QueryRow(ctx, "SELECT name FROM organizations WHERE id = $1", DefaultOrgID).Scan(&orgName); err != nil {
		t.Fatalf("reading default organization: %v", err)
	}
	if orgName != DefaultOrgName {
		t.Errorf("default org name = %q, want %q", orgName, DefaultOrgName)
	}

	for _, w := range want {
		t.Run(w.ID, func(t *testing.T) {
			var got auth.Team
			var org, priority string
			var keyHash *string
			if err := conn.QueryRow(ctx, `
				SELECT organization_id, name, priority, rpm, tpm, monthly_budget_micros,
				       allowed_providers, allowed_models, is_admin, key_hash, key_source
				FROM teams WHERE id = $1 AND deleted_at IS NULL`, w.ID).Scan(
				&org, &got.Name, &priority, &got.RateLimits.RPM, &got.RateLimits.TPM,
				&got.MonthlyBudgetMicros, &got.AllowedProviders, &got.AllowedModels,
				&got.IsAdmin, &keyHash, &got.KeySource,
			); err != nil {
				t.Fatalf("reading team back: %v", err)
			}

			if org != DefaultOrgID {
				t.Errorf("organization_id = %q, want %q", org, DefaultOrgID)
			}
			if got.Name != w.Name || priority != string(w.Priority) {
				t.Errorf("name/priority = %q/%q, want %q/%q", got.Name, priority, w.Name, w.Priority)
			}
			if got.RateLimits != w.RateLimits {
				t.Errorf("rate limits = %+v, want %+v", got.RateLimits, w.RateLimits)
			}
			if got.MonthlyBudgetMicros != w.MonthlyBudgetMicros {
				t.Errorf("budget = %d micros, want %d", got.MonthlyBudgetMicros, w.MonthlyBudgetMicros)
			}
			if got.IsAdmin != w.IsAdmin {
				t.Errorf("is_admin = %v, want %v", got.IsAdmin, w.IsAdmin)
			}
			if keyHash == nil || *keyHash != w.KeyHash {
				t.Errorf("key_hash = %v, want %q", keyHash, w.KeyHash)
			}
			if got.KeySource != auth.KeySourceConfig {
				t.Errorf("key_source = %q, want %q", got.KeySource, auth.KeySourceConfig)
			}
			if !equalStrings(got.AllowedProviders, w.AllowedProviders) {
				t.Errorf("allowed_providers = %v, want %v", got.AllowedProviders, w.AllowedProviders)
			}
			if !equalStrings(got.AllowedModels, w.AllowedModels) {
				t.Errorf("allowed_models = %v, want %v", got.AllowedModels, w.AllowedModels)
			}
		})
	}
}

// The idempotency guarantee, and the rule behind it: once teams exist, the YAML
// is dead. A second run must not duplicate, and must not resurrect a value an
// admin has since changed in the database.
func TestSeedSkipsWhenTeamsExist(t *testing.T) {
	conn, ctx := newMigratedSchema(t)

	if _, err := Seed(ctx, conn, configTeams()); err != nil {
		t.Fatalf("first Seed: %v", err)
	}
	if _, err := conn.Exec(ctx, "UPDATE teams SET rpm = 999 WHERE id = 'acme'"); err != nil {
		t.Fatalf("simulating an admin edit: %v", err)
	}

	n, err := Seed(ctx, conn, configTeams())
	if err != nil {
		t.Fatalf("second Seed: %v", err)
	}
	if n != 0 {
		t.Errorf("second Seed wrote %d teams, want 0", n)
	}

	var count, rpm int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM teams").Scan(&count); err != nil {
		t.Fatalf("counting teams: %v", err)
	}
	if count != len(configTeams()) {
		t.Errorf("teams table has %d rows, want %d", count, len(configTeams()))
	}
	if err := conn.QueryRow(ctx, "SELECT rpm FROM teams WHERE id = 'acme'").Scan(&rpm); err != nil {
		t.Fatalf("reading rpm: %v", err)
	}
	if rpm != 999 {
		t.Errorf("rpm = %d, want the admin's 999 — the seed reverted a live edit", rpm)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
