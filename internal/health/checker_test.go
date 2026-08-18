package health

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

func newTestLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

// testMonitorConfig mirrors the plan's example thresholds (10%/50% error
// rate) with round numbers for the two counts the plan leaves unspecified.
func testMonitorConfig() MonitorConfig {
	return MonitorConfig{
		DegradedErrorRate:       0.10,
		DownErrorRate:           0.50,
		DownConsecutiveFailures: 3,
		RecoveryStreak:          3,
	}
}

// newTestMonitor builds a Monitor with a nil Redis client — Monitor.persist
// treats that as "no Redis configured" and skips the write, which is exactly
// what Checker's own tests need: they exercise the ticking and observation
// wiring, not Redis persistence.
func newTestMonitor(t *testing.T, providers []provider.Provider, log *slog.Logger) *Monitor {
	t.Helper()
	m, err := NewMonitor(providers, NewRecorder(providers), nil, nil, log, testMonitorConfig())
	if err != nil {
		t.Fatalf("NewMonitor() error: %v", err)
	}
	return m
}

func TestNewCheckerValidation(t *testing.T) {
	log, _ := newTestLogger()
	mocks := []provider.Provider{&provider.Mock{ProviderName: "p1"}}
	monitor := newTestMonitor(t, mocks, log)

	tests := map[string]struct {
		interval, timeout time.Duration
		wantErr           bool
	}{
		"valid":                    {interval: 30 * time.Second, timeout: 5 * time.Second},
		"zero interval":            {interval: 0, timeout: 5 * time.Second, wantErr: true},
		"negative interval":        {interval: -1, timeout: 5 * time.Second, wantErr: true},
		"zero timeout":             {interval: 30 * time.Second, timeout: 0, wantErr: true},
		"timeout equal interval":   {interval: 5 * time.Second, timeout: 5 * time.Second, wantErr: true},
		"timeout exceeds interval": {interval: 5 * time.Second, timeout: 10 * time.Second, wantErr: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := NewChecker(mocks, monitor, log, tt.interval, tt.timeout)
			if tt.wantErr && err == nil {
				t.Fatalf("NewChecker() error = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("NewChecker() unexpected error: %v", err)
			}
		})
	}
}

// TestCheckerRunPingsImmediatelyAndOnEachTick proves the Step 5.1 shape: one
// goroutine per provider, an immediate check, and further checks on every
// tick — for every provider independently, not just the first.
func TestCheckerRunPingsImmediatelyAndOnEachTick(t *testing.T) {
	log, _ := newTestLogger()
	mockA := &provider.Mock{ProviderName: "a", Models: []string{"a-model"}}
	mockB := &provider.Mock{ProviderName: "b", Models: []string{"b-model"}}
	providers := []provider.Provider{mockA, mockB}
	monitor := newTestMonitor(t, providers, log)

	checker, err := NewChecker(providers, monitor, log, 15*time.Millisecond, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("NewChecker() error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		checker.Run(ctx)
		close(done)
	}()

	// Long enough for the immediate ping plus at least two more ticks at 15ms.
	time.Sleep(70 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run() did not return after ctx was cancelled — a watch goroutine leaked")
	}

	if got := mockA.Attempts(); got < 2 {
		t.Errorf("provider a: Attempts() = %d, want at least 2", got)
	}
	if got := mockB.Attempts(); got < 2 {
		t.Errorf("provider b: Attempts() = %d, want at least 2", got)
	}
}

// TestCheckerCheckUsesItsOwnTimeout proves a ping is bounded by the checker's
// timeout rather than however long the provider takes to answer — the reason
// Step 5.1 asks for a timeout independent of the provider's request timeout.
func TestCheckerCheckUsesItsOwnTimeout(t *testing.T) {
	log, buf := newTestLogger()
	slow := &provider.Mock{ProviderName: "slow", Models: []string{"m"}, Delay: 50 * time.Millisecond}
	monitor := newTestMonitor(t, []provider.Provider{slow}, log)

	checker, err := NewChecker([]provider.Provider{slow}, monitor, log, time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("NewChecker() error: %v", err)
	}

	start := time.Now()
	checker.check(context.Background(), slow)
	elapsed := time.Since(start)

	if elapsed >= slow.Delay {
		t.Errorf("check() took %s, want well under the provider's %s delay — the 10ms ping timeout should have cut it short", elapsed, slow.Delay)
	}
	if !strings.Contains(buf.String(), "active health check failed") {
		t.Errorf("log output = %q, want a failed-check line", buf.String())
	}
}

func TestCheckerCheckLogsSuccess(t *testing.T) {
	log, buf := newTestLogger()
	healthy := &provider.Mock{ProviderName: "healthy", Models: []string{"m"}}
	monitor := newTestMonitor(t, []provider.Provider{healthy}, log)

	checker, err := NewChecker([]provider.Provider{healthy}, monitor, log, time.Second, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("NewChecker() error: %v", err)
	}

	checker.check(context.Background(), healthy)

	if !strings.Contains(buf.String(), "active health check ok") {
		t.Errorf("log output = %q, want an ok-check line", buf.String())
	}
}
