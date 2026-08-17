package health

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// windowCapacity and windowDuration together implement Step 5.2's rolling
// window: "last 100 requests or last 60s, whichever is shorter." The ring
// buffer never holds more than windowCapacity samples; Stats additionally
// discards any sample older than windowDuration even when the buffer isn't
// full yet, which is what makes a burst of very old traffic followed by
// silence age out instead of permanently anchoring the stats.
const (
	windowCapacity = 100
	windowDuration = 60 * time.Second
)

// sample is one real request's outcome against a provider.
type sample struct {
	at      time.Time
	latency time.Duration
	failed  bool
	timeout bool
}

// window is a fixed-capacity ring buffer of recent samples for one provider.
// It carries its own mutex because, unlike proxy's requestMetrics, it is
// written from whatever goroutine net/http assigned each in-flight request —
// many requests are recording into the same provider's window concurrently.
type window struct {
	mu      sync.Mutex
	samples [windowCapacity]sample
	next    int
	count   int
}

func (w *window) record(s sample) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.samples[w.next] = s
	w.next = (w.next + 1) % windowCapacity
	if w.count < windowCapacity {
		w.count++
	}
}

// Stats summarizes a provider's passive signal over the rolling window, as of
// the moment it was computed. The zero Stats (Count 0, everything else 0)
// correctly describes "nothing recorded in the window" rather than needing a
// separate ok bool — Step 5.3's status computation can treat it exactly like
// any other empty-window case.
type Stats struct {
	Count       int
	ErrorRate   float64
	TimeoutRate float64
	P99Latency  time.Duration
}

func (w *window) stats(now time.Time) Stats {
	w.mu.Lock()
	defer w.mu.Unlock()

	latencies := make([]time.Duration, 0, w.count)
	var errCount, timeoutCount int
	for i := 0; i < w.count; i++ {
		s := w.samples[i]
		if now.Sub(s.at) > windowDuration {
			continue
		}
		latencies = append(latencies, s.latency)
		if s.failed {
			errCount++
		}
		if s.timeout {
			timeoutCount++
		}
	}

	n := len(latencies)
	if n == 0 {
		return Stats{}
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p99Index := int(float64(n) * 0.99)
	if p99Index >= n {
		p99Index = n - 1
	}

	return Stats{
		Count:       n,
		ErrorRate:   float64(errCount) / float64(n),
		TimeoutRate: float64(timeoutCount) / float64(n),
		P99Latency:  latencies[p99Index],
	}
}

// Recorder is Step 5.2's passive signal: internal/proxy calls Record after
// every real provider call, success or failure, and Step 5.3's status
// computation will read Stats back out to compute healthy/degraded/down.
//
// One window per provider is built once, at construction, from the registry —
// the same shape Checker uses for the active checks. That means the map
// itself is never mutated after NewRecorder returns, so concurrent Record and
// Stats calls need no lock around the map, only around each window's own
// samples.
type Recorder struct {
	windows map[string]*window
}

// NewRecorder builds a Recorder with one window per provider.
func NewRecorder(providers []provider.Provider) *Recorder {
	windows := make(map[string]*window, len(providers))
	for _, p := range providers {
		windows[p.Name()] = &window{}
	}
	return &Recorder{windows: windows}
}

// Record stores one real request's outcome against the named provider.
// latency is whatever the caller considers this request's response time —
// full round trip for a non-streaming call, time to first byte for a stream,
// matching what Stats' P99Latency is meant to answer: "how long before this
// provider starts responding."
//
// An unrecognized provider name is silently dropped rather than erroring:
// recording a passive signal must never be able to fail a request, the same
// promise CLAUDE.md makes for telemetry generally.
func (r *Recorder) Record(providerName string, latency time.Duration, err error) {
	w, ok := r.windows[providerName]
	if !ok {
		return
	}

	s := sample{at: time.Now(), latency: latency, failed: err != nil}
	if err != nil {
		var provErr *provider.Error
		if errors.As(err, &provErr) && provErr.Kind == provider.KindTimeout {
			s.timeout = true
		}
	}
	w.record(s)
}

// Stats returns the named provider's current rolling-window signal. A name
// this Recorder does not track returns the zero Stats, which reads the same
// as "tracked but nothing recorded yet" — both mean there is no signal to act
// on.
func (r *Recorder) Stats(providerName string) Stats {
	w, ok := r.windows[providerName]
	if !ok {
		return Stats{}
	}
	return w.stats(time.Now())
}
