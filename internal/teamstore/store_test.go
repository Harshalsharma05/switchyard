package teamstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Harshalsharma05/switchyard/internal/auth"
	"github.com/Harshalsharma05/switchyard/internal/logstore"
	"github.com/Harshalsharma05/switchyard/migrations"
)

const (
	acmeKey   = "sk-switchyard-dev-acme-9f2b1c"
	globexKey = "sk-switchyard-dev-globex-7a4e0d"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newSeededPool gives back a pool scoped to a fresh schema, migrated and seeded
// with the two config teams — the state a gateway boots into after cmd/migrate.
// newPool builds further pools against that same schema, which is how the tests
// simulate a restart or a second replica.
func newSeededPool(t *testing.T) (*pgxpool.Pool, func() *pgxpool.Pool, context.Context) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	dsn := testDSN(t)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("no Postgres reachable: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })

	schema := fmt.Sprintf("teamstore_store_%d", time.Now().UnixNano())
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

	newPool := func() *pgxpool.Pool {
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("parsing pool config: %v", err)
		}
		// Pin every connection in the pool to this test's schema, so the store
		// sees only its own teams table.
		cfg.ConnConfig.RuntimeParams["search_path"] = schema
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("building pool: %v", err)
		}
		t.Cleanup(pool.Close)
		return pool
	}

	// Seed through the schema-scoped connection rather than a DSN, so the teams
	// land in this test's schema.
	if _, err := Seed(ctx, conn, configTeams()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	return newPool(), newPool, ctx
}

