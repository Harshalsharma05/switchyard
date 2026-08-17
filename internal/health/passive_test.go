package health

import (
	"errors"
	"testing"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

func TestWindowStatsAgesOutOldSamples(t *testing.T) {
	now := time.Now()
	w := &window{}

	// Older than windowDuration (60s): must not count toward Stats at all.
	w.record(sample{at: now.Add(-90 * time.Second), latency: 10 * time.Millisecond})
	// Within the window: this is the only sample Stats should see.
	w.record(sample{at: now, latency: 20 * time.Millisecond, failed: true})

	got := w.stats(now)
	if got.Count != 1 {
		t.Fatalf("Count = %d, want 1 (the aged-out sample must be excluded)", got.Count)
	}
	if got.ErrorRate != 1.0 {
		t.Errorf("ErrorRate = %v, want 1.0", got.ErrorRate)
	}
	if got.P99Latency != 20*time.Millisecond {
		t.Errorf("P99Latency = %v, want 20ms", got.P99Latency)
	}
}

func TestWindowStatsErrorAndTimeoutRates(t *testing.T) {
	now := time.Now()
	w := &window{}

	for i := 0; i < 7; i++ {
		w.record(sample{at: now, latency: time.Millisecond})
	}
	for i := 0; i < 2; i++ {
		w.record(sample{at: now, latency: time.Millisecond, failed: true})
	}
	// A timeout is also a failure, so this counts toward both rates.
	w.record(sample{at: now, latency: time.Millisecond, failed: true, timeout: true})

	got := w.stats(now)
	if got.Count != 10 {
		t.Fatalf("Count = %d, want 10", got.Count)
	}
	if got.ErrorRate != 0.3 {
		t.Errorf("ErrorRate = %v, want 0.3", got.ErrorRate)
	}
	if got.TimeoutRate != 0.1 {
		t.Errorf("TimeoutRate = %v, want 0.1", got.TimeoutRate)
	}
}

// TestWindowIsARingBufferNotAnUnboundedSlice proves the Step 5.2 checklist
// item directly: writing past capacity overwrites the oldest entries rather
// than growing forever.
func TestWindowIsARingBufferNotAnUnboundedSlice(t *testing.T) {
	now := time.Now()
	w := &window{}

	for i := 0; i < windowCapacity; i++ {
		w.record(sample{at: now, latency: time.Millisecond})
	}
	// windowCapacity more, all failures: this must wrap around and overwrite
	// every success recorded above, not grow the buffer to hold both.
	for i := 0; i < windowCapacity; i++ {
		w.record(sample{at: now, latency: time.Millisecond, failed: true})
	}

	got := w.stats(now)
	if got.Count != windowCapacity {
		t.Fatalf("Count = %d, want capped at %d", got.Count, windowCapacity)
	}
	if got.ErrorRate != 1.0 {
		t.Errorf("ErrorRate = %v, want 1.0 — every original success should have been overwritten", got.ErrorRate)
	}
}

// TestWindowEvictsOldestFirst pins down the ring buffer's eviction order, not
// just its capacity: writing capacity/2 more samples must overwrite exactly
// the oldest half, leaving the newer half intact.
func TestWindowEvictsOldestFirst(t *testing.T) {
	now := time.Now()
	w := &window{}

	half := windowCapacity / 2
	for i := 0; i < windowCapacity; i++ {
		w.record(sample{at: now, latency: time.Millisecond})
	}
	for i := 0; i < half; i++ {
		w.record(sample{at: now, latency: time.Millisecond, failed: true})
	}

	got := w.stats(now)
	if got.Count != windowCapacity {
		t.Fatalf("Count = %d, want %d", got.Count, windowCapacity)
	}
	wantErrorRate := float64(half) / float64(windowCapacity)
	if got.ErrorRate != wantErrorRate {
		t.Errorf("ErrorRate = %v, want %v (only the oldest half should have been overwritten)", got.ErrorRate, wantErrorRate)
	}
}

func TestRecorderRecordAndStats(t *testing.T) {
	providers := []provider.Provider{
		&provider.Mock{ProviderName: "openai"},
		&provider.Mock{ProviderName: "ollama"},
	}
	r := NewRecorder(providers)

	r.Record("openai", 5*time.Millisecond, nil)
	r.Record("openai", 5*time.Millisecond, errors.New("boom"))
	r.Record("openai", 5*time.Millisecond, &provider.Error{Kind: provider.KindTimeout})

	got := r.Stats("openai")
	if got.Count != 3 {
		t.Fatalf("Count = %d, want 3", got.Count)
	}
	if want := 2.0 / 3.0; got.ErrorRate != want {
		t.Errorf("ErrorRate = %v, want %v", got.ErrorRate, want)
	}
	if want := 1.0 / 3.0; got.TimeoutRate != want {
		t.Errorf("TimeoutRate = %v, want %v", got.TimeoutRate, want)
	}

	// ollama got no calls at all — its window must read as empty, not error.
	if got := r.Stats("ollama"); got.Count != 0 {
		t.Errorf("ollama Stats().Count = %d, want 0", got.Count)
	}
}

// TestRecorderIgnoresUnknownProvider proves Record can never be the reason a
// request fails: a name it doesn't recognize (a typo, or a provider removed
// from configs/providers.yaml since NewRecorder ran) is dropped, not panicked
// or errored.
func TestRecorderIgnoresUnknownProvider(t *testing.T) {
	r := NewRecorder([]provider.Provider{&provider.Mock{ProviderName: "known"}})

	r.Record("unknown", time.Millisecond, errors.New("boom"))

	if got := r.Stats("unknown"); got.Count != 0 {
		t.Errorf("Stats(\"unknown\").Count = %d, want 0", got.Count)
	}
}

func TestRecorderNonProviderErrorIsNotATimeout(t *testing.T) {
	r := NewRecorder([]provider.Provider{&provider.Mock{ProviderName: "p"}})

	r.Record("p", time.Millisecond, errors.New("connection reset"))

	got := r.Stats("p")
	if got.ErrorRate != 1.0 {
		t.Errorf("ErrorRate = %v, want 1.0", got.ErrorRate)
	}
	if got.TimeoutRate != 0 {
		t.Errorf("TimeoutRate = %v, want 0 — a plain error is not a timeout", got.TimeoutRate)
	}
}
