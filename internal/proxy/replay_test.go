package proxy

import (
	"strings"
	"testing"
)

// Replay must reassemble byte-for-byte. Splitting by bytes instead of runes
// would corrupt multi-byte characters silently — the response still streams,
// it just arrives subtly wrong, which no downstream check would catch.
func TestSplitForReplay(t *testing.T) {
	tests := map[string]string{
		"empty":        "",
		"short":        "Paris.",
		"exact chunk":  strings.Repeat("a", replayChunkRunes),
		"long ascii":   strings.Repeat("quicksort ", 40),
		"multibyte":    strings.Repeat("日本語のテキスト", 12),
		"emoji":        strings.Repeat("🎉 done ", 15),
		"mixed widths": "café 日本 🎉 naïve résumé " + strings.Repeat("ü", 30),
	}

	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			parts := splitForReplay(content)

			if got := strings.Join(parts, ""); got != content {
				t.Fatalf("reassembled %q, want %q", got, content)
			}

			for i, p := range parts {
				if !utf8Valid(p) {
					t.Fatalf("part %d is not valid UTF-8: %q", i, p)
				}
			}

			if content == "" && len(parts) != 0 {
				t.Fatalf("empty content produced %d parts", len(parts))
			}
		})
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}
