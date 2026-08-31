package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCacheFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cache.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The shipped file is the one that actually runs, so it is loaded here rather
// than only synthetic fixtures — a typo in configs/cache.yaml would otherwise
// only surface at startup.
func TestLoadCacheShippedFile(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "test-key")

	got, err := LoadCache(filepath.Join("..", "..", "configs", "cache.yaml"))
	if err != nil {
		t.Fatalf("loading shipped config: %v", err)
	}

	if !got.Enabled || !got.Look.SemanticEnabled {
		t.Fatal("shipped config should enable both tiers")
	}
	if got.Look.Threshold <= 0 || got.Look.Threshold > 1 {
		t.Fatalf("threshold = %v, outside a usable range", got.Look.Threshold)
	}
	if got.Embed.Dimensions <= 0 || got.TTL.Default <= 0 || got.Store.MaxCandidates <= 0 {
		t.Fatalf("incomplete config: %+v", got)
	}
	if len(got.TTL.Rules) == 0 {
		t.Fatal("shipped config should declare ttl rules")
	}
}

// A missing file means the cache is off, which is the pre-Phase-7 behaviour and
// must stay a supported configuration rather than a startup failure.
func TestLoadCacheMissingFileDisables(t *testing.T) {
	got, err := LoadCache(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil || got.Enabled {
		t.Fatalf("missing file: enabled=%v err=%v", got.Enabled, err)
	}
}

func TestLoadCacheValidation(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "test-key")

	tests := map[string]struct {
		body    string
		wantErr bool
	}{
		"threshold above one": {
			body:    "cache:\n  semantic:\n    threshold: 1.5\n",
			wantErr: true,
		},
		"threshold below minus one": {
			body:    "cache:\n  semantic:\n    threshold: -2\n",
			wantErr: true,
		},
		"unparseable ttl": {
			body:    "cache:\n  ttl: 1 hour\n  semantic:\n    threshold: 0.9\n",
			wantErr: true,
		},
		"unknown key": {
			body:    "cache:\n  thresholdd: 0.9\n",
			wantErr: true,
		},
		"missing api key env": {
			body:    "cache:\n  semantic:\n    threshold: 0.9\n  embedding:\n    api_key_env: DEFINITELY_UNSET_KEY\n",
			wantErr: true,
		},
		"disabled skips embedding validation": {
			body: "cache:\n  enabled: false\n  embedding:\n    api_key_env: DEFINITELY_UNSET_KEY\n",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := LoadCache(writeCacheFile(t, tc.body))
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

// An absent semantic block must not silently disable the semantic tier, and an
// absent duration must fall back rather than becoming zero.
func TestLoadCacheDefaults(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "test-key")

	got, err := LoadCache(writeCacheFile(t, "cache:\n  semantic:\n    threshold: 0.9\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Look.SemanticEnabled {
		t.Fatal("semantic tier should default on")
	}
	if got.TTL.Default != time.Hour || got.Store.ReadTimeout != 50*time.Millisecond {
		t.Fatalf("defaults not applied: ttl=%v read=%v", got.TTL.Default, got.Store.ReadTimeout)
	}
}
