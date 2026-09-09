package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Harshalsharma05/switchyard/internal/auth"
)

// Fixtures use an ollama-only provider entry throughout: it needs no
// api_key_env, so these tests need no environment variables set up around
// them — the same reason internal/config's own tests keep an ollama entry in
// their minimal fixtures.

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

const validProvidersA = `
providers:
  - name: ollama
    type: ollama
    base_url: http://localhost:11434
    timeout: 120s
    default_max_tokens: 512
    models:
      - name: llama3.2:3b
        input_per_1m_usd: 0.0
        output_per_1m_usd: 0.0
`

// validProvidersB differs from A only by adding a second model, so a
// reload's before/after is observable through the public registry API
// (ForModel) rather than by reaching into unexported state.
const validProvidersB = `
providers:
  - name: ollama
    type: ollama
    base_url: http://localhost:11434
    timeout: 120s
    default_max_tokens: 512
    models:
      - name: llama3.2:3b
        input_per_1m_usd: 0.0
        output_per_1m_usd: 0.0
      - name: llama3.2:1b
        input_per_1m_usd: 0.0
        output_per_1m_usd: 0.0
`

const invalidProviders = `
providers:
  - name: ollama
    type: bogus-type
    base_url: http://localhost:11434
    timeout: 120s
    default_max_tokens: 512
    models:
      - name: llama3.2:3b
`

func teamsYAML(t *testing.T, ids ...string) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("teams:\n")
	for _, id := range ids {
		sb.WriteString("  - id: " + id + "\n")
		sb.WriteString("    name: " + id + "\n")
		sb.WriteString("    api_key_hash: " + auth.HashKey(id+"-key") + "\n")
		sb.WriteString("    allowed_providers: [ollama]\n")
		sb.WriteString("    allowed_models: [llama3.2:3b]\n")
		sb.WriteString("    rate_limits: {rpm: 60, tpm: 100000}\n")
		sb.WriteString("    monthly_budget_usd: 50.00\n")
		sb.WriteString("    priority: realtime\n")
	}
	return sb.String()
}

const invalidTeams = `
teams:
  - id: acme
    name: Acme Corp
    api_key_hash: not-a-valid-sha256-hash
    allowed_providers: [ollama]
    allowed_models: [llama3.2:3b]
    rate_limits: {rpm: 60, tpm: 100000}
    monthly_budget_usd: 50.00
    priority: realtime
`

// fakeTeams adapts an in-memory auth.Registry to the teamStore interface. The
// real store persists to Postgres; nothing in this file is testing that, so the
// registry's own semantics are exactly the right stand-in.
type fakeTeams struct{ reg *auth.Registry }

func newFakeTeams(t *testing.T, ids ...string) *fakeTeams {
	t.Helper()
	teams := make([]auth.Team, 0, len(ids))
	for _, id := range ids {
		teams = append(teams, auth.Team{
			ID: id, Name: id, KeyHash: auth.HashKey(id + "-key"),
			AllowedProviders: []string{"ollama"}, AllowedModels: []string{"llama3.2:3b"},
			RateLimits: auth.RateLimits{RPM: 60, TPM: 1000}, MonthlyBudgetMicros: 1_000_000,
			Priority: auth.PriorityRealtime, KeySource: auth.KeySourceConfig,
		})
	}
	reg, err := auth.NewRegistry(teams)
	if err != nil {
		t.Fatalf("building fake team registry: %v", err)
	}
	return &fakeTeams{reg: reg}
}

func (f *fakeTeams) Authenticate(k string) (*auth.Team, error) { return f.reg.Authenticate(k) }
func (f *fakeTeams) List() []auth.Team                         { return f.reg.List() }
func (f *fakeTeams) Get(id string) (auth.Team, error)          { return f.reg.Get(id) }
func (f *fakeTeams) Update(_ context.Context, id string, p auth.TeamPatch) (auth.Team, error) {
	return f.reg.Update(id, p)
}
func (f *fakeTeams) RotateKey(_ context.Context, id, h, m string) (auth.Team, error) {
	return f.reg.RotateKey(id, h, m)
}
func (f *fakeTeams) RevokeKey(_ context.Context, id string) (auth.Team, error) {
	return f.reg.RevokeKey(id)
}

func TestLoadLiveConfigValid(t *testing.T) {
	dir := t.TempDir()
	providersPath := writeFile(t, dir, "providers.yaml", validProvidersA)

	live, providerCount, err := loadLiveConfig(providersPath)
	if err != nil {
		t.Fatalf("loadLiveConfig: %v", err)
	}
	if providerCount != 1 {
		t.Errorf("providerCount = %d, want 1", providerCount)
	}

	if _, err := live.registry.ForModel("llama3.2:3b"); err != nil {
		t.Errorf("registry does not resolve llama3.2:3b: %v", err)
	}
	if cost, err := live.calc.Cost("llama3.2:3b", 100, 100); err != nil || cost != 0 {
		t.Errorf("calc.Cost = (%d, %v), want (0, nil) for a free model", cost, err)
	}
}

func TestLoadLiveConfigInvalidProvidersErrors(t *testing.T) {
	dir := t.TempDir()
	providersPath := writeFile(t, dir, "providers.yaml", invalidProviders)

	if _, _, err := loadLiveConfig(providersPath); err == nil {
		t.Fatal("loadLiveConfig succeeded against an invalid providers.yaml")
	}
}

// --- configStore delegation -------------------------------------------------

