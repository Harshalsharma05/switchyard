// Package quality decides which completed requests are worth an async quality
// score (Step 9.1) and, from Step 9.2, runs the LLM-as-judge worker.
package quality

import (
	"math/rand/v2"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// Candidate is what the sampler needs to judge one finished request. It is
// assembled on the request goroutine after the response is already on the
// wire, and nothing in it is persisted.
type Candidate struct {
	Routed        bool    // cost-aware routing chose the served model
	Downgraded    bool    // routing served a cheaper tier than the caller's default
	CacheHit      bool    // the response came from the semantic cache
	CacheSemantic bool    // the hit was a similarity match, not an exact-key match
	Similarity    float32 // winning cosine score on a semantic hit
	Threshold     float32 // the similarity threshold that hit had to clear
	Flagged       bool    // a caller marked this response for review
}

// Config is the sampling policy. It is configured rather than hard-coded
// because the policy is precisely what a quality number ends up meaning.
type Config struct {
	// RoutedRate is the fraction of routed responses scored for their own
	// sake. Downgraded-tier and near-threshold hits are always scored and do
	// not draw from this rate.
	RoutedRate float64

	// NearThresholdBand turns "cleared the threshold" into "only just cleared
	// it": a semantic hit with similarity in [Threshold, Threshold+band) is
	// always scored, because a marginal hit is the cache's riskiest output.
	NearThresholdBand float32
}

// Reason names why a candidate was sampled. It travels with the sample so
// Step 9.3's feedback loops can tell what a low score is evidence about.
type Reason string

const (
	ReasonFlagged       Reason = "flagged"
	ReasonDowngraded    Reason = "downgraded"
	ReasonNearThreshold Reason = "near_threshold_cache_hit"
	ReasonRoutedSample  Reason = "routed_sample"
)

// Sampler applies the Step 9.1 policy. It holds no mutable state, so one
// instance is shared across every request.
type Sampler struct {
	cfg Config
	rng func() float64 // injectable for tests; defaults to rand.Float64
}

func NewSampler(cfg Config) *Sampler {
	return &Sampler{cfg: cfg, rng: rand.Float64}
}

// Decide reports whether this request should be scored, and why. The
// deterministic reasons are checked before the probabilistic one so a
// downgraded or near-threshold response is never missed on a dice roll.
func (s *Sampler) Decide(c Candidate) (Reason, bool) {
	if s == nil {
		return "", false
	}
	switch {
	case c.Flagged:
		return ReasonFlagged, true
	case c.CacheHit && c.CacheSemantic && s.nearThreshold(c):
		return ReasonNearThreshold, true
	case c.Downgraded:
		return ReasonDowngraded, true
	case c.Routed && s.rng() < s.cfg.RoutedRate:
		return ReasonRoutedSample, true
	default:
		return "", false
	}
}

func (s *Sampler) nearThreshold(c Candidate) bool {
	return c.Similarity >= c.Threshold && c.Similarity < c.Threshold+s.cfg.NearThresholdBand
}

// Sample is one selected request handed to the async worker. It is built on
// the request goroutine after the response is on the wire and lives only in
// memory and in the worker's queue — the prompt and response text it carries
// are never written to Postgres, the same boundary the request log itself keeps.
type Sample struct {
	RequestID string
	TeamID    string
	Model     string
	Provider  string
	Reason    Reason

	Prompt   []provider.Message
	Response string
}
