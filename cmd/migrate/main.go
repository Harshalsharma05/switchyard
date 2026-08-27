// Command migrate applies the request-log database migrations and exits.
//
// It is its own binary so the schema can be brought up as a one-shot step —
// a compose service that runs to completion before the gateway starts —
// rather than something the gateway does to itself on boot. Same connection
// settings as the gateway: POSTGRES_PASSWORD plus the SWITCHYARD_POSTGRES_*
// vars.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/logstore"
	"github.com/Harshalsharma05/switchyard/migrations"
)

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
		return
	}
	log.Info("migrations applied",
		slog.String("database", cfg.Redacted()),
		slog.Any("files", applied),
	)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
