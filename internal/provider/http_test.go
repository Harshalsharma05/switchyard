package provider

import (
	"strings"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	tests := map[string]struct {
		header string
		want   time.Duration
	}{
		"seconds":            {"30", 30 * time.Second},
		"seconds padded":     {"  30  ", 30 * time.Second},
		"zero means nothing": {"0", 0},
		"negative ignored":   {"-5", 0},
		"absent":             {"", 0},
		"garbage ignored":    {"soon", 0},
		"http date ahead":    {"Sat, 15 Aug 2026 12:00:45 GMT", 45 * time.Second},
		"http date past":     {"Sat, 15 Aug 2026 11:59:00 GMT", 0},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := parseRetryAfter(tc.header, now); got != tc.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}

func TestTruncateMessage(t *testing.T) {
	t.Run("short strings pass through", func(t *testing.T) {
		if got := truncateMessage("  rate limit reached  "); got != "rate limit reached" {
			t.Errorf("got %q, want the trimmed original", got)
		}
	})

	t.Run("long strings are bounded", func(t *testing.T) {
		got := truncateMessage(strings.Repeat("a", maxErrorMessageBytes*2))
		if len(got) > maxErrorMessageBytes+len("…") {
			t.Errorf("length %d exceeded the cap", len(got))
		}
	})

	t.Run("multibyte runes survive the cut", func(t *testing.T) {
		// A rune boundary that does not align with the byte cap must not produce
		// invalid UTF-8 in a log line.
		got := truncateMessage(strings.Repeat("é", maxErrorMessageBytes))
		if !strings.ContainsRune(got, '…') {
			t.Error("expected a truncation marker")
		}
		for _, r := range got {
			if r == '�' {
				t.Error("truncation produced an invalid rune")
			}
		}
	})
}
