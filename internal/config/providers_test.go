package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// writeConfig puts YAML in a temp file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "providers.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return path
}

const validConfig = `
providers:
  - name: groq
    type: openai-compatible
    base_url: https://api.groq.com/openai/v1
    api_key_env: TEST_GROQ_KEY
    timeout: 30s
    default_max_tokens: 1024
    ping_model: openai/gpt-oss-20b
    models:
      - name: openai/gpt-oss-120b
        input_per_1m_usd: 0.59
        output_per_1m_usd: 0.79
      - name: openai/gpt-oss-20b
        input_per_1m_usd: 0.05
        output_per_1m_usd: 0.08
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

func TestLoadProvidersValid(t *testing.T) {
	t.Setenv("TEST_GROQ_KEY", "gsk-secret")

	got, err := LoadProviders(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}

	if len(got.Configs) != 2 {
		t.Fatalf("loaded %d providers, want 2", len(got.Configs))
	}

	// File order is preserved so logs and admin listings are stable.
	if got.Configs[0].Name != "groq" || got.Configs[1].Name != "ollama" {
		t.Errorf("order = %q, %q; want file order", got.Configs[0].Name, got.Configs[1].Name)
	}

	groq := got.Configs[0]
	if groq.Type != provider.TypeOpenAICompatible {
		t.Errorf("Type = %q, want %q", groq.Type, provider.TypeOpenAICompatible)
	}
	if groq.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s", groq.Timeout)
	}
	// The credential comes from the environment, never from the file.
	if groq.APIKey != "gsk-secret" {
		t.Errorf("APIKey = %q, want it resolved from TEST_GROQ_KEY", groq.APIKey)
	}
	if len(groq.Models) != 2 {
		t.Errorf("Models = %v, want 2", groq.Models)
	}

	// Ollama takes no credential and must not be required to name one.
	if got.Configs[1].APIKey != "" {
		t.Errorf("ollama APIKey = %q, want empty", got.Configs[1].APIKey)
	}
}

// Prices are authored as decimals and stored as integer micro-dollars, so that
// Phase 4 can accumulate without float drift.
func TestPricingConvertsToMicroDollars(t *testing.T) {
	t.Setenv("TEST_GROQ_KEY", "gsk-secret")

	got, err := LoadProviders(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}

	tests := map[string]ModelPricing{
		"openai/gpt-oss-120b": {InputPer1M: 590_000, OutputPer1M: 790_000},
		"openai/gpt-oss-20b":  {InputPer1M: 50_000, OutputPer1M: 80_000},
		"llama3.2:3b":         {InputPer1M: 0, OutputPer1M: 0},
	}

	for model, want := range tests {
		t.Run(model, func(t *testing.T) {
			got, ok := got.Pricing[model]
			if !ok {
				t.Fatalf("no pricing recorded for %q", model)
			}
			if got != want {
				t.Errorf("pricing = %+v, want %+v", got, want)
			}
		})
	}
}

// A fractional price that has no exact float representation must still round to
// the right integer.
func TestPricingRoundsFractionalCents(t *testing.T) {
	t.Setenv("TEST_KEY", "x")

	path := writeConfig(t, `
providers:
  - name: gemini
    type: gemini
    base_url: https://example.invalid
    api_key_env: TEST_KEY
    timeout: 30s
    default_max_tokens: 1024
    models:
      - name: gemini-2.0-flash-lite
        input_per_1m_usd: 0.075
        output_per_1m_usd: 0.30
`)

	got, err := LoadProviders(path)
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}

	want := ModelPricing{InputPer1M: 75_000, OutputPer1M: 300_000}
	if got.Pricing["gemini-2.0-flash-lite"] != want {
		t.Errorf("pricing = %+v, want %+v", got.Pricing["gemini-2.0-flash-lite"], want)
	}
}

// A disabled entry is validated but not constructed, so a provider whose paid
// key has not arrived can stay documented in the file.
func TestDisabledProviderIsSkippedWithoutItsKey(t *testing.T) {
	t.Setenv("TEST_GROQ_KEY", "gsk-secret")

	path := writeConfig(t, validConfig+`
  - name: openai
    type: openai-compatible
    enabled: false
    base_url: https://api.openai.com/v1
    api_key_env: DEFINITELY_UNSET_KEY
    timeout: 30s
    default_max_tokens: 1024
    models:
      - name: gpt-4o
        input_per_1m_usd: 2.50
        output_per_1m_usd: 10.00
`)

	got, err := LoadProviders(path)
	if err != nil {
		t.Fatalf("LoadProviders: %v, want the disabled entry to be skipped rather than fail on its missing key", err)
	}

	for _, cfg := range got.Configs {
		if cfg.Name == "openai" {
			t.Error("disabled provider was constructed")
		}
	}
	// Its models must not be routable either.
	if _, ok := got.Pricing["gpt-4o"]; ok {
		t.Error("a disabled provider's model appeared in the pricing table")
	}
}

// But a disabled entry still reserves its model names, so enabling it later
// cannot introduce an ambiguity that was invisible while it was off.
func TestDisabledProviderStillReservesItsModels(t *testing.T) {
	t.Setenv("TEST_GROQ_KEY", "gsk-secret")

	path := writeConfig(t, validConfig+`
  - name: openai
    type: openai-compatible
    enabled: false
    base_url: https://api.openai.com/v1
    api_key_env: DEFINITELY_UNSET_KEY
    timeout: 30s
    default_max_tokens: 1024
    models:
      - name: openai/gpt-oss-120b
        input_per_1m_usd: 2.50
        output_per_1m_usd: 10.00
