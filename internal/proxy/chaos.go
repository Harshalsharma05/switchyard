package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// Step 7.5's failure simulation harness: forge provider failures on demand so
// the Phase 7 breaker, the Phase 6 retry and fallback chain, and the Phase 5
// health signals can all be exercised without breaking a real provider or
// spending real money.
//
// It lives in this package because injection has to happen on the request
// path, and internal/proxy is the only package that owns that. The operator
// API for it lives in internal/admin, which reaches this through an interface
// of its own — admin never imports proxy, and cmd/gateway wires the two.
//
// Faults are injected immediately before the provider call inside
// ChatCompletions, not around it, so an injected failure travels the exact
// same path a real one does: it is classified as a *provider.Error, recorded
// in Phase 5's passive health window, counted by the Phase 7 breaker, and
// retried or failed over by Phase 6. A harness that bypassed any of those
// would be testing a code path that does not exist in production.

// devEnvironment is the only value of SWITCHYARD_ENV under which chaos can be
// turned on at all.
const devEnvironment = "dev"

// ErrChaosUnavailable is returned by every mutating Chaos method when the
// harness is not available. Callers surface it as a 404: an operator poking
// at a production gateway should learn the endpoint does not exist there, not
// that it exists and declined.
var ErrChaosUnavailable = errors.New("chaos harness is not available in this environment")

// ChaosMode is one kind of injected fault. The four here are exactly the
// modes Step 7.5 lists.
type ChaosMode string

const (
	// ChaosError forges a 500-class provider failure: retryable, and
	// eligible for fallback.
	ChaosError ChaosMode = "error"

	// ChaosLatency delays the call and then lets it proceed normally, which
	// is what makes it the mode for exercising timeouts and Phase 5's p99
	// baseline rather than its error rate.
	ChaosLatency ChaosMode = "latency"

	// ChaosRateLimit forges a 429 carrying a Retry-After, so Step 6.1's
	// "honour the provider's own backoff" path is reachable on demand.
	ChaosRateLimit ChaosMode = "rate_limit"

	// ChaosDrop forges a transport failure with no HTTP response at all —
	// the shape of a connection dropped mid-flight.
	ChaosDrop ChaosMode = "drop"
)

// chaosRateLimitRetryAfter is what a forged 429 tells the caller to wait.
// Short, because the point is to exercise the honour-Retry-After path, not to
// make a demo sit still.
const chaosRateLimitRetryAfter = time.Second

// ChaosRule is one targeting rule: which calls to break, and how.
//
// An empty Provider matches every provider and an empty Model every model,
// but not both at once — see validate. That is Step 7.5's "targetable at a
// specific provider or model" turned into something a fat-fingered rule
// cannot violate.
type ChaosRule struct {
	Provider string
	Model    string
	Mode     ChaosMode
	Latency  time.Duration
}

func (r ChaosRule) validate() error {
	if r.Provider == "" && r.Model == "" {
		return errors.New("a chaos rule must target a provider, a model, or both")
	}

	switch r.Mode {
	case ChaosError, ChaosRateLimit, ChaosDrop:
		if r.Latency != 0 {
			return fmt.Errorf("latency is only meaningful for mode %q, got mode %q", ChaosLatency, r.Mode)
		}
	case ChaosLatency:
		if r.Latency <= 0 {
			return fmt.Errorf("mode %q requires a positive latency", ChaosLatency)
		}
	default:
		return fmt.Errorf("unknown chaos mode %q", r.Mode)
	}

	return nil
}

// matches reports whether this rule targets a given call. An empty field is a
// wildcard.
func (r ChaosRule) matches(providerName, model string) bool {
	if r.Provider != "" && r.Provider != providerName {
		return false
	}
	if r.Model != "" && r.Model != model {
		return false
	}
	return true
}

// Chaos is the fault injector. The zero value is not usable; build one with
// NewChaos.
type Chaos struct {
	// available is set once, at construction, and never mutated afterwards.
	// That is the whole guard: there is no method that can turn it on, so a
	// gateway built outside a dev environment has no reachable path to
	// injecting faults, however its endpoints are called and whatever a
	// future call site forgets to check.
	available bool

	log *slog.Logger

	mu    sync.RWMutex
	rules []ChaosRule
}

// NewChaos builds the harness. It is available only when the environment is
// exactly dev *and* the operator opted in — Step 7.5's "disabled by default"
// and "can never be enabled in a non-dev environment" as two independent
// conditions rather than one.
//
// Two flags rather than one because they are set by different people for
// different reasons: SWITCHYARD_ENV is a property of the deployment, stamped
// by whatever platform launches the process, while the enable flag is a
// deliberate operator action. Requiring both means neither a stray
// enable-flag left in a production config nor a mislabelled environment is
// sufficient on its own.
//
// It always returns a usable *Chaos. An unavailable one refuses every
// mutation and injects nothing, which keeps the wiring in cmd/gateway free of
// the nil-interface trap that a nil return would invite.
func NewChaos(env string, enabled bool, log *slog.Logger) *Chaos {
	available := env == devEnvironment && enabled

	if available {
		// Logged at Warn, once, at startup: a gateway that can forge provider
		// failures on request is not a normal gateway, and that fact should be
		// impossible to miss in the logs of anything that has it turned on.
		log.Warn("chaos harness is ENABLED; this gateway can inject synthetic provider failures",
			slog.String("env", env))
	} else if enabled {
		// Asked for and refused. Loud, because the operator explicitly wanted
		// this and needs to know why it is not happening.
		log.Warn("chaos harness was requested but refused: not a dev environment",
			slog.String("env", env), slog.String("required_env", devEnvironment))
	}

	return &Chaos{available: available, log: log}
}

