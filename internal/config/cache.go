package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/Harshalsharma05/switchyard/internal/cache"
)

// Cache is the validated result of loading configs/cache.yaml. It carries
// already-built cache package config so nothing downstream re-parses durations
// or reads the environment.
type Cache struct {
	Enabled bool

	Store cache.StoreConfig
	Embed cache.EmbedConfig
	Look  cache.LookupConfig
	TTL   cache.TTLPolicy
}

// --- on-disk shape ----------------------------------------------------------

type ttlRuleEntry struct {
	Name string `yaml:"name"`

	// TTL is a string so "0" and "0s" both mean never cache, and a malformed
	// value names the field instead of failing inside the YAML decoder.
	TTL      string   `yaml:"ttl"`
	Patterns []string `yaml:"patterns"`
}

type cacheFile struct {
	Cache cacheEntry `yaml:"cache"`
}

type cacheEntry struct {
	// Enabled defaults to true when absent, matching providers.yaml's
	// treatment of the same key.
	Enabled *bool `yaml:"enabled"`

	ReadTimeout  string `yaml:"read_timeout"`
	WriteTimeout string `yaml:"write_timeout"`

	TTL struct {
		Default string         `yaml:"default"`
		Max     string         `yaml:"max"`
		Rules   []ttlRuleEntry `yaml:"rules"`
	} `yaml:"ttl"`

	Semantic struct {
		Enabled       *bool   `yaml:"enabled"`
		Threshold     float32 `yaml:"threshold"`
		MaxCandidates int     `yaml:"max_candidates"`
	} `yaml:"semantic"`

	Embedding struct {
		BaseURL    string `yaml:"base_url"`
		APIKeyEnv  string `yaml:"api_key_env"`
		Model      string `yaml:"model"`
		Dimensions int    `yaml:"dimensions"`
		Timeout    string `yaml:"timeout"`
	} `yaml:"embedding"`
}

// LoadCache reads, validates, and resolves configs/cache.yaml.
//
// A missing file is not an error: it means the cache is off, which is the
// pre-Phase-7 behaviour and must stay a supported configuration.
func LoadCache(path string) (Cache, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Cache{Enabled: false}, nil
		}
		return Cache{}, fmt.Errorf("reading cache config: %w", err)
	}

	var file cacheFile
	if err := yaml.UnmarshalWithOptions(raw, &file, yaml.DisallowUnknownField()); err != nil {
		return Cache{}, fmt.Errorf("parsing %s:\n%s", path, yaml.FormatError(err, false, true))
	}

	e := file.Cache
	out := Cache{Enabled: enabledOrDefault(e.Enabled)}
	if !out.Enabled {
		return out, nil
	}

	readTimeout, err := parseCacheDuration(path, "read_timeout", e.ReadTimeout, 50*time.Millisecond)
	if err != nil {
		return Cache{}, err
	}
	writeTimeout, err := parseCacheDuration(path, "write_timeout", e.WriteTimeout, 500*time.Millisecond)
	if err != nil {
		return Cache{}, err
	}
	out.TTL, err = resolveTTLPolicy(path, e)
	if err != nil {
		return Cache{}, err
	}

	semanticOn := enabledOrDefault(e.Semantic.Enabled)

	if e.Semantic.MaxCandidates < 0 {
		return Cache{}, fmt.Errorf("%s: semantic.max_candidates must not be negative", path)
	}
	out.Store = cache.StoreConfig{
		MaxCandidates: e.Semantic.MaxCandidates,
		ReadTimeout:   readTimeout,
		WriteTimeout:  writeTimeout,
	}

	// A threshold outside [-1, 1] cannot be reached by a cosine similarity, so
	// it is a typo rather than an aggressive setting.
	if semanticOn && (e.Semantic.Threshold < -1 || e.Semantic.Threshold > 1) {
		return Cache{}, fmt.Errorf("%s: semantic.threshold must be between -1 and 1, got %v", path, e.Semantic.Threshold)
	}
	out.Look = cache.LookupConfig{
		SemanticEnabled: semanticOn,
		Threshold:       e.Semantic.Threshold,
	}

	if !semanticOn {
		return out, nil
	}

	timeout, err := parseCacheDuration(path, "embedding.timeout", e.Embedding.Timeout, 5*time.Second)
	if err != nil {
		return Cache{}, err
	}

	// The key is read here rather than inside internal/cache, so that package
	// never touches the environment and stays testable without one.
	apiKey := ""
	if e.Embedding.APIKeyEnv != "" {
		apiKey = os.Getenv(e.Embedding.APIKeyEnv)
		if apiKey == "" {
			return Cache{}, fmt.Errorf("%s: embedding.api_key_env %s is unset", path, e.Embedding.APIKeyEnv)
		}
	}

	out.Embed = cache.EmbedConfig{
		BaseURL:    e.Embedding.BaseURL,
		APIKey:     apiKey,
		Model:      e.Embedding.Model,
		Dimensions: e.Embedding.Dimensions,
		Timeout:    timeout,
	}
	return out, nil
}

