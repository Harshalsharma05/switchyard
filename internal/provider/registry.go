package provider

import (
	"errors"
	"fmt"
)

// Adapter type identifiers. These name Go implementations, not vendors — which
// is why "groq" is not among them: Groq is an instance of type
// TypeOpenAICompatible, configured in configs/providers.yaml.
const (
	TypeOpenAICompatible = "openai-compatible"
	TypeAnthropic        = "anthropic"
	TypeGemini           = "gemini"
	TypeOllama           = "ollama"
)

// ErrModelNotSupported is returned when no configured instance serves a model.
// It is a sentinel so the Step 1.5 handler can map it to a 404 rather than
// matching on an error string.
var ErrModelNotSupported = errors.New("no provider serves this model")

// ErrProviderNotFound is returned when no instance carries a given name.
var ErrProviderNotFound = errors.New("no such provider")

// Registry resolves a model name to the provider instance that serves it.
//
// It is built once at boot and never mutated afterwards, so concurrent reads
// need no lock. Phase 4's hot reload will swap a whole new Registry in behind
// an atomic pointer rather than mutating this one — in-flight requests keep the
// registry they started with.
type Registry struct {
	byName  map[string]Provider
	byModel map[string]Provider

	// order preserves config file order so admin listings and logs are stable
	// rather than following Go's randomized map iteration.
	order []Provider
}

// NewRegistry constructs every instance and indexes it.
//
// It fails on the first problem rather than accumulating a partial registry: a
// gateway that starts with a silently missing provider is worse than one that
// refuses to start and says why.
func NewRegistry(cfgs []Config) (*Registry, error) {
	if len(cfgs) == 0 {
		return nil, errors.New("provider registry: no providers configured")
	}

	r := &Registry{
		byName:  make(map[string]Provider, len(cfgs)),
		byModel: make(map[string]Provider),
		order:   make([]Provider, 0, len(cfgs)),
	}

	for _, cfg := range cfgs {
		if _, dup := r.byName[cfg.Name]; dup {
			return nil, fmt.Errorf("provider registry: duplicate provider name %q", cfg.Name)
		}

		p, err := newAdapter(cfg)
		if err != nil {
			return nil, fmt.Errorf("provider registry: %w", err)
		}

		for _, model := range cfg.Models {
			// A model must resolve to exactly one instance, or ForModel has no
			// defensible answer. Serving one model from several providers is
			// what Phase 6's fallback tiers are for.
			if existing, dup := r.byModel[model]; dup {
				return nil, fmt.Errorf(
					"provider registry: model %q is served by both %q and %q; a model must map to one provider",
					model, existing.Name(), cfg.Name,
				)
			}
			r.byModel[model] = p
		}

		r.byName[cfg.Name] = p
		r.order = append(r.order, p)
	}

	return r, nil
}

// ForModel resolves a model name to its provider.
func (r *Registry) ForModel(model string) (Provider, error) {
	p, ok := r.byModel[model]
	if !ok {
		return nil, fmt.Errorf("resolving model %q: %w", model, ErrModelNotSupported)
	}
	return p, nil
}

// ByName resolves an instance name, for the Phase 4 admin API and the Phase 5
// health checker.
func (r *Registry) ByName(name string) (Provider, error) {
	p, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("resolving provider %q: %w", name, ErrProviderNotFound)
	}
	return p, nil
}

// Providers returns every instance in config file order.
func (r *Registry) Providers() []Provider {
	out := make([]Provider, len(r.order))
	copy(out, r.order)
	return out
}

// Models returns every model the registry can serve. The Step 1.5 handler uses
// it to tell a caller what is available when resolution fails.
func (r *Registry) Models() []string {
	out := make([]string, 0, len(r.byModel))
	for _, p := range r.order {
		for model, owner := range r.byModel {
			if owner == p {
				out = append(out, model)
			}
		}
	}
	return out
}

// newAdapter constructs the implementation named by cfg.Type.
//
// Each branch assigns to a concrete variable and checks the error before
// returning, rather than returning the constructor's results directly. That is
// deliberate: returning a nil *OpenAICompatible into the Provider interface
// would produce an interface value that is not equal to nil, and any later
// `if p == nil` check would quietly fail to fire.
func newAdapter(cfg Config) (Provider, error) {
	switch cfg.Type {
	case TypeOpenAICompatible:
		p, err := NewOpenAICompatible(cfg)
		if err != nil {
			return nil, err
		}
		return p, nil

	case TypeAnthropic:
		p, err := NewAnthropic(cfg)
		if err != nil {
			return nil, err
		}
		return p, nil

	case TypeGemini:
		p, err := NewGemini(cfg)
		if err != nil {
			return nil, err
		}
		return p, nil

	case TypeOllama:
		p, err := NewOllama(cfg)
		if err != nil {
			return nil, err
		}
		return p, nil

	default:
		return nil, fmt.Errorf(
			"provider %q: unknown type %q (want one of %s, %s, %s, %s)",
			cfg.Name, cfg.Type,
			TypeOpenAICompatible, TypeAnthropic, TypeGemini, TypeOllama,
		)
	}
}
