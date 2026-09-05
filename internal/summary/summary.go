// Package summary is the Prometheus query layer behind GET /admin/summary.
// It turns a handful of PromQL aggregates into the numbers the Overview screen
// needs, and never returns an error to the caller: a Prometheus failure comes
// back as a Result with Degraded set, so the dashboard shows partial data
// instead of an error page.
package summary

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Result is the Prometheus-derived half of the summary response. The admin
// handler adds live provider health (read in-process, not from Prometheus)
// and serves the merge. Pointer fields are nil when there is no data —
// no traffic in the window, or a query that failed — so the UI can render an
// explicit empty state rather than a misleading zero.
type Result struct {
	Range       string
	GeneratedAt time.Time
	Degraded    bool

	RequestCount *float64
	ErrorRate    *float64
	CacheHitRate *float64

	// QualityAvg is the mean async judge score in the window, QualityScored
	// the number of responses scored. Both nil until the worker has scored
	// anything — Overview shows an empty state, not a zero.
	QualityAvg    *float64
	QualityScored *float64

	OverheadP50 *float64
	OverheadP95 *float64
	OverheadP99 *float64
	CostUSD     *float64

	// Series is the time-series behind Overview's two charts. Nil when
	// Prometheus is unreachable or a range query failed — the charts then show
	// their own empty state, the KPIs still render from whatever scalars came
	// back.
	Series *Series
}

// Series holds the points for the traffic and overhead charts, already aligned
// to one step grid.
type Series struct {
	StepSeconds int
	Traffic     []TrafficPoint
	Overhead    []OverheadPoint
	Quality     []QualityPoint
}

// QualityPoint is the mean judge score in one step-wide bucket. Avg is nil
// for a bucket in which nothing was scored.
type QualityPoint struct {
	T   time.Time
	Avg *float64
}

// TrafficPoint is the request count in one step-wide bucket.
type TrafficPoint struct {
	T     time.Time
	Value float64
}

// OverheadPoint is the three overhead percentiles at one instant, in
// milliseconds. Any percentile may be nil for a bucket with no traffic.
type OverheadPoint struct {
	T   time.Time
	P50 *float64
	P95 *float64
	P99 *float64
}

// Options selects the window and the team scope. TeamID == "" means all teams
// (an admin caller); any other value scopes the team-labelled metrics to that
// team.
type Options struct {
	Range  string
	TeamID string
}

var validRanges = map[string]bool{"1h": true, "24h": true, "7d": true}

// ValidRange reports whether r is an accepted ?range= value.
func ValidRange(r string) bool { return validRanges[r] }

type Config struct {
	PrometheusURL string
	CacheTTL      time.Duration
	HTTPTimeout   time.Duration
}

// Service builds summary Results, caching each (range, team) briefly so a
// short poll interval across several open tabs does not hammer Prometheus.
type Service struct {
	prom *promClient // nil when no Prometheus URL is configured
	ttl  time.Duration

	mu    sync.Mutex
	cache map[string]cachedResult
}

type cachedResult struct {
	at     time.Time
	result Result
}

func NewService(cfg Config) *Service {
	s := &Service{ttl: cfg.CacheTTL, cache: make(map[string]cachedResult)}
	if cfg.PrometheusURL != "" {
		s.prom = &promClient{
			base: cfg.PrometheusURL,
			http: &http.Client{Timeout: cfg.HTTPTimeout},
		}
	}
	return s
}

// Build returns the summary for opts, from cache when a recent one exists.
func (s *Service) Build(ctx context.Context, opts Options) Result {
	key := opts.Range + "\x00" + opts.TeamID

	s.mu.Lock()
	if c, ok := s.cache[key]; ok && time.Since(c.at) < s.ttl {
		s.mu.Unlock()
		return c.result
	}
	s.mu.Unlock()

	r := s.build(ctx, opts)

	s.mu.Lock()
	s.cache[key] = cachedResult{at: time.Now(), result: r}
	s.mu.Unlock()
	return r
}

func (s *Service) build(ctx context.Context, opts Options) Result {
	r := Result{Range: opts.Range, GeneratedAt: time.Now().UTC()}
	if s.prom == nil {
		r.Degraded = true
		return r
	}

	win := opts.Range
	teamSel := ""
	if opts.TeamID != "" {
		teamSel = fmt.Sprintf("team=%q", opts.TeamID)
	}

	// q runs one query, flipping Degraded on a hard failure and returning nil
	// for both failure and genuine no-data.
	q := func(promql string) *float64 {
		v, ok, err := s.prom.queryScalar(ctx, promql)
		if err != nil {
			r.Degraded = true
			return nil
		}
		if !ok {
			return nil
		}
		return &v
	}

	reqTotal := selector("switchyard_requests_total", teamSel)
	req5xx := selector("switchyard_requests_total", join(teamSel, `status=~"5.."`))
	cost := selector("switchyard_cost_microdollars_total", teamSel)

	r.RequestCount = q(fmt.Sprintf("sum(increase(%s[%s]))", reqTotal, win))
	r.ErrorRate = q(fmt.Sprintf("sum(increase(%s[%s])) / sum(increase(%s[%s]))", req5xx, win, reqTotal, win))
	r.OverheadP50 = q(overheadQuantile(0.5, win))
	r.OverheadP95 = q(overheadQuantile(0.95, win))
	r.OverheadP99 = q(overheadQuantile(0.99, win))
	r.CostUSD = q(fmt.Sprintf("sum(increase(%s[%s])) / 1e6", cost, win))

	// Cache hit rate counts every lookup as the denominator, including the
	// cheap misses that never reached the semantic tier — excluding those would
	// flatter the number by dropping the requests the cache could not help.
	cacheAll := selector("switchyard_cache_lookups_total", teamSel)
	cacheHits := selector("switchyard_cache_lookups_total", join(teamSel, `result="hit"`))
	r.CacheHitRate = q(fmt.Sprintf("sum(increase(%s[%s])) / sum(increase(%s[%s]))", cacheHits, win, cacheAll, win))

	// Quality: the judge score histogram carries a team label, so a non-admin
	// sees its own responses' scores. Sum over count is the mean; both come
	// from the same histogram so they cannot disagree about the denominator.
	qSum := selector("switchyard_quality_score_sum", teamSel)
	qCount := selector("switchyard_quality_score_count", teamSel)
	r.QualityScored = q(fmt.Sprintf("sum(increase(%s[%s]))", qCount, win))
	r.QualityAvg = q(fmt.Sprintf("sum(increase(%s[%s])) / sum(increase(%s[%s]))", qSum, win, qCount, win))
	if r.QualityAvg != nil && math.IsNaN(*r.QualityAvg) {
		r.QualityAvg = nil
	}

	// A ratio with no denominator comes back from Prometheus as no-data, which
	// q already maps to nil — but guard the pathological case anyway.
	if r.ErrorRate != nil && math.IsNaN(*r.ErrorRate) {
		r.ErrorRate = nil
	}
	if r.CacheHitRate != nil && math.IsNaN(*r.CacheHitRate) {
		r.CacheHitRate = nil
	}

	if series, err := s.buildSeries(ctx, opts, teamSel); err != nil {
		r.Degraded = true
	} else {
		r.Series = series
	}
	return r
}

