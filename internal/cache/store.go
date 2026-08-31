package cache

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// Entry is one cached completion. Everything Step 7.2 requires stored lives
// here except the fingerprint bucket membership, which is the index ZSET.
type Entry struct {
	Response     string
	FinishReason provider.FinishReason
	InputTokens  int
	OutputTokens int
	Model        string
	Provider     string
	CreatedAt    time.Time
	Hits         int64
	Embedding    []float32
}

// Store is the Redis persistence layer for cached entries.
//
// Every method reports Redis failures as errors and never as a fabricated hit;
// the caller turns them into a miss, because a cache outage must not fail a
// request.
type Store struct {
	rdb           *redis.Client
	maxCandidates int
	readTimeout   time.Duration
	writeTimeout  time.Duration
}

// StoreConfig tunes the Redis layer. Defaults are applied for zero values.
type StoreConfig struct {
	// MaxCandidates caps how many entries one fingerprint bucket keeps, which
	// bounds the brute-force similarity scan. Plain Redis 7 has no vector
	// index, so this cap is what keeps the scan predictable.
	MaxCandidates int

	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

func NewStore(rdb *redis.Client, cfg StoreConfig) *Store {
	if cfg.MaxCandidates <= 0 {
		cfg.MaxCandidates = 100
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 50 * time.Millisecond
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 500 * time.Millisecond
	}
	return &Store{
		rdb:           rdb,
		maxCandidates: cfg.MaxCandidates,
		readTimeout:   cfg.ReadTimeout,
		writeTimeout:  cfg.WriteTimeout,
	}
}

func entryKey(id string) string { return keyPrefix + ":entry:" + id }

// Exact fetches the entry whose ID the key already determines — the cheap tier,
// one round trip, no embedding required.
func (s *Store) Exact(ctx context.Context, k Key) (*Entry, bool, error) {
	return s.fetch(ctx, k.EntryID)
}

func (s *Store) fetch(ctx context.Context, id string) (*Entry, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.readTimeout)
	defer cancel()

	fields, err := s.rdb.HGetAll(ctx, entryKey(id)).Result()
	if err != nil {
		return nil, false, fmt.Errorf("reading cache entry: %w", err)
	}
	if len(fields) == 0 {
		return nil, false, nil
	}

	entry, err := decodeEntry(fields)
	if err != nil {
		return nil, false, err
	}
	return entry, true, nil
}

// Candidates returns the fingerprint bucket's entry IDs and embeddings, newest
// first and capped at MaxCandidates, in two round trips. IDs whose entry has
// since expired are pruned from the index rather than returned.
func (s *Store) Candidates(ctx context.Context, k Key) ([]string, [][]float32, error) {
	ctx, cancel := context.WithTimeout(ctx, s.readTimeout)
	defer cancel()

	ids, err := s.rdb.ZRevRange(ctx, k.IndexKey(), 0, int64(s.maxCandidates-1)).Result()
	if err != nil {
		return nil, nil, fmt.Errorf("reading cache index: %w", err)
	}

	liveIDs, vectors, stale, err := s.loadVectors(ctx, ids)
	if err != nil {
		return nil, nil, err
	}

	// Entries expire on their own TTL, leaving their index membership behind.
	// Pruning lazily here avoids a sweeper goroutine. It gets its own deadline
	// rather than inheriting the read context, which defer-cancels on return.
	if len(stale) > 0 {
		pruneCtx, pruneCancel := context.WithTimeout(context.WithoutCancel(ctx), s.writeTimeout)
		s.rdb.ZRem(pruneCtx, k.IndexKey(), toAny(stale)...)
		pruneCancel()
	}

	return liveIDs, vectors, nil
}

