package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/Harshalsharma05/switchyard/internal/router"
)

// Router is the validated result of loading configs/router.yaml. It carries an
// already-built router config so nothing downstream re-parses this file.
type Router struct {
	Enabled    bool
	Policy     router.Policy
	Classifier router.ClassifierConfig
}

// --- on-disk shape ----------------------------------------------------------

type routerFile struct {
	Router routerEntry `yaml:"router"`
}

type routerEntry struct {
	// Enabled defaults to true when absent, matching cache.yaml and
	// providers.yaml's treatment of the same key.
	Enabled    *bool           `yaml:"enabled"`
	Policy     policyEntry     `yaml:"policy"`
	Classifier classifierEntry `yaml:"classifier"`
}

// policyEntry names a providers.yaml tier per complexity level. That the
// named tiers exist is checked at startup, where providers.yaml is in scope.
type policyEntry struct {
	Simple  string `yaml:"simple"`
	Complex string `yaml:"complex"`
}

type classifierEntry struct {
	Threshold float64 `yaml:"threshold"`

	Weights struct {
		Length      float64 `yaml:"length"`
		Reasoning   float64 `yaml:"reasoning"`
		Constraints float64 `yaml:"constraints"`
		Context     float64 `yaml:"context"`
		Format      float64 `yaml:"format"`
		Simple      float64 `yaml:"simple"`
	} `yaml:"weights"`

	Scales struct {
		LengthTokens      int `yaml:"length_tokens"`
		ReasoningMatches  int `yaml:"reasoning_matches"`
		ConstraintMatches int `yaml:"constraint_matches"`
		FormatMatches     int `yaml:"format_matches"`
	} `yaml:"scales"`

	Lexicon struct {
		Reasoning   []string `yaml:"reasoning"`
		Constraints []string `yaml:"constraints"`
		Format      []string `yaml:"format"`
		Simple      []string `yaml:"simple"`
	} `yaml:"lexicon"`
}

// LoadRouter reads, validates, and resolves configs/router.yaml.
//
// A missing file is not an error: it means routing is off, which is the
// pre-Phase-8 behaviour and must stay a supported configuration.
func LoadRouter(path string) (Router, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Router{Enabled: false}, nil
		}
		return Router{}, fmt.Errorf("reading router config: %w", err)
	}

	var file routerFile
	if err := yaml.UnmarshalWithOptions(raw, &file, yaml.DisallowUnknownField()); err != nil {
		return Router{}, fmt.Errorf("parsing %s:\n%s", path, yaml.FormatError(err, false, true))
	}

	e := file.Router
	out := Router{Enabled: enabledOrDefault(e.Enabled)}
	if !out.Enabled {
		return out, nil
	}

	if e.Policy.Simple == "" || e.Policy.Complex == "" {
		return Router{}, fmt.Errorf("%s: policy.simple and policy.complex must both name a tier", path)
	}
	if e.Policy.Simple == e.Policy.Complex {
		return Router{}, fmt.Errorf("%s: policy.simple and policy.complex both name tier %q, which makes routing a no-op",
			path, e.Policy.Simple)
	}
	if e.Policy.Simple == router.AutoModel || e.Policy.Complex == router.AutoModel {
		return Router{}, fmt.Errorf("%s: a tier may not be named %q, which is the reserved routing keyword", path, router.AutoModel)
	}
	out.Policy = router.Policy{Simple: e.Policy.Simple, Complex: e.Policy.Complex}

	c := e.Classifier

	// A non-positive scale silently zeroes its feature rather than failing, so
	// it is rejected here instead of producing a classifier that quietly
	// ignores an input the config claims to weight.
	scales := map[string]int{
		"length_tokens":      c.Scales.LengthTokens,
		"reasoning_matches":  c.Scales.ReasoningMatches,
		"constraint_matches": c.Scales.ConstraintMatches,
		"format_matches":     c.Scales.FormatMatches,
	}
	for _, name := range []string{"length_tokens", "reasoning_matches", "constraint_matches", "format_matches"} {
		if scales[name] <= 0 {
			return Router{}, fmt.Errorf("%s: classifier.scales.%s must be positive, got %d", path, name, scales[name])
		}
	}

	lexicons := map[string][]string{
		"reasoning":   c.Lexicon.Reasoning,
		"constraints": c.Lexicon.Constraints,
		"format":      c.Lexicon.Format,
		"simple":      c.Lexicon.Simple,
	}
	for _, name := range []string{"reasoning", "constraints", "format", "simple"} {
		if err := validateLexicon(path, name, lexicons[name]); err != nil {
			return Router{}, err
		}
	}

	out.Classifier = router.ClassifierConfig{
		Threshold: c.Threshold,
		Weights: router.Weights{
			Length:      c.Weights.Length,
			Reasoning:   c.Weights.Reasoning,
			Constraints: c.Weights.Constraints,
			Context:     c.Weights.Context,
			Format:      c.Weights.Format,
			Simple:      c.Weights.Simple,
		},
		Scales: router.Scales{
			LengthTokens:      c.Scales.LengthTokens,
			ReasoningMatches:  c.Scales.ReasoningMatches,
			ConstraintMatches: c.Scales.ConstraintMatches,
			FormatMatches:     c.Scales.FormatMatches,
		},
		Reasoning:   c.Lexicon.Reasoning,
		Constraints: c.Lexicon.Constraints,
		Format:      c.Lexicon.Format,
		Simple:      c.Lexicon.Simple,
	}
	return out, nil
}

// validateLexicon rejects patterns that can never match. Classification
// lowercases the prompt before scanning, so an uppercase pattern is dead
// config that would look correct in review.
func validateLexicon(path, name string, patterns []string) error {
	for i, p := range patterns {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("%s: classifier.lexicon.%s[%d] is empty", path, name, i)
		}
		if p != strings.ToLower(p) {
			return fmt.Errorf("%s: classifier.lexicon.%s[%d] %q must be lowercase; matching is case-folded and it would never fire",
				path, name, i, p)
		}
	}
	return nil
}