// buildSeries runs the range queries behind Overview's two charts and merges
// the three overhead percentiles onto one aligned time grid.
func (s *Service) buildSeries(ctx context.Context, opts Options, teamSel string) (*Series, error) {
	win, step := windowStep(opts.Range)
	end := time.Now().UTC()
	start := end.Add(-win)
	stepExpr := durationExpr(step)

	trafficPoints, err := s.prom.queryRange(ctx,
		fmt.Sprintf("sum(increase(%s[%s]))", selector("switchyard_requests_total", teamSel), stepExpr),
		start, end, step)
	if err != nil {
		return nil, err
	}

	out := &Series{StepSeconds: int(step.Seconds())}
	for _, p := range trafficPoints {
		out.Traffic = append(out.Traffic, TrafficPoint{T: p.T, Value: p.V})
	}

	byTime := map[int64]*OverheadPoint{}
	order := []int64{}
	assign := func(points []sample, set func(*OverheadPoint, float64)) {
		for _, p := range points {
			key := p.T.Unix()
			op, ok := byTime[key]
			if !ok {
				op = &OverheadPoint{T: p.T}
				byTime[key] = op
				order = append(order, key)
			}
			set(op, p.V)
		}
	}

	for _, spec := range []struct {
		q   float64
		set func(*OverheadPoint, float64)
	}{
		{0.5, func(o *OverheadPoint, v float64) { o.P50 = f64ptr(v) }},
		{0.95, func(o *OverheadPoint, v float64) { o.P95 = f64ptr(v) }},
		{0.99, func(o *OverheadPoint, v float64) { o.P99 = f64ptr(v) }},
	} {
		points, err := s.prom.queryRange(ctx, overheadQuantile(spec.q, stepExpr), start, end, step)
		if err != nil {
			return nil, err
		}
		assign(points, spec.set)
	}

	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	for _, k := range order {
		out.Overhead = append(out.Overhead, *byTime[k])
	}

	qPoints, err := s.prom.queryRange(ctx, fmt.Sprintf(
		"sum(rate(%s[%s])) / sum(rate(%s[%s]))",
		selector("switchyard_quality_score_sum", teamSel), stepExpr,
		selector("switchyard_quality_score_count", teamSel), stepExpr),
		start, end, step)
	if err != nil {
		return nil, err
	}
	for _, p := range qPoints {
		v := p.V
		out.Quality = append(out.Quality, QualityPoint{T: p.T, Avg: &v})
	}
	return out, nil
}

// f64ptr boxes a float so an OverheadPoint field can be nil for "no data".
func f64ptr(v float64) *float64 { return &v }

// selector renders metric{labels} — or bare metric when there are no labels.
func selector(metric, labels string) string {
	if labels == "" {
		return metric
	}
	return metric + "{" + labels + "}"
}

// join combines two label-matcher fragments, either of which may be empty.
func join(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "," + b
	}
}

// windowStep picks the total lookback and the resolution for a range's charts,
// keeping every range to roughly 60–100 points.
func windowStep(r string) (window, step time.Duration) {
	switch r {
	case "1h":
		return time.Hour, time.Minute
	case "24h":
		return 24 * time.Hour, 15 * time.Minute
	case "7d":
		return 7 * 24 * time.Hour, 2 * time.Hour
	default:
		return time.Hour, time.Minute
	}
}

// durationExpr renders a Go duration as a PromQL range-vector literal.
func durationExpr(d time.Duration) string {
	if d%time.Hour == 0 {
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	return strconv.Itoa(int(d/time.Minute)) + "m"
}

// overheadQuantile is the PromQL for the gateway-overhead histogram at
// quantile p, in milliseconds. The histogram carries no team label — overhead
// is a property of the gateway, not of a team's traffic — so it is never
// scoped.
func overheadQuantile(p float64, win string) string {
	return fmt.Sprintf(
		"histogram_quantile(%g, sum(rate(switchyard_gateway_overhead_seconds_bucket[%s])) by (le)) * 1000",
		p, win,
	)
}
