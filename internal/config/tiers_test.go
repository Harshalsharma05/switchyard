package config

import (
	"strings"
	"testing"
)

// tieredConfig extends validConfig with a disabled provider, so the tests below
// can exercise both halves of the enabled rule: a tier entry naming a live
// provider survives, one naming a disabled provider is dropped.
const tieredConfig = `
providers:
  - name: groq
    type: openai-compatible
    base_url: https://api.groq.com/openai/v1
    api_key_env: TEST_GROQ_KEY
    timeout: 30s
    default_max_tokens: 1024
    models:
      - name: fast-a
        input_per_1m_usd: 0.10
        output_per_1m_usd: 0.20
  - name: ollama
    type: ollama
    base_url: http://localhost:11434
    timeout: 120s
    default_max_tokens: 512
    models:
      - name: fast-b
        input_per_1m_usd: 0.0
        output_per_1m_usd: 0.0
  - name: offline
    type: openai-compatible
    enabled: false
    base_url: https://example.invalid/v1
    api_key_env: TEST_OFFLINE_KEY
    timeout: 30s
    default_max_tokens: 1024
    models:
      - name: fast-c
        input_per_1m_usd: 1.00
        output_per_1m_usd: 2.00

tiers:
  fast:
    - {provider: groq, model: fast-a}
    - {provider: offline, model: fast-c}
    - {provider: ollama, model: fast-b}
`

func TestLoadProvidersTiersPreserveOrderAndDropDisabled(t *testing.T) {
	t.Setenv("TEST_GROQ_KEY", "gsk-secret")

	got, err := LoadProviders(writeConfig(t, tieredConfig))
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}

	tier, ok := got.Tiers["fast"]
	if !ok {
		t.Fatalf("Tiers has no %q entry, got %v", "fast", got.Tiers)
	}
	// fast-c sits between the two live entries in the file and must vanish
	// without disturbing the order of what remains.
	if len(tier) != 2 {
		t.Fatalf("tier has %d entries, want 2 (the disabled provider's entry dropped): %+v", len(tier), tier)
	}
	if tier[0].Model != "fast-a" || tier[0].Provider != "groq" {
		t.Errorf("tier[0] = %+v, want groq/fast-a", tier[0])
	}
	if tier[1].Model != "fast-b" || tier[1].Provider != "ollama" {
		t.Errorf("tier[1] = %+v, want ollama/fast-b", tier[1])
	}
}

// TestLoadProvidersNoTiersIsNotAnError pins the pre-Phase-6 behaviour down: a
// file with no tiers block still loads, and simply has no fallback.
func TestLoadProvidersNoTiersIsNotAnError(t *testing.T) {
	t.Setenv("TEST_GROQ_KEY", "gsk-secret")

	got, err := LoadProviders(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}
	if len(got.Tiers) != 0 {
		t.Errorf("Tiers = %v, want empty when the file declares none", got.Tiers)
	}
}

func TestLoadProvidersTierValidation(t *testing.T) {
	// Each case is the tiers block appended to a fixed two-provider preamble,
	// so the table stays about the tier rule under test.
	const preamble = `
providers:
  - name: groq
    type: openai-compatible
    base_url: https://api.groq.com/openai/v1
    api_key_env: TEST_GROQ_KEY
    timeout: 30s
    default_max_tokens: 1024
    models:
      - name: model-a
        input_per_1m_usd: 0.10
        output_per_1m_usd: 0.20
  - name: ollama
    type: ollama
    base_url: http://localhost:11434
    timeout: 120s
    default_max_tokens: 512
    models:
      - name: model-b
        input_per_1m_usd: 0.0
        output_per_1m_usd: 0.0
`

	tests := map[string]struct {
		tiers    string
		wantErr  bool
		contains string
	}{
		"valid": {
			tiers: `
tiers:
  fast:
    - {provider: groq, model: model-a}
    - {provider: ollama, model: model-b}
`,
		},
		"unknown model": {
			tiers: `
tiers:
  fast:
    - {provider: groq, model: model-typo}
`,
			wantErr:  true,
			contains: "no provider defines",
		},
		"provider does not serve that model": {
			// model-b belongs to ollama, not groq. Catching this is the whole
			// reason the redundant provider field is required.
			tiers: `
tiers:
  fast:
    - {provider: groq, model: model-b}
`,
			wantErr:  true,
			contains: "served by",
		},
		"model in two tiers": {
			tiers: `
tiers:
  fast:
    - {provider: groq, model: model-a}
  frontier:
    - {provider: groq, model: model-a}
`,
			wantErr:  true,
			contains: "exactly one tier",
		},
		"model twice in one tier": {
			tiers: `
tiers:
  fast:
    - {provider: groq, model: model-a}
    - {provider: groq, model: model-a}
`,
			wantErr:  true,
			contains: "twice",
		},
		"empty tier": {
			tiers: `
tiers:
  fast: []
`,
			wantErr:  true,
			contains: "no entries",
		},
		"entry missing provider": {
			tiers: `
tiers:
  fast:
    - {model: model-a}
`,
			wantErr:  true,
			contains: "needs both a provider and a model",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TEST_GROQ_KEY", "gsk-secret")

			_, err := LoadProviders(writeConfig(t, preamble+tt.tiers))
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("LoadProviders: unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("LoadProviders: error = nil, want an error mentioning %q", tt.contains)
			}
			if !strings.Contains(err.Error(), tt.contains) {
				t.Errorf("error = %q, want it to mention %q", err, tt.contains)
			}
		})
	}
}

// TestLoadProvidersRealConfigTiers guards the committed configs/providers.yaml
// itself, not just synthetic fixtures — the file a reviewer actually runs.
func TestLoadProvidersRealConfigTiers(t *testing.T) {
	t.Setenv("GROQ_API_KEY", "gsk-test")
	t.Setenv("GEMINI_API_KEY", "gem-test")

	got, err := LoadProviders("../../configs/providers.yaml")
	if err != nil {
		t.Fatalf("LoadProviders on the committed config: %v", err)
	}

	// openai and anthropic are disabled in the committed file, so their tier
	// entries must be absent while every enabled one survives in order.
	wantFast := []string{"openai/gpt-oss-20b", "gemini-3.5-flash-lite", "llama3.2:3b"}
	fast := got.Tiers["fast"]
	if len(fast) != len(wantFast) {
		t.Fatalf("fast tier = %+v, want %d live entries", fast, len(wantFast))
	}
	for i, model := range wantFast {
		if fast[i].Model != model {
			t.Errorf("fast[%d].Model = %q, want %q", i, fast[i].Model, model)
		}
	}

	// Ollama last is load-bearing: it is the free local link that still answers
	// when every paid provider in the chain is down.
	if fast[len(fast)-1].Provider != "ollama" {
		t.Errorf("last fast-tier entry = %q, want ollama", fast[len(fast)-1].Provider)
	}
}
