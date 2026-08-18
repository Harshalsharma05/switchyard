// Command gateway is the SwitchYard entrypoint.
//
// It is the only place that knows about every package: it loads config, builds
// the registry, composes the two listeners, and owns graceful shutdown.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Harshalsharma05/switchyard/internal/admin"
	"github.com/Harshalsharma05/switchyard/internal/budget"
	"github.com/Harshalsharma05/switchyard/internal/health"
	"github.com/Harshalsharma05/switchyard/internal/proxy"
	"github.com/Harshalsharma05/switchyard/internal/ratelimit"
	"github.com/Harshalsharma05/switchyard/internal/resilience"
)

// Server settings come from the environment rather than configs/, because they
// describe how the process is deployed rather than what it routes to. Provider
// and team behaviour stays in YAML.
const (
	defaultProvidersPath = "configs/providers.yaml"
	defaultTeamsPath     = "configs/teams.yaml"
	defaultRedisAddr     = "localhost:6379"
	defaultPublicAddr    = ":8080"
	defaultAdminAddr     = ":9090"
	defaultDrainTimeout  = 25 * time.Second

	// Health check cadence (Phase 5). These stay process-level env vars rather
	// than configs/providers.yaml fields: every provider is probed on the same
	// schedule, so there's nothing to route per-provider yet, and it keeps the
	// same shape as SWITCHYARD_DRAIN_TIMEOUT above.
	defaultHealthCheckInterval = 30 * time.Second
	defaultHealthCheckTimeout  = 5 * time.Second

	// Step 5.3 status thresholds. The error rates are the plan's own "e.g."
	// examples; the two counts are round numbers for values the plan leaves
	// unspecified entirely.
	defaultDegradedErrorRate       = 0.10
	defaultDownErrorRate           = 0.50
	defaultDownConsecutiveFailures = 3
	defaultRecoveryStreak          = 3

	// Step 6.1 retry policy. MaxAttempts: 3 is the plan's own example
	// ("cap at 3 attempts"); BaseDelay has no example in the plan — 50ms is
	// enough to absorb a brief provider hiccup without noticeably slowing a
	// request that only needs one retry.
	defaultRetryMaxAttempts = 3
	defaultRetryBaseDelay   = 50 * time.Millisecond

	// Step 6.3's chain-wide ceiling on provider calls per client request.
	// Five leaves the primary its full three attempts and two more for the
	// tier behind it — enough for a real failover, far short of the fifteen
	// calls an unbounded 3-attempt policy would make against a five-entry
	// tier.
	defaultRetryMaxTotalAttempts = 5

	// readHeaderTimeout bounds how long a client may take to send its headers,
	// which is the Slowloris defence. Note that WriteTimeout is deliberately
	// left unset: a provider call can legitimately take 30 seconds and a Phase 2
	// stream much longer, so response duration is governed by the request
	// context instead.
	readHeaderTimeout = 10 * time.Second
)

