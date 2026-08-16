// Package auth resolves an inbound API key to the team that owns it and
// describes what that team is permitted to do.
//
// It knows nothing about HTTP: internal/proxy owns the Authorization header
// and the request context, and calls into this package only through the
// Authenticate method the proxy.Authenticator interface asks for. Phase 3.2's
// rate limiter and Phase 4's budget tracker read the same Team values this
// package produces; they do not belong here because neither is an
// authentication concern.
package auth

import (
	"crypto/sha256"
	"encoding/hex"
)

// Priority controls which requests are shed first when a team is near its
// rate limit. See Step 3.5 — nothing reads this yet.
type Priority string

const (
	PriorityRealtime Priority = "realtime"
	PriorityBatch    Priority = "batch"
)

// RateLimits are the two independent buckets Step 3.2's token bucket
// enforces: requests per minute and tokens per minute. Declared now, alongside
// the rest of the team schema, so Step 3.2 does not have to reopen this type.
type RateLimits struct {
	RPM int
	TPM int
}

// Team is one validated entry from configs/teams.yaml.
type Team struct {
	ID   string
	Name string

	// KeyHash is the SHA-256 hex digest of the team's API key, never the key
	// itself — configs/teams.yaml is committed to git, so only the digest may
	// live there. Authenticate compares digests; the plaintext key exists only
	// in the caller's own request.
	KeyHash string

	AllowedProviders []string
	AllowedModels    []string

	RateLimits RateLimits

	// MonthlyBudgetMicros is the spend cap in integer micro-dollars, converted
	// once at config load — same reasoning as provider pricing in
	// internal/config: float addition drifts over thousands of requests.
	MonthlyBudgetMicros int64

	Priority Priority
}

// AllowsModel reports whether the team's allowlist includes model. This is
// what Step 3.1's 403 check calls once the request body is decoded and the
// requested model is known.
func (t Team) AllowsModel(model string) bool {
	for _, m := range t.AllowedModels {
		if m == model {
			return true
		}
	}
	return false
}

// AllowsProvider reports whether the team's allowlist includes provider.
//
// Nothing calls this yet. It exists so Phase 6's fallback resolver can honour
// "never fall back to a provider a team isn't permitted to use, even if it's
// the only healthy one" without this package changing shape when that phase
// arrives — the same reasoning Phase 1 gave for declaring Ping on the Provider
// interface before any health checker existed to call it.
func (t Team) AllowsProvider(provider string) bool {
	for _, p := range t.AllowedProviders {
		if p == provider {
			return true
		}
	}
	return false
}

// HashKey returns the SHA-256 hex digest of a plaintext API key.
//
// SHA-256 rather than bcrypt: this runs on every request, not once at
// sign-up, and bcrypt's deliberate slowness — the property that makes it right
// for password storage — would put tens of milliseconds directly on the hot
// path this project measures in single-digit milliseconds. A leaked team key
// also doesn't carry a password's blast radius: it grants API quota under that
// team's rate limit and budget, not account access, so a fast hash is the
// right trade here.
func HashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
