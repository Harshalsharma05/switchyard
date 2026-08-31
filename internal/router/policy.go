package router

import "github.com/Harshalsharma05/switchyard/internal/provider"

// AutoModel is the model value that asks SwitchYard to choose. It is a
// protocol keyword, not a model name — startup rejects a real model that
// collides with it, and with either tier name.
const AutoModel = "auto"

// Policy maps a complexity level onto a tier name from providers.yaml. This
// is the only place the classifier's vocabulary and the config's meet, which
// is what keeps model and tier names out of the classifier entirely.
type Policy struct {
	Simple  string
	Complex string
}

// Tier names the tier a level routes to.
func (p Policy) Tier(c Complexity) string {
	if c == Complex {
		return p.Complex
	}
	return p.Simple
}

// Escalate names the tier to try when Tier has no permitted candidate. Only
// upward: routing up costs more and never costs capability, so it is the safe
// direction to fail in.
func (p Policy) Escalate(tier string) (string, bool) {
	if tier == p.Simple && p.Complex != "" && p.Complex != p.Simple {
		return p.Complex, true
	}
	return "", false
}

// Plan is the routing decision for one request.
type Plan struct {
	Tier  string
	Level Complexity

	// Pinned means the caller named the tier themselves, which forbids
	// escalating out of it: explicit beats inferred.
	Pinned bool

	Reason string
}

// Router turns a caller's model field into a routing plan.
type Router struct {
	classifier *Classifier
	policy     Policy
}

func New(c *Classifier, p Policy) *Router { return &Router{classifier: c, policy: p} }

func (r *Router) Policy() Policy { return r.policy }

// Options lists the values a caller may put in `model` to ask for routing.
// Nil-receiver safe, so a gateway with routing disabled reports none rather
// than needing the wiring to branch.
func (r *Router) Options() []string {
	if r == nil {
		return nil
	}
	return []string{AutoModel, r.policy.Simple, r.policy.Complex}
}

// Routes reports whether model asks to be routed at all. Separate from Plan so
// an explicitly named model never pays for building a canonical request.
func (r *Router) Routes(model string) bool {
	if model == AutoModel {
		return true
	}
	return model != "" && (model == r.policy.Simple || model == r.policy.Complex)
}

// Plan decides which tier serves this request. ok is false when the caller
// named a real model: an explicit model is never overridden, which is a
// correctness rule rather than a tuning choice.
func (r *Router) Plan(model string, req provider.Request) (Plan, bool) {
	if !r.Routes(model) {
		return Plan{}, false
	}

	if model != AutoModel {
		return Plan{Tier: model, Level: r.levelOf(model), Pinned: true, Reason: "tier=" + model + " pinned"}, true
	}

	d := r.classifier.Classify(req)
	return Plan{Tier: r.policy.Tier(d.Level), Level: d.Level, Reason: d.Reason()}, true
}

func (r *Router) levelOf(tier string) Complexity {
	if tier == r.policy.Complex {
		return Complex
	}
	return Simple
}