`)

	_, err := LoadProviders(path)
	if err == nil {
		t.Fatal("load succeeded, want a collision error even though the entry is disabled")
	}
	if !strings.Contains(err.Error(), "exactly one provider") {
		t.Errorf("error = %v, want it to explain the model collision", err)
	}
}

func TestLoadProvidersRejects(t *testing.T) {
	tests := map[string]struct {
		yaml     string
		wantText string
	}{
		"missing name": {
			yaml: `
providers:
  - type: ollama
    base_url: http://localhost:11434
    timeout: 30s
    default_max_tokens: 512
    models:
      - {name: llama3.2:3b, input_per_1m_usd: 0, output_per_1m_usd: 0}
`,
			wantText: "name is required",
		},
		"unknown type": {
			yaml: `
providers:
  - name: mystery
    type: cohere
    base_url: http://localhost
    timeout: 30s
    default_max_tokens: 512
    models:
      - {name: m, input_per_1m_usd: 0, output_per_1m_usd: 0}
`,
			wantText: "unknown type",
		},
		"timeout without units": {
			yaml: `
providers:
  - name: ollama
    type: ollama
    base_url: http://localhost:11434
    timeout: 30
    default_max_tokens: 512
    models:
      - {name: llama3.2:3b, input_per_1m_usd: 0, output_per_1m_usd: 0}
`,
			wantText: "is not a duration",
		},
		"zero max tokens": {
			yaml: `
providers:
  - name: ollama
    type: ollama
    base_url: http://localhost:11434
    timeout: 30s
    default_max_tokens: 0
    models:
      - {name: llama3.2:3b, input_per_1m_usd: 0, output_per_1m_usd: 0}
`,
			wantText: "default_max_tokens",
		},
		"no models": {
			yaml: `
providers:
  - name: ollama
    type: ollama
    base_url: http://localhost:11434
    timeout: 30s
    default_max_tokens: 512
    models: []
`,
			wantText: "at least one model",
		},
		"negative price": {
			yaml: `
providers:
  - name: ollama
    type: ollama
    base_url: http://localhost:11434
    timeout: 30s
    default_max_tokens: 512
    models:
      - {name: llama3.2:3b, input_per_1m_usd: -1, output_per_1m_usd: 0}
`,
			wantText: "negative price",
		},
		"ping model not in list": {
			yaml: `
providers:
  - name: ollama
    type: ollama
    base_url: http://localhost:11434
    timeout: 30s
    default_max_tokens: 512
    ping_model: something-else
    models:
      - {name: llama3.2:3b, input_per_1m_usd: 0, output_per_1m_usd: 0}
`,
			wantText: "ping_model",
		},
		"missing credential": {
			yaml: `
providers:
  - name: gemini
    type: gemini
    base_url: https://example.invalid
    api_key_env: DEFINITELY_UNSET_KEY
    timeout: 30s
    default_max_tokens: 512
    models:
      - {name: gemini-2.0-flash, input_per_1m_usd: 0.1, output_per_1m_usd: 0.4}
`,
			wantText: "is unset or empty",
		},
		"typo in a field name": {
			yaml: `
providers:
  - name: ollama
    type: ollama
    base_ur: http://localhost:11434
    timeout: 30s
    default_max_tokens: 512
    models:
      - {name: llama3.2:3b, input_per_1m_usd: 0, output_per_1m_usd: 0}
`,
			wantText: "base_ur",
		},
		"duplicate provider name": {
			yaml: `
providers:
  - name: ollama
    type: ollama
    base_url: http://localhost:11434
    timeout: 30s
    default_max_tokens: 512
    models:
      - {name: a, input_per_1m_usd: 0, output_per_1m_usd: 0}
  - name: ollama
    type: ollama
    base_url: http://localhost:11434
    timeout: 30s
    default_max_tokens: 512
    models:
      - {name: b, input_per_1m_usd: 0, output_per_1m_usd: 0}
`,
			wantText: "duplicate provider name",
		},
		"empty file": {
			yaml:     "providers: []\n",
			wantText: "no providers defined",
		},
		"everything disabled": {
			yaml: `
providers:
  - name: ollama
    type: ollama
    enabled: false
    base_url: http://localhost:11434
    timeout: 30s
    default_max_tokens: 512
    models:
      - {name: llama3.2:3b, input_per_1m_usd: 0, output_per_1m_usd: 0}
`,
			wantText: "every provider is disabled",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := LoadProviders(writeConfig(t, tc.yaml))
			if err == nil {
				t.Fatal("load succeeded, want startup to fail on a broken config")
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error = %v\nwant it to mention %q", err, tc.wantText)
			}
		})
	}
}

func TestLoadProvidersMissingFile(t *testing.T) {
	_, err := LoadProviders(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil {
		t.Fatal("load succeeded on a missing file")
	}
	if !strings.Contains(err.Error(), "reading provider config") {
		t.Errorf("error = %v, want it to name the operation that failed", err)
	}
}

// The committed configs/providers.yaml must itself be valid, or a fresh clone
// fails at boot. Credentials are stubbed so the test does not need real keys.
func TestCommittedConfigIsValid(t *testing.T) {
	t.Setenv("GROQ_API_KEY", "test")
	t.Setenv("GEMINI_API_KEY", "test")

	got, err := LoadProviders(filepath.Join("..", "..", "configs", "providers.yaml"))
	if err != nil {
		t.Fatalf("committed configs/providers.yaml is invalid: %v", err)
	}
	if len(got.Configs) == 0 {
		t.Fatal("committed config enabled no providers")
	}
	for _, cfg := range got.Configs {
		if _, ok := got.Pricing[cfg.Models[0]]; !ok {
			t.Errorf("provider %q has a model with no pricing", cfg.Name)
		}
	}
}
