package cache

import (
	"testing"
	"time"
)

func testPolicy() TTLPolicy {
	return TTLPolicy{
		Default: time.Hour,
		Max:     24 * time.Hour,
		Rules: []TTLRule{
			{Name: "volatile", TTL: 0, Patterns: []string{"today", "weather"}},
			{Name: "recent", TTL: 15 * time.Minute, Patterns: []string{"this week"}},
			{Name: "stable", TTL: 24 * time.Hour, Patterns: []string{"what is the capital"}},
		},
	}
}

// Precedence decides whether a time-sensitive answer gets cached at all, which
// is the difference between a stale response and a correct one.
func TestTTLPrecedence(t *testing.T) {
	tests := map[string]struct {
		query       string
		override    time.Duration
		hasOverride bool
		wantTTL     time.Duration
		wantCache   bool
	}{
		"no match falls to default": {
			query: "Explain quicksort", wantTTL: time.Hour, wantCache: true,
		},
		"volatile is never cached": {
			query: "What is the weather in Delhi?", wantCache: false,
		},
		"volatile match is case insensitive": {
			query: "What happened TODAY?", wantCache: false,
		},
		"stable gets the long ttl": {
			query: "What is the capital of France?", wantTTL: 24 * time.Hour, wantCache: true,
		},
		"first matching rule wins": {
			// Matches volatile before stable, in declared order.
			query: "What is the capital of France today?", wantCache: false,
		},
		"override beats a volatile rule": {
			query: "What is the weather in Delhi?", override: 5 * time.Minute, hasOverride: true,
			wantTTL: 5 * time.Minute, wantCache: true,
		},
		"override of zero opts out": {
			query: "Explain quicksort", override: 0, hasOverride: true, wantCache: false,
		},
		"override is clamped to max": {
			query: "Explain quicksort", override: 400 * time.Hour, hasOverride: true,
			wantTTL: 24 * time.Hour, wantCache: true,
		},
	}

	policy := testPolicy()

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			gotTTL, gotCache := policy.TTLFor(tc.query, tc.override, tc.hasOverride)

			if gotCache != tc.wantCache {
				t.Fatalf("cacheable = %v, want %v", gotCache, tc.wantCache)
			}
			if gotCache && gotTTL != tc.wantTTL {
				t.Fatalf("ttl = %v, want %v", gotTTL, tc.wantTTL)
			}
		})
	}
}

// A rule TTL above max would otherwise let one config line quietly outlive the
// ceiling the same file sets.
func TestTTLRuleClampedToMax(t *testing.T) {
	policy := TTLPolicy{
		Default: time.Hour,
		Max:     2 * time.Hour,
		Rules:   []TTLRule{{Name: "stable", TTL: 100 * time.Hour, Patterns: []string{"define"}}},
	}

	got, ok := policy.TTLFor("define recursion", 0, false)
	if !ok || got != 2*time.Hour {
		t.Fatalf("ttl = %v (cacheable=%v), want 2h", got, ok)
	}
}
