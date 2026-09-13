// Phase 4 Step 4.3 — the semantic cache is the subtlest tenant-isolation
// vector: a request never mentions another organisation's ID anywhere, so the
// only way to catch a leak is to send the same prompt from both orgs and
// watch which one actually reaches the provider. Its own fixture, purpose-built
// rather than reusing Step 4.1's: this needs two teams that share one provider
// and model — the same request in every respect except which team sends it —
// which the two-org fixture's deliberately separate providers don't give it.
//
//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// cacheStatusHeader mirrors internal/proxy's HeaderCache constant. Importing
// internal/proxy is what this whole test package exists to avoid — it black-box
// tests the compiled binary — so the header name is duplicated here the same
// way the harness already duplicates HeaderCacheTTL's shape in its chat
// helpers, rather than importing the package to name one constant.
const cacheStatusHeader = "X-Switchyard-Cache"

// chatBodyWithContent is chatBody with the message content overridden, so two
// otherwise-identical requests can carry different prompts and stay distinct
// cache entries.
func chatBodyWithContent(model, content string) chatRequestBody {
	b := chatBody(model)
	b.Messages[0].Content = content
	return b
}

// cacheStatus issues one chat completion and returns the cache tier header
// alongside the response, so a caller can assert "miss" or "exact" without
// repeating the request/response plumbing at every call site.
func cacheStatus(t *testing.T, gw *gatewayInstance, apiKey string, body chatRequestBody) string {
	t.Helper()
	resp := postChat(t, gw, apiKey, body)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("chat completion: want 200, got %d", resp.StatusCode)
	}
	return resp.Header.Get(cacheStatusHeader)
}

// warmCache repeats the same request against apiKey until it comes back as an
// exact cache hit, proving the miss that preceded it has actually landed in
// Redis before the test moves on. storeInCache runs synchronously in the
// handler after the client's bytes are written, not in a background
// goroutine, but "the client already has its response" is not the same
// guarantee as "the next HTTP request I make happens after the server-side
// write" — polling for the self-hit is what closes that gap instead of
// guessing at a sleep.
func warmCache(t *testing.T, gw *gatewayInstance, apiKey string, body chatRequestBody) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cacheStatus(t, gw, apiKey, body) == "exact" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("own team's cache entry never became an exact hit within 2s")
}

// TestCacheIsolationAcrossOrgs is Step 4.3's headline case: org A and org B
// send the identical prompt, against the same provider and model, and neither
// may ever receive the other's cached answer. Tested in both orderings, since
// a scope bug could plausibly show up only when one particular org writes the
// entry first.
func TestCacheIsolationAcrossOrgs(t *testing.T) {
	requireRedis(t)

	orgB := uniqueID("org-b")
	teamA := uniqueID("team-a")
	teamAKey := "key-" + teamA
	prov := uniqueID("prov-shared")
	model := uniqueID("model-shared")

	up := newMockUpstream(t, prov)

	cachePath := filepath.Join(t.TempDir(), "cache.yaml")
	// semantic.enabled: false keeps this to the exact tier -- a plain Redis
	// HGETALL on the fingerprint -- with no embedder or external API key
	// needed. That tier alone is enough to prove or disprove scope isolation:
	// the two prompts below are byte-identical, so a semantic comparison adds
	// nothing this test needs.
	if err := os.WriteFile(cachePath, []byte("cache:\n  enabled: true\n  semantic:\n    enabled: false\n"), 0o644); err != nil {
		t.Fatalf("writing cache config: %v", err)
	}

	cfg := harnessConfig{
		providers: []providerSpec{{name: prov, url: up.URL(), models: []string{model}}},
		teams: []teamSpec{
			defaultTeam(teamA, teamAKey, []string{prov}, []string{model}),
		},
		env: map[string]string{"SWITCHYARD_CACHE_CONFIG": cachePath},
	}

	gw := startGateway(t, cfg, up)
	ctx := context.Background()

	// org B does not exist until its organisation row does; cmd/migrate's
	// seed only ever creates "personal", so org B is created directly here
	// the same way Step 4.1's fixture does it.
	conn, err := pgx.Connect(ctx, postgresDSN(gw.DB))
	if err != nil {
		t.Fatalf("connecting to test database: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `INSERT INTO organizations (id, name) VALUES ($1, $2)`, orgB, "Org B"); err != nil {
		t.Fatalf("creating organization %s: %v", orgB, err)
	}

	superadminCookie, superadminCSRF := adminSession(t)
	teamBKey := createTeamViaAdmin(t, gw, superadminCookie, superadminCSRF, uniqueID("team-b"), orgB, prov, model)

	t.Run("org A first, then org B", func(t *testing.T) {
		prompt := chatBodyWithContent(model, "identical prompt, org A first")

		if status := cacheStatus(t, gw, teamAKey, prompt); status != "miss" {
			t.Fatalf("org A's first request: want miss, got %q", status)
		}
		warmCache(t, gw, teamAKey, prompt)
		up.ResetHits()

		status := cacheStatus(t, gw, teamBKey, prompt)
		if status == "exact" {
			t.Fatal("org B received a cache hit for a prompt only org A had sent — cross-tenant cache leak")
		}
		if status != "miss" {
			t.Fatalf("org B's request for the same prompt: want miss, got %q", status)
		}
		if hits := up.Hits(); hits != 1 {
			t.Fatalf("org B's request should have reached the provider exactly once, got %d hits", hits)
		}
	})

	t.Run("org B first, then org A", func(t *testing.T) {
		prompt := chatBodyWithContent(model, "identical prompt, org B first")
		up.ResetHits()

		if status := cacheStatus(t, gw, teamBKey, prompt); status != "miss" {
			t.Fatalf("org B's first request: want miss, got %q", status)
		}
		warmCache(t, gw, teamBKey, prompt)
		up.ResetHits()

		status := cacheStatus(t, gw, teamAKey, prompt)
		if status == "exact" {
			t.Fatal("org A received a cache hit for a prompt only org B had sent — cross-tenant cache leak")
		}
		if status != "miss" {
			t.Fatalf("org A's request for the same prompt: want miss, got %q", status)
		}
		if hits := up.Hits(); hits != 1 {
			t.Fatalf("org A's request should have reached the provider exactly once, got %d hits", hits)
		}
	})

	t.Run("control: same org still hits its own cache", func(t *testing.T) {
		prompt := chatBodyWithContent(model, "same-org control prompt")
		up.ResetHits()

		if status := cacheStatus(t, gw, teamAKey, prompt); status != "miss" {
			t.Fatalf("first request: want miss, got %q", status)
		}
		warmCache(t, gw, teamAKey, prompt)
		if hits := up.Hits(); hits != 1 {
			t.Fatalf("warming the cache should have called the provider exactly once, got %d hits", hits)
		}

		up.ResetHits()
		if status := cacheStatus(t, gw, teamAKey, prompt); status != "exact" {
			t.Fatalf("repeating the same team's prompt: want exact, got %q", status)
		}
		if hits := up.Hits(); hits != 0 {
			t.Fatalf("a cache hit must not call the provider, got %d hits", hits)
		}
	})
}
