package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Harshalsharma05/switchyard/internal/router"
)

// Both guards below reject config that would otherwise load cleanly and then
// silently do nothing — the failure mode worth a test.
func TestLoadRouterRejectsSilentMisconfiguration(t *testing.T) {
	const valid = `
router:
  policy: {simple: fast, complex: frontier}
  classifier:
    threshold: 1.0
    weights: {length: 0.8, reasoning: 1.0, constraints: 0.8, context: 0.7, format: 0.4, simple: -1.0}
    scales: {length_tokens: %s, reasoning_matches: 1, constraint_matches: 3, format_matches: 1}
    lexicon:
      reasoning: ["%s"]
      constraints: ["must "]
      format: ["json"]
      simple: ["define "]
`

	cases := map[string]struct {
		scale, pattern string
		wantErr        string
	}{
		"ok":                {scale: "350", pattern: "analyze"},
		"zero scale":        {scale: "0", pattern: "analyze", wantErr: "must be positive"},
		"uppercase pattern": {scale: "350", pattern: "Analyze", wantErr: "must be lowercase"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "router.yaml")
			body := strings.Replace(strings.Replace(valid, "%s", tc.scale, 1), "%s", tc.pattern, 1)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := LoadRouter(path)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// A missing file means routing is off, which is the pre-Phase-8 behaviour and
// has to stay a supported configuration.
func TestLoadRouterMissingFileDisables(t *testing.T) {
	got, err := LoadRouter(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if got.Enabled {
		t.Fatal("missing file should leave routing disabled")
	}
}

// The policy is the only place tier names appear, so a policy that silently
// routes nowhere would disable the feature without any signal.
func TestLoadRouterRejectsDegeneratePolicy(t *testing.T) {
	const body = `
router:
  policy: {simple: %s, complex: %s}
  classifier:
    threshold: 1.0
    weights: {length: 0.8, reasoning: 1.0, constraints: 0.8, context: 0.7, format: 0.4, simple: -1.0}
    scales: {length_tokens: 350, reasoning_matches: 1, constraint_matches: 3, format_matches: 1}
    lexicon: {reasoning: ["analyze"], constraints: ["must "], format: ["json"], simple: ["define "]}
`

	cases := map[string]struct {
		simple, complex, wantErr string
	}{
		"both tiers named": {simple: "fast", complex: "frontier"},
		"missing tier":     {simple: "fast", complex: "", wantErr: "must both name a tier"},
		"same tier twice":  {simple: "fast", complex: "fast", wantErr: "makes routing a no-op"},
		"reserved keyword": {simple: "auto", complex: "frontier", wantErr: "reserved routing keyword"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "router.yaml")
			out := strings.Replace(strings.Replace(body, "%s", tc.simple, 1), "%s", tc.complex, 1)
			if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := LoadRouter(path)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// The policy is the one place configs/router.yaml and configs/providers.yaml
// have to agree, and a mismatch is only otherwise caught at boot.
func TestShippedRoutingPolicyMatchesProviders(t *testing.T) {
	t.Setenv("GROQ_API_KEY", "test")
	t.Setenv("GEMINI_API_KEY", "test")

	r, err := LoadRouter(filepath.Join("..", "..", "configs", "router.yaml"))
	if err != nil {
		t.Fatalf("loading shipped router config: %v", err)
	}
	if !r.Enabled {
		t.Fatal("shipped config should enable routing")
	}

	p, err := LoadProviders(filepath.Join("..", "..", "configs", "providers.yaml"))
	if err != nil {
		t.Fatalf("loading shipped provider config: %v", err)
	}

	for _, tier := range []string{r.Policy.Simple, r.Policy.Complex} {
		if len(p.Tiers[tier]) == 0 {
			t.Errorf("routing policy names tier %q, which providers.yaml does not declare", tier)
		}
	}

	// A model sharing a routing keyword's name would be unreachable by its
	// own name, since route() would claim it first.
	for model := range p.Pricing {
		if model == router.AutoModel || model == r.Policy.Simple || model == r.Policy.Complex {
			t.Errorf("model %q collides with a routing keyword", model)
		}
	}
}
