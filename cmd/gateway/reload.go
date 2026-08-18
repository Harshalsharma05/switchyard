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
	"fmt"
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
	registry     *provider.Registry
	authRegistry *auth.Registry
	calc         *budget.Calculator

	// tiers is Step 6.2's fallback chains, pre-indexed by model: given the
	// model a caller asked for, the other candidates in its tier. Built once
	// per reload rather than searched on every request, since the request
	// path only ever asks this one question and a linear scan of every tier
	// would put configs/providers.yaml's size on the hot path.
	tiers map[string][]resilience.Candidate
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
type configStore struct {
	current atomic.Pointer[liveConfig]
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

func newConfigStore(initial *liveConfig) *configStore {
	s := &configStore{}
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

func (s *configStore) Authenticate(rawKey string) (*auth.Team, error) {
	return s.current.Load().authRegistry.Authenticate(rawKey)
}

func (s *configStore) Cost(model string, inputTokens, outputTokens int) (int64, error) {
	return s.current.Load().calc.Cost(model, inputTokens, outputTokens)
}

func (s *configStore) List() []auth.Team {
	return s.current.Load().authRegistry.List()
}

func (s *configStore) Get(id string) (auth.Team, error) {
	return s.current.Load().authRegistry.Get(id)
}

func (s *configStore) Update(id string, patch auth.TeamPatch) (auth.Team, error) {
	return s.current.Load().authRegistry.Update(id, patch)
}

func (s *configStore) Configs() []provider.Config {
	return s.current.Load().registry.Configs()
}

// loadLiveConfig reads and validates configs/*.yaml and builds a fresh
// liveConfig from scratch — the exact same steps run() takes at boot. Used
// both there and by reload, so the two can never drift into checking
// different things.
func loadLiveConfig(providersPath, teamsPath string) (*liveConfig, int, int, error) {
	providers, err := config.LoadProviders(providersPath)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("loading provider config: %w", err)
	}
	registry, err := provider.NewRegistry(providers.Configs)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("building provider registry: %w", err)
	}

	teams, err := config.LoadTeams(teamsPath)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("loading team config: %w", err)
	}
	authRegistry, err := auth.NewRegistry(teams)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("building auth registry: %w", err)
	}

	pricing := make(map[string]budget.Pricing, len(providers.Pricing))
	for model, p := range providers.Pricing {
		pricing[model] = budget.Pricing{InputPer1M: p.InputPer1M, OutputPer1M: p.OutputPer1M}
	}

	return &liveConfig{
		registry:     registry,
		authRegistry: authRegistry,
		calc:         budget.NewCalculator(pricing),
		tiers:        indexTiers(providers.Tiers),
	}, len(providers.Configs), len(teams), nil
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
func indexTiers(tiers map[string][]config.TierEntry) map[string][]resilience.Candidate {
	if len(tiers) == 0 {
		return nil
	}

	out := make(map[string][]resilience.Candidate)
	for _, entries := range tiers {
		candidates := make([]resilience.Candidate, 0, len(entries))
		for _, e := range entries {
			candidates = append(candidates, resilience.Candidate{Provider: e.Provider, Model: e.Model})
		}
		for _, c := range candidates {
			out[c.Model] = candidates
		}
	}
	return out
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
func newReloader(store *configStore, providersPath, teamsPath string) admin.Reloader {
	return func(ctx context.Context) (admin.ReloadSummary, error) {
		next, providerCount, teamCount, err := loadLiveConfig(providersPath, teamsPath)
		if err != nil {
			return admin.ReloadSummary{}, err
		}
		store.current.Store(next)
		return admin.ReloadSummary{Providers: providerCount, Teams: teamCount}, nil
	}
}
