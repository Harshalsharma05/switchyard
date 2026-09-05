package logstore

import (
	"testing"
	"time"
)

// TestQualityFeedbackSince covers the Step 9.3 aggregation: per-reason stats,
// the low-score filter, and the downgraded-only example list. Needs Postgres
// (skips without it, like every other logstore test).
func TestQualityFeedbackSince(t *testing.T) {
	pool, ctx := newTestPool(t)
	w := NewWriter(pool, Config{}, nil, discardLogger())

	base := time.Now().UTC().Truncate(time.Millisecond)
	rows := []Record{
		row("dg-low-1", base, "downgraded", 2),
		row("dg-low-2", base.Add(-time.Minute), "downgraded", 3),
		row("dg-ok", base, "downgraded", 5),
		row("cache-low", base, "near_threshold_cache_hit", 2.5),
		row("cache-ok", base, "near_threshold_cache_hit", 4),
	}
	seed(t, w, ctx, rows)
	for _, r := range rows {
		if _, err := w.SetQualityScore(ctx, r.ID, *r.QualityScore, r.QualitySampleReason); err != nil {
			t.Fatalf("SetQualityScore %s: %v", r.ID, err)
		}
	}

	fb, err := w.QualityFeedbackSince(ctx, base.Add(-time.Hour), "", 3, 10)
	if err != nil {
		t.Fatalf("QualityFeedbackSince: %v", err)
	}

	stat := map[string]QualityReasonStat{}
	for _, s := range fb.Reasons {
		stat[s.Reason] = s
	}
	if dg := stat["downgraded"]; dg.Scored != 3 || dg.LowScored != 2 {
		t.Errorf("downgraded: scored=%d low=%d, want 3 and 2", dg.Scored, dg.LowScored)
	}
	if c := stat["near_threshold_cache_hit"]; c.Scored != 2 || c.LowScored != 1 {
		t.Errorf("cache: scored=%d low=%d, want 2 and 1", c.Scored, c.LowScored)
	}

	// Examples are downgraded-only, low-only, newest first.
	if len(fb.Examples) != 2 {
		t.Fatalf("examples = %d, want 2", len(fb.Examples))
	}
	if fb.Examples[0].ID != "dg-low-1" || fb.Examples[1].ID != "dg-low-2" {
		t.Errorf("example order = %s,%s", fb.Examples[0].ID, fb.Examples[1].ID)
	}
}

func row(id string, ts time.Time, reason string, score float64) Record {
	r := sampleRecord(id)
	r.Timestamp = ts
	r.QualityScore = &score
	r.QualitySampleReason = reason
	r.RoutingTier = "fast"
	r.RoutingReason = "score=0.4 lookup"
	return r
}
