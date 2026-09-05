package quality

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type fakeScorer struct {
	verdict Verdict
	err     error
}

func (f fakeScorer) Score(context.Context, Sample) (Verdict, error) {
	return f.verdict, f.err
}

type fakeStore struct {
	mu     sync.Mutex
	scores map[string]float64
}

func (s *fakeStore) SetQualityScore(_ context.Context, id string, score float64, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scores == nil {
		s.scores = map[string]float64{}
	}
	s.scores[id] = score
	return true, nil
}

func (s *fakeStore) get(id string) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.scores[id]
	return v, ok
}

func newTestWorker(sc scorer, st store) *Worker {
	return NewWorker(WorkerConfig{Concurrency: 1}, NewSampler(Config{}), sc, st, nil, discardLogger())
}

func TestWorkerScoresAndStores(t *testing.T) {
	st := &fakeStore{}
	w := newTestWorker(fakeScorer{verdict: Verdict{Score: 4}}, st)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	w.Enqueue(Sample{RequestID: "r1", Reason: ReasonDowngraded})

	waitFor(t, func() bool { _, ok := st.get("r1"); return ok })
	if v, _ := st.get("r1"); v != 4 {
		t.Fatalf("stored score = %v, want 4", v)
	}
}

func TestWorkerScorerFailureIsSwallowed(t *testing.T) {
	st := &fakeStore{}
	w := newTestWorker(fakeScorer{err: errors.New("judge unavailable")}, st)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	w.Enqueue(Sample{RequestID: "r1", Reason: ReasonRoutedSample})
	time.Sleep(50 * time.Millisecond)
	if _, ok := st.get("r1"); ok {
		t.Fatal("a failed score must not be stored")
	}
}

func TestWorkerEnqueueNeverBlocks(t *testing.T) {
	// No Run, so nothing drains the queue: every Enqueue past QueueSize must
	// drop rather than block the caller.
	w := NewWorker(WorkerConfig{QueueSize: 2}, NewSampler(Config{}), fakeScorer{}, &fakeStore{}, nil, discardLogger())

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			w.Enqueue(Sample{RequestID: "r"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Enqueue blocked when the queue was full")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
