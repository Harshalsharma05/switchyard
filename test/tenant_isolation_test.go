// Phase 4 Step 4.1 — the two-organisation fixture every tenant-isolation
// attack in 4.2 and 4.3 runs against: two orgs, each with several teams,
// request history (some of it quality-scored), cache entries, and audit
// history, plus a superadmin who can act on either. Building it once here
// means every later attack starts from the same known-good world instead of
// re-deriving it.
//
//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Harshalsharma05/switchyard/internal/cache"
	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// tenantFixture is the running world Phase 4 attacks against: two
// organisations behind one gateway, each with two teams, plus the sessions
// needed to act as either org or as the superadmin.
type tenantFixture struct {
	gw  *gatewayInstance
	db  *pgxpool.Pool
	rdb *redis.Client

	orgA, orgB string

	// teamA/teamB is each org's primary team — the one 4.2/4.3 attacks
	// individually. teamA2/teamB2 exist only so "several projects per org" is
	// real, matching Step 4.1's setup requirement.
	teamA, teamAKey   string
	teamA2, teamA2Key string
	teamB, teamBKey   string
	teamB2, teamB2Key string

	modelA, modelB string

	superadminCookie, superadminCSRF string
	orgACookie, orgACSRF             string
	orgBCookie, orgBCSRF             string
}

// setupTenantIsolationFixture builds the two-org world: teams (seeded for org
// A the way production seeds them, created over the real admin API for org
// B), request history with a couple of quality-scored rows, cache entries for
// an identical prompt in both orgs, and audit history covering an org-A
// self-action, an org-B self-action, a superadmin action on an org-A
// resource, and a no-tenant action (a real config reload) — the four cases
// Step 4.3's audit-visibility check needs. Every ID is unique per call, since
// Redis and its TTLs outlive any one test's throwaway Postgres database.
func setupTenantIsolationFixture(t *testing.T) *tenantFixture {
	t.Helper()
	requireRedis(t)

	orgA := "personal" // teamstore.Seed always assigns DefaultOrgID; see below.
	orgB := uniqueID("org-b")

	teamA := uniqueID("team-a1")
	teamA2 := uniqueID("team-a2")
	teamB := uniqueID("team-b1")
	teamB2 := uniqueID("team-b2")

	teamAKey := "key-" + teamA
	teamA2Key := "key-" + teamA2

	provA := uniqueID("prov-a")
	provB := uniqueID("prov-b")
	modelA := uniqueID("model-a")
	modelB := uniqueID("model-b")

	upA := newMockUpstream(t, provA)
	upB := newMockUpstream(t, provB)

	cfg := harnessConfig{
		providers: []providerSpec{
			{name: provA, url: upA.URL(), models: []string{modelA}},
			{name: provB, url: upB.URL(), models: []string{modelB}},
		},
		teams: []teamSpec{
			// Only org A's teams go through cmd/migrate's seed: Seed() always
			// assigns teamstore.DefaultOrgID ("personal") to every team it
			// writes, so there is no way to land a second org's teams here.
			// Org B's teams are created afterwards, over the real admin API,
			// once its organisation row exists.
			defaultTeam(teamA, teamAKey, []string{provA}, []string{modelA}),
			defaultTeam(teamA2, teamA2Key, []string{provA}, []string{modelA}),
		},
	}

	gw := startGateway(t, cfg, upA, upB)

	db, err := pgxpool.New(context.Background(), postgresDSN(gw.DB))
	if err != nil {
		t.Fatalf("connecting to test database: %v", err)
	}
	t.Cleanup(db.Close)

	rdb := redis.NewClient(&redis.Options{Addr: testRedisAddr()})
	t.Cleanup(func() { rdb.Close() })

	// teams.organization_id is a foreign key: org B cannot exist until its
	// organisation row does.
	if _, err := db.Exec(context.Background(),
		`INSERT INTO organizations (id, name) VALUES ($1, $2)`, orgB, "Org B"); err != nil {
		t.Fatalf("creating organization %s: %v", orgB, err)
	}

	superadminCookie, superadminCSRF := adminSession(t)
	orgACookie, orgACSRF := sessionFor(t, uniqueID("usr-a"), orgA, false)
	orgBCookie, orgBCSRF := sessionFor(t, uniqueID("usr-b"), orgB, false)

	teamBKey := createTeamViaAdmin(t, gw, superadminCookie, superadminCSRF, teamB, orgB, provB, modelB)
	teamB2Key := createTeamViaAdmin(t, gw, superadminCookie, superadminCSRF, teamB2, orgB, provB, modelB)

	f := &tenantFixture{
		gw: gw, db: db, rdb: rdb,
		orgA: orgA, orgB: orgB,
		teamA: teamA, teamAKey: teamAKey, teamA2: teamA2, teamA2Key: teamA2Key,
		teamB: teamB, teamBKey: teamBKey, teamB2: teamB2, teamB2Key: teamB2Key,
		modelA: modelA, modelB: modelB,
		superadminCookie: superadminCookie, superadminCSRF: superadminCSRF,
		orgACookie: orgACookie, orgACSRF: orgACSRF,
		orgBCookie: orgBCookie, orgBCSRF: orgBCSRF,
	}

	f.seedRequestHistory(t)
	f.seedCacheEntries(t)
	f.seedAuditHistory(t)

	return f
}

