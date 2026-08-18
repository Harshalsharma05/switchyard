package main

import (
	"time"

	"github.com/Harshalsharma05/switchyard/internal/admin"
	"github.com/Harshalsharma05/switchyard/internal/proxy"
)

// chaosAdapter presents proxy.Chaos to internal/admin.
//
// It exists purely to translate between admin.ChaosRuleSpec and
// proxy.ChaosRule. internal/admin may not import internal/proxy — CLAUDE.md
// allows only cmd/ to know about both — so each side declares its own rule
// type and this, in the one package permitted to see them together, converts.
//
// It is the same shape as the Middleware values already passed into
// admin.NewRouter: composition happens here, not in either package.
type chaosAdapter struct {
	chaos *proxy.Chaos
}

func (a chaosAdapter) Available() bool { return a.chaos.Available() }

func (a chaosAdapter) Rules() []admin.ChaosRuleSpec {
	rules := a.chaos.Rules()
	out := make([]admin.ChaosRuleSpec, 0, len(rules))
	for _, r := range rules {
		out = append(out, admin.ChaosRuleSpec{
			Provider:  r.Provider,
			Model:     r.Model,
			Mode:      string(r.Mode),
			LatencyMS: int(r.Latency / time.Millisecond),
		})
	}
	return out
}

func (a chaosAdapter) SetRules(specs []admin.ChaosRuleSpec) error {
	rules := make([]proxy.ChaosRule, 0, len(specs))
	for _, s := range specs {
		rules = append(rules, proxy.ChaosRule{
			Provider: s.Provider,
			Model:    s.Model,
			Mode:     proxy.ChaosMode(s.Mode),
			Latency:  time.Duration(s.LatencyMS) * time.Millisecond,
		})
	}
	// Validation stays in proxy.Chaos rather than being duplicated here: an
	// unknown mode or a latency rule with no latency is rejected there, so
	// this adapter never has to know what a valid rule looks like.
	return a.chaos.SetRules(rules)
}

func (a chaosAdapter) Clear() error { return a.chaos.Clear() }
