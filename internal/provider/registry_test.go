package provider

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func registryConfigs() []Config {
	return []Config{
		{
			Name:             "groq",
			Type:             TypeOpenAICompatible,
			BaseURL:          "https://api.groq.com/openai/v1",
			APIKey:           "gsk-test",
			Timeout:          30 * time.Second,
			Models:           []string{"openai/gpt-oss-120b", "openai/gpt-oss-20b"},
			DefaultMaxTokens: 1024,
		},
		{
			Name:             "gemini",
			Type:             TypeGemini,
			BaseURL:          "https://generativelanguage.googleapis.com/v1beta",
			APIKey:           "goog-test",
			Timeout:          30 * time.Second,
			Models:           []string{"gemini-2.0-flash"},
			DefaultMaxTokens: 1024,
		},
		{
			Name:             "ollama",
			Type:             TypeOllama,
			BaseURL:          "http://localhost:11434",
			Timeout:          120 * time.Second,
			Models:           []string{"llama3.2:3b"},
			DefaultMaxTokens: 512,
		},
	}
}

func TestRegistryForModel(t *testing.T) {
	r, err := NewRegistry(registryConfigs())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	tests := map[string]string{
		"openai/gpt-oss-120b": "groq",
		"openai/gpt-oss-20b":  "groq",
		"gemini-2.0-flash":    "gemini",
		"llama3.2:3b":         "ollama",
	}

	for model, wantProvider := range tests {
		t.Run(model, func(t *testing.T) {
			p, err := r.ForModel(model)
			if err != nil {
				t.Fatalf("ForModel(%q): %v", model, err)
			}
			if p.Name() != wantProvider {
				t.Errorf("resolved to %q, want %q", p.Name(), wantProvider)
			}
		})
	}
}

// The Step 1.5 handler branches on this sentinel to return a 404 rather than
// matching on an error string.
func TestRegistryUnknownModelIsSentinel(t *testing.T) {
	r, err := NewRegistry(registryConfigs())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	_, err = r.ForModel("gpt-4o")
	if !errors.Is(err, ErrModelNotSupported) {
		t.Fatalf("err = %v, want ErrModelNotSupported", err)
	}
	// The wrapping must still name what was asked for.
	if !strings.Contains(err.Error(), "gpt-4o") {
		t.Errorf("error = %v, want it to name the model", err)
	}
}

// Each adapter type must construct through the registry, so a config-only
// provider swap really does work without code changes.
func TestRegistryBuildsEveryAdapterType(t *testing.T) {
	tests := map[string]struct {
		typ  string
		want string
	}{
		"openai compatible": {TypeOpenAICompatible, "*provider.OpenAICompatible"},
		"anthropic":         {TypeAnthropic, "*provider.Anthropic"},
		"gemini":            {TypeGemini, "*provider.Gemini"},
		"ollama":            {TypeOllama, "*provider.Ollama"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r, err := NewRegistry([]Config{{
				Name:             "instance",
				Type:             tc.typ,
				BaseURL:          "http://localhost",
				APIKey:           "k",
				Timeout:          time.Second,
				Models:           []string{"m"},
				DefaultMaxTokens: 16,
			}})
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}

			p, err := r.ForModel("m")
			if err != nil {
				t.Fatalf("ForModel: %v", err)
			}
			if got := typeName(p); got != tc.want {
				t.Errorf("built %s, want %s", got, tc.want)
			}
		})
	}
}

// Two instances of one adapter type must coexist — the arrangement that lets a
// free stand-in and the real provider both be described in one config file.
func TestRegistryAllowsTwoInstancesOfOneType(t *testing.T) {
	r, err := NewRegistry([]Config{
		{
			Name: "groq", Type: TypeOpenAICompatible,
			BaseURL: "https://api.groq.com/openai/v1", APIKey: "a",
			Timeout: time.Second, Models: []string{"openai/gpt-oss-20b"}, DefaultMaxTokens: 16,
		},
		{
			Name: "openai", Type: TypeOpenAICompatible,
			BaseURL: "https://api.openai.com/v1", APIKey: "b",
			Timeout: time.Second, Models: []string{"gpt-4o-mini"}, DefaultMaxTokens: 16,
		},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	groq, err := r.ForModel("openai/gpt-oss-20b")
	if err != nil {
		t.Fatalf("ForModel: %v", err)
	}
	openai, err := r.ForModel("gpt-4o-mini")
	if err != nil {
		t.Fatalf("ForModel: %v", err)
	}

	if groq.Name() == openai.Name() {
		t.Fatal("both models resolved to the same instance")
	}
	if groq == openai {
		t.Error("both instances are the same object; each needs its own base URL and credential")
	}
}

func TestRegistryRejects(t *testing.T) {
	valid := Config{
		Name: "a", Type: TypeOllama, BaseURL: "http://localhost",
		Timeout: time.Second, Models: []string{"m"}, DefaultMaxTokens: 16,
	}

	tests := map[string]struct {
		cfgs     []Config
		wantText string
	}{
		"no providers": {
			cfgs:     nil,
			wantText: "no providers configured",
		},
		"unknown type": {
			cfgs:     []Config{{Name: "a", Type: "cohere", BaseURL: "http://x", Timeout: time.Second, Models: []string{"m"}, DefaultMaxTokens: 16}},
			wantText: "unknown type",
		},
		"duplicate name": {
			cfgs:     []Config{valid, valid},
			wantText: "duplicate provider name",
		},
		"model served twice": {
			cfgs: []Config{
				valid,
				{Name: "b", Type: TypeOllama, BaseURL: "http://localhost", Timeout: time.Second, Models: []string{"m"}, DefaultMaxTokens: 16},
			},
			wantText: "must map to one provider",
		},
		"invalid instance config": {
			cfgs:     []Config{{Name: "a", Type: TypeOllama, BaseURL: "", Timeout: time.Second, Models: []string{"m"}, DefaultMaxTokens: 16}},
			wantText: "base_url is required",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := NewRegistry(tc.cfgs)
			if err == nil {
				t.Fatal("NewRegistry succeeded, want startup to fail")
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error = %v\nwant it to mention %q", err, tc.wantText)
			}
		})
	}
}

func TestRegistryByNameAndListing(t *testing.T) {
	cfgs := registryConfigs()
	r, err := NewRegistry(cfgs)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	p, err := r.ByName("gemini")
	if err != nil {
		t.Fatalf("ByName: %v", err)
	}
	if p.Name() != "gemini" {
		t.Errorf("ByName returned %q", p.Name())
	}

	if _, err := r.ByName("nope"); !errors.Is(err, ErrProviderNotFound) {
		t.Errorf("err = %v, want ErrProviderNotFound", err)
	}

	// Listing follows config order rather than Go's randomized map iteration.
	got := r.Providers()
	if len(got) != len(cfgs) {
		t.Fatalf("listed %d providers, want %d", len(got), len(cfgs))
	}
	for i, cfg := range cfgs {
		if got[i].Name() != cfg.Name {
			t.Errorf("position %d = %q, want %q", i, got[i].Name(), cfg.Name)
		}
	}
}

// typeName reports a value's concrete type without importing reflect into
// non-test code.
func typeName(v any) string {
	switch v.(type) {
	case *OpenAICompatible:
		return "*provider.OpenAICompatible"
	case *Anthropic:
		return "*provider.Anthropic"
	case *Gemini:
		return "*provider.Gemini"
	case *Ollama:
		return "*provider.Ollama"
	default:
		return "unknown"
	}
}