// seedRequestHistory writes a couple of request rows per team directly
// against Postgres. Real traffic through the gateway would eventually produce
// the same rows, but only after waiting on the async log writer's flush —
// machinery Phase 4 has no need to depend on when all it requires is rows
// that exist with the right team, so a direct insert is both simpler and
// deterministic. One row per org additionally carries a quality score, as if
// Phase 9's async judge had already scored it.
func (f *tenantFixture) seedRequestHistory(t *testing.T) {
	t.Helper()

	type row struct {
		id, teamID, model, provider string
		quality                     *float64
		reason                      *string
	}

	lowScore := 2.0
	reason := "downgraded"

	rows := []row{
		{id: uniqueID("req"), teamID: f.teamA, model: f.modelA, provider: f.teamA},
		{id: uniqueID("req"), teamID: f.teamA, model: f.modelA, provider: f.teamA, quality: &lowScore, reason: &reason},
		{id: uniqueID("req"), teamID: f.teamA2, model: f.modelA, provider: f.teamA2},
		{id: uniqueID("req"), teamID: f.teamB, model: f.modelB, provider: f.teamB},
		{id: uniqueID("req"), teamID: f.teamB, model: f.modelB, provider: f.teamB, quality: &lowScore, reason: &reason},
		{id: uniqueID("req"), teamID: f.teamB2, model: f.modelB, provider: f.teamB2},
	}

	for _, r := range rows {
		if _, err := f.db.Exec(context.Background(), `
			INSERT INTO requests (
				id, ts, team_id, requested_model, served_model, provider,
				status_code, input_tokens, output_tokens, cost_micros,
				latency_ms, overhead_ms, fallback, cache_hit,
				quality_score, quality_sample_reason
			) VALUES ($1, now(), $2, $3, $3, $4, 200, 42, 17, 1000, 120.5, 3.2, false, false, $5, $6)`,
			r.id, r.teamID, r.model, r.provider, r.quality, r.reason,
		); err != nil {
			t.Fatalf("inserting request row for team %s: %v", r.teamID, err)
		}
	}
}

// seedCacheEntries writes one cache entry per org for the exact same prompt,
// each under its own scope. Same fingerprint inputs, deliberately: this is
// the pair Step 4.3's "org A and org B send identical prompts" attack needs,
// and NewKey's scope-first hashing is what is supposed to keep their EntryIDs
// apart.
func (f *tenantFixture) seedCacheEntries(t *testing.T) {
	t.Helper()
	store := cache.NewStore(f.rdb, cache.StoreConfig{})

	req := provider.Request{
		Model:    f.modelA,
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "what is our roadmap for next quarter"}},
	}

	for _, s := range []struct {
		org, team string
	}{
		{f.orgA, f.teamA},
		{f.orgB, f.teamB},
	} {
		key := cache.NewKey(cache.Scope{Org: s.org, Team: s.team}, req)
		entry := cache.Entry{
			Response:     "cached answer for " + s.org,
			FinishReason: provider.FinishStop,
			InputTokens:  10,
			OutputTokens: 20,
			Model:        f.modelA,
			Provider:     s.team,
			CreatedAt:    time.Now(),
		}
		if err := store.Put(context.Background(), key, entry, time.Hour); err != nil {
			t.Fatalf("seeding cache entry for org %s: %v", s.org, err)
		}
	}
}