// Available reports whether the harness may be used at all.
func (c *Chaos) Available() bool {
	return c != nil && c.available
}

// Rules returns the active rules, in the order they were set. The slice is a
// copy: a caller must not be able to edit live rules without going through
// SetRules and its validation.
func (c *Chaos) Rules() []ChaosRule {
	if c == nil {
		return nil
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]ChaosRule(nil), c.rules...)
}

// SetRules validates and replaces the whole rule set.
//
// Replace rather than append, so a demo script can put the gateway in a known
// state with one call instead of having to clear first and hope nothing was
// left behind.
func (c *Chaos) SetRules(rules []ChaosRule) error {
	if !c.Available() {
		return ErrChaosUnavailable
	}

	// Validated in full before anything is stored, so a bad rule at the end
	// cannot leave half a rule set applied.
	for i, r := range rules {
		if err := r.validate(); err != nil {
			return fmt.Errorf("chaos rule %d: %w", i, err)
		}
	}

	c.mu.Lock()
	c.rules = append([]ChaosRule(nil), rules...)
	c.mu.Unlock()

	c.log.Warn("chaos rules applied", slog.Int("rules", len(rules)))
	return nil
}

// Clear removes every rule, returning the gateway to normal behaviour.
func (c *Chaos) Clear() error {
	if !c.Available() {
		return ErrChaosUnavailable
	}

	c.mu.Lock()
	c.rules = nil
	c.mu.Unlock()

	c.log.Warn("chaos rules cleared")
	return nil
}

// Apply is the request-path hook: it returns the error this call should fail
// with, or nil to let it proceed. A latency rule sleeps here and returns nil.
//
// The first matching rule wins, so an operator can put a narrow
// provider+model rule ahead of a broad provider-wide one and have the narrow
// one take effect.
//
// A nil receiver, an unavailable harness, and an empty rule set are all
// no-ops taking nothing but an uncontended RLock — this sits on the hot path
// of every proxied request, so the cost when chaos is off has to be
// negligible.
func (c *Chaos) Apply(ctx context.Context, providerName, model string) error {
	if !c.Available() {
		return nil
	}

	c.mu.RLock()
	var rule ChaosRule
	var found bool
	for _, r := range c.rules {
		if r.matches(providerName, model) {
			rule, found = r, true
			break
		}
	}
	c.mu.RUnlock()

	if !found {
		return nil
	}

	c.log.LogAttrs(ctx, slog.LevelWarn, "injecting chaos fault",
		slog.String("request_id", RequestIDFrom(ctx)),
		slog.String("provider", providerName),
		slog.String("model", model),
		slog.String("mode", string(rule.Mode)),
	)

	if rule.Mode == ChaosLatency {
		// Raced against ctx rather than slept blindly: a client that gives up
		// during an injected delay must not leave the gateway holding a
		// goroutine until the delay expires. Running out of context here is
		// reported as a timeout, which is what a genuinely slow provider
		// would have produced.
		select {
		case <-time.After(rule.Latency):
			return nil
		case <-ctx.Done():
			return chaosError(providerName, model, provider.KindTimeout, http.StatusGatewayTimeout,
				"chaos: injected latency exceeded the request deadline")
		}
	}

	return chaosFault(rule, providerName, model)
}

// chaosFault builds the forged failure for a non-latency mode.
//
// Every one is Retryable, because all three model conditions that Step 6.1
// classifies as retryable — a 5xx, a 429, a dropped connection. A harness
// that forged non-retryable failures would exercise the give-up path and
// leave retry, fallback, and the breaker untested, which is the opposite of
// what it is for.
func chaosFault(rule ChaosRule, providerName, model string) *provider.Error {
	switch rule.Mode {
	case ChaosRateLimit:
		err := chaosError(providerName, model, provider.KindRateLimited, http.StatusTooManyRequests,
			"chaos: injected rate limit")
		err.RetryAfter = chaosRateLimitRetryAfter
		return err

	case ChaosDrop:
		// StatusCode stays 0: a dropped connection never received a response,
		// and provider.Error documents 0 as exactly that.
		return chaosError(providerName, model, provider.KindNetworkError, 0,
			"chaos: injected connection drop")

	default: // ChaosError
		return chaosError(providerName, model, provider.KindServerError, http.StatusBadGateway,
			"chaos: injected provider error")
	}
}

// chaosError builds a *provider.Error indistinguishable in shape from a real
// one, so everything downstream classifies it identically. Every message is
// prefixed "chaos:" so nobody reading a log or a 503 breakdown mistakes an
// injected failure for a real outage.
func chaosError(providerName, model string, kind provider.Kind, status int, message string) *provider.Error {
	return &provider.Error{
		Kind:       kind,
		Provider:   providerName,
		Model:      model,
		StatusCode: status,
		Retryable:  true,
		Message:    message,
	}
}
