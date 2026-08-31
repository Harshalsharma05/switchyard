package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Harshalsharma05/switchyard/internal/auth"
	"github.com/Harshalsharma05/switchyard/internal/provider"
	"github.com/Harshalsharma05/switchyard/internal/resilience"
	"github.com/Harshalsharma05/switchyard/internal/router"
)

// Step 8.2's three non-negotiables — explicit beats inferred, allowlists win,
// health gates — plus the escalation rule. These are the correctness claims
// the feature rests on, so they are tested through the real handler with a
// real classifier rather than against route() directly.

// testComplexityRouter classifies on one word so the tests read as routing
// tests rather than as classifier tests. Length weight is zero, so only the
// reasoning lexicon can move a prompt up a tier.
func testComplexityRouter() *router.Router {
	return router.New(router.NewClassifier(router.ClassifierConfig{
		Threshold: 1.0,
		Weights:   router.Weights{Reasoning: 1.0},
		Scales:    router.Scales{LengthTokens: 1, ReasoningMatches: 1, ConstraintMatches: 1, FormatMatches: 1},
		Reasoning: router.Lexicon{"analyze"},
	}), router.Policy{Simple: "fast", Complex: "frontier"})
}

// newRoutedServer wires two named tiers over three mocks: "fast" holds
// cheap-a then cheap-b, "frontier" holds dear-a alone.
func newRoutedServer(t *testing.T, team *auth.Team, breakers Breakers) (*httptest.Server, map[string]*provider.Mock) {
	t.Helper()

	mocks := map[string]*provider.Mock{
		"cheap-a": okMock("groq", "cheap-a"),
		"cheap-b": okMock("ollama", "cheap-b"),
		"dear-a":  okMock("groq", "dear-a"),
	}
	byModel := make(map[string]provider.Provider, len(mocks))
	for model, p := range mocks {
		byModel[model] = p
	}

	resolver := stubResolver{
		byModel: byModel,
		tiersByName: map[string][]resilience.Candidate{
			"fast": {
				{Provider: "groq", Model: "cheap-a"},
				{Provider: "ollama", Model: "cheap-b"},
			},
			"frontier": {{Provider: "groq", Model: "dear-a"}},
		},
	}

	srv := httptest.NewServer(NewRouter(resolver, stubAuthenticator{team: team}, stubRateLimiter{},
		stubBudgetTracker{}, stubCostCalculator{}, stubHealthRecorder{}, nil, breakers, nil,
		noRetryConfig(t), nil, nil, discardLogger(), func() bool { return true },
		WithRouting(testComplexityRouter())))
	t.Cleanup(srv.Close)
	return srv, mocks
}

func TestRouting(t *testing.T) {
	permissive := &auth.Team{
		ID:               "permissive",
		AllowedProviders: []string{"groq", "ollama"},
		AllowedModels:    []string{"cheap-a", "cheap-b", "dear-a"},
	}
	// Forbids the fast tier's head, so a router that ignored the allowlist
	// would visibly serve cheap-a.
	ollamaOnly := &auth.Team{
		ID:               "ollama-only",
		AllowedProviders: []string{"ollama"},
		AllowedModels:    []string{"cheap-b"},
	}
	// Permitted nothing at all in the fast tier.
	frontierOnly := &auth.Team{
		ID:               "frontier-only",
		AllowedProviders: []string{"groq"},
		AllowedModels:    []string{"dear-a"},
	}

	simple := `{"model":"%s","messages":[{"role":"user","content":"hi"}]}`
	complex := `{"model":"%s","messages":[{"role":"user","content":"analyze this"}]}`

	cases := map[string]struct {
		team        *auth.Team
		openBreaker *resilience.Candidate
		model       string
		prompt      string
		wantStatus  int
		wantModel   string

		// wantTier is the X-Switchyard-Route-Tier value; empty means the
		// routing headers must be absent entirely.
		wantTier string
	}{
		"simple prompt routes down": {
			team: permissive, model: "auto", prompt: simple,
			wantStatus: http.StatusOK, wantModel: "cheap-a", wantTier: "fast",
		},
		"complex prompt routes up": {
			team: permissive, model: "auto", prompt: complex,
			wantStatus: http.StatusOK, wantModel: "dear-a", wantTier: "frontier",
		},
		// The rule that makes routing safe to ship at all.
		"explicit model is never downgraded": {
			team: permissive, model: "dear-a", prompt: simple,
			wantStatus: http.StatusOK, wantModel: "dear-a", wantTier: "",
		},
		"a pinned tier skips the classifier": {
			team: permissive, model: "frontier", prompt: simple,
			wantStatus: http.StatusOK, wantModel: "dear-a", wantTier: "frontier",
		},
		"routing never violates the allowlist": {
			team: ollamaOnly, model: "auto", prompt: simple,
			wantStatus: http.StatusOK, wantModel: "cheap-b", wantTier: "fast",
		},
		"an open breaker removes a candidate": {
			team: permissive, model: "auto", prompt: simple,
			openBreaker: &resilience.Candidate{Provider: "groq", Model: "cheap-a"},
			wantStatus:  http.StatusOK, wantModel: "cheap-b", wantTier: "fast",
		},
		// An inferred tier may escalate when the team may use none of it.
		"an empty inferred tier escalates": {
			team: frontierOnly, model: "auto", prompt: simple,
			wantStatus: http.StatusOK, wantModel: "dear-a", wantTier: "frontier",
		},
		// A pinned one may not: the caller named it, so they get its answer.
		"a pinned empty tier does not escalate": {
			team: frontierOnly, model: "fast", prompt: simple,
			wantStatus: http.StatusForbidden,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Left a nil interface when unset: a typed nil pointer inside a
			// non-nil interface would defeat the handler's own nil check.
			var breakers Breakers
			if tc.openBreaker != nil {
				reg := newProxyBreakerRegistry(t)
				openBreakerFor(t, reg, *tc.openBreaker)
				breakers = reg
			}

			srv, mocks := newRoutedServer(t, tc.team, breakers)
			resp := post(t, srv, fmt.Sprintf(tc.prompt, tc.model))

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantModel == "" {
				return
			}

			if got := resp.Header.Get(HeaderServedModel); got != tc.wantModel {
				t.Errorf("%s = %q, want %q", HeaderServedModel, got, tc.wantModel)
			}
			if got := mocks[tc.wantModel].Attempts(); got != 1 {
				t.Errorf("%s attempts = %d, want 1", tc.wantModel, got)
			}
			// The caller's own value survives, so the response says both what
			// was asked for and what was decided.
			if got := resp.Header.Get(HeaderRequestedModel); got != tc.model {
				t.Errorf("%s = %q, want %q", HeaderRequestedModel, got, tc.model)
			}

			// Step 8.3: a routed request always says which tier and why; an
			// unrouted one says nothing rather than reporting a false negative.
			if got := resp.Header.Get(HeaderRouteTier); got != tc.wantTier {
				t.Errorf("%s = %q, want %q", HeaderRouteTier, got, tc.wantTier)
			}
			reason := resp.Header.Get(HeaderRouteReason)
			if tc.wantTier == "" && reason != "" {
				t.Errorf("%s = %q on an unrouted request, want absent", HeaderRouteReason, reason)
			}
			if tc.wantTier != "" && reason == "" {
				t.Errorf("%s is empty; a routing decision must carry its rationale", HeaderRouteReason)
			}
		})
	}
}