// seedAuditHistory performs four real mutations over the admin API so the
// resulting audit_log rows are exactly what production would write, not a
// hand-built approximation: an org-A self action, an org-B self action, a
// superadmin action against one of org A's own teams, and a config reload —
// the no-tenant action stampActor files under no organisation at all. These
// are the four cases Step 4.3's "audit visibility, both halves" check has to
// tell apart.
func (f *tenantFixture) seedAuditHistory(t *testing.T) {
	t.Helper()

	resetBudgetViaAdmin(t, f.gw, f.orgACookie, f.orgACSRF, f.teamA)
	resetBudgetViaAdmin(t, f.gw, f.orgBCookie, f.orgBCSRF, f.teamB)
	resetBudgetViaAdmin(t, f.gw, f.superadminCookie, f.superadminCSRF, f.teamA2)

	req, err := http.NewRequest(http.MethodPost, f.gw.AdminURL+"/admin/reload", nil)
	if err != nil {
		t.Fatalf("building reload request: %v", err)
	}
	req.Header.Set("Cookie", f.superadminCookie)
	req.Header.Set("X-CSRF-Token", f.superadminCSRF)
	resp, err := f.gw.Client.Do(req)
	if err != nil {
		t.Fatalf("triggering config reload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("config reload: want 200, got %d", resp.StatusCode)
	}
}

// createTeamViaAdmin creates one team in org via the real POST /admin/teams,
// authenticated with cookie/csrf, and returns its API key. Used for org B:
// cmd/migrate's seed cannot place a team anywhere but the default org, so a
// second organisation's teams only ever come from this endpoint, exactly as
// they would for a real org-B signup.
func createTeamViaAdmin(t *testing.T, gw *gatewayInstance, cookie, csrf, name, org, providerName, model string) string {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"name":               name,
		"organization_id":    org,
		"priority":           "realtime",
		"rpm":                1000,
		"tpm":                1_000_000,
		"monthly_budget_usd": 1000.0,
		"allowed_providers":  []string{providerName},
		"allowed_models":     []string{model},
	})
	if err != nil {
		t.Fatalf("marshalling create-team body: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, gw.AdminURL+"/admin/teams", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building create-team request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("X-CSRF-Token", csrf)

	resp, err := gw.Client.Do(req)
	if err != nil {
		t.Fatalf("creating team %s: %v", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("creating team %s: want 201, got %d", name, resp.StatusCode)
	}

	var out struct {
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding create-team response for %s: %v", name, err)
	}
	return out.APIKey
}

// resetBudgetViaAdmin calls POST /admin/teams/{id}/reset-budget, a mutation
// that needs no request body and exists purely to produce a real audit row
// attributed to whichever session calls it.
func resetBudgetViaAdmin(t *testing.T, gw *gatewayInstance, cookie, csrf, teamID string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, gw.AdminURL+"/admin/teams/"+teamID+"/reset-budget", nil)
	if err != nil {
		t.Fatalf("building reset-budget request: %v", err)
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("X-CSRF-Token", csrf)

	resp, err := gw.Client.Do(req)
	if err != nil {
		t.Fatalf("resetting budget for team %s: %v", teamID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resetting budget for team %s: want 200, got %d", teamID, resp.StatusCode)
	}
}

// TestTenantIsolationFixtureSetup is Step 4.1 itself: proof the two-org world
// is well-formed before Phase 4.2 starts attacking it. It is not yet an
// attack — it does not try org B's ID against org A's session — it only
// confirms the fixture built what it claims to have built.
func TestTenantIsolationFixtureSetup(t *testing.T) {
	f := setupTenantIsolationFixture(t)
	ctx := context.Background()

	if f.orgA == f.orgB {
		t.Fatalf("org A and org B must be distinct, got %q twice", f.orgA)
	}

	var count int
	if err := f.db.QueryRow(ctx,
		`SELECT count(*) FROM requests WHERE team_id IN ($1, $2, $3, $4)`,
		f.teamA, f.teamA2, f.teamB, f.teamB2,
	).Scan(&count); err != nil {
		t.Fatalf("counting seeded request rows: %v", err)
	}
	if count != 6 {
		t.Errorf("request history: want 6 rows across both orgs, got %d", count)
	}

	var scored int
	if err := f.db.QueryRow(ctx,
		`SELECT count(*) FROM requests WHERE team_id IN ($1, $2) AND quality_score IS NOT NULL`,
		f.teamA, f.teamB,
	).Scan(&scored); err != nil {
		t.Fatalf("counting quality-scored rows: %v", err)
	}
	if scored != 2 {
		t.Errorf("quality scores: want 1 scored row in each of org A and org B's primary team, got %d total", scored)
	}

	var orgAAudits, orgBAudits, noTenantAudits int
	for _, q := range []struct {
		where string
		args  []any
		into  *int
	}{
		{`organization_id = $1`, []any{f.orgA}, &orgAAudits},
		{`organization_id = $1`, []any{f.orgB}, &orgBAudits},
		{`organization_id IS NULL`, nil, &noTenantAudits},
	} {
		if err := f.db.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE `+q.where, q.args...).Scan(q.into); err != nil {
			t.Fatalf("counting audit rows (%s): %v", q.where, err)
		}
	}
	// Org A has two: the org-A self action (on teamA) and the superadmin
	// action filed against teamA2, which still belongs to org A. Org A's
	// teams themselves are YAML-seeded by cmd/migrate, not created over the
	// audited admin API, so their creation adds no rows.
	if orgAAudits != 2 {
		t.Errorf("org A audit rows: want 2 (self action + superadmin-on-org-A), got %d", orgAAudits)
	}
	// Org B has three: unlike org A's, its two teams (teamB, teamB2) are
	// created over the real POST /admin/teams as superadmin — each creation
	// is itself an audited, superadmin-attributed mutation filed under org B
	// — plus the org-B self action on teamB.
	if orgBAudits != 3 {
		t.Errorf("org B audit rows: want 3 (2 team creations + self action), got %d", orgBAudits)
	}
	if noTenantAudits < 1 {
		t.Errorf("no-tenant audit rows: want at least 1 (config reload), got %d", noTenantAudits)
	}

	req := provider.Request{
		Model:    f.modelA,
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "what is our roadmap for next quarter"}},
	}
	keyA := cache.NewKey(cache.Scope{Org: f.orgA, Team: f.teamA}, req)
	keyB := cache.NewKey(cache.Scope{Org: f.orgB, Team: f.teamB}, req)
	if keyA.EntryID == keyB.EntryID {
		t.Fatal("identical prompts in org A and org B hashed to the same cache entry ID")
	}

	store := cache.NewStore(f.rdb, cache.StoreConfig{})
	entryA, hit, err := store.Exact(ctx, keyA)
	if err != nil {
		t.Fatalf("fetching org A's cache entry: %v", err)
	}
	if !hit || entryA == nil {
		t.Fatal("org A's seeded cache entry is missing")
	}
	entryB, hit, err := store.Exact(ctx, keyB)
	if err != nil {
		t.Fatalf("fetching org B's cache entry: %v", err)
	}
	if !hit || entryB == nil {
		t.Fatal("org B's seeded cache entry is missing")
	}
	if entryA.Response == entryB.Response {
		t.Fatal("org A and org B's cache entries hold the same response — they were not seeded distinctly")
	}

	fmt.Printf(
		"tenant fixture ready: org A=%s (teams %s, %s), org B=%s (teams %s, %s)\n",
		f.orgA, f.teamA, f.teamA2, f.orgB, f.teamB, f.teamB2,
	)
}
