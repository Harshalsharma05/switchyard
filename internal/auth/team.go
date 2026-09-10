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
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
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

// Team is one tenant, stored in Postgres: seeded from configs/teams.yaml on a
// first boot, or created through the admin API.
type Team struct {
	ID   string
	Name string

	// OrganizationID is the organisation this team belongs to. There is one,
	// the default, until multi-org exists; the field is here so the concept
	// is real in the data model without any behaviour hanging off it yet.
	OrganizationID string

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

	// IsAdmin lets this team read other teams' rows on the admin API's
	// request-log endpoints. Enforced server-side in the handler.
	IsAdmin bool

	// Key lifecycle metadata (Part 2, Step 6.4). None of it is the key or the
	// hash: KeySource says where the current key came from, KeyMasked is a
	// display-only "sk-…a097" for a key this gateway minted, and KeyCreatedAt is
	// when it did. A config-seeded team has KeySourceConfig, an empty KeyMasked
	// (the gateway never saw its plaintext), and a nil KeyCreatedAt. Persisted
	// in Postgres by internal/teamstore since Tier 1 Phase 2, so a rotation
	// survives both a restart and a POST /admin/reload.
	KeySource    string
	KeyMasked    string
	KeyCreatedAt *time.Time
}

const (
	KeySourceConfig  = "config"  // key hash came from configs/teams.yaml
	KeySourceRotated = "rotated" // key was minted by POST /admin/teams/{id}/key/rotate
	KeySourceRevoked = "revoked" // key was removed by DELETE /admin/teams/{id}/key, or the team was deleted
	KeySourceCreated = "created" // key was minted with the team by POST /admin/teams
)

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

// GenerateKey mints a new plaintext API key for a team, shaped like the dev keys
// already in configs/teams.yaml: sk-switchyard-<team>-<32 hex chars>.
//
// crypto/rand, not math/rand: this is a credential. math/rand is a deterministic
// PRNG whose output an attacker who learns the seed can reproduce, which is why
// it returns an error — a failure of the OS entropy source must abort the
// rotation, not silently fall back to something guessable.
func GenerateKey(teamID string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating api key: %w", err)
	}
	return fmt.Sprintf("sk-switchyard-%s-%s", teamID, hex.EncodeToString(b[:])), nil
}

// MaskKey renders a key for display: a fixed prefix and the last four
// characters, enough for an operator to tell one key from another and far too
// little to use. Only ever called on a key this process just generated — a
// config-seeded key's plaintext is never seen, so it has no mask.
func MaskKey(raw string) string {
	if len(raw) < 4 {
		return "sk-…"
	}
	return "sk-…" + raw[len(raw)-4:]
}

// maxIDLength bounds a derived team ID. The ID ends up in every key the team is
// issued, every log line, and every metric label, so it stays short.
const maxIDLength = 48

// Slug derives a team ID from a display name: lowercase ASCII letters and
// digits, every run of anything else collapsed to one hyphen, none at either
// end. "Acme Corp" becomes "acme-corp".
//
// A team ID is permanent — it is baked into every key the team is issued, and a
// deleted team keeps its ID forever so a new team can never inherit its history
// — so this runs once, at creation. Non-ASCII letters are dropped rather than
// transliterated; a name with no ASCII letter or digit yields "", which the
// caller must reject.
func Slug(name string) string {
	var b strings.Builder
	pendingHyphen := false
	for _, r := range strings.ToLower(name) {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !isAlnum {
			pendingHyphen = b.Len() > 0
			continue
		}
		if pendingHyphen {
			b.WriteByte('-')
			pendingHyphen = false
		}
		b.WriteRune(r)
	}
	id := b.String()
	if len(id) > maxIDLength {
		id = strings.TrimRight(id[:maxIDLength], "-")
	}
	return id
}

// Validate reports whether t is a usable team: the rules config.LoadTeams
// applies to the seed file, applied to a team built any other way — today, one
// created through the admin API.
func (t Team) Validate() error {
	if t.ID == "" {
		return errors.New("id is required")
	}
	if strings.TrimSpace(t.Name) == "" {
		return errors.New("name is required")
	}
	if len(t.AllowedProviders) == 0 {
		return errors.New("at least one allowed provider is required")
	}
	if len(t.AllowedModels) == 0 {
		return errors.New("at least one allowed model is required")
	}
	if err := validateLimits(t); err != nil {
		return err
	}
	if t.Priority != PriorityRealtime && t.Priority != PriorityBatch {
		return fmt.Errorf("priority must be %s or %s, got %q", PriorityRealtime, PriorityBatch, t.Priority)
	}
	return nil
}
