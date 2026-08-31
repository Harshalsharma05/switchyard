package cache

import (
	"context"
	"log/slog"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// Tier names how far a lookup got, not only what produced a hit. A miss after
// the semantic scan costs an embedding call and a miss before it costs one
// Redis round trip; labelling both "none" would put a 500ms lookup and a 1ms
// one in the same latency bucket.
type Tier string

const (
	TierNone     Tier = ""
	TierExact    Tier = "exact"
	TierSemantic Tier = "semantic"
)

// LookupConfig tunes the threshold decision.
type LookupConfig struct {
	SemanticEnabled bool

	// Threshold is the cosine similarity at or above which a candidate is
	// served. The central tension of the feature: too low serves subtly wrong
	// answers, too high collapses the hit rate.
	Threshold float32
}

// Result is the outcome of one cache lookup. Miss and error both arrive as
// Hit == false — the caller proceeds identically either way.
type Result struct {
	Hit   bool
	Tier  Tier
	Entry *Entry

	// Similarity is the winning score on a semantic hit, and the best score
	// seen on a semantic miss — the near-miss number Step 7.6 charts.
	Similarity float32

	// Embedding is the query vector, returned on a miss so the caller can store
	// the eventual response without paying for a second embedding call.
	Embedding []float32

	// Latency is the wall time this lookup cost, hit or miss.
	Latency time.Duration

	// EmbedLatency is the part of Latency spent in the hosted embedding API.
	// It is split out because the caller excludes it from gateway overhead,
	// on the same grounds provider time is excluded: an external round trip
	// the gateway does not control.
	EmbedLatency time.Duration
}

// Cache is the two-tier lookup that sits on the request path.
//
// Every dependency here fails open: a Redis outage, an embedding failure, or a
// timeout all degrade to a miss, because the gateway must never be the reason
// a request fails.
type Cache struct {
	store    entryStore
	embedder Embedder
	cfg      LookupConfig
	log      *slog.Logger
	obs      Observer
}

// entryStore is the slice of *Store the lookup path uses. Declared here, by the
// consumer, so the fail-open paths can be tested without a live Redis.
type entryStore interface {
	Exact(ctx context.Context, k Key) (*Entry, bool, error)
	Candidates(ctx context.Context, k Key) ([]string, [][]float32, error)
	Fetch(ctx context.Context, id string) (*Entry, bool, error)
	Put(ctx context.Context, k Key, e Entry, ttl time.Duration) error
	RecordHit(ctx context.Context, id string) error
}

// Observer receives every lookup outcome. It exists so internal/cache does not
// import internal/telemetry, keeping the dependency arrow pointing one way.
//
// tier crosses as a plain string rather than Tier so an implementation in
// internal/telemetry can satisfy this without importing this package back.
type Observer interface {
	ObserveLookup(team, tier string, hit bool, similarity float32, d time.Duration)
	ObserveDegraded(reason string)
}

type nopObserver struct{}

func (nopObserver) ObserveLookup(string, string, bool, float32, time.Duration) {}
func (nopObserver) ObserveDegraded(string)                                     {}

func New(store entryStore, embedder Embedder, cfg LookupConfig, log *slog.Logger, obs Observer) *Cache {
	if obs == nil {
		obs = nopObserver{}
	}
	return &Cache{store: store, embedder: embedder, cfg: cfg, log: log, obs: obs}
}

// Lookup runs the exact tier, then the semantic tier if the exact tier missed.
//
// The ordering is the whole latency argument: the exact tier is one Redis round
// trip and keeps the overhead budget intact, so the embedding call is only paid
// for by requests that had no cheap answer available.
func (c *Cache) Lookup(ctx context.Context, k Key) Result {
	start := time.Now()

	entry, found, err := c.store.Exact(ctx, k)
	if err != nil {
		c.degrade("redis_read", err)
		return c.finish(k.TeamID, Result{Tier: TierExact, Latency: time.Since(start)})
	}
	if found {
		c.recordHit(ctx, k.EntryID)
		return c.finish(k.TeamID, Result{Hit: true, Tier: TierExact, Entry: entry, Similarity: 1, Latency: time.Since(start)})
	}

	if !c.cfg.SemanticEnabled || c.embedder == nil {
		return c.finish(k.TeamID, Result{Tier: TierExact, Latency: time.Since(start)})
	}

	embedStart := time.Now()
	vector, err := c.embedder.Embed(ctx, k.Query)
	embedLatency := time.Since(embedStart)
	if err != nil {
		c.degrade("embed", err)
		return c.finish(k.TeamID, Result{Tier: TierSemantic, EmbedLatency: embedLatency, Latency: time.Since(start)})
	}

	ids, vectors, err := c.store.Candidates(ctx, k)
	if err != nil {
		c.degrade("redis_index", err)
		// The embedding is still good, so the caller can store the response it
		// is about to fetch rather than wasting what was already paid for.
		return c.finish(k.TeamID, Result{Tier: TierSemantic, Embedding: vector, EmbedLatency: embedLatency, Latency: time.Since(start)})
	}

	bestID, best := nearest(vector, ids, vectors)

	if bestID == "" || best < c.cfg.Threshold {
		return c.finish(k.TeamID, Result{Tier: TierSemantic, Similarity: best, Embedding: vector, EmbedLatency: embedLatency, Latency: time.Since(start)})
	}

	hitEntry, found, err := c.store.Fetch(ctx, bestID)
	if err != nil || !found {
		if err != nil {
			c.degrade("redis_read", err)
		}
		return c.finish(k.TeamID, Result{Tier: TierSemantic, Similarity: best, Embedding: vector, EmbedLatency: embedLatency, Latency: time.Since(start)})
	}

	c.recordHit(ctx, bestID)
	return c.finish(k.TeamID, Result{Hit: true, Tier: TierSemantic, Entry: hitEntry, Similarity: best, Embedding: vector, EmbedLatency: embedLatency, Latency: time.Since(start)})
}

// nearest returns the highest-scoring candidate. Vectors are unit length, so
// the comparison is a dot product with no square roots on the lookup path.
func nearest(query []float32, ids []string, vectors [][]float32) (string, float32) {
	var bestID string
	var best float32 = -2

	for i, v := range vectors {
		if len(v) != len(query) {
			continue
		}
		if s := Similarity(query, v); s > best {
			best, bestID = s, ids[i]
		}
	}
	if bestID == "" {
		return "", 0
	}
	return bestID, best
}

// Store writes a freshly fetched response under the key that just missed. It is
// called off the response path, so its failures are logged and dropped.
func (c *Cache) Store(ctx context.Context, k Key, embedding []float32, e Entry, ttl time.Duration) {
	e.Embedding = embedding
	if err := c.store.Put(ctx, k, e, ttl); err != nil {
		c.degrade("redis_write", err)
	}
}

// Cacheable reports whether a response is worth storing. An empty or truncated
// completion is not: replaying it would serve a known-bad answer indefinitely.
func Cacheable(resp *provider.Response) bool {
	return resp != nil && resp.Content != "" && resp.FinishReason == provider.FinishStop
}

func (c *Cache) recordHit(ctx context.Context, id string) {
	if err := c.store.RecordHit(context.WithoutCancel(ctx), id); err != nil {
		c.degrade("redis_write", err)
	}
}

func (c *Cache) finish(team string, r Result) Result {
	c.obs.ObserveLookup(team, string(r.Tier), r.Hit, r.Similarity, r.Latency)
	return r
}

// degrade records that the cache silently stopped working. It logs loudly and
// fires a metric, matching the rate limiter's fail-open contract: the request
// still succeeds, but the operator finds out.
func (c *Cache) degrade(reason string, err error) {
	c.obs.ObserveDegraded(reason)
	if c.log != nil {
		c.log.Warn("cache degraded, serving as miss", "reason", reason, "error", err)
	}
}
