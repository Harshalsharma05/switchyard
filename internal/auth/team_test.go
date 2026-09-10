package auth

import (
	"strings"
	"testing"
)

func TestTeamAllowsModel(t *testing.T) {
	team := Team{AllowedModels: []string{"gpt-4o", "llama3.2:3b"}}

	tests := map[string]bool{
		"gpt-4o":      true,
		"llama3.2:3b": true,
		"gpt-4o-mini": false,
		"":            false,
	}

	for model, want := range tests {
		if got := team.AllowsModel(model); got != want {
			t.Errorf("AllowsModel(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestTeamAllowsProvider(t *testing.T) {
	team := Team{AllowedProviders: []string{"groq", "ollama"}}

	tests := map[string]bool{
		"groq":      true,
		"ollama":    true,
		"anthropic": false,
	}

	for provider, want := range tests {
		if got := team.AllowsProvider(provider); got != want {
			t.Errorf("AllowsProvider(%q) = %v, want %v", provider, got, want)
		}
	}
}

func TestHashKeyIsDeterministicAndKnownLength(t *testing.T) {
	h1 := HashKey("sk-switchyard-dev-acme-9f2b1c")
	h2 := HashKey("sk-switchyard-dev-acme-9f2b1c")

	if h1 != h2 {
		t.Fatal("HashKey is not deterministic for the same input")
	}
	if len(h1) != 64 {
		t.Errorf("hash length = %d, want 64 (SHA-256 hex)", len(h1))
	}
	if HashKey("different-key") == h1 {
		t.Error("different keys hashed to the same digest")
	}
}

// A team ID is permanent and ends up in keys, logs, and metric labels, so the
// edge cases of deriving one are worth pinning.
func TestSlug(t *testing.T) {
	tests := map[string]string{
		"Acme Corp":          "acme-corp",
		"  Globex -- Inc!! ": "globex-inc",
		"Team 42":            "team-42",
		"already-a-slug":     "already-a-slug",
		"Café Olé":           "caf-ol",
		"日本チーム":              "",
		"!!!":                "",
	}
	for name, want := range tests {
		if got := Slug(name); got != want {
			t.Errorf("Slug(%q) = %q, want %q", name, got, want)
		}
	}

	long := Slug(strings.Repeat("abc ", 20))
	if len(long) > maxIDLength || strings.HasSuffix(long, "-") {
		t.Errorf("Slug of a long name = %q (%d chars), want at most %d and no trailing hyphen", long, len(long), maxIDLength)
	}
}
