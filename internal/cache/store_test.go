package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// testRedisAddr lets CI or a differently-configured machine point these tests
// at a Redis that isn't on localhost, matching internal/budget's convention.
func testRedisAddr() string {
	if addr := os.Getenv("TEST_REDIS_ADDR"); addr != "" {
		return addr
	}
	return "localhost:6379"
}

// newTestStore skips rather than fails when no Redis is reachable: these
// exercise real round trips against a real server, so "no Redis" is an
// environment fact, not a code failure.
func newTestStore(t *testing.T, cfg StoreConfig) (*Store, *redis.Client, context.Context) {
	t.Helper()

	rdb := redis.NewClient(&redis.Options{Addr: testRedisAddr()})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		t.Skipf("no Redis reachable at %s (start it with `docker compose -f deploy/docker-compose.yml up -d redis`): %v",
			testRedisAddr(), err)
	}
	t.Cleanup(func() { rdb.Close() })

	return NewStore(rdb, cfg), rdb, ctx
}

// uniqueOrg keeps one test's scope keys clear of another's. The scope index
// carries no TTL by design, so a fixed name would outlive the run.
func uniqueOrg(t *testing.T) string {
	t.Helper()
	var b [8]byte
	rand.Read(b[:])
	return "org-" + hex.EncodeToString(b[:])
}

func cleanupKeys(t *testing.T, rdb *redis.Client, keys ...Key) {
	t.Cleanup(func() {
		for _, k := range keys {
			rdb.Del(context.Background(), k.EntryKey(), k.IndexKey(), k.TeamKey(), k.ScopeKey())
		}
	})
}

// The Step 2.3 property, through real Redis rather than through the key
// derivation alone: an organisation shares one cache, and nothing reaches
// across organisations however identical the request.
func TestStoreScopeIsolation(t *testing.T) {
	store, rdb, ctx := newTestStore(t, StoreConfig{})
	org := uniqueOrg(t)
	req := baseRequest()

	writer := NewKey(Scope{Org: org, Team: "acme"}, req)
	sibling := NewKey(Scope{Org: org, Team: "beta"}, req)
	stranger := NewKey(Scope{Org: uniqueOrg(t), Team: "acme"}, req)
	isolated := NewKey(Scope{Org: org, Team: "acme", Isolated: true}, req)
	cleanupKeys(t, rdb, writer, sibling, stranger, isolated)

	if err := store.Put(ctx, writer, Entry{Response: "42", Embedding: []float32{1}}, time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}

	cases := map[string]struct {
		key     Key
		wantHit bool
	}{
		"the writing team reads its own entry":  {writer, true},
		"a sibling team in the same org shares": {sibling, true},
		"another organisation cannot reach it":  {stranger, false},
		"a team that opted out cannot reach it": {isolated, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, found, err := store.Exact(ctx, tc.key)
			if err != nil {
				t.Fatalf("Exact: %v", err)
			}
			if found != tc.wantHit {
				t.Errorf("found = %v, want %v", found, tc.wantHit)
			}
		})
	}
}

// The fill test behind the per-tenant cap. It asserts the entries themselves
// are deleted, not merely unlinked from the index: the cap exists to bound
// memory, and trimming the index alone would leave them occupying it until TTL.
func TestStoreTrimsScopeToCap(t *testing.T) {
	store, rdb, ctx := newTestStore(t, StoreConfig{MaxScopeEntries: 3})
	scope := Scope{Org: uniqueOrg(t), Team: "acme"}

	keys := make([]Key, 5)
	for i := range keys {
		req := baseRequest()
		req.Messages[1].Content = "question " + strconv.Itoa(i)
		keys[i] = NewKey(scope, req)
	}
	cleanupKeys(t, rdb, keys...)

	// Explicit, increasing timestamps so "oldest" is deterministic rather than
	// whatever order the wall clock happened to produce within one nanosecond.
	base := time.Now()
	for i, k := range keys {
		e := Entry{
			Response:  "answer",
			Embedding: []float32{1},
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		}
		if err := store.Put(ctx, k, e, time.Minute); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	for i, k := range keys {
		_, found, err := store.Exact(ctx, k)
		if err != nil {
			t.Fatalf("Exact %d: %v", i, err)
		}
		if want := i >= 2; found != want {
			t.Errorf("entry %d found = %v, want %v (the two oldest should be trimmed)", i, found, want)
		}
	}

	n, err := rdb.ZCard(ctx, keys[0].ScopeKey()).Result()
	if err != nil {
		t.Fatalf("ZCard: %v", err)
	}
	if n != 3 {
		t.Errorf("scope index holds %d members, want 3", n)
	}
}
