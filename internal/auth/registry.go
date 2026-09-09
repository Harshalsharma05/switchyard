package auth

import (
	"errors"
	"fmt"
	"sync"
	"time"
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
		team := t
		byID[t.ID] = &team

		// A revoked team has no hash and simply is not indexed by one. Skipping
		// rather than indexing "" matters now that teams come from Postgres: two
		// revoked teams would otherwise collide with each other and fail the
		// whole build, taking every other team down with them.
		if t.KeyHash == "" {
			continue
		}
		if existing, dup := byHash[t.KeyHash]; dup {
			return nil, fmt.Errorf("teams %q and %q share the same api_key_hash", existing.ID, t.ID)
		}
		byHash[t.KeyHash] = &team
	}

	return &Registry{byHash: byHash, byID: byID}, nil
}

// Authenticate resolves a plaintext bearer token to its team.
func (r *Registry) Authenticate(rawKey string) (*Team, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	team, ok := r.byHash[HashKey(rawKey)]
	if !ok || team.KeyHash == "" {
		// The empty-hash check is belt and braces: RevokeKey deletes the byHash
		// entry outright, so a revoked team is already unreachable here. But
		// HashKey("") is a real 64-char digest rather than "", so an empty-hash
		// team that somehow stayed indexed would otherwise be resolvable by a
		// caller who sent the empty string — this closes that off for good.
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

// Apply merges the patch onto t and validates the result against the same
// rules config.LoadTeams enforces at boot, returning the new value without
// touching t.
//
// Separate from Update so a caller that must persist the change before it is
// visible — the Postgres-backed store in internal/teamstore — can compute and
// validate the merged team before it writes, and get the identical answer
// Update would produce.
func (p TeamPatch) Apply(t Team) (Team, error) {
	updated := t
	if p.RPM != nil {
		updated.RateLimits.RPM = *p.RPM
	}
	if p.TPM != nil {
		updated.RateLimits.TPM = *p.TPM
	}
	if p.MonthlyBudgetMicros != nil {
		updated.MonthlyBudgetMicros = *p.MonthlyBudgetMicros
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
	return updated, nil
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

	updated, err := patch.Apply(*existing)
	if err != nil {
		return Team{}, err
	}

	r.byID[id] = &updated
	r.byHash[updated.KeyHash] = &updated
	return updated, nil
}

// RotateKey swaps a team's key for a freshly minted one (Part 2, Step 6.4).
//
// It is a separate method from Update, not a wider TeamPatch, for one specific
// reason: Update writes r.byHash[updated.KeyHash] without deleting the old
// entry, which is harmless only while the hash cannot change. Here it does
// change, so the old hash is deleted first — otherwise the rotated-away key
// would keep authenticating against a now-orphaned *Team forever.
//
// Same copy-on-write discipline as Update: a request already holding the old
// *Team keeps seeing the old key valid until it finishes, then that struct is
// unreferenced. The next request to present the new key authenticates
// immediately, with nothing to invalidate.
//
// In memory only. A restart or a POST /admin/reload rebuilds the registry from
// configs/teams.yaml and the rotation is gone — a documented limitation until
// team storage moves to Postgres.
func (r *Registry) RotateKey(id, newHash, newMasked string) (Team, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, ok := r.byID[id]
	if !ok {
		return Team{}, ErrUnknownTeam
	}
	if newHash == "" {
		return Team{}, fmt.Errorf("rotate %s: new key hash is empty", id)
	}
	if other, taken := r.byHash[newHash]; taken && other.ID != id {
		return Team{}, fmt.Errorf("rotate %s: new key already belongs to team %q", id, other.ID)
	}

	now := time.Now().UTC()
	updated := *existing
	updated.KeyHash = newHash
	updated.KeySource = KeySourceRotated
	updated.KeyMasked = newMasked
	updated.KeyCreatedAt = &now

	delete(r.byHash, existing.KeyHash)
	r.byID[id] = &updated
	r.byHash[newHash] = &updated
	return updated, nil
}

// RevokeKey removes a team's key entirely. The team stays in the registry — an
// admin can still see it and rotate it a new key — but no credential resolves
// to it until then. Same in-memory-only caveat as RotateKey.
func (r *Registry) RevokeKey(id string) (Team, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, ok := r.byID[id]
	if !ok {
		return Team{}, ErrUnknownTeam
	}

	updated := *existing
	updated.KeyHash = ""
	updated.KeySource = KeySourceRevoked
	updated.KeyMasked = ""
	updated.KeyCreatedAt = nil

	delete(r.byHash, existing.KeyHash)
	r.byID[id] = &updated
	return updated, nil
}
