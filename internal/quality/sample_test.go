package quality

import "testing"

func sampler(cfg Config, roll float64) *Sampler {
	return &Sampler{cfg: cfg, rng: func() float64 { return roll }}
}

func TestSamplerDecide(t *testing.T) {
	cfg := Config{RoutedRate: 0.10, NearThresholdBand: 0.03}

	cases := map[string]struct {
		cand   Candidate
		roll   float64 // value the routed-rate dice roll returns
		want   Reason
		wantOK bool
	}{
		"plain unrouted request is never sampled": {
			cand: Candidate{}, roll: 0, want: "", wantOK: false,
		},
		"flagged always wins": {
			cand: Candidate{Flagged: true}, roll: 0.99, want: ReasonFlagged, wantOK: true,
		},
		"downgraded tier is always sampled": {
			cand: Candidate{Routed: true, Downgraded: true}, roll: 0.99,
			want: ReasonDowngraded, wantOK: true,
		},
		"semantic hit just over the threshold is sampled": {
			cand: Candidate{CacheHit: true, CacheSemantic: true, Similarity: 0.94, Threshold: 0.93},
			roll: 0.99, want: ReasonNearThreshold, wantOK: true,
		},
		"semantic hit well clear of the threshold is not near-threshold": {
			cand: Candidate{CacheHit: true, CacheSemantic: true, Similarity: 0.99, Threshold: 0.93},
			roll: 0.99, want: "", wantOK: false,
		},
		"exact hit is never near-threshold even at a low score": {
			cand: Candidate{CacheHit: true, CacheSemantic: false, Similarity: 0.93, Threshold: 0.93},
			roll: 0.99, want: "", wantOK: false,
		},
		"routed response is sampled when the roll is under the rate": {
			cand: Candidate{Routed: true}, roll: 0.05, want: ReasonRoutedSample, wantOK: true,
		},
		"routed response is skipped when the roll is over the rate": {
			cand: Candidate{Routed: true}, roll: 0.5, want: "", wantOK: false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := sampler(cfg, tc.roll).Decide(tc.cand)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("Decide() = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestSamplerNilIsSafe(t *testing.T) {
	var s *Sampler
	if _, ok := s.Decide(Candidate{Flagged: true}); ok {
		t.Fatal("nil Sampler should never sample")
	}
}
