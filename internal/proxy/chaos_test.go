package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/auth"
	"github.com/Harshalsharma05/switchyard/internal/provider"
)

func devChaos(t *testing.T) *Chaos {
	t.Helper()
	return NewChaos(devEnvironment, true, discardLogger())
}

// --- the guard ---------------------------------------------------------------

// TestChaosRequiresBothDevEnvAndTheFlag is Step 7.5's headline safety
// property, and the checklist item "chaos endpoint refuses to enable without
// the explicit dev env flag." Neither condition on its own is sufficient.
func TestChaosRequiresBothDevEnvAndTheFlag(t *testing.T) {
	tests := map[string]struct {
		env     string
		enabled bool
		want    bool
	}{
		"dev and enabled":         {env: "dev", enabled: true, want: true},
		"dev but not enabled":     {env: "dev", enabled: false},
		"enabled but production":  {env: "production", enabled: true},
		"enabled but staging":     {env: "staging", enabled: true},
		"enabled but empty env":   {env: "", enabled: true},
		"neither":                 {env: "production", enabled: false},
		"near-miss env name":      {env: "development", enabled: true},
		"case-sensitive env name": {env: "DEV", enabled: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := NewChaos(tt.env, tt.enabled, discardLogger())
			if got := c.Available(); got != tt.want {
				t.Errorf("Available() = %v, want %v for env %q enabled %v", got, tt.want, tt.env, tt.enabled)
			}
		})
	}
}

// TestUnavailableChaosRefusesEveryMutation proves the guard is not merely
// advisory: there is no method that turns an unavailable harness on, so a
// call site that forgets to check Available cannot inject anything.
func TestUnavailableChaosRefusesEveryMutation(t *testing.T) {
	c := NewChaos("production", true, discardLogger())

	rule := ChaosRule{Provider: "groq", Mode: ChaosError}
	if err := c.SetRules([]ChaosRule{rule}); !errors.Is(err, ErrChaosUnavailable) {
		t.Errorf("SetRules() error = %v, want ErrChaosUnavailable", err)
	}
	if err := c.Clear(); !errors.Is(err, ErrChaosUnavailable) {
		t.Errorf("Clear() error = %v, want ErrChaosUnavailable", err)
	}
	if got := c.Rules(); len(got) != 0 {
		t.Errorf("Rules() = %v, want none", got)
	}
	if err := c.Apply(context.Background(), "groq", "m"); err != nil {
		t.Errorf("Apply() = %v, want nil — an unavailable harness must inject nothing", err)
	}
}

// TestNilChaosIsANoOp covers the handler built without a harness at all.
func TestNilChaosIsANoOp(t *testing.T) {
	var c *Chaos

	if c.Available() {
		t.Errorf("Available() = true on a nil harness, want false")
	}
	if err := c.Apply(context.Background(), "groq", "m"); err != nil {
		t.Errorf("Apply() = %v, want nil", err)
	}
	if got := c.Rules(); got != nil {
		t.Errorf("Rules() = %v, want nil", got)
	}
}

// --- rule validation ----------------------------------------------------------

func TestChaosRuleValidation(t *testing.T) {
	tests := map[string]struct {
		rule    ChaosRule
		wantErr bool
	}{
		"provider only":       {rule: ChaosRule{Provider: "groq", Mode: ChaosError}},
		"model only":          {rule: ChaosRule{Model: "m", Mode: ChaosDrop}},
		"provider and model":  {rule: ChaosRule{Provider: "groq", Model: "m", Mode: ChaosRateLimit}},
		"latency with a wait": {rule: ChaosRule{Provider: "groq", Mode: ChaosLatency, Latency: time.Second}},

		// Neither field set would silently target every call the gateway
		// makes, which is not something a fat-fingered rule should be able
		// to do even in dev.
		"untargeted":            {rule: ChaosRule{Mode: ChaosError}, wantErr: true},
		"unknown mode":          {rule: ChaosRule{Provider: "groq", Mode: "explode"}, wantErr: true},
		"empty mode":            {rule: ChaosRule{Provider: "groq"}, wantErr: true},
		"latency without wait":  {rule: ChaosRule{Provider: "groq", Mode: ChaosLatency}, wantErr: true},
		"negative latency":      {rule: ChaosRule{Provider: "groq", Mode: ChaosLatency, Latency: -time.Second}, wantErr: true},
		"latency on error mode": {rule: ChaosRule{Provider: "groq", Mode: ChaosError, Latency: time.Second}, wantErr: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := tt.rule.validate()
			if tt.wantErr && err == nil {
				t.Fatalf("validate() = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validate() unexpected error: %v", err)
			}
		})
	}
}

