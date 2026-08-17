package health

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// newTestRedis connects to a real Redis and skips rather than fails when none
// is reachable — the same convention internal/ratelimit and internal/budget
// use, since this is what Monitor.persist and restoreFromRedis actually talk
// to, not a mock.
func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()

	addr := "localhost:6379"
	rdb := redis.NewClient(&redis.Options{Addr: addr})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		t.Skipf("no Redis reachable at %s: %v", addr, err)
	}

	t.Cleanup(func() { rdb.Close() })
	return rdb
}

// TestMonitorStatusSurvivesRestart proves the Phase 5 checklist item: a
// status persisted by one Monitor is picked back up by a fresh one built
// against the same Redis and the same provider name — the shape of a gateway
// restart, without actually restarting a process.
func TestMonitorStatusSurvivesRestart(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	log, _ := newTestLogger()
	name := "restart-test-" + time.Now().Format("150405.000000000")
	providers := []provider.Provider{&provider.Mock{ProviderName: name}}
	t.Cleanup(func() { rdb.Del(context.Background(), statusKey(name)) })

	cfg := testMonitorConfig()
	first, err := NewMonitor(providers, NewRecorder(providers), rdb, log, cfg)
	if err != nil {
		t.Fatalf("NewMonitor() error: %v", err)
	}
	for i := 0; i < cfg.DownConsecutiveFailures; i++ {
		first.Observe(ctx, name, errPingFailed)
	}
	if got := first.Status(name); got != StatusDown {
		t.Fatalf("first.Status() = %v, want %v before restart", got, StatusDown)
	}

	// A fresh Monitor, same Redis, same provider name: this stands in for the
	// gateway process restarting while Redis stays up.
	second, err := NewMonitor(providers, NewRecorder(providers), rdb, log, cfg)
	if err != nil {
		t.Fatalf("NewMonitor() error: %v", err)
	}
	if got := second.Status(name); got != StatusDown {
		t.Fatalf("second.Status() = %v, want %v restored from Redis", got, StatusDown)
	}
}

// TestMonitorRestoreIsFailOpenOnUnknownKey proves a provider that was never
// persisted (a brand new one in configs/providers.yaml, or a Redis that was
// flushed) restores to the optimistic Healthy default rather than erroring.
func TestMonitorRestoreIsFailOpenOnUnknownKey(t *testing.T) {
	rdb := newTestRedis(t)
	log, _ := newTestLogger()
	name := "never-persisted-" + time.Now().Format("150405.000000000")
	providers := []provider.Provider{&provider.Mock{ProviderName: name}}

	m, err := NewMonitor(providers, NewRecorder(providers), rdb, log, testMonitorConfig())
	if err != nil {
		t.Fatalf("NewMonitor() error: %v", err)
	}
	if got := m.Status(name); got != StatusHealthy {
		t.Errorf("Status() = %v, want %v for a provider with no persisted status", got, StatusHealthy)
	}
}