// resolveTTLPolicy builds Step 7.4's lifetime policy.
//
// Rule order is preserved from the file: matching is first-wins, so the author
// controls precedence by writing the narrower rule higher up.
func resolveTTLPolicy(path string, e cacheEntry) (cache.TTLPolicy, error) {
	var out cache.TTLPolicy
	var err error

	out.Default, err = parseCacheDuration(path, "ttl.default", e.TTL.Default, time.Hour)
	if err != nil {
		return out, err
	}
	if out.Default <= 0 {
		return out, fmt.Errorf("%s: ttl.default must be positive", path)
	}

	out.Max, err = parseCacheDuration(path, "ttl.max", e.TTL.Max, 24*time.Hour)
	if err != nil {
		return out, err
	}
	if out.Max < out.Default {
		return out, fmt.Errorf("%s: ttl.max (%v) is below ttl.default (%v)", path, out.Max, out.Default)
	}

	seen := make(map[string]struct{}, len(e.TTL.Rules))
	for i, rule := range e.TTL.Rules {
		label := rule.Name
		if label == "" {
			return out, fmt.Errorf("%s: ttl.rules[%d] has no name", path, i)
		}
		if _, dup := seen[label]; dup {
			return out, fmt.Errorf("%s: duplicate ttl rule name %q", path, label)
		}
		seen[label] = struct{}{}

		if len(rule.Patterns) == 0 {
			return out, fmt.Errorf("%s: ttl rule %q has no patterns", path, label)
		}

		// A zero TTL is meaningful here — it is how a rule says "never cache" —
		// so the default is zero rather than the policy default.
		d, err := parseCacheDuration(path, "ttl.rules["+label+"].ttl", rule.TTL, 0)
		if err != nil {
			return out, err
		}
		if d < 0 {
			return out, fmt.Errorf("%s: ttl rule %q has a negative ttl", path, label)
		}

		// Lowercased once at load so the hot path never has to.
		patterns := make([]string, 0, len(rule.Patterns))
		for _, p := range rule.Patterns {
			if strings.TrimSpace(p) == "" {
				return out, fmt.Errorf("%s: ttl rule %q has an empty pattern", path, label)
			}
			patterns = append(patterns, strings.ToLower(p))
		}

		out.Rules = append(out.Rules, cache.TTLRule{Name: label, TTL: d, Patterns: patterns})
	}

	return out, nil
}

// enabledOrDefault mirrors providerEntry.enabled: a *bool so that an absent key
// means true, while an explicit false stays false.
func enabledOrDefault(b *bool) bool { return b == nil || *b }

func parseCacheDuration(path, field, value string, fallback time.Duration) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %s %q is not a duration: %w", path, field, value, err)
	}
	return d, nil
}