// TestSetRulesIsAllOrNothing proves a bad rule at the end cannot leave half a
// rule set applied.
func TestSetRulesIsAllOrNothing(t *testing.T) {
	c := devChaos(t)

	good := ChaosRule{Provider: "groq", Mode: ChaosError}
	if err := c.SetRules([]ChaosRule{good}); err != nil {
		t.Fatalf("SetRules() error: %v", err)
	}

	bad := []ChaosRule{{Provider: "ollama", Mode: ChaosDrop}, {Provider: "groq", Mode: "nonsense"}}
	if err := c.SetRules(bad); err == nil {
		t.Fatalf("SetRules() = nil, want an error for the invalid rule")
	}

	rules := c.Rules()
	if len(rules) != 1 || rules[0].Provider != "groq" || rules[0].Mode != ChaosError {
		t.Errorf("rules = %+v, want the previous set left untouched", rules)
	}
}

// TestRulesReturnsACopy proves a caller cannot edit live rules behind the
// validation.
func TestRulesReturnsACopy(t *testing.T) {
	c := devChaos(t)
	if err := c.SetRules([]ChaosRule{{Provider: "groq", Mode: ChaosError}}); err != nil {
		t.Fatalf("SetRules() error: %v", err)
	}

	got := c.Rules()
	got[0].Mode = "tampered"

	if after := c.Rules(); after[0].Mode != ChaosError {
		t.Errorf("mode = %q, want the live rule unchanged by an edit to the returned slice", after[0].Mode)
	}
}

// --- targeting and modes --------------------------------------------------------

// TestChaosTargeting is the plan's "targetable at a specific provider or
// model," including the wildcard behaviour of an empty field.
func TestChaosTargeting(t *testing.T) {
	tests := map[string]struct {
		rule          ChaosRule
		provider      string
		model         string
		wantIntercept bool
	}{
		"provider matches":           {rule: ChaosRule{Provider: "groq", Mode: ChaosError}, provider: "groq", model: "a", wantIntercept: true},
		"provider does not match":    {rule: ChaosRule{Provider: "groq", Mode: ChaosError}, provider: "ollama", model: "a"},
		"model matches any provider": {rule: ChaosRule{Model: "a", Mode: ChaosError}, provider: "ollama", model: "a", wantIntercept: true},
		"model does not match":       {rule: ChaosRule{Model: "a", Mode: ChaosError}, provider: "groq", model: "b"},
		"both match":                 {rule: ChaosRule{Provider: "groq", Model: "a", Mode: ChaosError}, provider: "groq", model: "a", wantIntercept: true},
		"provider right model wrong": {rule: ChaosRule{Provider: "groq", Model: "a", Mode: ChaosError}, provider: "groq", model: "b"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := devChaos(t)
			if err := c.SetRules([]ChaosRule{tt.rule}); err != nil {
				t.Fatalf("SetRules() error: %v", err)
			}

			err := c.Apply(context.Background(), tt.provider, tt.model)
			if tt.wantIntercept && err == nil {
				t.Errorf("Apply(%q, %q) = nil, want an injected fault", tt.provider, tt.model)
			}
			if !tt.wantIntercept && err != nil {
				t.Errorf("Apply(%q, %q) = %v, want nil", tt.provider, tt.model, err)
			}
		})
	}
}

// TestChaosFirstMatchingRuleWins lets an operator put a narrow rule ahead of
// a broad one.
func TestChaosFirstMatchingRuleWins(t *testing.T) {
	c := devChaos(t)
	err := c.SetRules([]ChaosRule{
		{Provider: "groq", Model: "a", Mode: ChaosRateLimit},
		{Provider: "groq", Mode: ChaosError},
	})
	if err != nil {
		t.Fatalf("SetRules() error: %v", err)
	}

	var provErr *provider.Error
	if got := c.Apply(context.Background(), "groq", "a"); !errors.As(got, &provErr) || provErr.Kind != provider.KindRateLimited {
		t.Errorf("groq/a = %v, want the narrower rate-limit rule", got)
	}
	if got := c.Apply(context.Background(), "groq", "b"); !errors.As(got, &provErr) || provErr.Kind != provider.KindServerError {
		t.Errorf("groq/b = %v, want the broader error rule", got)
	}
}

