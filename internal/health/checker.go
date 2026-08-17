// Package health implements Phase 5's health checking. Step 5.1 is the active
// half: a background goroutine per provider that calls Provider.Ping on a
// fixed schedule, entirely outside the request path, so a provider's liveness
// signal does not depend on the gateway currently receiving traffic for it.
package health

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// Checker runs one ticking goroutine per provider. Interval and Timeout are
// process-level settings (SWITCHYARD_HEALTH_CHECK_INTERVAL and
// SWITCHYARD_HEALTH_CHECK_TIMEOUT in cmd/gateway), not a configs/providers.yaml
// field: every provider is probed on the same schedule, so there is nothing to
// route per-provider yet. Step 5.3 may need to revisit this once status
// computation gives a reason to check a struggling provider more often.
type Checker struct {
	providers []provider.Provider
	monitor   *Monitor
	log       *slog.Logger
	interval  time.Duration
	timeout   time.Duration
}

// NewChecker validates the schedule and returns a Checker ready to Run.
// monitor receives every ping's raw outcome via Observe, which is what turns
// Step 5.1's active checks into Step 5.3's healthy/degraded/down status —
// Checker itself only ever logs a single ping's result, it does not decide
// what that means for the provider overall.
func NewChecker(providers []provider.Provider, monitor *Monitor, log *slog.Logger, interval, timeout time.Duration) (*Checker, error) {
	if interval <= 0 {
		return nil, fmt.Errorf("health checker: interval must be positive, got %s", interval)
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("health checker: timeout must be positive, got %s", timeout)
	}
	// A ping that's allowed to take as long as (or longer than) the gap between
	// ticks can still be running when the next tick arrives. watch only reads
	// ticker.C between checks, so that tick is simply absorbed rather than
	// causing an overlapping ping — but the check would then be running behind
	// schedule indefinitely against a wedged provider. Rejecting it at
	// construction is cheaper than debugging a health checker that silently
	// falls behind.
	if timeout >= interval {
		return nil, fmt.Errorf("health checker: timeout (%s) must be shorter than interval (%s)", timeout, interval)
	}

	return &Checker{providers: providers, monitor: monitor, log: log, interval: interval, timeout: timeout}, nil
}

// Run starts one ticking goroutine per provider and blocks until ctx is
// cancelled and every one of them has exited. Callers start it with
// `go checker.Run(ctx)`; there is no separate Stop method — the same ctx that
// cancels on SIGINT/SIGTERM and stops the gateway's listeners stops the
// checker too, so its lifecycle can't drift from the rest of the process.
func (c *Checker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, p := range c.providers {
		wg.Add(1)
		go func(p provider.Provider) {
			defer wg.Done()
			c.watch(ctx, p)
		}(p)
	}
	wg.Wait()
}

// watch pings one provider immediately, then on every tick, until ctx is
// cancelled. Checking immediately means a freshly booted gateway has a
// liveness signal for every provider well within the first interval, rather
// than reporting nothing until the first tick fires.
func (c *Checker) watch(ctx context.Context, p provider.Provider) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	c.check(ctx, p)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.check(ctx, p)
		}
	}
}

// check runs a single ping under its own timeout, independent of the
// provider's configured request timeout (configs/providers.yaml Timeout,
// sized for a real completion). A ping is a much smaller request and needs to
// be declared dead far sooner than that.
//
// This intentionally never touches internal/ratelimit or internal/budget:
// Provider.Ping calls Complete directly on the adapter, bypassing the proxy's
// middleware chain entirely, so health checks never consume a team's rate
// limit or budget.
func (c *Checker) check(ctx context.Context, p provider.Provider) {
	pingCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	start := time.Now()
	err := p.Ping(pingCtx)
	latency := time.Since(start)

	c.monitor.Observe(ctx, p.Name(), err)

	if err != nil {
		c.log.Warn("active health check failed",
			slog.String("provider", p.Name()),
			slog.Duration("latency", latency),
			slog.Any("error", err),
		)
		return
	}

	c.log.Info("active health check ok",
		slog.String("provider", p.Name()),
		slog.Duration("latency", latency),
	)
}
