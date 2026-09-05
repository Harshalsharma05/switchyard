package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeQualityFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "quality.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The shipped file is what actually runs, so load it here: a typo would
// otherwise only surface at startup.
func TestLoadQualityShippedFile(t *testing.T) {
	got, err := LoadQuality(filepath.Join("..", "..", "configs", "quality.yaml"))
	if err != nil {
		t.Fatalf("loading shipped config: %v", err)
	}
	if !got.Enabled {
		t.Fatal("shipped config should enable quality verification")
	}
	if got.Sampling.RoutedRate <= 0 || got.Sampling.NearThresholdBand <= 0 {
		t.Fatalf("incomplete sampling policy: %+v", got.Sampling)
	}
}

func TestLoadQualityValidation(t *testing.T) {
	if _, err := LoadQuality(filepath.Join(t.TempDir(), "absent.yaml")); err != nil {
		t.Fatalf("a missing file means disabled, not an error: %v", err)
	}

	bad := map[string]string{
		"routed_rate above 1": "quality:\n  sampling:\n    routed_rate: 1.5\n    near_threshold_band: 0.03\n",
		"negative band":       "quality:\n  sampling:\n    routed_rate: 0.1\n    near_threshold_band: -0.01\n",
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadQuality(writeQualityFile(t, body)); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}
