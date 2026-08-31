package cache

import (
	"strings"
	"time"
)

// TTLRule assigns a lifetime to queries containing any of its patterns.
// A zero TTL means the response is never cached at all.
type TTLRule struct {
	Name string
	TTL  time.Duration

	// Patterns are lowercase substrings. Matching is a substring scan rather
	// than a regexp or a classifier: it runs in microseconds on the hot path,
	// and anything that costs a model call would cost more than the cache saves.
	Patterns []string
}

// TTLPolicy decides how long one response may live.
//
// Content matching is a heuristic and will misclassify. That is why a caller
// can override it: whoever wrote the prompt knows its volatility better than a
// substring scan does. The rules are the default for callers who say nothing.
type TTLPolicy struct {
	Default time.Duration
	Max     time.Duration
	Rules   []TTLRule
}

// TTLFor returns the lifetime for a query and whether it may be cached.
//
// An explicit caller override wins over every rule, clamped to Max — including
// an override of zero, which is how a caller opts a single request out of the
// cache entirely.
func (p TTLPolicy) TTLFor(query string, override time.Duration, hasOverride bool) (time.Duration, bool) {
	if hasOverride {
		return clampTTL(override, p.Max)
	}

	lowered := strings.ToLower(query)
	for _, rule := range p.Rules {
		for _, pattern := range rule.Patterns {
			if strings.Contains(lowered, pattern) {
				return clampTTL(rule.TTL, p.Max)
			}
		}
	}

	return clampTTL(p.Default, p.Max)
}

// MatchedRule names the rule a query hits, for logging and the admin view.
// Empty when nothing matched and the default applies.
func (p TTLPolicy) MatchedRule(query string) string {
	lowered := strings.ToLower(query)
	for _, rule := range p.Rules {
		for _, pattern := range rule.Patterns {
			if strings.Contains(lowered, pattern) {
				return rule.Name
			}
		}
	}
	return ""
}

func clampTTL(d, max time.Duration) (time.Duration, bool) {
	if d <= 0 {
		return 0, false
	}
	if max > 0 && d > max {
		return max, true
	}
	return d, true
}
