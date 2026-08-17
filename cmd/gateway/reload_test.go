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

func TestLoadLiveConfigValid(t *testing.T) {
	dir := t.TempDir()
	providersPath := writeFile(t, dir, "providers.yaml", validProvidersA)
	teamsPath := writeFile(t, dir, "teams.yaml", teamsYAML(t, "acme", "globex"))

	live, providerCount, teamCount, err := loadLiveConfig(providersPath, teamsPath)
	if err != nil {
		t.Fatalf("loadLiveConfig: %v", err)
	}
	if providerCount != 1 || teamCount != 2 {
		t.Errorf("counts = (%d, %d), want (1, 2)", providerCount, teamCount)
	}

	if _, err := live.registry.ForModel("llama3.2:3b"); err != nil {
		t.Errorf("registry does not resolve llama3.2:3b: %v", err)
	}
	if _, err := live.authRegistry.Authenticate("acme-key"); err != nil {
		t.Errorf("authRegistry does not resolve acme-key: %v", err)
	}
	if cost, err := live.calc.Cost("llama3.2:3b", 100, 100); err != nil || cost != 0 {
		t.Errorf("calc.Cost = (%d, %v), want (0, nil) for a free model", cost, err)
	}
}

func TestLoadLiveConfigInvalidProvidersErrors(t *testing.T) {
	dir := t.TempDir()
	providersPath := writeFile(t, dir, "providers.yaml", invalidProviders)
	teamsPath := writeFile(t, dir, "teams.yaml", teamsYAML(t, "acme"))

	if _, _, _, err := loadLiveConfig(providersPath, teamsPath); err == nil {
		t.Fatal("loadLiveConfig succeeded against an invalid providers.yaml")
	}
}

func TestLoadLiveConfigInvalidTeamsErrors(t *testing.T) {
	dir := t.TempDir()
	providersPath := writeFile(t, dir, "providers.yaml", validProvidersA)
	teamsPath := writeFile(t, dir, "teams.yaml", invalidTeams)

	if _, _, _, err := loadLiveConfig(providersPath, teamsPath); err == nil {
		t.Fatal("loadLiveConfig succeeded against an invalid teams.yaml")
	}
}

// --- configStore delegation -------------------------------------------------

func TestConfigStoreDelegatesToCurrent(t *testing.T) {
	dir := t.TempDir()
	providersPath := writeFile(t, dir, "providers.yaml", validProvidersA)
	teamsPath := writeFile(t, dir, "teams.yaml", teamsYAML(t, "acme"))

	live, _, _, err := loadLiveConfig(providersPath, teamsPath)
	if err != nil {
		t.Fatalf("loadLiveConfig: %v", err)
	}
	store := newConfigStore(live)

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
	if _, err := store.Update("acme", auth.TeamPatch{RPM: &rpm}); err != nil {
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
	teamsPath := writeFile(t, dir, "teams.yaml", teamsYAML(t, "acme"))

	live, _, _, err := loadLiveConfig(providersPath, teamsPath)
	if err != nil {
		t.Fatalf("loadLiveConfig: %v", err)
	}
	store := newConfigStore(live)

	// Confirm the "before" state: the second model isn't there yet.
	if _, err := store.ForModel("llama3.2:1b"); err == nil {
		t.Fatal("llama3.2:1b already resolves before the reload; fixture setup is wrong")
	}

	// Rewrite providers.yaml on disk (same path) to the B fixture and reload.
	writeFile(t, dir, "providers.yaml", validProvidersB)
	reload := newReloader(store, providersPath, teamsPath)

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
	teamsPath := writeFile(t, dir, "teams.yaml", teamsYAML(t, "acme"))

	live, _, _, err := loadLiveConfig(providersPath, teamsPath)
	if err != nil {
		t.Fatalf("loadLiveConfig: %v", err)
	}
	store := newConfigStore(live)

	// Break providers.yaml on disk and attempt a reload.
	writeFile(t, dir, "providers.yaml", invalidProviders)
	reload := newReloader(store, providersPath, teamsPath)

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

// A reload is all-or-nothing across *both* files: a valid providers.yaml
// paired with a broken teams.yaml must reject the whole attempt, not swap
// in a new provider registry alongside the stale team registry.
func TestReloadRejectedTeamsLeavesBothFilesOnOldConfig(t *testing.T) {
	dir := t.TempDir()
	providersPath := writeFile(t, dir, "providers.yaml", validProvidersA)
	teamsPath := writeFile(t, dir, "teams.yaml", teamsYAML(t, "acme"))

	live, _, _, err := loadLiveConfig(providersPath, teamsPath)
	if err != nil {
		t.Fatalf("loadLiveConfig: %v", err)
	}
	store := newConfigStore(live)

	writeFile(t, dir, "providers.yaml", validProvidersB) // this half is fine
	writeFile(t, dir, "teams.yaml", invalidTeams)        // this half is not
	reload := newReloader(store, providersPath, teamsPath)

	if _, err := reload(context.Background()); err == nil {
		t.Fatal("reload succeeded despite an invalid teams.yaml")
	}

	// The provider side must not have been swapped in either, even though
	// providers.yaml itself was valid — the bundle is atomic.
	if _, err := store.ForModel("llama3.2:1b"); err == nil {
		t.Fatal("llama3.2:1b resolves after a reload that should have been rejected wholesale")
	}
}
