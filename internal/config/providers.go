// Package config loads and validates SwitchYard's YAML configuration.
//
// It is the only package that knows the on-disk file format. Everything
// downstream receives already-validated structs, so a schema change here never
// reaches an adapter.
package config

import (
	"fmt"
	"math"
	"os"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// microsPerUSD is the fixed-point scale for money.
//
// Costs are held as integer micro-dollars because Phase 4 accumulates them over
// thousands of requests, and repeated float addition drifts. Prices are authored
// as readable decimals in YAML and converted exactly once, here.
const microsPerUSD = 1_000_000

// ModelPricing is the cost of a model in micro-dollars per 1,000,000 tokens.
type ModelPricing struct {
	InputPer1M  int64
	OutputPer1M int64
}

// Providers is the validated result of loading configs/providers.yaml.
//
// Pricing is kept out of provider.Config on purpose: internal/provider
// translates requests and calls APIs, and knows nothing about money. Phase 4's
// budget accounting reads pricing from here instead.
type Providers struct {
	// Configs holds the enabled instances, in file order.
	Configs []provider.Config

	// Pricing is keyed by model name, which is unambiguous because a model may
	// be served by exactly one instance.
	Pricing map[string]ModelPricing
}

// --- on-disk shape ----------------------------------------------------------
//
// These types are unexported: the file layout is an implementation detail of
// this package.

type providersFile struct {
	Providers []providerEntry `yaml:"providers"`
}

type providerEntry struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`

	// Enabled defaults to true when the key is absent — see decodeEnabled. An
	// entry can therefore stay in the file, documented and validated, while its
	// credential is unavailable.
	Enabled *bool `yaml:"enabled"`

	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`

	// Timeout is a string parsed with time.ParseDuration rather than decoded
	// directly, so "30" instead of "30s" produces a message naming the field
	// instead of a type error from inside the YAML library.
	Timeout string `yaml:"timeout"`

	DefaultMaxTokens int    `yaml:"default_max_tokens"`
	PingModel        string `yaml:"ping_model"`

	Models []modelEntry `yaml:"models"`
}

type modelEntry struct {
	Name         string  `yaml:"name"`
	InputPer1MU  float64 `yaml:"input_per_1m_usd"`
	OutputPer1MU float64 `yaml:"output_per_1m_usd"`
}

// --- loading ----------------------------------------------------------------

// LoadProviders reads, validates, and resolves configs/providers.yaml.
//
// Every failure is fatal. The plan's rule is that a malformed config must
// prevent startup rather than produce a half-built registry, so this returns an
// error at the first problem and names the field responsible.
func LoadProviders(path string) (*Providers, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading provider config: %w", err)
	}

	var file providersFile
	// DisallowUnknownField turns a typo like "base_ur:" into a startup failure
	// instead of a silently empty field that fails on the first request.
	if err := yaml.UnmarshalWithOptions(raw, &file, yaml.DisallowUnknownField()); err != nil {
		return nil, fmt.Errorf("parsing %s:\n%s", path, yaml.FormatError(err, false, true))
	}

	if len(file.Providers) == 0 {
		return nil, fmt.Errorf("%s: no providers defined", path)
	}

	out := &Providers{
		Configs: make([]provider.Config, 0, len(file.Providers)),
		Pricing: make(map[string]ModelPricing),
	}

	seenNames := make(map[string]struct{}, len(file.Providers))
	modelOwner := make(map[string]string)

	for i, entry := range file.Providers {
		// Identify the entry by name where possible, by index where the name is
		// what is missing.
		label := entry.Name
		if label == "" {
			label = fmt.Sprintf("providers[%d]", i)
		}

		if _, dup := seenNames[entry.Name]; dup && entry.Name != "" {
			return nil, fmt.Errorf("%s: duplicate provider name %q", path, entry.Name)
		}
		seenNames[entry.Name] = struct{}{}

		cfg, pricing, err := entry.resolve(label)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}

		// Model uniqueness is enforced across every entry, enabled or not, so
		// that toggling `enabled` can never introduce an ambiguity that was
		// invisible while the entry was off.
		for model := range pricing {
			if owner, dup := modelOwner[model]; dup {
				return nil, fmt.Errorf(
					"%s: model %q is listed under both %q and %q; a model must map to exactly one provider",
					path, model, owner, label,
				)
			}
			modelOwner[model] = label
		}

		if !entry.enabled() {
			// Validated but not constructed. Its models stay reserved above so a
			// later enable cannot collide.
			continue
		}

		for model, p := range pricing {
			out.Pricing[model] = p
		}
		out.Configs = append(out.Configs, cfg)
	}

	if len(out.Configs) == 0 {
		return nil, fmt.Errorf("%s: every provider is disabled; the gateway would have nothing to route to", path)
	}

	return out, nil
}

