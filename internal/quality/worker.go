package quality

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/telemetry"
)

// scorer grades one sample. Split out so the worker is tested without a model.
type scorer interface {
	Score(ctx context.Context, s Sample) (Verdict, error)
}

// store persists a score against its request row. logstore.Writer satisfies it.
type store interface {
	SetQualityScore(ctx context.Context, id string, score float64, reason string) (bool, error)
}

// WorkerConfig tunes the async worker. Zero values get sane defaults.
type WorkerConfig struct {
	QueueSize    int
	Concurrency  int
	ScoreTimeout time.Duration
}

func (c WorkerConfig) withDefaults() WorkerConfig {
	if c.QueueSize <= 0 {
		c.QueueSize = 256
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 4
	}
	if c.ScoreTimeout <= 0 {
		c.ScoreTimeout = 20 * time.Second
	}
	return c
}

// Worker samples finished requests and scores the selected ones off the
// request path. It embeds *Sampler, so the handler calls Decide and Enqueue
// through one interface.
type Worker struct {
	*Sampler

	cfg     WorkerConfig
	queue   chan Sample
	scorer  scorer
	store   store
	metrics *telemetry.Metrics
	log     *slog.Logger

	wg   sync.WaitGroup
	done chan struct{}
}

func NewWorker(cfg WorkerConfig, sampler *Sampler, sc scorer, st store, metrics *telemetry.Metrics, log *slog.Logger) *Worker {
	cfg = cfg.withDefaults()
	return &Worker{
		Sampler: sampler,
		cfg:     cfg,
		queue:   make(chan Sample, cfg.QueueSize),
		scorer:  sc,
		store:   st,
		metrics: metrics,
		log:     log,
		done:    make(chan struct{}),
	}
}

// Enqueue offers a selected sample to the worker. It never blocks: a full
// queue drops the sample and fires a metric, because nothing about a quality
// score is worth slowing a request goroutine for.
func (w *Worker) Enqueue(s Sample) {
	select {
	case w.queue <- s:
		w.count(s.Reason, "enqueued")
	default:
		w.count(s.Reason, "dropped")
	}
	w.reportDepth()
}

// Run scores samples until ctx is cancelled, then returns without draining:
// an unscored sample is a missed sample, which the sampling policy already
// tolerates, and shutdown must not wait on judge calls.
func (w *Worker) Run(ctx context.Context) {
	defer close(w.done)

	for i := 0; i < w.cfg.Concurrency; i++ {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			for {
				select {
				case s := <-w.queue:
					w.process(ctx, s)
					w.reportDepth()
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	w.wg.Wait()
}

// Wait blocks until Run's goroutines have all returned, or timeout elapses.
func (w *Worker) Wait(timeout time.Duration) {
	select {
	case <-w.done:
	case <-time.After(timeout):
		w.log.Warn("quality worker did not stop in time", slog.Duration("timeout", timeout))
	}
}

func (w *Worker) process(ctx context.Context, s Sample) {
	ctx, cancel := context.WithTimeout(ctx, w.cfg.ScoreTimeout)
	defer cancel()

	v, err := w.scorer.Score(ctx, s)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("quality scoring failed",
				slog.String("request_id", s.RequestID), slog.Any("error", err))
		}
		w.count(s.Reason, "error")
		return
	}

	if _, err := w.store.SetQualityScore(ctx, s.RequestID, v.Score, string(s.Reason)); err != nil {
		w.log.Warn("storing quality score",
			slog.String("request_id", s.RequestID), slog.Any("error", err))
		w.count(s.Reason, "error")
		return
	}

	w.log.LogAttrs(ctx, slog.LevelInfo, "quality scored",
		slog.String("request_id", s.RequestID),
		slog.String("reason", string(s.Reason)),
		slog.Float64("score", v.Score),
	)
	w.count(s.Reason, "scored")
	if w.metrics != nil {
		w.metrics.QualityScore.WithLabelValues(s.TeamID).Observe(v.Score)
	}
}

func (w *Worker) count(reason Reason, outcome string) {
	if w.metrics != nil {
		w.metrics.QualitySamplesTotal.WithLabelValues(string(reason), outcome).Inc()
	}
}

func (w *Worker) reportDepth() {
	if w.metrics != nil {
		w.metrics.QualityQueueDepth.Set(float64(len(w.queue)))
	}
}
