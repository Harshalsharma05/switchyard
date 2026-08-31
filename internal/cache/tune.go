package cache

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// SweepPoint is what one candidate threshold would have produced.
type SweepPoint struct {
	Threshold float32 `json:"threshold"`

	// WouldHit counts entries that an earlier entry in the same fingerprint
	// bucket would have answered at this threshold — requests that were paid
	// for but need not have been.
	WouldHit int `json:"would_hit"`

	Comparable int     `json:"comparable"`
	HitRate    float64 `json:"hit_rate"`
}

// SweepBin is one column of the best-match similarity histogram. The shape of
// this distribution is what shows whether a threshold sits on a cliff or in a
// flat region, which is the difference between a tuned number and a guess.
type SweepBin struct {
	Low   float32 `json:"low"`
	High  float32 `json:"high"`
	Count int     `json:"count"`
}

// SweepReport is the tuning endpoint's answer.
type SweepReport struct {
	Buckets      int          `json:"buckets"`
	Entries      int          `json:"entries"`
	Comparable   int          `json:"comparable"`
	Points       []SweepPoint `json:"points"`
	Distribution []SweepBin   `json:"distribution"`

	// Limitation is reported inline because it changes how the numbers should
	// be read, and a caller who sees only hit rates will over-trust them.
	Limitation string `json:"limitation"`
	ElapsedMS  int64  `json:"elapsed_ms"`
}

const sweepLimitation = "hit rate only: measures how many stored entries a lower threshold would have collapsed, not whether collapsing them would have been correct. Prompt text is never stored, so true replay is not possible."

// Sweep replays the cache's own index at a range of thresholds.
//
// Postgres stores no prompt text by design, so the stored embeddings are the
// only record of real traffic available. For each fingerprint bucket the
// entries are walked oldest-first and scored against their predecessors, which
// reconstructs the decision the lookup would have faced at insert time.
func (s *Store) Sweep(ctx context.Context, thresholds []float32, maxBuckets int) (*SweepReport, error) {
	start := time.Now()

	if len(thresholds) == 0 {
		thresholds = defaultThresholds()
	}
	sort.Slice(thresholds, func(i, j int) bool { return thresholds[i] < thresholds[j] })

	indexKeys, err := s.scanIndexKeys(ctx, maxBuckets)
	if err != nil {
		return nil, err
	}

	report := &SweepReport{
		Buckets:    len(indexKeys),
		Limitation: sweepLimitation,
	}

	// bestScores holds, per entry that had at least one predecessor, its
	// closest predecessor's similarity. Every threshold question is answered
	// from this one pass.
	var bestScores []float32

	for _, indexKey := range indexKeys {
		vectors, err := s.bucketVectors(ctx, indexKey)
		if err != nil {
			return nil, err
		}
		report.Entries += len(vectors)

		// bucketVectors returns newest-first; reverse so each entry is scored
		// only against entries that already existed when it was written.
		for i := len(vectors) - 2; i >= 0; i-- {
			later := vectors[i]
			var best float32 = -2
			for j := i + 1; j < len(vectors); j++ {
				if len(vectors[j]) != len(later) {
					continue
				}
				if sc := Similarity(later, vectors[j]); sc > best {
					best = sc
				}
			}
			if best > -2 {
				bestScores = append(bestScores, best)
			}
		}
	}

	report.Comparable = len(bestScores)

	for _, t := range thresholds {
		point := SweepPoint{Threshold: t, Comparable: len(bestScores)}
		for _, sc := range bestScores {
			if sc >= t {
				point.WouldHit++
			}
		}
		if point.Comparable > 0 {
			point.HitRate = float64(point.WouldHit) / float64(point.Comparable)
		}
		report.Points = append(report.Points, point)
	}

	report.Distribution = histogram(bestScores)
	report.ElapsedMS = time.Since(start).Milliseconds()
	return report, nil
}

// scanIndexKeys lists the fingerprint buckets to sweep, capped so an operator
// call can never walk an unbounded keyspace.
func (s *Store) scanIndexKeys(ctx context.Context, maxBuckets int) ([]string, error) {
	if maxBuckets <= 0 {
		maxBuckets = 1000
	}

	keys, err := s.scanKeys(ctx, keyPrefix+":index:*", maxBuckets)
	if err != nil {
		return nil, err
	}
	if len(keys) > maxBuckets {
		keys = keys[:maxBuckets]
	}
	return keys, nil
}

// bucketVectors loads one fingerprint bucket's embeddings, newest first.
func (s *Store) bucketVectors(ctx context.Context, indexKey string) ([][]float32, error) {
	ids, err := s.rdb.ZRevRange(ctx, indexKey, 0, int64(s.maxCandidates-1)).Result()
	if err != nil {
		return nil, fmt.Errorf("reading cache index: %w", err)
	}

	_, vectors, _, err := s.loadVectors(ctx, ids)
	if err != nil {
		return nil, err
	}
	return vectors, nil
}

func defaultThresholds() []float32 {
	out := make([]float32, 0, 21)
	for i := 0; i <= 20; i++ {
		out = append(out, 0.5+float32(i)*0.025)
	}
	return out
}

const histogramBins = 20

func histogram(scores []float32) []SweepBin {
	bins := make([]SweepBin, histogramBins)
	for i := range bins {
		bins[i].Low = float32(i) / histogramBins
		bins[i].High = float32(i+1) / histogramBins
	}
	for _, sc := range scores {
		if sc < 0 {
			sc = 0
		}
		i := int(sc * histogramBins)
		if i >= histogramBins {
			i = histogramBins - 1
		}
		bins[i].Count++
	}
	return bins
}
