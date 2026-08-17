package auth

import (
	"errors"
	"fmt"
	"sync"
)

// ErrUnknownKey means no team's key hash matched the presented credential. It
// is a sentinel so proxy's Auth middleware can map it onto a 401 by identity
// rather than by matching an error string.
var ErrUnknownKey = errors.New("unknown api key")

// ErrUnknownTeam means no team matches the given ID. It is the admin API's
// counterpart to ErrUnknownKey: the public API resolves a request by
// credential, the admin API resolves one by ID, and each needs its own
// sentinel so a caller can map either onto the right HTTP status by identity.
var ErrUnknownTeam = errors.New("unknown team id")

// Registry resolves a plaintext API key to the team that owns it, and — from
// Step 4.3 onward — lets the admin API read and mutate a team's limits and
// budget without a restart.
//
// It is indexed twice: by hash, for Authenticate's per-request lookup, and by
// ID, for the admin API's List/Get/Update. Both indexes are guarded by one
// mutex, because Update must keep them pointing at the same team.
//
// Update never mutates a *Team in place — it builds a new value and swaps the
// pointer in both maps under the write lock. A request that already holds
// the *Team Authenticate handed it therefore keeps seeing the values as of
// when it authenticated, even if an admin PATCHes that same team mid-request:
// the pointer it holds still points at the old, now-orphaned struct, which
// nothing mutates further. This is the same "in-flight requests continue on
// the old state" guarantee Step 4.4's config hot reload gives for the whole
// file, applied here to one team at a time — and it is why RateLimits and
// MonthlyBudgetMicros are passed into ratelimit and budget on every call
// rather than cached once: the very next request that authenticates picks up
// a PATCH immediately, with nothing to invalidate.
type Registry struct {
	mu     sync.RWMutex
	byHash map[string]*Team
	byID   map[string]*Team
}

// NewRegistry indexes teams by hash and by ID.
//
// It fails if two teams share a hash. Two teams cannot share a key: every
// downstream rate limit and budget check assumes a key identifies exactly one
// team, and a collision here would make requests from two teams
// indistinguishable at every layer above this one.
func NewRegistry(teams []Team) (*Registry, error) {
	byHash := make(map[string]*Team, len(teams))
	byID := make(map[string]*Team, len(teams))

	for _, t := range teams {
		if existing, dup := byHash[t.KeyHash]; dup {
			return nil, fmt.Errorf("teams %q and %q share the same api_key_hash", existing.ID, t.ID)
		}
		team := t
		byHash[t.KeyHash] = &team
		byID[t.ID] = &team
	}

	return &Registry{byHash: byHash, byID: byID}, nil
}

// Authenticate resolves a plaintext bearer token to its team.
func (r *Registry) Authenticate(rawKey string) (*Team, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	team, ok := r.byHash[HashKey(rawKey)]
	if !ok {
		return nil, ErrUnknownKey
	}
	return team, nil
}

// List returns every team, in no particular order, as value copies — a
// caller can never mutate registry state through what List returns, only
// through Update.
func (r *Registry) List() []Team {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Team, 0, len(r.byID))
	for _, t := range r.byID {
		out = append(out, *t)
	}
	return out
}

// Get returns one team by ID.
func (r *Registry) Get(id string) (Team, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	t, ok := r.byID[id]
	if !ok {
		return Team{}, ErrUnknownTeam
	}
	return *t, nil
}

// TeamPatch carries optional field updates for Update. A nil field means
// "leave this alone" — the same convention a JSON PATCH body uses, so the
// admin handler can decode a request body almost directly into this type.
// Deliberately limited to what Step 4.3 asks for — "adjust limits and
// budget" — not every field on Team: allowlists, priority, and identity stay
// config-file concerns.
type TeamPatch struct {
	RPM                 *int
	TPM                 *int
	MonthlyBudgetMicros *int64
}

// Update applies patch to one team and returns the result.
//
// It never mutates the existing *Team in place — see the Registry doc
// comment for why. A fresh Team value is built, validated against the same
// rules config.LoadTeams enforces at boot, and only then swapped into both
// indexes under the write lock.
func (r *Registry) Update(id string, patch TeamPatch) (Team, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, ok := r.byID[id]
	if !ok {
		return Team{}, ErrUnknownTeam
	}

	updated := *existing
	if patch.RPM != nil {
		updated.RateLimits.RPM = *patch.RPM
	}
	if patch.TPM != nil {
		updated.RateLimits.TPM = *patch.TPM
	}
	if patch.MonthlyBudgetMicros != nil {
		updated.MonthlyBudgetMicros = *patch.MonthlyBudgetMicros
	}

	if updated.RateLimits.RPM <= 0 {
		return Team{}, fmt.Errorf("rpm must be a positive integer, got %d", updated.RateLimits.RPM)
	}
	if updated.RateLimits.TPM <= 0 {
		return Team{}, fmt.Errorf("tpm must be a positive integer, got %d", updated.RateLimits.TPM)
	}
	if updated.MonthlyBudgetMicros <= 0 {
		return Team{}, fmt.Errorf("monthly budget must be positive, got %d micro-dollars", updated.MonthlyBudgetMicros)
	}

	r.byID[id] = &updated
	r.byHash[updated.KeyHash] = &updated
	return updated, nil
}
