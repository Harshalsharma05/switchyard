package config

import (
	"fmt"
	"os"

	"github.com/goccy/go-yaml"

	"github.com/Harshalsharma05/switchyard/internal/auth"
)

// --- on-disk shape ----------------------------------------------------------
//
// Unexported for the same reason providers.go's file types are: the layout of
// configs/teams.yaml is an implementation detail of this package.

type teamsFile struct {
	Teams []teamEntry `yaml:"teams"`
}

type teamEntry struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`

	// APIKeyHash is the SHA-256 hex digest of the team's key, generated once
	// out of band (see the comment atop configs/teams.yaml) and pasted in here
	// — never the plaintext key itself, since this file is committed to git.
	APIKeyHash string `yaml:"api_key_hash"`

	AllowedProviders []string `yaml:"allowed_providers"`
	AllowedModels    []string `yaml:"allowed_models"`

	RateLimits struct {
		RPM int `yaml:"rpm"`
		TPM int `yaml:"tpm"`
	} `yaml:"rate_limits"`

	MonthlyBudgetUSD float64 `yaml:"monthly_budget_usd"`

	Priority string `yaml:"priority"`
}

// --- loading ------------------------------------------------------------

// LoadTeams reads, validates, and resolves configs/teams.yaml.
//
// As with LoadProviders, every failure is fatal: a malformed teams file must
// prevent startup, not silently produce a registry missing an entry that only
// surfaces the first time that team's key 401s for the wrong reason.
func LoadTeams(path string) ([]auth.Team, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading team config: %w", err)
	}

	var file teamsFile
	if err := yaml.UnmarshalWithOptions(raw, &file, yaml.DisallowUnknownField()); err != nil {
		return nil, fmt.Errorf("parsing %s:\n%s", path, yaml.FormatError(err, false, true))
	}

	if len(file.Teams) == 0 {
		return nil, fmt.Errorf("%s: no teams defined", path)
	}

	teams := make([]auth.Team, 0, len(file.Teams))
	seenIDs := make(map[string]struct{}, len(file.Teams))
	seenHashes := make(map[string]string, len(file.Teams))

	for i, entry := range file.Teams {
		label := entry.ID
		if label == "" {
			label = fmt.Sprintf("teams[%d]", i)
		}

		if _, dup := seenIDs[entry.ID]; dup && entry.ID != "" {
			return nil, fmt.Errorf("%s: duplicate team id %q", path, entry.ID)
		}
		seenIDs[entry.ID] = struct{}{}

		team, err := entry.resolve(label)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}

		// Caught here rather than left for auth.NewRegistry so the error names
		// the file and both team IDs in one place, the same way model
		// collisions are caught in LoadProviders rather than in the registry.
		if owner, dup := seenHashes[team.KeyHash]; dup {
			return nil, fmt.Errorf("%s: teams %q and %q share the same api_key_hash", path, owner, team.ID)
		}
		seenHashes[team.KeyHash] = team.ID

		teams = append(teams, team)
	}

	return teams, nil
}

func (e teamEntry) resolve(label string) (auth.Team, error) {
	var zero auth.Team

	if e.ID == "" {
		return zero, fmt.Errorf("%s: id is required", label)
	}
	if e.Name == "" {
		return zero, fmt.Errorf("%s: name is required", label)
	}
	if !isSHA256Hex(e.APIKeyHash) {
		return zero, fmt.Errorf("%s: api_key_hash must be a 64-character lowercase hex SHA-256 digest", label)
	}
	if len(e.AllowedProviders) == 0 {
		return zero, fmt.Errorf("%s: at least one allowed provider is required", label)
	}
	if len(e.AllowedModels) == 0 {
		return zero, fmt.Errorf("%s: at least one allowed model is required", label)
	}
	if e.RateLimits.RPM <= 0 {
		return zero, fmt.Errorf("%s: rate_limits.rpm must be a positive integer", label)
	}
	if e.RateLimits.TPM <= 0 {
		return zero, fmt.Errorf("%s: rate_limits.tpm must be a positive integer", label)
	}
	if e.MonthlyBudgetUSD <= 0 {
		return zero, fmt.Errorf("%s: monthly_budget_usd must be positive", label)
	}

	var priority auth.Priority
	switch e.Priority {
	case string(auth.PriorityRealtime):
		priority = auth.PriorityRealtime
	case string(auth.PriorityBatch):
		priority = auth.PriorityBatch
	case "":
		return zero, fmt.Errorf("%s: priority is required (%s or %s)", label, auth.PriorityRealtime, auth.PriorityBatch)
	default:
		return zero, fmt.Errorf("%s: unknown priority %q (want %s or %s)",
			label, e.Priority, auth.PriorityRealtime, auth.PriorityBatch)
	}

	return auth.Team{
		ID:                  e.ID,
		Name:                e.Name,
		KeyHash:             e.APIKeyHash,
		AllowedProviders:    e.AllowedProviders,
		AllowedModels:       e.AllowedModels,
		RateLimits:          auth.RateLimits{RPM: e.RateLimits.RPM, TPM: e.RateLimits.TPM},
		MonthlyBudgetMicros: usdToMicros(e.MonthlyBudgetUSD),
		Priority:            priority,
	}, nil
}

// isSHA256Hex reports whether s looks like a SHA-256 digest: 64 lowercase hex
// characters. It catches a truncated hash, an accidentally-committed
// plaintext key, or an uppercased digest at startup instead of as a
// permanent, silent 401 for that team on every request.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