// TestChaosModesForgeTheRightErrorKinds proves each mode produces a
// *provider.Error that the rest of the stack classifies as intended — the
// whole reason a forged failure is useful for testing at all.
func TestChaosModesForgeTheRightErrorKinds(t *testing.T) {
	tests := map[string]struct {
		mode           ChaosMode
		wantKind       provider.Kind
		wantStatus     int
		wantRetryAfter bool
	}{
		"error":      {mode: ChaosError, wantKind: provider.KindServerError, wantStatus: http.StatusBadGateway},
		"rate_limit": {mode: ChaosRateLimit, wantKind: provider.KindRateLimited, wantStatus: http.StatusTooManyRequests, wantRetryAfter: true},
		"drop":       {mode: ChaosDrop, wantKind: provider.KindNetworkError, wantStatus: 0},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := devChaos(t)
			if err := c.SetRules([]ChaosRule{{Provider: "groq", Mode: tt.mode}}); err != nil {
				t.Fatalf("SetRules() error: %v", err)
			}

			err := c.Apply(context.Background(), "groq", "a")

			var provErr *provider.Error
			if !errors.As(err, &provErr) {
				t.Fatalf("Apply() = %v, want a *provider.Error", err)
			}
			if provErr.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", provErr.Kind, tt.wantKind)
			}
			if provErr.StatusCode != tt.wantStatus {
				t.Errorf("StatusCode = %d, want %d", provErr.StatusCode, tt.wantStatus)
			}
			if !provErr.Retryable {
				t.Errorf("Retryable = false, want true — a forged failure must exercise retry and fallback")
			}
			if got := provErr.RetryAfter > 0; got != tt.wantRetryAfter {
				t.Errorf("RetryAfter set = %v, want %v", got, tt.wantRetryAfter)
			}
			if !strings.HasPrefix(provErr.Message, "chaos:") {
				t.Errorf("Message = %q, want a chaos: prefix so it is never mistaken for a real outage", provErr.Message)
			}
		})
	}
}

// TestChaosLatencyDelaysThenProceeds proves latency is a delay, not a
// failure: the call goes through afterwards.
func TestChaosLatencyDelaysThenProceeds(t *testing.T) {
	c := devChaos(t)
	delay := 40 * time.Millisecond
	if err := c.SetRules([]ChaosRule{{Provider: "groq", Mode: ChaosLatency, Latency: delay}}); err != nil {
		t.Fatalf("SetRules() error: %v", err)
	}

	start := time.Now()
	err := c.Apply(context.Background(), "groq", "a")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Apply() = %v, want nil — latency delays the call, it does not fail it", err)
	}
	if elapsed < delay {
		t.Errorf("elapsed = %v, want at least the injected %v", elapsed, delay)
	}
}

