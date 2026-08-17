// Package budget computes and enforces per-team spend.
//
// It knows nothing about HTTP, teams, or providers beyond what it needs to
// turn token counts into a price: internal/proxy decides when a request's
// cost is computed and, from Step 4.2 onward, what happens once a team's
// spend crosses a threshold. This file only answers "what does this many
// tokens of this model cost."
package budget

import "fmt"

// microsPerUnit is the price table's own scale: input/output prices are
// integer micro-dollars per 1,000,000 tokens, matching internal/config's
// conversion of providers.yaml's decimal prices. Naming it documents where
// the final division in Cost comes from, rather than leaving a bare
// 1_000_000 unexplained at the call site.
const microsPerUnit = 1_000_000

// Pricing is one model's cost, in integer micro-dollars per 1,000,000 tokens.
//
// Deliberately not config.ModelPricing reused: this package does not know how
// the number reached it, and reusing config's YAML-loading type here would
// leak a loading concern into a calculation concern. internal/config and this
// package agree only on the shape, at the call site in cmd/gateway that
// builds a Calculator.
type Pricing struct {
	InputPer1M  int64
	OutputPer1M int64
}

// Calculator turns token usage into cost.
//
// It is built once, from the same pricing table internal/config resolved at
// boot, and never mutated afterwards — concurrent reads need no lock, the
// same reasoning provider.Registry gives for its own read-only maps.
type Calculator struct {
	pricing map[string]Pricing
}

// NewCalculator builds a Calculator from a model-name-keyed pricing table.
// The map is copied so a caller mutating its own map after construction can
// never change prices out from under a Calculator already handed to a
// Handler.
func NewCalculator(pricing map[string]Pricing) *Calculator {
	cp := make(map[string]Pricing, len(pricing))
	for model, p := range pricing {
		cp[model] = p
	}
	return &Calculator{pricing: cp}
}

// Cost returns a request's price in integer micro-dollars:
// (inputTokens × InputPer1M + outputTokens × OutputPer1M) / 1,000,000.
//
// The division truncates rather than rounds. At micro-dollar resolution the
// error that discards is at most 0.999999 of a micro-dollar per request — six
// orders of magnitude below a cent — so truncation versus rounding is not a
// meaningful choice here. What matters, per CLAUDE.md's rule that cost is
// never a float, is that every step of the computation stays integer
// arithmetic.
//
// An unpriced model is reported as an error rather than silently costing
// zero. Every model the registry can resolve has a pricing entry by
// construction — internal/config populates both a provider's Models list and
// its Pricing map from the same providers.yaml entry — so a miss here means
// the caller resolved a model this Calculator was never built to know about:
// a wiring bug between the registry and the pricing table it was built from,
// not a real pricing gap.
func (c *Calculator) Cost(model string, inputTokens, outputTokens int) (int64, error) {
	p, ok := c.pricing[model]
	if !ok {
		return 0, fmt.Errorf("no pricing configured for model %q", model)
	}
	return (int64(inputTokens)*p.InputPer1M + int64(outputTokens)*p.OutputPer1M) / microsPerUnit, nil
}
