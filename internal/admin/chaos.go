package admin

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Step 7.5's operator controls for the fault-injection harness.
//
// Mounted on the admin listener only, which is the first of the two guards
// the plan asks for: nothing reachable from the port serving customer traffic
// can turn fault injection on, as a matter of which socket the route is bound
// to rather than of routing convention. The second guard lives in the harness
// itself — see proxy.NewChaos — and is the one that actually makes "never in
// a non-dev environment" true, because it cannot be flipped at runtime by any
// call, including these.

// ChaosController is the slice of proxy.Chaos this package needs.
//
// Declared here rather than imported because internal/admin must not import
// internal/proxy — only cmd/ may know about both, the same rule the
// Middleware type at the top of router.go exists for.
type ChaosController interface {
	// Available reports whether the harness may be used in this environment.
	Available() bool

	// Rules returns the active rules.
	Rules() []ChaosRuleSpec

	// SetRules validates and replaces the whole rule set.
	SetRules(rules []ChaosRuleSpec) error

	// Clear removes every rule.
	Clear() error
}

// ChaosRuleSpec is one targeting rule as it crosses the admin boundary.
//
// It is a distinct type from proxy.ChaosRule, and cmd/gateway translates
// between them. That is a little more wiring than sharing one struct, and it
// is what keeps this package free of an import it is not allowed to have —
// the same trade newTransitionView makes for health.Transition, just across a
// boundary that is enforced rather than stylistic.
type ChaosRuleSpec struct {
	Provider  string
	Model     string
	Mode      string
	LatencyMS int
}

// chaosRuleView is one rule's wire shape.
type chaosRuleView struct {
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	Mode      string `json:"mode"`
	LatencyMS int    `json:"latency_ms,omitempty"`
}

// chaosView is GET /admin/chaos's response: whether the harness is usable
// here, and what it is currently doing.
type chaosView struct {
	Available bool            `json:"available"`
	Rules     []chaosRuleView `json:"rules"`
}

// chaosRequest is the POST body.
type chaosRequest struct {
	Rules []chaosRuleView `json:"rules"`
}

// maxChaosBodyBytes bounds the POST body. Small, because a rule set is a
// handful of short strings and nothing legitimate approaches it.
const maxChaosBodyBytes = 64 << 10

func newChaosRuleView(r ChaosRuleSpec) chaosRuleView {
	return chaosRuleView{Provider: r.Provider, Model: r.Model, Mode: r.Mode, LatencyMS: r.LatencyMS}
}

func (v chaosRuleView) toSpec() ChaosRuleSpec {
	return ChaosRuleSpec{Provider: v.Provider, Model: v.Model, Mode: v.Mode, LatencyMS: v.LatencyMS}
}

// getChaos serves GET /admin/chaos.
func getChaos(chaos ChaosController, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !chaosAvailable(w, chaos, log) {
			return
		}

		rules := chaos.Rules()
		views := make([]chaosRuleView, 0, len(rules))
		for _, rule := range rules {
			views = append(views, newChaosRuleView(rule))
		}
		writeJSON(w, log, http.StatusOK, chaosView{Available: true, Rules: views})
	}
}

// setChaos serves POST /admin/chaos, replacing the whole rule set.
func setChaos(chaos ChaosController, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !chaosAvailable(w, chaos, log) {
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxChaosBodyBytes)

		var req chaosRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, log, http.StatusBadRequest, "invalid_request_error",
				"request body is not valid JSON: "+err.Error())
			return
		}

		specs := make([]ChaosRuleSpec, 0, len(req.Rules))
		for _, v := range req.Rules {
			specs = append(specs, v.toSpec())
		}

		// Availability was checked above and cannot change afterwards — the
		// harness fixes it at construction — so every error reaching here is
		// a rule the operator got wrong.
		if err := chaos.SetRules(specs); err != nil {
			writeError(w, log, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}

		// Step 4.3's rule that every mutation is logged, and doubly warranted
		// here: this one changes whether the gateway tells the truth about its
		// providers.
		log.WarnContext(r.Context(), "chaos rules set", slog.Int("rules", len(specs)))

		views := make([]chaosRuleView, 0, len(specs))
		for _, rule := range specs {
			views = append(views, newChaosRuleView(rule))
		}
		writeJSON(w, log, http.StatusOK, chaosView{Available: true, Rules: views})
	}
}

// deleteChaos serves DELETE /admin/chaos, returning the gateway to normal.
func deleteChaos(chaos ChaosController, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !chaosAvailable(w, chaos, log) {
			return
		}

		if err := chaos.Clear(); err != nil {
			writeError(w, log, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}

		log.WarnContext(r.Context(), "chaos rules cleared")
		writeJSON(w, log, http.StatusOK, chaosView{Available: true, Rules: []chaosRuleView{}})
	}
}

// chaosAvailable writes the 404 and reports false when the harness is off.
func chaosAvailable(w http.ResponseWriter, chaos ChaosController, log *slog.Logger) bool {
	if chaos == nil || !chaos.Available() {
		writeChaosUnavailable(w, log)
		return false
	}
	return true
}

// writeChaosUnavailable answers as though the route did not exist.
//
// 404 rather than 403: a 403 would confirm that a chaos endpoint is there and
// merely refusing, which tells anyone probing a production gateway that the
// capability exists and is worth attacking. A 404 is the same answer they
// would get from a build that never had it.
func writeChaosUnavailable(w http.ResponseWriter, log *slog.Logger) {
	writeError(w, log, http.StatusNotFound, "not_found", "no such endpoint")
}