// enabled reports the entry's enabled state, defaulting to true when the key is
// absent. The field is a *bool precisely so "absent" and "false" are
// distinguishable — a plain bool would make every entry default to disabled.
func (e providerEntry) enabled() bool {
	return e.Enabled == nil || *e.Enabled
}

// resolve validates one entry and converts it into a provider.Config plus its
// pricing table.
func (e providerEntry) resolve(label string) (provider.Config, map[string]ModelPricing, error) {
	var zero provider.Config

	if e.Name == "" {
		return zero, nil, fmt.Errorf("%s: name is required", label)
	}
	if e.BaseURL == "" {
		return zero, nil, fmt.Errorf("%s: base_url is required", label)
	}
	if e.DefaultMaxTokens <= 0 {
		return zero, nil, fmt.Errorf("%s: default_max_tokens must be a positive integer", label)
	}
	if len(e.Models) == 0 {
		return zero, nil, fmt.Errorf("%s: at least one model must be listed", label)
	}

	switch e.Type {
	case provider.TypeOpenAICompatible, provider.TypeAnthropic, provider.TypeGemini, provider.TypeOllama:
	case "":
		return zero, nil, fmt.Errorf("%s: type is required", label)
	default:
		return zero, nil, fmt.Errorf(
			"%s: unknown type %q (want one of %s, %s, %s, %s)",
			label, e.Type,
			provider.TypeOpenAICompatible, provider.TypeAnthropic,
			provider.TypeGemini, provider.TypeOllama,
		)
	}

	if e.Timeout == "" {
		return zero, nil, fmt.Errorf("%s: timeout is required (for example \"30s\")", label)
	}
	timeout, err := time.ParseDuration(e.Timeout)
	if err != nil {
		return zero, nil, fmt.Errorf("%s: timeout %q is not a duration (for example \"30s\"): %w", label, e.Timeout, err)
	}
	if timeout <= 0 {
		return zero, nil, fmt.Errorf("%s: timeout must be positive, got %s", label, timeout)
	}

	models := make([]string, 0, len(e.Models))
	pricing := make(map[string]ModelPricing, len(e.Models))
	seen := make(map[string]struct{}, len(e.Models))

	for j, m := range e.Models {
		if m.Name == "" {
			return zero, nil, fmt.Errorf("%s: models[%d] has no name", label, j)
		}
		if _, dup := seen[m.Name]; dup {
			return zero, nil, fmt.Errorf("%s: model %q listed twice", label, m.Name)
		}
		seen[m.Name] = struct{}{}

		if m.InputPer1MU < 0 || m.OutputPer1MU < 0 {
			return zero, nil, fmt.Errorf("%s: model %q has a negative price", label, m.Name)
		}

		models = append(models, m.Name)
		pricing[m.Name] = ModelPricing{
			InputPer1M:  usdToMicros(m.InputPer1MU),
			OutputPer1M: usdToMicros(m.OutputPer1MU),
		}
	}

	if e.PingModel != "" {
		if _, ok := seen[e.PingModel]; !ok {
			return zero, nil, fmt.Errorf("%s: ping_model %q is not in this provider's model list", label, e.PingModel)
		}
	}

	// Credentials are resolved only for entries that will actually be built.
	// A disabled entry with an unset key is the normal state of a provider
	// waiting on a paid account, not a config error.
	var apiKey string
	if e.enabled() {
		apiKey, err = resolveAPIKey(label, e.Type, e.APIKeyEnv)
		if err != nil {
			return zero, nil, err
		}
	}

	return provider.Config{
		Name:             e.Name,
		Type:             e.Type,
		BaseURL:          e.BaseURL,
		APIKey:           apiKey,
		Timeout:          timeout,
		Models:           models,
		DefaultMaxTokens: e.DefaultMaxTokens,
		PingModel:        e.PingModel,
	}, pricing, nil
}

// resolveAPIKey reads the credential from the environment.
//
// The config names an environment variable rather than holding the secret, so
// configs/ stays committable. Ollama is exempt because it takes no credential;
// for everything else a missing or empty variable fails startup, since the
// alternative is a provider that 401s on every request until someone reads the
// logs.
func resolveAPIKey(label, providerType, envVar string) (string, error) {
	if providerType == provider.TypeOllama {
		return "", nil
	}
	if envVar == "" {
		return "", fmt.Errorf("%s: api_key_env is required for type %q", label, providerType)
	}

	key := os.Getenv(envVar)
	if key == "" {
		return "", fmt.Errorf(
			"%s: environment variable %s is unset or empty; set it, or mark this provider `enabled: false`",
			label, envVar,
		)
	}
	return key, nil
}

// usdToMicros converts a decimal price into integer micro-dollars. Rounding
// happens here, once, and never again.
func usdToMicros(usd float64) int64 {
	return int64(math.Round(usd * microsPerUSD))
}