// loadVectors fetches the embeddings for ids in one pipelined round trip,
// separating IDs whose entry has expired so the caller can prune them.
func (s *Store) loadVectors(ctx context.Context, ids []string) (live []string, vectors [][]float32, stale []string, err error) {
	if len(ids) == 0 {
		return nil, nil, nil, nil
	}

	pipe := s.rdb.Pipeline()
	cmds := make([]*redis.SliceCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HMGet(ctx, entryKey(id), "embedding")
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, nil, nil, fmt.Errorf("reading candidate embeddings: %w", err)
	}

	live = make([]string, 0, len(ids))
	vectors = make([][]float32, 0, len(ids))

	for i, cmd := range cmds {
		vals, cmdErr := cmd.Result()
		if cmdErr != nil || len(vals) == 0 || vals[0] == nil {
			stale = append(stale, ids[i])
			continue
		}
		raw, ok := vals[0].(string)
		if !ok {
			stale = append(stale, ids[i])
			continue
		}
		live = append(live, ids[i])
		vectors = append(vectors, unpackVector([]byte(raw)))
	}
	return live, vectors, stale, nil
}

// Fetch loads one entry by ID, for the semantic tier's winning candidate.
func (s *Store) Fetch(ctx context.Context, id string) (*Entry, bool, error) {
	return s.fetch(ctx, id)
}

// Put writes an entry and registers it in its fingerprint bucket. The bucket is
// trimmed to MaxCandidates and expires with the entries it points at.
func (s *Store) Put(ctx context.Context, k Key, e Entry, ttl time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, s.writeTimeout)
	defer cancel()

	now := e.CreatedAt
	if now.IsZero() {
		now = time.Now()
	}

	pipe := s.rdb.Pipeline()
	pipe.HSet(ctx, entryKey(k.EntryID), map[string]any{
		"response":      e.Response,
		"finish_reason": string(e.FinishReason),
		"input_tokens":  e.InputTokens,
		"output_tokens": e.OutputTokens,
		"model":         e.Model,
		"provider":      e.Provider,
		"created_at":    now.UnixNano(),
		"hits":          0,
		"embedding":     string(packVector(e.Embedding)),
	})
	pipe.Expire(ctx, entryKey(k.EntryID), ttl)
	pipe.ZAdd(ctx, k.IndexKey(), redis.Z{Score: float64(now.UnixNano()), Member: k.EntryID})
	pipe.ZRemRangeByRank(ctx, k.IndexKey(), 0, int64(-s.maxCandidates-1))
	pipe.Expire(ctx, k.IndexKey(), ttl)

	// The team index exists only so Step 7.4 can purge by team: the team ID is
	// hashed into the fingerprint, so there is nothing else to match on. It is
	// written on the store path, which already runs after the client has its
	// response, so it costs the request nothing.
	if k.TeamID != "" {
		pipe.ZAdd(ctx, k.TeamKey(), redis.Z{Score: float64(now.UnixNano()), Member: k.EntryID})
		pipe.Expire(ctx, k.TeamKey(), ttl)
	}

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("writing cache entry: %w", err)
	}
	return nil
}

// RecordHit increments an entry's hit counter without extending its TTL —
// a popular entry still ages out on schedule.
func (s *Store) RecordHit(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, s.writeTimeout)
	defer cancel()

	if err := s.rdb.HIncrBy(ctx, entryKey(id), "hits", 1).Err(); err != nil {
		return fmt.Errorf("recording cache hit: %w", err)
	}
	return nil
}

// --- encoding ---------------------------------------------------------------

// packVector stores a vector as little-endian float32 bytes: 3KB for 768
// dimensions against roughly 10KB as JSON, on a value read on every lookup.
func packVector(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

func unpackVector(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

func decodeEntry(f map[string]string) (*Entry, error) {
	created, err := strconv.ParseInt(f["created_at"], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("decoding cache entry timestamp: %w", err)
	}

	hits, _ := strconv.ParseInt(f["hits"], 10, 64)
	in, _ := strconv.Atoi(f["input_tokens"])
	out, _ := strconv.Atoi(f["output_tokens"])

	return &Entry{
		Response:     f["response"],
		FinishReason: provider.FinishReason(f["finish_reason"]),
		InputTokens:  in,
		OutputTokens: out,
		Model:        f["model"],
		Provider:     f["provider"],
		CreatedAt:    time.Unix(0, created),
		Hits:         hits,
		Embedding:    unpackVector([]byte(f["embedding"])),
	}, nil
}

func toAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}
