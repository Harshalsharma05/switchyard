// Command migrate applies the database migrations, seeds teams on a first
// boot, and exits.
//
// It is its own binary so the schema can be brought up as a one-shot step —
// a compose service that runs to completion before the gateway starts —
// rather than something the gateway does to itself on boot. Same connection
// settings as the gateway: POSTGRES_PASSWORD plus the SWITCHYARD_POSTGRES_*
// vars.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/config"
	"github.com/Harshalsharma05/switchyard/internal/logstore"
	"github.com/Harshalsharma05/switchyard/internal/teamstore"
	"github.com/Harshalsharma05/switchyard/migrations"
)

const defaultTeamsPath = "configs/teams.yaml"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg := logstore.DBConfig{
		Host:     envOr("SWITCHYARD_POSTGRES_HOST", "localhost:5432"),
		User:     envOr("SWITCHYARD_POSTGRES_USER", "switchyard"),
		Password: os.Getenv("POSTGRES_PASSWORD"),
		Database: envOr("SWITCHYARD_POSTGRES_DB", "switchyard"),
		SSLMode:  envOr("SWITCHYARD_POSTGRES_SSLMODE", "disable"),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	applied, err := logstore.MigrateDSN(ctx, cfg.DSN(), migrations.FS)
	if err != nil {
		log.Error("migration failed", slog.String("database", cfg.Redacted()), slog.Any("error", err))
		os.Exit(1)
	}

	if len(applied) == 0 {
		log.Info("database already up to date", slog.String("database", cfg.Redacted()))
	} else {
		log.Info("migrations applied",
			slog.String("database", cfg.Redacted()),
			slog.Any("files", applied),
		)
	}

	if err := seedTeams(ctx, log, cfg); err != nil {
		log.Error("team seed failed", slog.String("database", cfg.Redacted()), slog.Any("error", err))
		os.Exit(1)
	}
}

// seedTeams runs the one-time import of configs/teams.yaml (Tier 1, Step 2.2).
//
// The file is a first-boot seed for an empty database and nothing more:
// teamstore.Seed writes nothing once any team exists, so this is safe on every
// deploy. It is checked for existence first rather than loaded unconditionally,
// because an operator who deletes the file after the import — which the rule
// permits — must not break every subsequent migrate run.
func seedTeams(ctx context.Context, log *slog.Logger, cfg logstore.DBConfig) error {
	path := envOr("SWITCHYARD_TEAMS_CONFIG", defaultTeamsPath)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		log.Info("no team seed file, skipping team import", slog.String("path", path))
		return nil
	}

	teams, err := config.LoadTeams(path)
	if err != nil {
		return fmt.Errorf("loading team seed: %w", err)
	}

	seeded, err := teamstore.SeedDSN(ctx, cfg.DSN(), teams)
	if err != nil {
		return err
	}
	if seeded == 0 {
		log.Info("teams already present, seed skipped", slog.String("path", path))
		return nil
	}
	log.Info("teams seeded",
		slog.Int("teams", seeded),
		slog.String("organization", teamstore.DefaultOrgID),
		slog.String("path", path),
	)
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