func newStore(t *testing.T, pool *pgxpool.Pool, ctx context.Context) *Store {
	t.Helper()
	s, err := New(ctx, pool, time.Minute, nil, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// The hot path: a seeded key resolves, an unknown one is refused. Both answers
// come from the snapshot, so neither depends on Postgres being reachable.
func TestAuthenticateResolvesFromSnapshot(t *testing.T) {
	pool, _, ctx := newSeededPool(t)
	s := newStore(t, pool, ctx)

	team, err := s.Authenticate(acmeKey)
	if err != nil {
		t.Fatalf("Authenticate(acme): %v", err)
	}
	if team.ID != "acme" || team.RateLimits.RPM != 60 || !team.IsAdmin {
		t.Errorf("team = %+v, want acme with rpm 60 and admin", team)
	}

	if _, err := s.Authenticate("sk-switchyard-nope"); !errors.Is(err, auth.ErrUnknownKey) {
		t.Errorf("unknown key error = %v, want ErrUnknownKey", err)
	}
}

// The phase's headline fix: a rotation must survive a restart. The second store
// is a fresh process's view of the same database.
func TestRotateKeySurvivesRestart(t *testing.T) {
	pool, newPool, ctx := newSeededPool(t)
	s := newStore(t, pool, ctx)

	rotated := "sk-switchyard-acme-rotated"
	if _, err := s.RotateKey(ctx, "acme", auth.HashKey(rotated), auth.MaskKey(rotated)); err != nil {
		t.Fatalf("RotateKey: %v", err)
	}

	// Visible immediately on this instance, without waiting for a refresh.
	if _, err := s.Authenticate(rotated); err != nil {
		t.Fatalf("rotated key on the rotating instance: %v", err)
	}
	if _, err := s.Authenticate(acmeKey); !errors.Is(err, auth.ErrUnknownKey) {
		t.Errorf("old key still authenticates on the rotating instance: %v", err)
	}

	restarted := newStore(t, newPool(), ctx)
	if _, err := restarted.Authenticate(rotated); err != nil {
		t.Errorf("rotated key after restart: %v", err)
	}
	if _, err := restarted.Authenticate(acmeKey); !errors.Is(err, auth.ErrUnknownKey) {
		t.Errorf("old key still authenticates after restart: %v", err)
	}
}

// Revocation must also outlive a restart, and must leave the team itself intact
// so an admin can still see it and issue a new key.
func TestRevokeKeySurvivesRestart(t *testing.T) {
	pool, newPool, ctx := newSeededPool(t)
	s := newStore(t, pool, ctx)

	if _, err := s.RevokeKey(ctx, "acme"); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}

	restarted := newStore(t, newPool(), ctx)
	if _, err := restarted.Authenticate(acmeKey); !errors.Is(err, auth.ErrUnknownKey) {
		t.Errorf("revoked key still authenticates after restart: %v", err)
	}
	team, err := restarted.Get("acme")
	if err != nil {
		t.Fatalf("revoked team disappeared: %v", err)
	}
	if team.KeySource != auth.KeySourceRevoked {
		t.Errorf("key_source = %q, want %q", team.KeySource, auth.KeySourceRevoked)
	}
}

// A limit edit persists, and is visible immediately rather than at the next
// refresh.
func TestUpdatePersists(t *testing.T) {
	pool, newPool, ctx := newSeededPool(t)
	s := newStore(t, pool, ctx)

	rpm := 250
	got, err := s.Update(ctx, "acme", auth.TeamPatch{RPM: &rpm})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.RateLimits.RPM != rpm {
		t.Errorf("returned rpm = %d, want %d", got.RateLimits.RPM, rpm)
	}

	restarted := newStore(t, newPool(), ctx)
	after, err := restarted.Get("acme")
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if after.RateLimits.RPM != rpm {
		t.Errorf("rpm after restart = %d, want %d", after.RateLimits.RPM, rpm)
	}
}

// An invalid patch is rejected before anything is written, with the same error
// the in-memory registry gave before teams moved to Postgres.
func TestUpdateRejectsInvalidPatchWithoutWriting(t *testing.T) {
	pool, _, ctx := newSeededPool(t)
	s := newStore(t, pool, ctx)

	zero := 0
	if _, err := s.Update(ctx, "acme", auth.TeamPatch{RPM: &zero}); err == nil {
		t.Fatal("Update accepted rpm 0")
	}

	team, err := s.Get("acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if team.RateLimits.RPM != 60 {
		t.Errorf("rpm = %d, want the original 60 — a rejected patch was written", team.RateLimits.RPM)
	}
}

// The deliberate stale-data case. With Postgres unreachable, a key already in
// the snapshot keeps working, an unknown key is still refused, and the store
// reports itself degraded rather than pretending it is current.
func TestServesStaleSnapshotWhenPostgresIsDown(t *testing.T) {
	pool, _, ctx := newSeededPool(t)
	s := newStore(t, pool, ctx)

	if degraded, _ := s.Degraded(); degraded {
		t.Fatal("store reported degraded before any failure")
	}

	pool.Close() // the database going away, from this gateway's point of view

	if err := s.Refresh(ctx); err == nil {
		t.Fatal("Refresh succeeded against a closed pool")
	}
	degraded, loadedAt := s.Degraded()
	if !degraded {
		t.Error("store is not degraded after a failed refresh")
	}
	if loadedAt.IsZero() {
		t.Error("degraded snapshot lost its load time")
	}

	if _, err := s.Authenticate(acmeKey); err != nil {
		t.Errorf("cached key stopped working while Postgres was down: %v", err)
	}
	if _, err := s.Authenticate(globexKey); err != nil {
		t.Errorf("cached key stopped working while Postgres was down: %v", err)
	}
	if _, err := s.Authenticate("sk-switchyard-never-seen"); !errors.Is(err, auth.ErrUnknownKey) {
		t.Errorf("uncached key error = %v, want ErrUnknownKey — auth must never fail open", err)
	}
}

// A change made elsewhere — another replica, or psql — reaches this instance at
// the next refresh. This is the TTL half of the invalidation story.
func TestRefreshPicksUpExternalChanges(t *testing.T) {
	pool, newPool, ctx := newSeededPool(t)
	s := newStore(t, pool, ctx)

	other := newPool()
	if _, err := other.Exec(ctx,
		"UPDATE teams SET key_hash = NULL, key_source = $1 WHERE id = 'globex'",
		auth.KeySourceRevoked,
	); err != nil {
		t.Fatalf("revoking from another connection: %v", err)
	}

	// Still valid here: the snapshot has not been refreshed yet, which is the
	// documented revocation window.
	if _, err := s.Authenticate(globexKey); err != nil {
		t.Fatalf("key stopped working before a refresh: %v", err)
	}

	if err := s.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, err := s.Authenticate(globexKey); !errors.Is(err, auth.ErrUnknownKey) {
		t.Errorf("externally revoked key still authenticates after a refresh: %v", err)
	}
}
