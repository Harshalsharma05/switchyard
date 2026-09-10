// Hot config reload (Step 4.4).
//
// This lives in cmd/gateway, not a new internal/ package: it orchestrates
// across config, provider, auth, and budget to rebuild every registry a
// request depends on, and main.go is already the one place allowed to know
// about every package. Nothing here is proxy- or admin-specific business
// logic — it is wiring, the same job the rest of this file does.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sync/atomic"

	"github.com/Harshalsharma05/switchyard/internal/admin"
	"github.com/Harshalsharma05/switchyard/internal/auth"
	"github.com/Harshalsharma05/switchyard/internal/budget"
	"github.com/Harshalsharma05/switchyard/internal/config"
	"github.com/Harshalsharma05/switchyard/internal/provider"
	"github.com/Harshalsharma05/switchyard/internal/proxy"
	"github.com/Harshalsharma05/switchyard/internal/resilience"
)

// liveConfig bundles everything one reload replaces together. Grouping
// these three into one struct, swapped by one atomic.Pointer.Store, is what
// guarantees a request never sees a provider registry built from one
// generation of providers.yaml paired with a pricing table or team registry
// from another — the whole bundle is either the old generation or the new
// one, never a mix.
type liveConfig struct {
	registry *provider.Registry
	calc     *budget.Calculator

	// tiers is Step 6.2's fallback chains, pre-indexed by model: given the
	// model a caller asked for, the other candidates in its tier. Built once
	// per reload rather than searched on every request, since the request
	// path only ever asks this one question and a linear scan of every tier
	// would put configs/providers.yaml's size on the hot path.
	tiers map[string][]resilience.Candidate

	// tierNames is the same tiers keyed by tier name instead of by model.
	// Step 8.2's routing starts from a tier the policy named, so it has no
	// model to key on; both indexes share the same backing slices.
	tierNames map[string][]resilience.Candidate

	// configHash is a SHA-256 over the raw bytes of providers.yaml as it was
	// when this generation was built. The System panel shows it so an operator
	// can tell at a glance whether the running config matches what is on disk;
	// it changes on every reload by construction. teams.yaml is deliberately
	// not part of it — since Tier 1 Phase 2 that file is a first-boot seed and
	// has no bearing on what the running gateway is doing.
	configHash string
}

// configStore is the atomic swap point behind every hot-reloadable
// dependency. It implements proxy.Resolver, proxy.Authenticator,
// proxy.CostCalculator, admin.TeamStore, and admin.ProviderLister all at
// once, purely by reading through current on every call — Go's structural
// typing is what lets one swap point serve every consumer without a
// near-identical wrapper type for each interface.
//
// Each method calls Load() independently, so a reload landing in the
// microsecond gap between two calls within the same request (e.g. resolve()
// then reserveTokens() reading DefaultMaxTokensFor a moment later) is a
// theoretical inconsistency window: deliberately left unaddressed here.
// Closing it would mean threading one pinned snapshot through the request
// context for every dependency, and the failure mode without that plumbing
// is, at worst, a single spurious error that succeeds on an immediate
// retry — out of proportion to how this reload is actually triggered: a
// rare, manual, operator-initiated POST, not a hot path.
// teamStore is the slice of teamstore.Store this package needs. Declared here,
// by the consumer, for the same reason every other dependency is — and so the
// reload tests can substitute a plain in-memory registry instead of requiring a
// Postgres to test config reload.
type teamStore interface {
	Authenticate(rawKey string) (*auth.Team, error)
	List() []auth.Team
	Get(id string) (auth.Team, error)
	Update(ctx context.Context, id string, patch auth.TeamPatch) (auth.Team, error)
	RotateKey(ctx context.Context, id, newHash, newMasked string) (auth.Team, error)
	RevokeKey(ctx context.Context, id string) (auth.Team, error)
	Create(ctx context.Context, t auth.Team) (auth.Team, error)
	Delete(ctx context.Context, id string) (auth.Team, error)
}

type configStore struct {
	current atomic.Pointer[liveConfig]

	// teams is not part of the atomic swap above: teams live in Postgres since
	// Tier 1 Phase 2 and are not reloadable from a file at all. It is a plain
	// field because it is set once at construction and never replaced — the
	// store does its own refreshing behind these calls.
	teams teamStore
}

// Compile-time proof configStore satisfies every interface it is wired into
// below — a mismatch fails the build here, at the type itself, instead of
// as a confusing error at the NewRouter call site.
var (
	_ proxy.Resolver       = (*configStore)(nil)
	_ proxy.Authenticator  = (*configStore)(nil)
	_ proxy.CostCalculator = (*configStore)(nil)
	_ admin.TeamStore      = (*configStore)(nil)
	_ admin.ProviderLister = (*configStore)(nil)
)

func newConfigStore(initial *liveConfig, teams teamStore) *configStore {
	s := &configStore{teams: teams}
	s.current.Store(initial)
	return s
}

func (s *configStore) ForModel(model string) (provider.Provider, error) {
	return s.current.Load().registry.ForModel(model)
}

func (s *configStore) DefaultMaxTokensFor(model string) (int, bool) {
	return s.current.Load().registry.DefaultMaxTokensFor(model)
}

// TierFor returns model's fallback tier. The returned slice is shared with
// every other caller and must never be mutated — resilience.BuildChain only
// reads it, and copying per request would allocate on the hot path for no
// benefit.
func (s *configStore) TierFor(model string) []resilience.Candidate {
	return s.current.Load().tiers[model]
}

