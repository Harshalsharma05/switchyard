package resilience

import (
	"github.com/Harshalsharma05/switchyard/internal/health"
)

// Candidate is one option in a fallback chain: a model and the provider
// instance that serves it. Provider is carried alongside Model rather than
// re-derived from it because every rule below — the allowlist check, the
// health check, the log line naming what was tried — is about the provider,
// not the model.
type Candidate struct {
	Provider string
	Model    string
}

// ChainInput is everything BuildChain needs to order a request's options.
//
// The two policy inputs are functions rather than data because the caller is
// the only thing that knows them: health lives in a Monitor that changes
// under its own goroutine, and the allowlist belongs to whichever team
// authenticated on this specific request.
type ChainInput struct {
	// Requested is the model the caller asked for, and the provider that
	// serves it. It heads the chain whenever nothing below disqualifies it.
	Requested Candidate

	// Tier is the requested model's tier from configs/providers.yaml, in
	// declared order. Empty for a model that belongs to no tier: that request
	// simply has no fallback, which is how every request behaved before this
	// step existed.
	Tier []Candidate

	// StatusOf reports a provider's current health. A nil func treats every
	// provider as healthy — the same optimistic default CLAUDE.md mandates
	// for a stale health checker, and the reason a health outage degrades
	// ordering rather than routing.
	StatusOf func(providerName string) health.Status

	// Allowed reports whether the calling team may use this provider with
	// this model. A nil func allows everything, which is only ever the case
	// in tests: the real call site always passes the authenticated team's
	// allowlists.
	Allowed func(providerName, model string) bool
}

// BuildChain turns a request into the ordered list of candidates to try.
//
// It is a pure function — no I/O, no clock, no logger — which is what makes
// Step 6.2's ordering rules testable without a registry, a provider, or a
// Redis. Walking the chain (retrying each entry, recording what failed) is
// internal/proxy's job; deciding what the chain *is* happens here.
//
// The rules, in the order they apply:
//
//  1. The requested model heads the list, then its tier in declared order.
//     Duplicates collapse, so a tier that also lists the requested model
//     does not try it twice.
//  2. Anything the team's allowlist forbids is dropped outright. This
//     happens before the health pass and is never undone — an unpermitted
//     provider stays unpermitted even when it is the only one left standing.
//     That is Step 6.2's compliance point: availability never overrides
//     authorization.
//  3. Providers that are Down are dropped, and Degraded ones sink below
//     Healthy ones while keeping their relative order. A degraded provider
//     is still answering requests, so it is worth trying after the healthy
//     options rather than being excluded with the dead ones.
//  4. If rule 3 empties the chain — every permitted provider is Down —
//     the permitted list comes back unfiltered. Health is a signal, not a
//     verdict: a request that might succeed against a provider the monitor
//     believes is down beats a request the gateway refused to attempt.
//
// A nil return means rule 2 left nothing: this team may use none of the
// options, and the caller must return an error rather than route somewhere
// it isn't permitted.
func BuildChain(in ChainInput) []Candidate {
	ordered := make([]Candidate, 0, len(in.Tier)+1)
	seen := make(map[Candidate]struct{}, len(in.Tier)+1)

	// A struct key works here because Candidate is two comparable fields;
	// Go maps accept any comparable type, so no separate string join is
	// needed to dedupe on the pair.
	add := func(c Candidate) {
		if c.Provider == "" || c.Model == "" {
			return
		}
		if _, dup := seen[c]; dup {
			return
		}
		seen[c] = struct{}{}
		ordered = append(ordered, c)
	}

	add(in.Requested)
	for _, c := range in.Tier {
		add(c)
	}

	permitted := make([]Candidate, 0, len(ordered))
	for _, c := range ordered {
		if in.Allowed != nil && !in.Allowed(c.Provider, c.Model) {
			continue
		}
		permitted = append(permitted, c)
	}
	if len(permitted) == 0 {
		return nil
	}

	statusOf := in.StatusOf
	if statusOf == nil {
		statusOf = func(string) health.Status { return health.StatusHealthy }
	}

	// Two passes rather than a sort: with exactly two surviving statuses,
	// this is both shorter than a sort.SliceStable comparator and stable by
	// construction, so configs/providers.yaml's declared order is preserved
	// within each status band.
	chain := make([]Candidate, 0, len(permitted))
	for _, band := range []health.Status{health.StatusHealthy, health.StatusDegraded} {
		for _, c := range permitted {
			if statusOf(c.Provider) == band {
				chain = append(chain, c)
			}
		}
	}

	if len(chain) == 0 {
		return permitted
	}
	return chain
}
