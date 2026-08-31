package cache

import (
	"math"
	"testing"
)

// Normalize and Similarity are the arithmetic every threshold decision rests
// on: a silent bug here serves confidently wrong cache hits.
func TestNormalizeAndSimilarity(t *testing.T) {
	tests := map[string]struct {
		a, b []float32
		want float32
	}{
		"identical":  {[]float32{3, 4}, []float32{3, 4}, 1},
		"orthogonal": {[]float32{1, 0}, []float32{0, 1}, 0},
		"opposite":   {[]float32{1, 0}, []float32{-1, 0}, -1},
		"scaled":     {[]float32{1, 1}, []float32{5, 5}, 1},
		"zero":       {[]float32{0, 0}, []float32{1, 0}, 0},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := Similarity(Normalize(tc.a), Normalize(tc.b))
			if math.Abs(float64(got-tc.want)) > 1e-6 {
				t.Fatalf("similarity = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSimilarityMismatchedLengths(t *testing.T) {
	if got := Similarity([]float32{1, 0}, []float32{1, 0, 0}); got != 0 {
		t.Fatalf("similarity = %v, want 0 for mismatched dimensions", got)
	}
}
