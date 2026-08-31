package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/cache"
	"github.com/Harshalsharma05/switchyard/internal/provider"
	"github.com/Harshalsharma05/switchyard/internal/telemetry"
)

// cacheStoreTimeout bounds the write that happens after the client already has
// its response. It is generous because nothing is waiting on it, and bounded
// because a hung Redis must not pin the handler goroutine.
const cacheStoreTimeout = 2 * time.Second

// HeaderCacheTTL lets a caller override Step 7.4's content-matched lifetime,
// in seconds, with 0 meaning "do not cache this response".
//
// A header rather than a body field so /v1/chat/completions stays byte-for-byte
// an OpenAI request: how long a gateway keeps an answer is not a model
// parameter, and putting it in the body would make the schema non-standard for
// every client.
const HeaderCacheTTL = "X-Switchyard-Cache-TTL"

// maxCacheTTLSeconds bounds the header before it is parsed, so a caller cannot
// hand us a duration that overflows on conversion.
const maxCacheTTLSeconds = 365 * 24 * 60 * 60

// replayChunkRunes is how finely a cached response is cut back into SSE deltas.
//
// Step 7.5 replays a hit as a real multi-delta stream so a client's rendering
// code sees no difference from a live one — but with no artificial delay
// between chunks. Pacing the replay to mimic generation speed would give back
// exactly the latency the cache exists to remove.
const replayChunkRunes = 24

// sseDone is the SSE terminator the live streaming path also writes.
const sseDone = "data: [DONE]\n\n"

// SemanticCache is the slice of internal/cache this package needs.
//
// Declared here, by the consumer, for the same reason Resolver and
// CostCalculator are: the handler depends on asking whether a request has
// already been answered, not on how embeddings or Redis work.
type SemanticCache interface {
	Lookup(ctx context.Context, k cache.Key) cache.Result
	Store(ctx context.Context, k cache.Key, embedding []float32, e cache.Entry, ttl time.Duration)
}

// consultCache runs the lookup and records its cost, returning the key so a
// miss can be stored later.
//
// It is called after authorization and before any reservation: a cache hit
// spends no provider tokens and no money, so charging the team's TPM bucket or
// budget for one would both be wrong and make Step 7.6's savings number an
// accounting artifact rather than a measurement.
//
// Streaming requests skip the cache entirely for now — Step 7.5 decides how a
// stored response is replayed as chunks, and half-answering that here would
// mean storing entries the streaming path cannot serve.
func (h *Handler) consultCache(r *http.Request, req chatRequest) (cache.Key, cache.Result, bool) {
	if h.cache == nil {
		return cache.Key{}, cache.Result{}, false
	}

	team := TeamFrom(r.Context())
	if team == nil {
		return cache.Key{}, cache.Result{}, false
	}

	ctx, span := telemetry.Tracer().Start(r.Context(), "switchyard.cache.lookup")
	defer span.End()

	key := cache.NewKey(team.ID, req.toProviderRequest())
	result := h.cache.Lookup(ctx, key)

	if m := metricsFrom(r.Context()); m != nil {
		hit := result.Hit
		m.cacheHit = &hit
		// The header reports the outcome, not the depth reached: a caller
		// cares whether it got a cached answer, not which tier declined.
		m.cacheTier = string(result.Tier)
		if !result.Hit {
			m.cacheTier = "miss"
		}
		// Excluded from overhead by requestMetrics.overhead, and reported on
		// its own header so the exclusion is visible.
		m.embedTime = result.EmbedLatency
	}

	return key, result, true
}

// serveFromCache writes a stored entry as an ordinary completion response.
//
// Provider and Model come from the entry, not from what was requested: the
// caller is told which provider originally produced these tokens rather than
// being led to believe a call just happened.
func (h *Handler) serveFromCache(w http.ResponseWriter, r *http.Request, entry *cache.Entry) {
	resp := &provider.Response{
		Content:      entry.Response,
		FinishReason: entry.FinishReason,
		Usage:        provider.Usage{InputTokens: entry.InputTokens, OutputTokens: entry.OutputTokens},
		Model:        entry.Model,
		Provider:     entry.Provider,
	}

	if m := metricsFrom(r.Context()); m != nil {
		m.providerName = entry.Provider
		m.servedModel = entry.Model
		m.usage = resp.Usage

		// Deliberately not recordCost: a cache hit spends nothing upstream, so
		// its cost is zero. The tokens are still reported, which is what lets
		// Step 7.6 price what the hit would have cost and call it savings.
	}

	h.writeSuccess(w, r, resp)
}

