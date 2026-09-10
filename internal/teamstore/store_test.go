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
	dto "github.com/prometheus/client_model/go"

	"github.com/Harshalsharma05/switchyard/internal/auth"
	"github.com/Harshalsharma05/switchyard/internal/logstore"
	"github.com/Harshalsharma05/switchyard/internal/telemetry"
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

// The checklist's "create a team, use its key immediately, restart the gateway,
// key still works" — a second store stands in for the restarted process.
func TestCreateSurvivesRestart(t *testing.T) {
	pool, newPool, ctx := newSeededPool(t)
	s := newStore(t, pool, ctx)

	raw := "sk-switchyard-initech-test"
	created, err := s.Create(ctx, auth.Team{
		ID: "initech", Name: "Initech",
		AllowedProviders: []string{"groq"}, AllowedModels: []string{"openai/gpt-oss-20b"},
		RateLimits: auth.RateLimits{RPM: 30, TPM: 10_000}, MonthlyBudgetMicros: 2_000_000,
		Priority: auth.PriorityBatch,
		KeyHash:  auth.HashKey(raw), KeyMasked: auth.MaskKey(raw),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.OrganizationID != DefaultOrgID || created.KeySource != auth.KeySourceCreated {
		t.Errorf("created org/source = %q/%q, want %q/%q",
			created.OrganizationID, created.KeySource, DefaultOrgID, auth.KeySourceCreated)
	}
	if _, err := s.Authenticate(raw); err != nil {
		t.Fatalf("new key on the creating instance: %v", err)
	}

	restarted := newStore(t, newPool(), ctx)
	got, err := restarted.Authenticate(raw)
	if err != nil {
		t.Fatalf("new key after restart: %v", err)
	}
	if got.RateLimits.RPM != 30 || got.OrganizationID != DefaultOrgID {
		t.Errorf("after restart rpm/org = %d/%q, want 30/%q", got.RateLimits.RPM, got.OrganizationID, DefaultOrgID)
	}
}

// Soft delete: the key dies on the next request, the team leaves every
// snapshot, the row stays for request-log history, and the ID is burned.
func TestDeleteIsSoftAndBurnsTheID(t *testing.T) {
	pool, newPool, ctx := newSeededPool(t)
	s := newStore(t, pool, ctx)

	if _, err := s.Delete(ctx, "globex"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Authenticate(globexKey); !errors.Is(err, auth.ErrUnknownKey) {
		t.Errorf("deleted team's key still authenticates: %v", err)
	}
	if _, err := s.Get("globex"); !errors.Is(err, auth.ErrUnknownTeam) {
		t.Errorf("deleted team still in the snapshot: %v", err)
	}

	var deletedAt *time.Time
	var keyHash *string
	if err := pool.QueryRow(ctx, "SELECT deleted_at, key_hash FROM teams WHERE id = 'globex'").Scan(&deletedAt, &keyHash); err != nil {
		t.Fatalf("reading the deleted row: %v — a soft delete must leave it for request-log history", err)
	}
	if deletedAt == nil || keyHash != nil {
		t.Errorf("deleted_at = %v, key_hash = %v; want set and NULL", deletedAt, keyHash)
	}

	restarted := newStore(t, newPool(), ctx)
	if _, err := restarted.Get("globex"); !errors.Is(err, auth.ErrUnknownTeam) {
		t.Errorf("deleted team came back after a restart: %v", err)
	}

	reuse := auth.Team{
		ID: "globex", Name: "Globex Again",
		AllowedProviders: []string{"groq"}, AllowedModels: []string{"m"},
		RateLimits: auth.RateLimits{RPM: 1, TPM: 1}, MonthlyBudgetMicros: 1,
		Priority: auth.PriorityBatch,
	}
	if _, err := s.Create(ctx, reuse); !errors.Is(err, auth.ErrTeamExists) {
		t.Errorf("reusing a deleted team's ID: err = %v, want ErrTeamExists", err)
	}
}

// Boot fails closed: with no teams there is no snapshot worth serving, and an
// empty one would reject every key without saying why.
func TestNewFailsOnAnEmptyTeamsTable(t *testing.T) {
	pool, _, ctx := newSeededPool(t)
	if _, err := pool.Exec(ctx, "DELETE FROM teams"); err != nil {
		t.Fatalf("emptying teams: %v", err)
	}
	if _, err := New(ctx, pool, time.Minute, nil, discardLogger()); err == nil {
		t.Fatal("New succeeded against an empty teams table")
	}
}

// Every mutation on a team that does not exist is ErrUnknownTeam, so the admin
// API can map it onto a 404 by identity — whether the miss is caught against
// the snapshot or by an UPDATE that matched no row.
func TestMutationsOnAnUnknownTeamAreErrUnknownTeam(t *testing.T) {
	pool, _, ctx := newSeededPool(t)
	s := newStore(t, pool, ctx)

	rpm := 10
	tests := map[string]func() error{
		"update": func() error { _, err := s.Update(ctx, "nope", auth.TeamPatch{RPM: &rpm}); return err },
		"rotate": func() error { _, err := s.RotateKey(ctx, "nope", auth.HashKey("k"), "sk-…k"); return err },
		"revoke": func() error { _, err := s.RevokeKey(ctx, "nope"); return err },
		"delete": func() error { _, err := s.Delete(ctx, "nope"); return err },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			if err := mutate(); !errors.Is(err, auth.ErrUnknownTeam) {
				t.Errorf("err = %v, want ErrUnknownTeam", err)
			}
		})
	}
}

func TestCreateWithAnUnknownOrganization(t *testing.T) {
	pool, _, ctx := newSeededPool(t)
	s := newStore(t, pool, ctx)

	_, err := s.Create(ctx, auth.Team{
		ID: "orphan", Name: "Orphan", OrganizationID: "no-such-org",
		AllowedProviders: []string{"groq"}, AllowedModels: []string{"m"},
		RateLimits: auth.RateLimits{RPM: 1, TPM: 1}, MonthlyBudgetMicros: 1,
		Priority: auth.PriorityBatch,
	})
	if !errors.Is(err, auth.ErrUnknownOrganization) {
		t.Fatalf("err = %v, want ErrUnknownOrganization", err)
	}
	if _, err := s.Get("orphan"); !errors.Is(err, auth.ErrUnknownTeam) {
		t.Errorf("a team that failed to insert is in the snapshot: %v", err)
	}
}

// Run's ticker is the TTL half of invalidation: a change made on another
// replica reaches this one without anyone calling Refresh by hand.
func TestRunRefreshesOnItsTicker(t *testing.T) {
	pool, newPool, ctx := newSeededPool(t)
	s, err := New(ctx, pool, 50*time.Millisecond, nil, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(runCtx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	if _, err := newPool().Exec(ctx,
		"UPDATE teams SET key_hash = NULL, key_source = $1 WHERE id = 'globex'", auth.KeySourceRevoked,
	); err != nil {
		t.Fatalf("revoking from another connection: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := s.Authenticate(globexKey); errors.Is(err, auth.ErrUnknownKey) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("an externally revoked key still authenticates after several ticks")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func counterValue(t *testing.T, c interface{ Write(*dto.Metric) error }) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("reading metric: %v", err)
	}
	if m.Counter != nil {
		return m.GetCounter().GetValue()
	}
	return m.GetGauge().GetValue()
}

// The checklist wants the no-query-per-request claim and the degraded state
// verified with metrics. Lookups count against the snapshot; a failed refresh
// flips the degraded gauge and counts an error, and never touches the lookups.
func TestMetricsReportLookupsAndDegradation(t *testing.T) {
	pool, _, ctx := newSeededPool(t)
	m, err := telemetry.NewMetrics()
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	s, err := New(ctx, pool, time.Minute, m, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := 0; i < 5; i++ {
		s.Authenticate(acmeKey)
	}
	s.Authenticate("sk-switchyard-unknown")

	if got := counterValue(t, m.TeamLookupsTotal.WithLabelValues("hit")); got != 5 {
		t.Errorf("hit lookups = %v, want 5", got)
	}
	if got := counterValue(t, m.TeamLookupsTotal.WithLabelValues("unknown")); got != 1 {
		t.Errorf("unknown lookups = %v, want 1", got)
	}
	if got := counterValue(t, m.TeamSnapshotRefreshTotal.WithLabelValues("ok")); got != 1 {
		t.Errorf("ok refreshes = %v, want 1 — six lookups must not have queried Postgres", got)
	}
	if got := counterValue(t, m.TeamStoreDegraded); got != 0 {
		t.Errorf("degraded = %v before any failure, want 0", got)
	}

	pool.Close()
	if err := s.Refresh(ctx); err == nil {
		t.Fatal("Refresh succeeded against a closed pool")
	}
	if got := counterValue(t, m.TeamStoreDegraded); got != 1 {
		t.Errorf("degraded = %v after a failed refresh, want 1", got)
	}
	if got := counterValue(t, m.TeamSnapshotRefreshTotal.WithLabelValues("error")); got != 1 {
		t.Errorf("error refreshes = %v, want 1", got)
	}
}
