package cache

import (
	"context"
	"errors"
	"testing"
	"time"
)

var errRedisDown = errors.New("connection refused")

type fakeStore struct {
	exact      *Entry
	exactErr   error
	ids        []string
	vectors    [][]float32
	candErr    error
	fetched    map[string]*Entry
	fetchErr   error
	recordedID string
}

func (f *fakeStore) Exact(context.Context, Key) (*Entry, bool, error) {
	return f.exact, f.exact != nil, f.exactErr
}
func (f *fakeStore) Candidates(context.Context, Key) ([]string, [][]float32, error) {
	return f.ids, f.vectors, f.candErr
}
func (f *fakeStore) Fetch(_ context.Context, id string) (*Entry, bool, error) {
	e, ok := f.fetched[id]
	return e, ok, f.fetchErr
}
func (f *fakeStore) Put(context.Context, Key, Entry, time.Duration) error { return nil }
func (f *fakeStore) RecordHit(_ context.Context, id string) error {
	f.recordedID = id
	return nil
}

type fakeEmbedder struct {
	vector []float32
	err    error
	calls  int
}

func (e *fakeEmbedder) Embed(context.Context, string) ([]float32, error) {
	e.calls++
	return e.vector, e.err
}
func (e *fakeEmbedder) Dimensions() int { return len(e.vector) }

func newTestCache(store entryStore, emb Embedder, threshold float32) *Cache {
	return New(store, emb, LookupConfig{SemanticEnabled: true, Threshold: threshold}, nil, nil)
}

// The threshold decision is the feature's central tension, and fail-open is a
// load-bearing design constraint. Both are verified here; nothing else in the
// lookup path can produce a wrong answer that something downstream would catch.
func TestLookupTiersAndThreshold(t *testing.T) {
	hit := &Entry{Response: "cached"}

	tests := map[string]struct {
		store       *fakeStore
		embedder    *fakeEmbedder
		threshold   float32
		semanticOff bool
		wantHit     bool
		wantTier    Tier
		wantEmbeds  int
	}{
		"exact hit skips the embedder": {
			store:      &fakeStore{exact: hit},
			embedder:   &fakeEmbedder{vector: []float32{1, 0}},
			threshold:  0.9,
			wantHit:    true,
			wantTier:   TierExact,
			wantEmbeds: 0,
		},
		"semantic hit above threshold": {
			store: &fakeStore{
				ids:     []string{"a"},
				vectors: [][]float32{{1, 0}},
				fetched: map[string]*Entry{"a": hit},
			},
			embedder:   &fakeEmbedder{vector: []float32{1, 0}},
			threshold:  0.9,
			wantHit:    true,
			wantTier:   TierSemantic,
			wantEmbeds: 1,
		},
		// Tier on a miss reports how far the lookup got, so a 500ms semantic
		// miss is distinguishable from a 1ms exact-only one in the metrics.
		"below threshold misses": {
			store: &fakeStore{
				ids:     []string{"a"},
				vectors: [][]float32{{0, 1}},
				fetched: map[string]*Entry{"a": hit},
			},
			embedder:   &fakeEmbedder{vector: []float32{1, 0}},
			threshold:  0.9,
			wantTier:   TierSemantic,
			wantEmbeds: 1,
		},
		"empty bucket misses": {
			store:      &fakeStore{},
			embedder:   &fakeEmbedder{vector: []float32{1, 0}},
			threshold:  0.9,
			wantTier:   TierSemantic,
			wantEmbeds: 1,
		},
		"semantic disabled stops at the exact tier": {
			store:       &fakeStore{},
			embedder:    &fakeEmbedder{vector: []float32{1, 0}},
			threshold:   0.9,
			semanticOff: true,
			wantTier:    TierExact,
			wantEmbeds:  0,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := New(tc.store, tc.embedder,
				LookupConfig{SemanticEnabled: !tc.semanticOff, Threshold: tc.threshold}, nil, nil)
			got := c.Lookup(context.Background(), Key{})

			if got.Hit != tc.wantHit {
				t.Fatalf("hit = %v, want %v", got.Hit, tc.wantHit)
			}
			if got.Tier != tc.wantTier {
				t.Fatalf("tier = %q, want %q", got.Tier, tc.wantTier)
			}
			if tc.embedder.calls != tc.wantEmbeds {
				t.Fatalf("embed calls = %d, want %d", tc.embedder.calls, tc.wantEmbeds)
			}
		})
	}
}

// Every dependency of the cache fails open: the request must proceed as a miss,
// never as an error, because the gateway is never the reason a request fails.
func TestLookupFailsOpen(t *testing.T) {
	tests := map[string]struct {
		store    *fakeStore
		embedder *fakeEmbedder

		// wantEmbedding asserts a paid-for vector is still handed back so the
		// caller can store the response it is about to fetch.
		wantEmbedding bool
	}{
		"redis down on exact read": {
			store:    &fakeStore{exactErr: errRedisDown},
			embedder: &fakeEmbedder{vector: []float32{1, 0}},
		},
		"embedding unavailable": {
			store:    &fakeStore{},
			embedder: &fakeEmbedder{err: ErrEmbedUnavailable},
		},
		"redis down on index read": {
			store:         &fakeStore{candErr: errRedisDown},
			embedder:      &fakeEmbedder{vector: []float32{1, 0}},
			wantEmbedding: true,
		},
		"winning candidate vanished": {
			store: &fakeStore{
				ids:     []string{"a"},
				vectors: [][]float32{{1, 0}},
				fetched: map[string]*Entry{},
			},
			embedder:      &fakeEmbedder{vector: []float32{1, 0}},
			wantEmbedding: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := newTestCache(tc.store, tc.embedder, 0.9).Lookup(context.Background(), Key{})

			if got.Hit {
				t.Fatal("degraded cache must report a miss, never a hit")
			}
			if hasEmbedding := got.Embedding != nil; hasEmbedding != tc.wantEmbedding {
				t.Fatalf("embedding returned = %v, want %v", hasEmbedding, tc.wantEmbedding)
			}
		})
	}
}

// A semantic hit must credit the entry it actually served, not the ID the exact
// tier computed — otherwise hit counts and TTL policy attach to the wrong row.
func TestSemanticHitRecordsWinningEntry(t *testing.T) {
	store := &fakeStore{
		ids:     []string{"near", "far"},
		vectors: [][]float32{{0.99, 0.14}, {0, 1}},
		fetched: map[string]*Entry{"near": {Response: "cached"}},
	}

	got := newTestCache(store, &fakeEmbedder{vector: []float32{1, 0}}, 0.9).Lookup(context.Background(), Key{EntryID: "computed"})

	if !got.Hit || store.recordedID != "near" {
		t.Fatalf("recorded %q, want \"near\" (hit=%v)", store.recordedID, got.Hit)
	}
}