// TierNamed returns a tier by its configs/providers.yaml name. Same sharing
// rule as TierFor: the slice is read-only for every caller.
func (s *configStore) TierNamed(name string) []resilience.Candidate {
	return s.current.Load().tierNames[name]
}

func (s *configStore) Authenticate(rawKey string) (*auth.Team, error) {
	return s.teams.Authenticate(rawKey)
}

func (s *configStore) Cost(model string, inputTokens, outputTokens int) (int64, error) {
	return s.current.Load().calc.Cost(model, inputTokens, outputTokens)
}

func (s *configStore) List() []auth.Team {
	return s.teams.List()
}

func (s *configStore) Get(id string) (auth.Team, error) {
	return s.teams.Get(id)
}

func (s *configStore) Update(ctx context.Context, id string, patch auth.TeamPatch) (auth.Team, error) {
	return s.teams.Update(ctx, id, patch)
}

func (s *configStore) RotateKey(ctx context.Context, id, newHash, newMasked string) (auth.Team, error) {
	return s.teams.RotateKey(ctx, id, newHash, newMasked)
}

func (s *configStore) RevokeKey(ctx context.Context, id string) (auth.Team, error) {
	return s.teams.RevokeKey(ctx, id)
}

func (s *configStore) Create(ctx context.Context, t auth.Team) (auth.Team, error) {
	return s.teams.Create(ctx, t)
}

func (s *configStore) Delete(ctx context.Context, id string) (auth.Team, error) {
	return s.teams.Delete(ctx, id)
}

func (s *configStore) Configs() []provider.Config {
	return s.current.Load().registry.Configs()
}

// ConfigHash returns the fingerprint of the currently-live config files.
func (s *configStore) ConfigHash() string {
	return s.current.Load().configHash
}

// loadLiveConfig reads and validates configs/*.yaml and builds a fresh
// liveConfig from scratch — the exact same steps run() takes at boot. Used
// both there and by reload, so the two can never drift into checking
// different things.
func loadLiveConfig(providersPath string) (*liveConfig, int, error) {
	hash, err := configHash(providersPath)
	if err != nil {
		return nil, 0, err
	}

	providers, err := config.LoadProviders(providersPath)
	if err != nil {
		return nil, 0, fmt.Errorf("loading provider config: %w", err)
	}
	registry, err := provider.NewRegistry(providers.Configs)
	if err != nil {
		return nil, 0, fmt.Errorf("building provider registry: %w", err)
	}

	byModel, byName := indexTiers(providers.Tiers)

	pricing := make(map[string]budget.Pricing, len(providers.Pricing))
	for model, p := range providers.Pricing {
		pricing[model] = budget.Pricing{InputPer1M: p.InputPer1M, OutputPer1M: p.OutputPer1M}
	}

	return &liveConfig{
		registry:   registry,
		calc:       budget.NewCalculator(pricing),
		tiers:      byModel,
		tierNames:  byName,
		configHash: hash,
	}, len(providers.Configs), nil
}

// configHash fingerprints the config file the running gateway depends on. A
// second read of a file config.LoadProviders also reads is cheap and only
// happens at boot and on a manual reload — never on the request path.
func configHash(providersPath string) (string, error) {
	h := sha256.New()
	b, err := os.ReadFile(providersPath)
	if err != nil {
		return "", fmt.Errorf("hashing config %s: %w", providersPath, err)
	}
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// indexTiers flips config's tier-name-keyed map into the model-keyed one the
// request path wants: every model in a tier maps to that whole tier, in
// declared order. The requested model is left in its own list because
// resilience.BuildChain dedupes it against the chain head anyway, and
// removing it here would mean building one slice per model instead of
// sharing one per tier.
//
// The conversion happens in cmd/gateway rather than internal/config for the
// same reason the pricing conversion above does: config's job is to validate
// the file, and mapping its output onto another package's types is wiring.
func indexTiers(tiers map[string][]config.TierEntry) (byModel, byName map[string][]resilience.Candidate) {
	if len(tiers) == 0 {
		return nil, nil
	}

	byModel = make(map[string][]resilience.Candidate)
	byName = make(map[string][]resilience.Candidate, len(tiers))
	for name, entries := range tiers {
		candidates := make([]resilience.Candidate, 0, len(entries))
		for _, e := range entries {
			candidates = append(candidates, resilience.Candidate{Provider: e.Provider, Model: e.Model})
		}
		byName[name] = candidates
		for _, c := range candidates {
			byModel[c.Model] = candidates
		}
	}
	return byModel, byName
}

// newReloader returns the closure admin.NewRouter calls for POST
// /admin/reload. It captures the file paths and the store by closure, the
// same shape main.go already uses for every other dependency it builds
// once and threads through.
//
// A failure at any step — a malformed file, a duplicate team, an unknown
// provider type — returns before configStore.current is ever touched, so
// the checklist's "invalid config reload is rejected, gateway keeps running
// on the old config" holds by construction rather than by a rollback step
// that could itself fail partway.
func newReloader(store *configStore, providersPath string) admin.Reloader {
	return func(ctx context.Context) (admin.ReloadSummary, error) {
		next, providerCount, err := loadLiveConfig(providersPath)
		if err != nil {
			return admin.ReloadSummary{}, err
		}
		store.current.Store(next)
		// Reported, not reloaded: the count comes from the live team store so
		// the summary describes what is actually serving, and reload cannot
		// touch it.
		return admin.ReloadSummary{Providers: providerCount, Teams: len(store.teams.List())}, nil
	}
}