// The other half of routing's hot-path cost, alongside internal/router's
// BenchmarkClassify: tier lookup, chain construction, and the breaker reads.
func BenchmarkSelectCandidate(b *testing.B) {
	h := &Handler{resolver: stubResolver{
		tiersByName: map[string][]resilience.Candidate{
			"fast": {
				{Provider: "groq", Model: "cheap-a"},
				{Provider: "ollama", Model: "cheap-b"},
			},
		},
	}}

	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := h.selectCandidate(ctx, "fast"); err != nil {
			b.Fatal(err)
		}
	}
}

// perModelCalc prices each model differently, which stubCostCalculator cannot:
// the whole point of a savings figure is the gap between two models' prices.
type perModelCalc map[string]int64

func (c perModelCalc) Cost(model string, _, _ int) (int64, error) {
	micros, ok := c[model]
	if !ok {
		return 0, fmt.Errorf("no pricing for %q", model)
	}
	return micros, nil
}

// Step 8.4's headline number. An error here is a wrong figure on the Usage &
// Cost screen, which is worse than no figure — hence the nil-vs-zero cases.
func TestRecordRoutingSavings(t *testing.T) {
	calc := perModelCalc{"cheap": 40, "dear": 100}

	cases := map[string]struct {
		tier, baseline string
		actualCost     int64
		want           *int64
	}{
		"downgraded saves the price gap": {
			tier: "fast", baseline: "dear", actualCost: 40, want: ptr(int64(60)),
		},
		// Routing ran and chose the top tier: it saved nothing, which is a
		// different fact from never having run.
		"routed to the top tier saves zero": {
			tier: "frontier", baseline: "dear", actualCost: 100, want: ptr(int64(0)),
		},
		"unrouted records nothing": {
			tier: "", baseline: "", actualCost: 40, want: nil,
		},
		// Absent is honest; zero would claim routing saved nothing when the
		// truth is the gateway could not work it out.
		"unpriceable baseline records nothing": {
			tier: "fast", baseline: "gone-from-config", actualCost: 40, want: nil,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := &Handler{calc: calc, log: discardLogger()}
			m := &requestMetrics{
				routedTier:         tc.tier,
				routeBaselineModel: tc.baseline,
				costMicros:         tc.actualCost,
			}

			h.recordRoutingSavings(context.Background(), m, provider.Usage{InputTokens: 10, OutputTokens: 20})

			switch {
			case tc.want == nil && m.routingSavingsMicros != nil:
				t.Errorf("savings = %d, want unrecorded", *m.routingSavingsMicros)
			case tc.want != nil && m.routingSavingsMicros == nil:
				t.Errorf("savings unrecorded, want %d", *tc.want)
			case tc.want != nil && *m.routingSavingsMicros != *tc.want:
				t.Errorf("savings = %d, want %d", *m.routingSavingsMicros, *tc.want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