func TestConfigStoreDelegatesToCurrent(t *testing.T) {
	dir := t.TempDir()
	providersPath := writeFile(t, dir, "providers.yaml", validProvidersA)

	live, _, err := loadLiveConfig(providersPath)
	if err != nil {
		t.Fatalf("loadLiveConfig: %v", err)
	}
	store := newConfigStore(live, newFakeTeams(t, "acme"))

	if _, err := store.ForModel("llama3.2:3b"); err != nil {
		t.Errorf("ForModel: %v", err)
	}
	if _, ok := store.DefaultMaxTokensFor("llama3.2:3b"); !ok {
		t.Error("DefaultMaxTokensFor: not found")
	}
	if _, err := store.Authenticate("acme-key"); err != nil {
		t.Errorf("Authenticate: %v", err)
	}
	if _, err := store.Cost("llama3.2:3b", 10, 10); err != nil {
		t.Errorf("Cost: %v", err)
	}
	if len(store.List()) != 1 {
		t.Errorf("List() = %d teams, want 1", len(store.List()))
	}
	if _, err := store.Get("acme"); err != nil {
		t.Errorf("Get: %v", err)
	}
	rpm := 100
	if _, err := store.Update(context.Background(), "acme", auth.TeamPatch{RPM: &rpm}); err != nil {
		t.Errorf("Update: %v", err)
	}
	if len(store.Configs()) != 1 {
		t.Errorf("Configs() = %d, want 1", len(store.Configs()))
	}
}

// --- reload: the Step 4.4 checklist's central claims ------------------------

func TestReloadSwapsInNewConfigOnSuccess(t *testing.T) {
	dir := t.TempDir()
	providersPath := writeFile(t, dir, "providers.yaml", validProvidersA)

	live, _, err := loadLiveConfig(providersPath)
	if err != nil {
		t.Fatalf("loadLiveConfig: %v", err)
	}
	store := newConfigStore(live, newFakeTeams(t, "acme"))

	// Confirm the "before" state: the second model isn't there yet.
	if _, err := store.ForModel("llama3.2:1b"); err == nil {
		t.Fatal("llama3.2:1b already resolves before the reload; fixture setup is wrong")
	}

	// Rewrite providers.yaml on disk (same path) to the B fixture and reload.
	writeFile(t, dir, "providers.yaml", validProvidersB)
	reload := newReloader(store, providersPath)

	summary, err := reload(context.Background())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if summary.Providers != 1 || summary.Teams != 1 {
		t.Errorf("summary = %+v, want {Providers:1 Teams:1}", summary)
	}

	if _, err := store.ForModel("llama3.2:1b"); err != nil {
		t.Errorf("llama3.2:1b does not resolve after reload: %v — the swap did not take effect", err)
	}
}

// The checklist's own words: "Invalid config reload is rejected, gateway
// keeps running on the old config." This is the test that actually proves
// it, against the real configStore, not just against the HTTP handler's
// error-formatting (admin's own TestReloadRejectedIs400 covers that half).
func TestReloadRejectedLeavesStoreOnOldConfig(t *testing.T) {
	dir := t.TempDir()
	providersPath := writeFile(t, dir, "providers.yaml", validProvidersA)

	live, _, err := loadLiveConfig(providersPath)
	if err != nil {
		t.Fatalf("loadLiveConfig: %v", err)
	}
	store := newConfigStore(live, newFakeTeams(t, "acme"))

	// Break providers.yaml on disk and attempt a reload.
	writeFile(t, dir, "providers.yaml", invalidProviders)
	reload := newReloader(store, providersPath)

	if _, err := reload(context.Background()); err == nil {
		t.Fatal("reload succeeded against an invalid providers.yaml")
	}

	// The store must still resolve exactly what it did before the failed
	// attempt — nothing was touched.
	if _, err := store.ForModel("llama3.2:3b"); err != nil {
		t.Errorf("llama3.2:3b no longer resolves after a rejected reload: %v", err)
	}
	if _, err := store.Authenticate("acme-key"); err != nil {
		t.Errorf("acme-key no longer authenticates after a rejected reload: %v", err)
	}
}

// The Phase 0.1 before-state, inverted. A reload used to rebuild the team
// registry from teams.yaml, silently reverting any limit edit or key rotation
// made through the admin API. Teams now live in Postgres and reload does not
// read them at all, so the sequence that used to revert must no longer do so.
func TestReloadDoesNotRevertTeamChanges(t *testing.T) {
	dir := t.TempDir()
	providersPath := writeFile(t, dir, "providers.yaml", validProvidersA)

	live, _, err := loadLiveConfig(providersPath)
	if err != nil {
		t.Fatalf("loadLiveConfig: %v", err)
	}
	store := newConfigStore(live, newFakeTeams(t, "acme"))

	ctx := context.Background()
	rpm := 999
	if _, err := store.Update(ctx, "acme", auth.TeamPatch{RPM: &rpm}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	rotated := "acme-rotated-key"
	if _, err := store.RotateKey(ctx, "acme", auth.HashKey(rotated), auth.MaskKey(rotated)); err != nil {
		t.Fatalf("RotateKey: %v", err)
	}

	writeFile(t, dir, "providers.yaml", validProvidersB)
	if _, err := newReloader(store, providersPath)(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// The provider half of the reload still works.
	if _, err := store.ForModel("llama3.2:1b"); err != nil {
		t.Errorf("llama3.2:1b does not resolve after reload: %v", err)
	}

	team, err := store.Get("acme")
	if err != nil {
		t.Fatalf("Get after reload: %v", err)
	}
	if team.RateLimits.RPM != rpm {
		t.Errorf("rpm = %d after reload, want %d — reload reverted a limit edit", team.RateLimits.RPM, rpm)
	}
	if _, err := store.Authenticate(rotated); err != nil {
		t.Errorf("rotated key stopped working after reload: %v", err)
	}
	if _, err := store.Authenticate("acme-key"); err == nil {
		t.Error("the pre-rotation key authenticates again after reload — reload resurrected it")
	}
}
