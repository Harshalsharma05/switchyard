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
	"github.com/Harshalsharma05/switchyard/internal/cache"
	"github.com/Harshalsharma05/switchyard/internal/config"
	"github.com/Harshalsharma05/switchyard/internal/health"
	"github.com/Harshalsharma05/switchyard/internal/logstore"
	"github.com/Harshalsharma05/switchyard/internal/proxy"
	"github.com/Harshalsharma05/switchyard/internal/ratelimit"
	"github.com/Harshalsharma05/switchyard/internal/resilience"
	"github.com/Harshalsharma05/switchyard/internal/router"
	"github.com/Harshalsharma05/switchyard/internal/summary"
	"github.com/Harshalsharma05/switchyard/internal/telemetry"
)

// Server settings come from the environment rather than configs/, because they
// describe how the process is deployed rather than what it routes to. Provider
// and team behaviour stays in YAML.
const (
	defaultProvidersPath = "configs/providers.yaml"
	defaultTeamsPath     = "configs/teams.yaml"
	defaultCachePath     = "configs/cache.yaml"
	defaultRouterPath    = "configs/router.yaml"
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

	// Phase 7 circuit breaker. Process-level env vars for the same reason the
	// health thresholds above are: they say how quickly this deployment wants
	// to stop trying, not something that differs per provider.
	//
	// The plan specifies none of these numbers. Five failures in thirty
	// seconds is a real outage rather than a bad minute; a ten-second first
	// cooldown is long enough to matter and short enough that a brief blip
	// recovers quickly, doubling to five minutes if probes keep failing. Two
	// consecutive good probes to close, so one lucky request cannot reopen the
	// floodgates.
	defaultBreakerFailureThreshold = 5
	defaultBreakerWindow           = 30 * time.Second
	defaultBreakerCooldownBase     = 10 * time.Second
	defaultBreakerCooldownMax      = 5 * time.Minute
	defaultBreakerSuccessThreshold = 2

	// Sized above the largest provider timeout in configs/providers.yaml
	// (Ollama's 120s), so a slow-but-alive probe is never mistaken for an
	// abandoned one.
	defaultBreakerProbeTimeout = 150 * time.Second

	// Half a second of staleness against a value that changes a few times a
	// day: enough that the shared read amortises to nothing at any real
	// request rate, short enough that a stale verdict costs a handful of
	// requests. See resilience.BreakerConfig.StateCacheTTL.
	defaultBreakerStateCacheTTL = 500 * time.Millisecond

	// Step 7.5's chaos harness. The default environment is "production"
	// specifically so that a deployment which sets nothing at all cannot get
	// fault injection: the safe value is the one you get by forgetting.
	defaultEnvironment = "production"

	// Phase 8 tracing. defaultOTLPEndpoint is Jaeger's OTLP/HTTP receiver,
	// reached over the docker-compose network or localhost in dev.
	// defaultTraceSampleRatio: the plan asks for 100% in dev, so that is the
	// fallback here; a "production" deployment turns it down with
	// SWITCHYARD_TRACE_SAMPLE_RATIO. SWITCHYARD_ENV -- already read below
	// for the chaos harness -- doubles as the trace resource's
	// deployment.environment.name, rather than inventing a second
	// environment variable to say the same thing.
	defaultOTLPEndpoint     = "localhost:4318"
	defaultServiceVersion   = "dev"
	defaultTraceSampleRatio = 1.0

	// Bounds the final flush on shutdown, same reasoning as
	// defaultDrainTimeout: a slow exporter gets a fair window to deliver
	// whatever it's still holding, not an unbounded wait on process exit.
	defaultTracingShutdownTimeout = 5 * time.Second

	// Part 2 Phase 1 request log. QueueSize holds roughly a minute of Part 1's
	// 60 req/s load test, so a brief Postgres stall costs nothing; past that
	// rows are dropped rather than allowed to slow a request. FlushTimeout
	// also bounds the final flush on shutdown.
	defaultRequestLogQueueSize     = 4096
	defaultRequestLogBatchSize     = 100
	defaultRequestLogFlushInterval = time.Second
	defaultRequestLogFlushTimeout  = 5 * time.Second

	// Retention. 30 days of detail is the plan default; older rows are rolled
	// into requests_daily and dropped. Window 0 disables the sweep entirely.
	defaultRetentionWindow     = 30 * 24 * time.Hour
	defaultRetentionInterval   = time.Hour
	defaultRetentionBatchSize  = 5000
	defaultRetentionMaxBatches = 200

	defaultPostgresHost    = "localhost:5432"
	defaultPostgresUser    = "switchyard"
	defaultPostgresDB      = "switchyard"
	defaultPostgresSSLMode = "disable"

	// Part 2 Step 2.3's Overview summary. SWITCHYARD_PROMETHEUS_URL defaults to
	// the port docker-compose publishes Prometheus on, so a host-run gateway
	// with `docker compose up prometheus` works with no config; the compose
	// gateway service overrides it to the service name. Empty disables the
	// endpoint (it returns degraded). The cache TTL is deliberately short —
	// long enough that several polling tabs don't hammer Prometheus, short
	// enough that a stale summary is only a few seconds old.
	defaultPrometheusURL      = "http://localhost:9091"
	defaultSummaryCacheTTL    = 5 * time.Second
	defaultSummaryHTTPTimeout = 3 * time.Second

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

	// Phase 8: tracing setup, ahead of Redis. It has no dependency on
	// anything wired below and nothing below it depends on tracing to
	// function -- Step 8.2's spans are additive to a request that already
	// works without them. Setup itself never dials Jaeger (see its doc
	// comment), so a Jaeger that is not running yet cannot delay or fail
	// boot -- the same "must never be the reason a request fails" guarantee
	// CLAUDE.md states for telemetry, applied here to startup as well.
	shutdownTracing, err := telemetry.Setup(context.Background(), telemetry.Config{
		ServiceName:    "switchyard",
		ServiceVersion: envOr("SWITCHYARD_SERVICE_VERSION", defaultServiceVersion),
		Environment:    envOr("SWITCHYARD_ENV", defaultEnvironment),
		OTLPEndpoint:   envOr("SWITCHYARD_OTLP_ENDPOINT", defaultOTLPEndpoint),
		SampleRatio:    floatOr("SWITCHYARD_TRACE_SAMPLE_RATIO", defaultTraceSampleRatio),
	}, log)
	if err != nil {
		return fmt.Errorf("setting up tracing: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), defaultTracingShutdownTimeout)
		defer cancel()
		if err := shutdownTracing(ctx); err != nil {
			// Logged, not returned: a slow or failed final flush on the way
			// out is not a reason to report the shutdown itself as failed.
			log.Warn("tracing shutdown", slog.Any("error", err))
		}
	}()

	promMetrics, err := telemetry.NewMetrics()
	if err != nil {
		return fmt.Errorf("building metrics registry: %w", err)
	}
	if err := promMetrics.RegisterRuntimeCollectors(); err != nil {
		return fmt.Errorf("registering runtime collectors: %w", err)
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

	// Phase 7's breakers, one per provider+model, created lazily on first use.
	// Built before healthMonitor because Step 7.4 feeds breaker state into
	// health status, so the monitor needs this to exist first — the dependency
	// runs registry -> monitor and never back, which is also why
	// internal/health declares the BreakerOracle interface rather than
	// importing internal/resilience.
	breakerRegistry, err := resilience.NewBreakerRegistry(
		resilience.BreakerConfig{
			FailureThreshold: intOr("SWITCHYARD_BREAKER_FAILURE_THRESHOLD", defaultBreakerFailureThreshold),
			Window:           durationOr("SWITCHYARD_BREAKER_WINDOW", defaultBreakerWindow),
			CooldownBase:     durationOr("SWITCHYARD_BREAKER_COOLDOWN_BASE", defaultBreakerCooldownBase),
			CooldownMax:      durationOr("SWITCHYARD_BREAKER_COOLDOWN_MAX", defaultBreakerCooldownMax),
			SuccessThreshold: intOr("SWITCHYARD_BREAKER_SUCCESS_THRESHOLD", defaultBreakerSuccessThreshold),
			ProbeTimeout:     durationOr("SWITCHYARD_BREAKER_PROBE_TIMEOUT", defaultBreakerProbeTimeout),
			StateCacheTTL:    durationOr("SWITCHYARD_BREAKER_STATE_CACHE_TTL", defaultBreakerStateCacheTTL),
		},
		log,
		// Step 7.3: each breaker gets a Redis-backed store scoped to its own
		// provider+model keys, so replicas agree on which circuits are open.
		func(labels resilience.Labels) resilience.BreakerStore {
			return resilience.NewRedisStore(redisClient, labels)
		},
	)
	if err != nil {
		return fmt.Errorf("building circuit breaker registry: %w", err)
	}

	// Step 5.3's status computation reads healthRecorder's windows and is fed
	// the active checker's ping outcomes below. Its thresholds are
	// process-level env vars for the same reason the checker's interval and
	// timeout are: they say how cautious this deployment wants to be, not
	// something that differs per provider.
	healthMonitor, err := health.NewMonitor(
		initial.registry.Providers(),
		healthRecorder,
		breakerRegistry,
		redisClient,
		promMetrics,
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

	// Step 7.5's fault-injection harness. It refuses to become available
	// unless the environment is dev *and* the operator explicitly asked, so
	// the default deployment — which sets neither — has no reachable path to
	// injecting a failure. See proxy.NewChaos for why both are required.
	chaos := proxy.NewChaos(
		envOr("SWITCHYARD_ENV", defaultEnvironment),
		boolOr("SWITCHYARD_CHAOS_ENABLED", false),
		log,
	)

	// Part 2 Phase 1's request log. Left disabled when POSTGRES_PASSWORD is
	// unset, so a dev without Postgres still gets a working gateway; the
	// interface stays nil in that case, which makes the middleware a no-op.
	var reqLog proxy.RequestLogger
	var reqLogReader admin.RequestLogReader
	var logWriter *logstore.Writer
	var retainer *logstore.Retainer
	if pw := os.Getenv("POSTGRES_PASSWORD"); pw != "" {
		dbCfg := logstore.DBConfig{
			Host:     envOr("SWITCHYARD_POSTGRES_HOST", defaultPostgresHost),
			User:     envOr("SWITCHYARD_POSTGRES_USER", defaultPostgresUser),
			Password: pw,
			Database: envOr("SWITCHYARD_POSTGRES_DB", defaultPostgresDB),
			SSLMode:  envOr("SWITCHYARD_POSTGRES_SSLMODE", defaultPostgresSSLMode),
		}
		pool, err := logstore.NewPool(context.Background(), dbCfg)
		if err != nil {
			return fmt.Errorf("building request log pool: %w", err)
		}
		defer pool.Close()

		logWriter = logstore.NewWriter(pool, logstore.Config{
			QueueSize:     intOr("SWITCHYARD_REQUESTLOG_QUEUE_SIZE", defaultRequestLogQueueSize),
			BatchSize:     intOr("SWITCHYARD_REQUESTLOG_BATCH_SIZE", defaultRequestLogBatchSize),
			FlushInterval: durationOr("SWITCHYARD_REQUESTLOG_FLUSH_INTERVAL", defaultRequestLogFlushInterval),
			FlushTimeout:  durationOr("SWITCHYARD_REQUESTLOG_FLUSH_TIMEOUT", defaultRequestLogFlushTimeout),
		}, promMetrics, log)
		reqLog = logWriter
		reqLogReader = logWriter

		retainer = logstore.NewRetainer(pool, logstore.RetentionConfig{
			Window:     durationOr("SWITCHYARD_RETENTION_WINDOW", defaultRetentionWindow),
			Interval:   durationOr("SWITCHYARD_RETENTION_INTERVAL", defaultRetentionInterval),
			BatchSize:  intOr("SWITCHYARD_RETENTION_BATCH_SIZE", defaultRetentionBatchSize),
			MaxBatches: intOr("SWITCHYARD_RETENTION_MAX_BATCHES", defaultRetentionMaxBatches),
		}, promMetrics, log)

		log.Info("request log enabled", slog.String("database", dbCfg.Redacted()))
	} else {
		log.Warn("request log disabled: POSTGRES_PASSWORD is unset")
	}

	// Step 2.3's Overview summary service. It never blocks a request path and
	// degrades on its own when Prometheus is unreachable, so it is wired
	// unconditionally — an empty URL just makes every response degraded.
	summarySvc := summary.NewService(summary.Config{
		PrometheusURL: envOr("SWITCHYARD_PROMETHEUS_URL", defaultPrometheusURL),
		CacheTTL:      durationOr("SWITCHYARD_SUMMARY_CACHE_TTL", defaultSummaryCacheTTL),
		HTTPTimeout:   durationOr("SWITCHYARD_SUMMARY_HTTP_TIMEOUT", defaultSummaryHTTPTimeout),
	})

	// Step 7.3's semantic cache. A missing configs/cache.yaml, or enabled:
	// false, leaves semanticCache nil and the handler never consults one —
	// the pre-Phase-7 behaviour, and a supported configuration.
	//
	// Failure to build the embedder is fatal rather than silently degrading:
	// a cache that is configured on but never works would show up only as a
	// hit rate of zero, which is exactly the kind of quiet wrong that a
	// startup error prevents.
	cachePath := envOr("SWITCHYARD_CACHE_CONFIG", defaultCachePath)
	cacheCfg, err := config.LoadCache(cachePath)
	if err != nil {
		return fmt.Errorf("loading cache config: %w", err)
	}

	var (
		semanticCache *cache.Cache
		cacheStore    *cache.Store
		cacheTuner    admin.CacheTuner
	)
	if cacheCfg.Enabled {
		var embedder cache.Embedder
		if cacheCfg.Look.SemanticEnabled {
			embedder, err = cache.NewGemini(cacheCfg.Embed)
			if err != nil {
				return fmt.Errorf("building cache embedder: %w", err)
			}
		}

		cacheStore = cache.NewStore(redisClient, cacheCfg.Store)
		semanticCache = cache.New(cacheStore, embedder, cacheCfg.Look, log,
			telemetry.NewCacheObserver(promMetrics, cacheCfg.Look.Threshold))
		cacheTuner = cacheStore

		log.Info("semantic cache enabled",
			slog.Bool("semantic_tier", cacheCfg.Look.SemanticEnabled),
			slog.Float64("threshold", float64(cacheCfg.Look.Threshold)),
			slog.Duration("ttl_default", cacheCfg.TTL.Default),
			slog.Int("ttl_rules", len(cacheCfg.TTL.Rules)),
		)
	}

	// Step 8.2's cost-aware routing. A missing configs/router.yaml, or
	// enabled: false, leaves complexityRouter nil and every request is served
	// by the model the caller named — the pre-Phase-8 behaviour.
	routerPath := envOr("SWITCHYARD_ROUTER_CONFIG", defaultRouterPath)
	routerCfg, err := config.LoadRouter(routerPath)
	if err != nil {
		return fmt.Errorf("loading router config: %w", err)
	}

	var complexityRouter *router.Router
	if routerCfg.Enabled {
		if err := validateRoutingPolicy(store, routerCfg.Policy); err != nil {
			return err
		}
		complexityRouter = router.New(router.NewClassifier(routerCfg.Classifier), routerCfg.Policy)

		log.Info("cost-aware routing enabled",
			slog.String("simple_tier", routerCfg.Policy.Simple),
			slog.String("complex_tier", routerCfg.Policy.Complex),
			slog.Float64("threshold", routerCfg.Classifier.Threshold),
		)
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
		Handler:           proxy.NewRouter(store, store, limiter, budgetTracker, store, healthRecorder, healthMonitor, breakerRegistry, chaos, retryConfig, promMetrics, reqLog, log, isReady, append(cacheOptions(semanticCache, cacheCfg.TTL), routerOptions(complexityRouter)...)...),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	adminSrv := &http.Server{
		Addr: envOr("SWITCHYARD_ADMIN_ADDR", defaultAdminAddr),
		Handler: admin.NewRouter(isReady, store, budgetTracker, store, healthMonitor, breakerRegistry, chaosAdapter{chaos}, newReloader(store, providersPath, teamsPath), reqLogReader, store, summarySvc, cacheTuner, store, complexityRouter, promMetrics, log,
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

	// The flusher gets its own context, cancelled only after the HTTP drain
	// finishes: requests still completing during the drain enqueue rows, and
	// cancelling on the shutdown signal would flush before they arrive.
	logCtx, cancelLog := context.WithCancel(context.Background())
	defer cancelLog()
	if logWriter != nil {
		go logWriter.Run(logCtx)
	}

	// Retention stops on the shutdown signal, not on logCtx: an in-progress
	// sweep has nothing to hand off, unlike the flusher, which still has rows
	// arriving from requests finishing during the drain.
	if retainer != nil {
		go retainer.Run(ctx)
	}

	drainLog := func() {
		if logWriter == nil {
			return
		}
		cancelLog()
		logWriter.Wait(durationOr("SWITCHYARD_REQUESTLOG_FLUSH_TIMEOUT", defaultRequestLogFlushTimeout) + time.Second)
	}

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
		drainLog()
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received, draining")
	}

	// Fail readiness first. Between this and the drain below, a load balancer
	// gets a chance to stop routing new requests here while the ones already
	// accepted finish.
	ready.Store(false)

	err = shutdown(publicSrv, adminSrv, log)
	drainLog()
	return err
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

	return slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
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

// boolOr reads a boolean env var. Anything unparseable falls back rather than
// failing the boot — and for the one flag that uses it, the fallback is
// "off," so a typo can only ever disable chaos, never enable it.
func boolOr(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
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

// cacheOptions returns the handler option enabling the cache, or nothing when
// no cache was built. A typed nil *cache.Cache would satisfy the interface and
// panic on first use, so the nil check happens here rather than in the handler.
// validateRoutingPolicy fails startup when the policy cannot work: a tier
// providers.yaml does not declare, or a real model whose name collides with a
// routing keyword and would therefore be unreachable by its own name.
//
// A startup error rather than a runtime one, for the same reason a broken
// cache is: a policy that silently routes nowhere shows up only as a feature
// that never fires.
func validateRoutingPolicy(store *configStore, p router.Policy) error {
	for _, tier := range []string{p.Simple, p.Complex} {
		if len(store.TierNamed(tier)) == 0 {
			return fmt.Errorf("routing policy names tier %q, which configs/providers.yaml does not declare", tier)
		}
	}
	for _, reserved := range []string{router.AutoModel, p.Simple, p.Complex} {
		if _, err := store.ForModel(reserved); err == nil {
			return fmt.Errorf("model %q collides with a routing keyword; rename it in configs/providers.yaml", reserved)
		}
	}
	return nil
}

func routerOptions(r *router.Router) []proxy.Option {
	if r == nil {
		return nil
	}
	return []proxy.Option{proxy.WithRouting(r)}
}

func cacheOptions(c *cache.Cache, ttl cache.TTLPolicy) []proxy.Option {
	if c == nil {
		return nil
	}
	return []proxy.Option{proxy.WithCache(c, ttl)}
}