// TestChaosLatencyRespectsContextCancellation proves an injected delay cannot
// outlive the request it is delaying.
func TestChaosLatencyRespectsContextCancellation(t *testing.T) {
	c := devChaos(t)
	if err := c.SetRules([]ChaosRule{{Provider: "groq", Mode: ChaosLatency, Latency: 10 * time.Second}}); err != nil {
		t.Fatalf("SetRules() error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := c.Apply(ctx, "groq", "a")
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Errorf("Apply() took %v, want it to abandon the delay when the context expired", elapsed)
	}
	var provErr *provider.Error
	if !errors.As(err, &provErr) || provErr.Kind != provider.KindTimeout {
		t.Errorf("Apply() = %v, want a timeout *provider.Error", err)
	}
}

func TestClearRemovesEveryRule(t *testing.T) {
	c := devChaos(t)
	if err := c.SetRules([]ChaosRule{{Provider: "groq", Mode: ChaosError}}); err != nil {
		t.Fatalf("SetRules() error: %v", err)
	}
	if err := c.Clear(); err != nil {
		t.Fatalf("Clear() error: %v", err)
	}

	if got := c.Rules(); len(got) != 0 {
		t.Errorf("Rules() = %v after Clear, want none", got)
	}
	if err := c.Apply(context.Background(), "groq", "a"); err != nil {
		t.Errorf("Apply() = %v after Clear, want nil", err)
	}
}

// --- end to end ----------------------------------------------------------------

// TestChaosDrivesFallbackEndToEnd is what the harness exists for: breaking
// the primary with a rule makes a real request fail over, with no cooperation
// from the mock provider itself.
func TestChaosDrivesFallbackEndToEnd(t *testing.T) {
	groq := okMock("groq", "fast-a")
	ollama := okMock("ollama", "fast-b")

	chaos := devChaos(t)
	if err := chaos.SetRules([]ChaosRule{{Provider: "groq", Mode: ChaosError}}); err != nil {
		t.Fatalf("SetRules() error: %v", err)
	}

	resolver := stubResolver{
		byModel: map[string]provider.Provider{"fast-a": groq, "fast-b": ollama},
		tier:    fastTier,
	}
	srv := httptest.NewServer(NewRouter(resolver, stubAuthenticator{team: tieredTeam()}, stubRateLimiter{},
		stubBudgetTracker{}, stubCostCalculator{}, stubHealthRecorder{}, nil, nil, chaos,
		noRetryConfig(t), discardLogger(), func() bool { return true }))
	t.Cleanup(srv.Close)

	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via the fallback", resp.StatusCode)
	}
	if got := resp.Header.Get(HeaderProvider); got != "ollama" {
		t.Errorf("%s = %q, want ollama — the injected fault should have failed the primary over", HeaderProvider, got)
	}
	// The primary's own mock is perfectly healthy; only the injected rule
	// stopped it serving. That is the property that makes this a harness
	// rather than a second set of mocks.
	if got := groq.Attempts(); got != 0 {
		t.Errorf("primary was reached %d times, want 0 — chaos intercepts before the provider call", got)
	}
}

// TestChaosFeedsTheHealthRecorder proves an injected failure travels the same
// path a real one does. Without this, chaos would exercise retry and fallback
// but silently leave Phase 5's passive health signal untouched — and the
// demo's "kill a provider, watch health degrade" would not work.
func TestChaosFeedsTheHealthRecorder(t *testing.T) {
	groq := okMock("groq", "fast-a")
	ollama := okMock("ollama", "fast-b")

	chaos := devChaos(t)
	if err := chaos.SetRules([]ChaosRule{{Provider: "groq", Mode: ChaosError}}); err != nil {
		t.Fatalf("SetRules() error: %v", err)
	}

	recorder := &recordingHealthRecorder{}
	resolver := stubResolver{
		byModel: map[string]provider.Provider{"fast-a": groq, "fast-b": ollama},
		tier:    fastTier,
	}
	srv := httptest.NewServer(NewRouter(resolver, stubAuthenticator{team: tieredTeam()}, stubRateLimiter{},
		stubBudgetTracker{}, stubCostCalculator{}, recorder, nil, nil, chaos,
		noRetryConfig(t), discardLogger(), func() bool { return true }))
	t.Cleanup(srv.Close)

	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)
	resp.Body.Close()

	failures := recorder.failuresFor("groq")
	if failures == 0 {
		t.Errorf("health recorder saw no failures for the chaos-broken provider, want at least one")
	}
}

// TestChaosIsNotReachableFromThePublicRouter is the network half of the
// guard: the harness is injected on the request path but exposed nowhere on
// the public listener.
func TestChaosIsNotReachableFromThePublicRouter(t *testing.T) {
	chaos := devChaos(t)
	srv := httptest.NewServer(NewRouter(stubResolver{}, stubAuthenticator{team: &auth.Team{ID: "t"}}, stubRateLimiter{},
		stubBudgetTracker{}, stubCostCalculator{}, stubHealthRecorder{}, nil, nil, chaos,
		noRetryConfig(t), discardLogger(), func() bool { return true }))
	t.Cleanup(srv.Close)

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		req, err := http.NewRequest(method, srv.URL+"/admin/chaos", nil)
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s /admin/chaos on the public port = %d, want 404", method, resp.StatusCode)
		}
	}
}

// recordingHealthRecorder counts the failures reported per provider, so a
// test can assert that an injected fault reached Phase 5's passive signal.
type recordingHealthRecorder struct {
	mu       sync.Mutex
	failures map[string]int
}

func (r *recordingHealthRecorder) Record(providerName string, _ time.Duration, err error) {
	if err == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failures == nil {
		r.failures = map[string]int{}
	}
	r.failures[providerName]++
}

func (r *recordingHealthRecorder) failuresFor(providerName string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failures[providerName]
}
