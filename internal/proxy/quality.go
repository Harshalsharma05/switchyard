package proxy

import (
	"net/http"

	"github.com/Harshalsharma05/switchyard/internal/cache"
	"github.com/Harshalsharma05/switchyard/internal/quality"
)

// QualityVerifier is the slice of internal/quality this package needs.
//
// Declared here, by the consumer, like SemanticCache and ComplexityRouter: the
// handler decides whether a finished request is worth scoring and hands it
// over, and knows nothing about the queue, the judge, or the rubric.
type QualityVerifier interface {
	Decide(quality.Candidate) (quality.Reason, bool)
	Enqueue(quality.Sample)
}

// WithQuality enables Step 9.2's async quality verification.
func WithQuality(v QualityVerifier) Option {
	return func(h *Handler) { h.quality = v }
}

// considerQuality samples one finished request. It runs after the response is
// already on the wire — at the same point storeInCache does — so it adds
// nothing to the latency the client sees, and the sampling decision itself is
// a pure comparison with no I/O.
func (h *Handler) considerQuality(r *http.Request, req chatRequest, servedModel, servedProvider, response string, cacheResult cache.Result) {
	if h.quality == nil || response == "" {
		return
	}
	m := metricsFrom(r.Context())
	if m == nil || m.teamID == "" {
		return
	}

	cand := quality.Candidate{
		Routed: m.routedTier != "",
		// A positive saving is exactly "routing served a cheaper tier than the
		// caller's default" — the downgrade the feature exists to watch. Zero
		// means routed to the top tier, nil means not routed.
		Downgraded:    m.routingSavingsMicros != nil && *m.routingSavingsMicros > 0,
		CacheHit:      cacheResult.Hit,
		CacheSemantic: cacheResult.Tier == cache.TierSemantic,
		Similarity:    cacheResult.Similarity,
		Threshold:     cacheResult.Threshold,
	}

	reason, ok := h.quality.Decide(cand)
	if !ok {
		return
	}

	h.quality.Enqueue(quality.Sample{
		RequestID: RequestIDFrom(r.Context()),
		TeamID:    m.teamID,
		Model:     servedModel,
		Provider:  servedProvider,
		Reason:    reason,
		Prompt:    req.toProviderRequest().Messages,
		Response:  response,
	})
}