func main() {
	if err := run(); err != nil {
		// slog rather than panic, so startup failures read the same as every
		// other log line a operator will grep.
		slog.Error("startup failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	log := newLogger()

	providersPath := envOr("SWITCHYARD_PROVIDERS_CONFIG", defaultProvidersPath)
	teamsPath := envOr("SWITCHYARD_TEAMS_CONFIG", defaultTeamsPath)

	// loadLiveConfig (cmd/gateway/reload.go) is the one place providers.yaml
	// and teams.yaml are read, validated, and turned into registries — boot
	// and every later POST /admin/reload run the exact same steps, so the
	// two can never drift into checking different things.
	initial, providerCount, teamCount, err := loadLiveConfig(providersPath, teamsPath)
	if err != nil {
		return err
	}
	store := newConfigStore(initial)

	for _, p := range initial.registry.Providers() {
		log.Info("provider registered", slog.String("provider", p.Name()))
	}

	// redis.NewClient never dials eagerly — the connection is opened lazily on
	// the first command — so a Redis that is down right now does not stop the
	// gateway from starting. That is deliberate, not an oversight: per
	// CLAUDE.md, rate limiting fails open when Redis is unreachable, and
	// refusing to boot over it would contradict that at the one moment it
	// matters most.
	//
	// Timeouts and retries are tightened well below go-redis's defaults
	// (5s dial timeout, several retries with exponential backoff) on purpose.
	// Measured live: with the defaults, one request against a genuinely
	// unreachable Redis took 3.5 seconds to fail open — a "fail open" that
	// slow is the gateway being the reason a request is slow, which is
	// exactly what fail-open exists to prevent. These bounds trade a little
	// resilience to a single transient blip for detecting a real outage in
	// well under a second, every time.
	redisClient := redis.NewClient(&redis.Options{
		Addr:         envOr("SWITCHYARD_REDIS_ADDR", defaultRedisAddr),
		DialTimeout:  150 * time.Millisecond,
		ReadTimeout:  150 * time.Millisecond,
		WriteTimeout: 150 * time.Millisecond,
		MaxRetries:   1,
	})
	defer redisClient.Close()

	limiter := ratelimit.NewLimiter(redisClient)
	budgetTracker := budget.NewTracker(redisClient)

	// Step 5.2's passive signal: the proxy handler records every real
	// provider call's outcome here. Same one-window-per-provider
	// construction, from the same initial registry, as everything else built
	// in this section.
	healthRecorder := health.NewRecorder(initial.registry.Providers())

	// Step 5.3's status computation reads healthRecorder's windows and is fed
	// the active checker's ping outcomes below. Its thresholds are
	// process-level env vars for the same reason the checker's interval and
	// timeout are: they say how cautious this deployment wants to be, not
	// something that differs per provider.
	healthMonitor, err := health.NewMonitor(
		initial.registry.Providers(),
		healthRecorder,
		redisClient,
		log,
		health.MonitorConfig{
			DegradedErrorRate:       floatOr("SWITCHYARD_HEALTH_DEGRADED_ERROR_RATE", defaultDegradedErrorRate),
			DownErrorRate:           floatOr("SWITCHYARD_HEALTH_DOWN_ERROR_RATE", defaultDownErrorRate),
			DownConsecutiveFailures: intOr("SWITCHYARD_HEALTH_DOWN_CONSECUTIVE_FAILURES", defaultDownConsecutiveFailures),
			RecoveryStreak:          intOr("SWITCHYARD_HEALTH_RECOVERY_STREAK", defaultRecoveryStreak),
		},
	)
	if err != nil {
		return fmt.Errorf("building health monitor: %w", err)
	}

	// The Step 5.1 active health checker pings every provider on its own
	// schedule, independent of request traffic, and reports each ping's
	// outcome to healthMonitor. It's built from the initial registry's
	// provider list once, at boot — like the rest of this function, it does
	// not yet react to a hot reload replacing the registry.
	checker, err := health.NewChecker(
		initial.registry.Providers(),
		healthMonitor,
		log,
		durationOr("SWITCHYARD_HEALTH_CHECK_INTERVAL", defaultHealthCheckInterval),
		durationOr("SWITCHYARD_HEALTH_CHECK_TIMEOUT", defaultHealthCheckTimeout),
	)
	if err != nil {
		return fmt.Errorf("building health checker: %w", err)
	}

	// Step 6.1's retry policy: same-provider retry with full-jitter backoff,
	// applied inside internal/proxy before Step 6.2's fallback chains ever
	// consider a different provider, and bounded across the whole chain by
	// Step 6.3's total-attempt ceiling.
	retryConfig, err := resilience.NewConfig(
		intOr("SWITCHYARD_RETRY_MAX_ATTEMPTS", defaultRetryMaxAttempts),
		durationOr("SWITCHYARD_RETRY_BASE_DELAY", defaultRetryBaseDelay),
		intOr("SWITCHYARD_RETRY_MAX_TOTAL_ATTEMPTS", defaultRetryMaxTotalAttempts),
	)
	if err != nil {
		return fmt.Errorf("building retry policy: %w", err)
	}

	// ready gates /readyz. It flips true once wiring is complete and false again
	// the moment shutdown begins, so a load balancer stops sending new traffic
	// while in-flight requests drain.
	var ready atomic.Bool

	// Step 5.4: readiness also fails once every provider is Down —
	// healthMonitor.AllDown() — but never on a single bad provider, which is
	// a routing problem for Phase 6's fallback chains, not a readiness one.
	isReady := func() bool {
		return ready.Load() && !healthMonitor.AllDown()
	}

	publicSrv := &http.Server{
		Addr: envOr("SWITCHYARD_ADDR", defaultPublicAddr),
		// healthMonitor appears twice over: healthRecorder is the write side
		// (Step 5.2's passive samples), healthMonitor the read side Step 6.2's
		// chain ordering consults to skip a down provider and sink a degraded
		// one.
		Handler:           proxy.NewRouter(store, store, limiter, budgetTracker, store, healthRecorder, healthMonitor, retryConfig, log, isReady),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	adminSrv := &http.Server{
		Addr: envOr("SWITCHYARD_ADMIN_ADDR", defaultAdminAddr),
		Handler: admin.NewRouter(isReady, store, budgetTracker, store, healthMonitor, newReloader(store, providersPath, teamsPath), log,
			proxy.Recoverer(log),
			proxy.RequestID,
			proxy.Logger(log),
		),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	// Both ports are bound here, synchronously, rather than inside
	// ListenAndServe on a goroutine. Binding in the goroutine would let the
	// process announce itself started and flip /readyz to ready before it knew
	// whether the ports were actually available — so a port conflict would
	// surface as a gateway that reports ready and then dies.
	publicLn, err := net.Listen("tcp", publicSrv.Addr)
	if err != nil {
		return fmt.Errorf("binding public listener: %w", err)
	}
	adminLn, err := net.Listen("tcp", adminSrv.Addr)
	if err != nil {
		_ = publicLn.Close()
		return fmt.Errorf("binding admin listener: %w", err)
	}

	// NotifyContext cancels ctx on the first SIGINT or SIGTERM, which is what
	// docker stop and Kubernetes both send.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Buffered so a failing server can report and exit even if nobody is
	// receiving yet — an unbuffered send would leak the goroutine.
	serverErr := make(chan error, 2)

	go serve(publicSrv, publicLn, "public", log, serverErr)
	go serve(adminSrv, adminLn, "admin", log, serverErr)

	// checker.Run blocks until ctx is cancelled, which happens on the same
	// SIGINT/SIGTERM that starts draining the listeners below — so the health
	// checker's goroutines stop on the same signal rather than outliving it.
	go checker.Run(ctx)

	ready.Store(true)
	log.Info("gateway started",
		slog.String("public_addr", publicSrv.Addr),
		slog.String("admin_addr", adminSrv.Addr),
		slog.Int("providers", providerCount),
		slog.Int("teams", teamCount),
	)

	select {
	case err := <-serverErr:
		// One listener died. Drain the other rather than leaving it accepting
		// traffic it will never finish serving.
		ready.Store(false)
		_ = shutdown(publicSrv, adminSrv, log)
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received, draining")
	}

	// Fail readiness first. Between this and the drain below, a load balancer
	// gets a chance to stop routing new requests here while the ones already
	// accepted finish.
	ready.Store(false)

	return shutdown(publicSrv, adminSrv, log)
}

// serve runs one already-bound listener and reports anything other than an
// ordinary close.
func serve(srv *http.Server, ln net.Listener, name string, log *slog.Logger, errs chan<- error) {
	log.Info("listener serving", slog.String("listener", name), slog.String("addr", ln.Addr().String()))

	// ErrServerClosed is what Shutdown causes, so it is the expected outcome
	// rather than a failure.
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errs <- fmt.Errorf("%s listener: %w", name, err)
	}
}

// shutdown drains both listeners, giving in-flight requests a bounded window to
// complete rather than dropping them.
func shutdown(publicSrv, adminSrv *http.Server, log *slog.Logger) error {
	timeout := durationOr("SWITCHYARD_DRAIN_TIMEOUT", defaultDrainTimeout)

	// A fresh context: the one that triggered shutdown is already cancelled, and
	// passing it here would abort the drain instantly.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Both shut down concurrently so the drain window is shared rather than
	// consumed twice.
	errs := make(chan error, 2)
	go func() { errs <- publicSrv.Shutdown(ctx) }()
	go func() { errs <- adminSrv.Shutdown(ctx) }()

	var failed error
	for range 2 {
		if err := <-errs; err != nil {
			failed = errors.Join(failed, err)
		}
	}

	if failed != nil {
		return fmt.Errorf("draining after %s: %w", timeout, failed)
	}

	log.Info("shutdown complete")
	return nil
}

// newLogger builds the JSON logger. Every line the gateway emits goes through
// it, so log aggregation never has to parse two formats.
func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(envOr("SWITCHYARD_LOG_LEVEL", "info"))); err != nil {
		level = slog.LevelInfo
	}

	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func durationOr(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

func floatOr(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return f
}

func intOr(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return i
}
