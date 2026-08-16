package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Harshalsharma05/switchyard/internal/auth"
)

func writeTeamsConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "teams.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return path
}

const validHashA = "2ce25520782abab77e9a1fde48ff35112740f6bc078b28154dec3d32651b854b"
const validHashB = "dd06a2a0a097eb98b0bb25130211d5f748218c5b2c27397b2f853a11a3ff9577"

const validTeamsConfig = `
teams:
  - id: acme
    name: Acme Corp
    api_key_hash: ` + validHashA + `
    allowed_providers: [groq, ollama]
    allowed_models: [openai/gpt-oss-120b, llama3.2:3b]
    rate_limits: {rpm: 60, tpm: 100000}
    monthly_budget_usd: 50.00
    priority: realtime
  - id: globex
    name: Globex Inc
    api_key_hash: ` + validHashB + `
    allowed_providers: [ollama]
    allowed_models: [llama3.2:3b]
    rate_limits: {rpm: 10, tpm: 20000}
    monthly_budget_usd: 5.00
    priority: batch
`

func TestLoadTeamsValid(t *testing.T) {
	got, err := LoadTeams(writeTeamsConfig(t, validTeamsConfig))
	if err != nil {
		t.Fatalf("LoadTeams: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("loaded %d teams, want 2", len(got))
	}

	// File order is preserved, same reasoning as provider config order.
	if got[0].ID != "acme" || got[1].ID != "globex" {
		t.Errorf("order = %q, %q; want file order", got[0].ID, got[1].ID)
	}

	acme := got[0]
	if acme.Name != "Acme Corp" {
		t.Errorf("Name = %q, want Acme Corp", acme.Name)
	}
	if acme.KeyHash != validHashA {
		t.Errorf("KeyHash = %q, want %q", acme.KeyHash, validHashA)
	}
	if len(acme.AllowedProviders) != 2 || len(acme.AllowedModels) != 2 {
		t.Errorf("allowlists = %+v / %+v, want 2 entries each", acme.AllowedProviders, acme.AllowedModels)
	}
	if acme.RateLimits != (auth.RateLimits{RPM: 60, TPM: 100000}) {
		t.Errorf("RateLimits = %+v, want {60 100000}", acme.RateLimits)
	}
	if acme.Priority != auth.PriorityRealtime {
		t.Errorf("Priority = %q, want realtime", acme.Priority)
	}
}

// Budget is authored as a decimal and stored as integer micro-dollars, same
// reasoning as provider pricing: float addition drifts over thousands of
// requests.
func TestTeamBudgetConvertsToMicroDollars(t *testing.T) {
	got, err := LoadTeams(writeTeamsConfig(t, validTeamsConfig))
	if err != nil {
		t.Fatalf("LoadTeams: %v", err)
	}

	if got[0].MonthlyBudgetMicros != 50_000_000 {
		t.Errorf("acme budget = %d micros, want 50_000_000 (50.00 USD)", got[0].MonthlyBudgetMicros)
	}
	if got[1].MonthlyBudgetMicros != 5_000_000 {
		t.Errorf("globex budget = %d micros, want 5_000_000 (5.00 USD)", got[1].MonthlyBudgetMicros)
	}
}

func TestLoadTeamsRejects(t *testing.T) {
	tests := map[string]struct {
		yaml     string
		wantText string
	}{
		"missing id": {
			yaml: `
teams:
  - name: Acme
    api_key_hash: ` + validHashA + `
    allowed_providers: [groq]
    allowed_models: [m]
    rate_limits: {rpm: 1, tpm: 1}
    monthly_budget_usd: 1
    priority: realtime
`,
			wantText: "id is required",
		},
		"missing name": {
			yaml: `
teams:
  - id: acme
    api_key_hash: ` + validHashA + `
    allowed_providers: [groq]
    allowed_models: [m]
    rate_limits: {rpm: 1, tpm: 1}
    monthly_budget_usd: 1
    priority: realtime
`,
			wantText: "name is required",
		},
		"short hash": {
			yaml: `
teams:
  - id: acme
    name: Acme
    api_key_hash: deadbeef
    allowed_providers: [groq]
    allowed_models: [m]
    rate_limits: {rpm: 1, tpm: 1}
    monthly_budget_usd: 1
    priority: realtime
`,
			wantText: "64-character",
		},
		"uppercase hash": {
			yaml: `
teams:
  - id: acme
    name: Acme
    api_key_hash: ` + strings.ToUpper(validHashA) + `
    allowed_providers: [groq]
    allowed_models: [m]
    rate_limits: {rpm: 1, tpm: 1}
    monthly_budget_usd: 1
    priority: realtime
`,
			wantText: "64-character",
		},
		"no allowed providers": {
			yaml: `
teams:
  - id: acme
    name: Acme
    api_key_hash: ` + validHashA + `
    allowed_providers: []
    allowed_models: [m]
    rate_limits: {rpm: 1, tpm: 1}
    monthly_budget_usd: 1
    priority: realtime
`,
			wantText: "at least one allowed provider",
		},
		"no allowed models": {
			yaml: `
teams:
  - id: acme
    name: Acme
    api_key_hash: ` + validHashA + `
    allowed_providers: [groq]
    allowed_models: []
    rate_limits: {rpm: 1, tpm: 1}
    monthly_budget_usd: 1
    priority: realtime
`,
			wantText: "at least one allowed model",
		},
		"zero rpm": {
			yaml: `
teams:
  - id: acme
    name: Acme
    api_key_hash: ` + validHashA + `
    allowed_providers: [groq]
    allowed_models: [m]
    rate_limits: {rpm: 0, tpm: 1}
    monthly_budget_usd: 1
    priority: realtime
`,
			wantText: "rate_limits.rpm",
		},
		"zero tpm": {
			yaml: `
teams:
  - id: acme
    name: Acme
    api_key_hash: ` + validHashA + `
    allowed_providers: [groq]
    allowed_models: [m]
    rate_limits: {rpm: 1, tpm: 0}
    monthly_budget_usd: 1
    priority: realtime
`,
			wantText: "rate_limits.tpm",
		},
		"zero budget": {
			yaml: `
teams:
  - id: acme
    name: Acme
    api_key_hash: ` + validHashA + `
    allowed_providers: [groq]
    allowed_models: [m]
    rate_limits: {rpm: 1, tpm: 1}
    monthly_budget_usd: 0
    priority: realtime
`,
			wantText: "monthly_budget_usd",
		},
		"missing priority": {
			yaml: `
teams:
  - id: acme
    name: Acme
    api_key_hash: ` + validHashA + `
    allowed_providers: [groq]
    allowed_models: [m]
    rate_limits: {rpm: 1, tpm: 1}
    monthly_budget_usd: 1
`,
			wantText: "priority is required",
		},
		"unknown priority": {
			yaml: `
teams:
  - id: acme
    name: Acme
    api_key_hash: ` + validHashA + `
    allowed_providers: [groq]
    allowed_models: [m]
    rate_limits: {rpm: 1, tpm: 1}
    monthly_budget_usd: 1
    priority: urgent
`,
			wantText: "unknown priority",
		},
		"typo in a field name": {
			yaml: `
teams:
  - id: acme
    name: Acme
    api_key_hsh: ` + validHashA + `
    allowed_providers: [groq]
    allowed_models: [m]
    rate_limits: {rpm: 1, tpm: 1}
    monthly_budget_usd: 1
    priority: realtime
`,
			wantText: "api_key_hsh",
		},
		"duplicate team id": {
			yaml: `
teams:
  - id: acme
    name: Acme
    api_key_hash: ` + validHashA + `
    allowed_providers: [groq]
    allowed_models: [m]
    rate_limits: {rpm: 1, tpm: 1}
    monthly_budget_usd: 1
    priority: realtime
  - id: acme
    name: Acme Duplicate
    api_key_hash: ` + validHashB + `
    allowed_providers: [groq]
    allowed_models: [m]
    rate_limits: {rpm: 1, tpm: 1}
    monthly_budget_usd: 1
    priority: realtime
`,
			wantText: "duplicate team id",
		},
		"duplicate key hash": {
			yaml: `
teams:
  - id: acme
    name: Acme
    api_key_hash: ` + validHashA + `
    allowed_providers: [groq]
    allowed_models: [m]
    rate_limits: {rpm: 1, tpm: 1}
    monthly_budget_usd: 1
    priority: realtime
  - id: globex
    name: Globex
    api_key_hash: ` + validHashA + `
    allowed_providers: [groq]
    allowed_models: [m]
    rate_limits: {rpm: 1, tpm: 1}
    monthly_budget_usd: 1
    priority: realtime
`,
			wantText: "share the same api_key_hash",
		},
		"empty file": {
			yaml:     "teams: []\n",
			wantText: "no teams defined",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := LoadTeams(writeTeamsConfig(t, tc.yaml))
			if err == nil {
				t.Fatal("load succeeded, want startup to fail on a broken config")
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error = %v\nwant it to mention %q", err, tc.wantText)
			}
		})
	}
}

func TestLoadTeamsMissingFile(t *testing.T) {
	_, err := LoadTeams(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil {
		t.Fatal("load succeeded on a missing file")
	}
	if !strings.Contains(err.Error(), "reading team config") {
		t.Errorf("error = %v, want it to name the operation that failed", err)
	}
}

// The committed configs/teams.yaml must itself be valid, or a fresh clone
// fails at boot.
func TestCommittedTeamsConfigIsValid(t *testing.T) {
	got, err := LoadTeams(filepath.Join("..", "..", "configs", "teams.yaml"))
	if err != nil {
		t.Fatalf("committed configs/teams.yaml is invalid: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("committed config defined no teams")
	}
}
