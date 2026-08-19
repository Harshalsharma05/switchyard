package health

import (
	"context"
	"testing"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

func TestMonitorConfigValidation(t *testing.T) {
	log, _ := newTestLogger()

	tests := map[string]struct {
		cfg     MonitorConfig
		wantErr bool
	}{
		"valid": {
			cfg: MonitorConfig{DegradedErrorRate: 0.10, DownErrorRate: 0.50, DownConsecutiveFailures: 3, RecoveryStreak: 3},
		},
		"degraded rate zero": {
			cfg:     MonitorConfig{DegradedErrorRate: 0, DownErrorRate: 0.50, DownConsecutiveFailures: 3, RecoveryStreak: 3},
			wantErr: true,
		},
		"down rate over one": {
			cfg:     MonitorConfig{DegradedErrorRate: 0.10, DownErrorRate: 1.5, DownConsecutiveFailures: 3, RecoveryStreak: 3},
			wantErr: true,
		},
		"down not above degraded": {
			cfg:     MonitorConfig{DegradedErrorRate: 0.50, DownErrorRate: 0.50, DownConsecutiveFailures: 3, RecoveryStreak: 3},
			wantErr: true,
		},
		"zero consecutive failures": {
			cfg:     MonitorConfig{DegradedErrorRate: 0.10, DownErrorRate: 0.50, DownConsecutiveFailures: 0, RecoveryStreak: 3},
			wantErr: true,
		},
		"zero recovery streak": {
			cfg:     MonitorConfig{DegradedErrorRate: 0.10, DownErrorRate: 0.50, DownConsecutiveFailures: 3, RecoveryStreak: 0},
			wantErr: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			providers := []provider.Provider{&provider.Mock{ProviderName: "p"}}
			_, err := NewMonitor(providers, NewRecorder(providers), nil, nil, nil, log, tt.cfg)
			if tt.wantErr && err == nil {
				t.Fatalf("NewMonitor() error = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("NewMonitor() unexpected error: %v", err)
			}
		})
	}
}

// TestMonitorStartsHealthy proves the checklist's "all three providers report
// healthy at rest": a Monitor with no observations yet reports Healthy, not
// some other zero value.
func TestMonitorStartsHealthy(t *testing.T) {
	log, _ := newTestLogger()
	providers := []provider.Provider{&provider.Mock{ProviderName: "p"}}
	m, err := NewMonitor(providers, NewRecorder(providers), nil, nil, nil, log, testMonitorConfig())
	if err != nil {
		t.Fatalf("NewMonitor() error: %v", err)
	}

	if got := m.Status("p"); got != StatusHealthy {
		t.Errorf("Status() = %v, want %v", got, StatusHealthy)
	}
}

// TestMonitorConsecutivePingFailuresGoDown proves the active-signal half of
// Step 5.3: N consecutive failed pings take a provider down even with a
// perfectly clean passive window (no real traffic recorded at all).
func TestMonitorConsecutivePingFailuresGoDown(t *testing.T) {
	ctx := context.Background()
	log, _ := newTestLogger()
	providers := []provider.Provider{&provider.Mock{ProviderName: "p"}}
	cfg := testMonitorConfig()
	m, err := NewMonitor(providers, NewRecorder(providers), nil, nil, nil, log, cfg)
	if err != nil {
		t.Fatalf("NewMonitor() error: %v", err)
	}

	for i := 1; i < cfg.DownConsecutiveFailures; i++ {
		m.Observe(ctx, "p", errPingFailed)
		if got := m.Status("p"); got != StatusHealthy {
			t.Fatalf("after %d failures: Status() = %v, want still %v (threshold is %d)", i, got, StatusHealthy, cfg.DownConsecutiveFailures)
		}
	}

	m.Observe(ctx, "p", errPingFailed)
	if got := m.Status("p"); got != StatusDown {
		t.Fatalf("after %d failures: Status() = %v, want %v", cfg.DownConsecutiveFailures, got, StatusDown)
	}

	// A single success resets the streak and, with a clean passive window,
	// reads Healthy on the very next evaluation — but recovery from Down
	// still needs RecoveryStreak consecutive good evaluations, so it must not
	// jump straight back.
	m.Observe(ctx, "p", nil)
	if got := m.Status("p"); got == StatusHealthy {
		t.Fatalf("Status() = %v after only one good ping, want it still short of healthy (RecoveryStreak=%d)", got, cfg.RecoveryStreak)
	}
}

// TestMonitorErrorRateThresholdsAreImmediate proves passive-signal
// degradation applies on the very next evaluation, with no hysteresis on the
// way down — only recovery to Healthy is gated.
func TestMonitorErrorRateThresholdsAreImmediate(t *testing.T) {
	ctx := context.Background()
	log, _ := newTestLogger()
	providers := []provider.Provider{&provider.Mock{ProviderName: "p"}}
	recorder := NewRecorder(providers)
	m, err := NewMonitor(providers, recorder, nil, nil, nil, log, testMonitorConfig())
	if err != nil {
		t.Fatalf("NewMonitor() error: %v", err)
	}

	// 9 successes, 1 failure: exactly the 10% degraded threshold.
	for i := 0; i < 9; i++ {
		recorder.Record("p", time.Millisecond, nil)
	}
	recorder.Record("p", time.Millisecond, errPingFailed)

	m.Observe(ctx, "p", nil)
	if got := m.Status("p"); got != StatusDegraded {
		t.Fatalf("Status() = %v, want %v at exactly the degraded threshold", got, StatusDegraded)
	}
}

func TestMonitorHardErrorRateGoesDownImmediately(t *testing.T) {
	ctx := context.Background()
	log, _ := newTestLogger()
	providers := []provider.Provider{&provider.Mock{ProviderName: "p"}}
	recorder := NewRecorder(providers)
	m, err := NewMonitor(providers, recorder, nil, nil, nil, log, testMonitorConfig())
	if err != nil {
		t.Fatalf("NewMonitor() error: %v", err)
	}

	for i := 0; i < 5; i++ {
		recorder.Record("p", time.Millisecond, errPingFailed)
	}
	for i := 0; i < 5; i++ {
		recorder.Record("p", time.Millisecond, nil)
	}

	// A single Observe, starting from Healthy: down must apply immediately,
	// with no hysteresis required to get *worse*.
	m.Observe(ctx, "p", nil)
	if got := m.Status("p"); got != StatusDown {
		t.Fatalf("Status() = %v, want %v on the first evaluation of a 50%% error rate", got, StatusDown)
	}
}

// TestMonitorRecoveryRequiresConsecutiveHealthySignals is the checklist's
// hysteresis case: a provider parked at Down must see RecoveryStreak
// consecutive healthy evaluations, with no bad ones in between, before
// returning to Healthy.
func TestMonitorRecoveryRequiresConsecutiveHealthySignals(t *testing.T) {
	ctx := context.Background()
	log, _ := newTestLogger()
	providers := []provider.Provider{&provider.Mock{ProviderName: "p"}}
	cfg := testMonitorConfig()
	m, err := NewMonitor(providers, NewRecorder(providers), nil, nil, nil, log, cfg)
	if err != nil {
		t.Fatalf("NewMonitor() error: %v", err)
	}

	for i := 0; i < cfg.DownConsecutiveFailures; i++ {
		m.Observe(ctx, "p", errPingFailed)
	}
	if got := m.Status("p"); got != StatusDown {
		t.Fatalf("Status() = %v, want %v after driving it down", got, StatusDown)
	}

	for i := 1; i < cfg.RecoveryStreak; i++ {
		m.Observe(ctx, "p", nil)
		if got := m.Status("p"); got != StatusDown {
			t.Fatalf("after %d good pings: Status() = %v, want still %v (RecoveryStreak=%d)", i, got, StatusDown, cfg.RecoveryStreak)
		}
	}

	m.Observe(ctx, "p", nil)
	if got := m.Status("p"); got != StatusHealthy {
		t.Fatalf("after %d consecutive good pings: Status() = %v, want %v", cfg.RecoveryStreak, got, StatusHealthy)
	}
}

// TestMonitorRecoveryStreakResetsOnABadSignal proves the hysteresis counter
// doesn't just count good pings anywhere in history — a bad one in the middle
// must restart it from zero.
func TestMonitorRecoveryStreakResetsOnABadSignal(t *testing.T) {
	ctx := context.Background()
	log, _ := newTestLogger()
	providers := []provider.Provider{&provider.Mock{ProviderName: "p"}}
	cfg := testMonitorConfig()
	m, err := NewMonitor(providers, NewRecorder(providers), nil, nil, nil, log, cfg)
	if err != nil {
		t.Fatalf("NewMonitor() error: %v", err)
	}

	for i := 0; i < cfg.DownConsecutiveFailures; i++ {
		m.Observe(ctx, "p", errPingFailed)
	}

	// One good ping short of recovering, then a bad one: the streak must
	// restart, so RecoveryStreak-1 more good pings after this are still not
	// enough on their own.
	for i := 1; i < cfg.RecoveryStreak; i++ {
		m.Observe(ctx, "p", nil)
	}
	m.Observe(ctx, "p", errPingFailed)

	for i := 1; i < cfg.RecoveryStreak; i++ {
		m.Observe(ctx, "p", nil)
		if got := m.Status("p"); got == StatusHealthy {
			t.Fatalf("recovered after only %d good pings following the reset — the earlier streak should not have carried over", i)
		}
	}
}

// TestMonitorP99AboveBaselineDegrades exercises the EMA baseline path: a
// provider whose window has always been fast establishes a low baseline,
// and a later window whose p99 spikes past 3x that baseline degrades on the
// very next evaluation even with a perfect error rate.
func TestMonitorP99AboveBaselineDegrades(t *testing.T) {
	ctx := context.Background()
	log, _ := newTestLogger()
	providers := []provider.Provider{&provider.Mock{ProviderName: "p"}}
	recorder := NewRecorder(providers)
	m, err := NewMonitor(providers, recorder, nil, nil, nil, log, testMonitorConfig())
	if err != nil {
		t.Fatalf("NewMonitor() error: %v", err)
	}

	for i := 0; i < 10; i++ {
		recorder.Record("p", 10*time.Millisecond, nil)
	}
	m.Observe(ctx, "p", nil) // seeds the baseline at ~10ms
	if got := m.Status("p"); got != StatusHealthy {
		t.Fatalf("Status() = %v after a fast, clean window, want %v", got, StatusHealthy)
	}

	// One slow sample among the eleven now in the window becomes the p99 —
	// comfortably over 3x the ~10ms baseline just established.
	recorder.Record("p", 40*time.Millisecond, nil)
	m.Observe(ctx, "p", nil)
	if got := m.Status("p"); got != StatusDegraded {
		t.Fatalf("Status() = %v after a p99 spike to 40ms against a ~10ms baseline, want %v", got, StatusDegraded)
	}
}

func TestMonitorUnknownProviderIsFailOpenHealthy(t *testing.T) {
	log, _ := newTestLogger()
	providers := []provider.Provider{&provider.Mock{ProviderName: "known"}}
	m, err := NewMonitor(providers, NewRecorder(providers), nil, nil, nil, log, testMonitorConfig())
	if err != nil {
		t.Fatalf("NewMonitor() error: %v", err)
	}

	if got := m.Status("unknown"); got != StatusHealthy {
		t.Errorf("Status(\"unknown\") = %v, want %v", got, StatusHealthy)
	}

	// Must not panic on a name Monitor was never built to track.
	m.Observe(context.Background(), "unknown", errPingFailed)
}

// TestMonitorSnapshotReportsCurrentSignal proves Step 5.4's per-provider
// report — status, error rate, p99, last check — reads back what Observe
// actually computed, not some separately-tracked copy of it.
func TestMonitorSnapshotReportsCurrentSignal(t *testing.T) {
	ctx := context.Background()
	log, _ := newTestLogger()
	providers := []provider.Provider{&provider.Mock{ProviderName: "p"}}
	recorder := NewRecorder(providers)
	m, err := NewMonitor(providers, recorder, nil, nil, nil, log, testMonitorConfig())
	if err != nil {
		t.Fatalf("NewMonitor() error: %v", err)
	}

	before := time.Now()
	recorder.Record("p", 5*time.Millisecond, nil)
	m.Observe(ctx, "p", nil)

	snap, ok := m.Snapshot("p")
	if !ok {
		t.Fatalf("Snapshot(\"p\") ok = false, want true")
	}
	if snap.Status != StatusHealthy {
		t.Errorf("Status = %v, want %v", snap.Status, StatusHealthy)
	}
	if snap.ErrorRate != 0 {
		t.Errorf("ErrorRate = %v, want 0", snap.ErrorRate)
	}
	if snap.P99Latency != 5*time.Millisecond {
		t.Errorf("P99Latency = %v, want 5ms", snap.P99Latency)
	}
	if snap.LastCheckAt.Before(before) {
		t.Errorf("LastCheckAt = %v, want at or after %v", snap.LastCheckAt, before)
	}

	if _, ok := m.Snapshot("unknown"); ok {
		t.Errorf("Snapshot(\"unknown\") ok = true, want false")
	}
}

// TestMonitorSnapshotHistoryTracksTransitions proves Step 5.4's retained
// history: LastTransition and History both reflect real status changes, most
// recent first, with the reason that triggered each one.
func TestMonitorSnapshotHistoryTracksTransitions(t *testing.T) {
	ctx := context.Background()
	log, _ := newTestLogger()
	providers := []provider.Provider{&provider.Mock{ProviderName: "p"}}
	cfg := testMonitorConfig()
	m, err := NewMonitor(providers, NewRecorder(providers), nil, nil, nil, log, cfg)
	if err != nil {
		t.Fatalf("NewMonitor() error: %v", err)
	}

	// A freshly built Monitor starts Healthy without ever recording a
	// transition into it — so there is nothing to report yet.
	snap, _ := m.Snapshot("p")
	if snap.LastTransition != nil {
		t.Fatalf("LastTransition = %+v, want nil before any transition", snap.LastTransition)
	}
	if len(snap.History) != 0 {
		t.Fatalf("History = %+v, want empty before any transition", snap.History)
	}

	for i := 0; i < cfg.DownConsecutiveFailures; i++ {
		m.Observe(ctx, "p", errPingFailed)
	}

	snap, _ = m.Snapshot("p")
	if snap.LastTransition == nil {
		t.Fatalf("LastTransition = nil, want the Healthy->Down transition")
	}
	if snap.LastTransition.From != StatusHealthy || snap.LastTransition.To != StatusDown {
		t.Errorf("LastTransition = %+v, want From=Healthy To=Down", snap.LastTransition)
	}
	if snap.LastTransition.Reason != "consecutive_ping_failures" {
		t.Errorf("LastTransition.Reason = %q, want consecutive_ping_failures", snap.LastTransition.Reason)
	}
	if len(snap.History) != 1 {
		t.Fatalf("History = %+v, want exactly one entry", snap.History)
	}

	for i := 0; i < cfg.RecoveryStreak; i++ {
		m.Observe(ctx, "p", nil)
	}

	snap, _ = m.Snapshot("p")
	if len(snap.History) != 2 {
		t.Fatalf("History = %+v, want two entries after recovering", snap.History)
	}
	// recent() orders newest first.
	if snap.History[0].To != StatusHealthy || snap.History[1].To != StatusDown {
		t.Errorf("History order = %+v, want [recovered, down]", snap.History)
	}
}

// TestTransitionLogCapsAtHistoryCapacity proves the history log is a ring
// buffer, not an unbounded slice, the same requirement passive.go's window
// meets for request samples.
func TestTransitionLogCapsAtHistoryCapacity(t *testing.T) {
	l := &transitionLog{}
	for i := 0; i < transitionHistoryCapacity+10; i++ {
		l.append(Transition{At: time.Now(), From: StatusHealthy, To: StatusDown, Reason: "test"})
	}

	if got := len(l.recent()); got != transitionHistoryCapacity {
		t.Errorf("recent() returned %d entries, want capped at %d", got, transitionHistoryCapacity)
	}
}

func TestMonitorSnapshotsPreservesRegistryOrder(t *testing.T) {
	log, _ := newTestLogger()
	providers := []provider.Provider{
		&provider.Mock{ProviderName: "first"},
		&provider.Mock{ProviderName: "second"},
		&provider.Mock{ProviderName: "third"},
	}
	m, err := NewMonitor(providers, NewRecorder(providers), nil, nil, nil, log, testMonitorConfig())
	if err != nil {
		t.Fatalf("NewMonitor() error: %v", err)
	}

	snaps := m.Snapshots()
	if len(snaps) != 3 {
		t.Fatalf("Snapshots() returned %d entries, want 3", len(snaps))
	}
	want := []string{"first", "second", "third"}
	for i, name := range want {
		if snaps[i].Provider != name {
			t.Errorf("Snapshots()[%d].Provider = %q, want %q", i, snaps[i].Provider, name)
		}
	}
}

func TestMonitorAllDown(t *testing.T) {
	ctx := context.Background()
	log, _ := newTestLogger()
	providers := []provider.Provider{
		&provider.Mock{ProviderName: "a"},
		&provider.Mock{ProviderName: "b"},
	}
	cfg := testMonitorConfig()
	m, err := NewMonitor(providers, NewRecorder(providers), nil, nil, nil, log, cfg)
	if err != nil {
		t.Fatalf("NewMonitor() error: %v", err)
	}

	if m.AllDown() {
		t.Fatalf("AllDown() = true at rest, want false")
	}

	for i := 0; i < cfg.DownConsecutiveFailures; i++ {
		m.Observe(ctx, "a", errPingFailed)
	}
	if m.AllDown() {
		t.Fatalf("AllDown() = true with only one of two providers down, want false")
	}

	for i := 0; i < cfg.DownConsecutiveFailures; i++ {
		m.Observe(ctx, "b", errPingFailed)
	}
	if !m.AllDown() {
		t.Fatalf("AllDown() = false with every provider down, want true")
	}
}

// errPingFailed stands in for whatever Provider.Ping returns on failure —
// Monitor only cares whether it's nil, not its concrete type, for the
// consecutive-failure signal.
var errPingFailed = context.DeadlineExceeded

// --- Step 7.4: breaker state feeds health status ----------------------------

// stubBreakerOracle is the fake behind BreakerOracle.
type stubBreakerOracle struct {
	open map[string]bool
}

func (s stubBreakerOracle) AnyOpen(providerName string) bool { return s.open[providerName] }

// TestMonitorOpenBreakerDegradesAProvider is Step 7.4's health integration:
// the gateway is already refusing to send this provider traffic, so reporting
// it healthy would contradict the gateway's own behaviour.
func TestMonitorOpenBreakerDegradesAProvider(t *testing.T) {
	ctx := context.Background()
	log, _ := newTestLogger()
	providers := []provider.Provider{&provider.Mock{ProviderName: "p"}}
	breakers := stubBreakerOracle{open: map[string]bool{"p": true}}

	m, err := NewMonitor(providers, NewRecorder(providers), breakers, nil, nil, log, testMonitorConfig())
	if err != nil {
		t.Fatalf("NewMonitor() error: %v", err)
	}

	// A clean ping and an empty passive window: without the breaker signal
	// this provider would be unambiguously healthy.
	m.Observe(ctx, "p", nil)

	if got := m.Status("p"); got != StatusDegraded {
		t.Fatalf("Status() = %v, want %v with an open breaker", got, StatusDegraded)
	}

	snap, ok := m.Snapshot("p")
	if !ok {
		t.Fatalf("Snapshot() reported an untracked provider")
	}
	if snap.LastTransition == nil || snap.LastTransition.Reason != "circuit_breaker_open" {
		t.Errorf("last transition = %+v, want reason circuit_breaker_open", snap.LastTransition)
	}
}

// TestMonitorOpenBreakerIsNeverDown proves the breaker only ever contributes
// Degraded. A breaker is a decision to stop trying, not evidence the provider
// is unreachable, and conflating the two would mislead an operator reading
// status during an incident — it would also fail /readyz through AllDown on
// what may be a perfectly healthy fleet.
func TestMonitorOpenBreakerIsNeverDown(t *testing.T) {
	ctx := context.Background()
	log, _ := newTestLogger()
	providers := []provider.Provider{&provider.Mock{ProviderName: "p"}}
	breakers := stubBreakerOracle{open: map[string]bool{"p": true}}

	m, err := NewMonitor(providers, NewRecorder(providers), breakers, nil, nil, log, testMonitorConfig())
	if err != nil {
		t.Fatalf("NewMonitor() error: %v", err)
	}

	for i := 0; i < 10; i++ {
		m.Observe(ctx, "p", nil)
	}

	if got := m.Status("p"); got != StatusDegraded {
		t.Errorf("Status() = %v, want %v — an open breaker must never escalate to Down", got, StatusDegraded)
	}
	if m.AllDown() {
		t.Errorf("AllDown() = true with only an open breaker, want false")
	}
}

// TestMonitorClosedBreakerLeavesStatusAlone is the control: the signal only
// fires when a breaker is actually open.
func TestMonitorClosedBreakerLeavesStatusAlone(t *testing.T) {
	ctx := context.Background()
	log, _ := newTestLogger()
	providers := []provider.Provider{&provider.Mock{ProviderName: "p"}}
	breakers := stubBreakerOracle{open: map[string]bool{"other": true}}

	m, err := NewMonitor(providers, NewRecorder(providers), breakers, nil, nil, log, testMonitorConfig())
	if err != nil {
		t.Fatalf("NewMonitor() error: %v", err)
	}

	m.Observe(ctx, "p", nil)

	if got := m.Status("p"); got != StatusHealthy {
		t.Errorf("Status() = %v, want %v — another provider's open breaker must not degrade this one", got, StatusHealthy)
	}
}

// TestMonitorBreakerClosingAllowsRecovery proves the signal is not sticky:
// once the breaker closes, the provider recovers through the normal
// hysteresis path rather than staying degraded forever.
func TestMonitorBreakerClosingAllowsRecovery(t *testing.T) {
	ctx := context.Background()
	log, _ := newTestLogger()
	providers := []provider.Provider{&provider.Mock{ProviderName: "p"}}
	breakers := stubBreakerOracle{open: map[string]bool{"p": true}}
	cfg := testMonitorConfig()

	m, err := NewMonitor(providers, NewRecorder(providers), breakers, nil, nil, log, cfg)
	if err != nil {
		t.Fatalf("NewMonitor() error: %v", err)
	}

	m.Observe(ctx, "p", nil)
	if got := m.Status("p"); got != StatusDegraded {
		t.Fatalf("Status() = %v, want %v", got, StatusDegraded)
	}

	breakers.open["p"] = false
	for i := 0; i < cfg.RecoveryStreak; i++ {
		m.Observe(ctx, "p", nil)
	}

	if got := m.Status("p"); got != StatusHealthy {
		t.Errorf("Status() = %v after the breaker closed and the recovery streak was met, want %v", got, StatusHealthy)
	}
}