// parseCacheTTLHeader reads the caller's override. The bool reports whether one
// was present at all, which is what distinguishes "cache for zero seconds" from
// "say nothing and let the policy decide".
func parseCacheTTLHeader(r *http.Request) (time.Duration, bool) {
	raw := r.Header.Get(HeaderCacheTTL)
	if raw == "" {
		return 0, false
	}

	secs, err := strconv.Atoi(raw)
	if err != nil || secs < 0 || secs > maxCacheTTLSeconds {
		// A malformed override is ignored rather than rejected: the cache is
		// an optimisation, and a bad hint must not fail a valid completion.
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

// serveStreamFromCache replays a stored response as SSE.
//
// The chunking is what makes a hit indistinguishable from a live stream to a
// client: same event shape, same role-bearing first delta, same terminal
// finish_reason and [DONE]. Only the timing differs, and only by being faster.
func (h *Handler) serveStreamFromCache(w http.ResponseWriter, r *http.Request, entry *cache.Entry) {
	requestID := RequestIDFrom(r.Context())

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.log.ErrorContext(r.Context(), "response writer does not support flushing")
		writeError(w, h.log, http.StatusInternalServerError, "internal_error",
			"the gateway cannot stream a response")
		return
	}

	if m := metricsFrom(r.Context()); m != nil {
		m.providerName = entry.Provider
		m.servedModel = entry.Model
		m.usage = provider.Usage{InputTokens: entry.InputTokens, OutputTokens: entry.OutputTokens}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	id := chatCompletionID(requestID)
	created := time.Now().Unix()

	first := true
	for _, part := range splitForReplay(entry.Response) {
		if err := writeSSEJSON(w, toChatChunk(id, created, entry.Model, &provider.Chunk{Content: part}, first)); err != nil {
			h.log.LogAttrs(r.Context(), slog.LevelInfo, "writing cached stream chunk",
				slog.String("request_id", requestID), slog.Any("error", err))
			return
		}
		flusher.Flush()
		first = false
	}

	// The terminal chunk carries finish_reason and no content, exactly as the
	// live path emits it, so a client keying off finish_reason behaves the same.
	if err := writeSSEJSON(w, toChatChunk(id, created, entry.Model, &provider.Chunk{FinishReason: entry.FinishReason}, first)); err != nil {
		h.log.LogAttrs(r.Context(), slog.LevelInfo, "writing cached stream terminator",
			slog.String("request_id", requestID), slog.Any("error", err))
		return
	}
	flusher.Flush()

	if _, err := io.WriteString(w, sseDone); err != nil {
		h.log.LogAttrs(r.Context(), slog.LevelInfo, "writing cached stream [DONE]",
			slog.String("request_id", requestID), slog.Any("error", err))
		return
	}
	flusher.Flush()
}

// splitForReplay cuts content into rune-safe deltas. Slicing by bytes would
// split a multi-byte rune across two SSE events and corrupt it.
func splitForReplay(content string) []string {
	if content == "" {
		return nil
	}

	runes := []rune(content)
	parts := make([]string, 0, len(runes)/replayChunkRunes+1)
	for start := 0; start < len(runes); start += replayChunkRunes {
		end := min(start+replayChunkRunes, len(runes))
		parts = append(parts, string(runes[start:end]))
	}
	return parts
}

// storeInCache writes a freshly fetched response under the key that just
// missed. It runs after the client already has its bytes, so its latency is
// off the response path, and it reuses the embedding the lookup already paid
// for rather than calling the embedding API a second time.
func (h *Handler) storeInCache(r *http.Request, k cache.Key, result cache.Result, resp *provider.Response) {
	if h.cache == nil || !cache.Cacheable(resp) {
		return
	}

	override, hasOverride := parseCacheTTLHeader(r)
	ttl, cacheable := h.cacheTTL.TTLFor(k.Query, override, hasOverride)
	if !cacheable {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), cacheStoreTimeout)
	defer cancel()

	h.cache.Store(ctx, k, result.Embedding, cache.Entry{
		Response:     resp.Content,
		FinishReason: resp.FinishReason,
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		Model:        resp.Model,
		Provider:     resp.Provider,
		CreatedAt:    time.Now(),
	}, ttl)
}
