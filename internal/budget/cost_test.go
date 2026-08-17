package budget

import "testing"

func TestCalculatorCost(t *testing.T) {
	// gpt-4o and llama3.2:3b prices are taken straight from configs/providers.yaml
	// (already converted to micro-dollars, the way internal/config would),
	// so this test doubles as the Phase 4 checklist's "matches manual
	// calculation against provider pricing" check for those two entries.
	calc := NewCalculator(map[string]Pricing{
		"gpt-4o":      {InputPer1M: 2_500_000, OutputPer1M: 10_000_000},
		"llama3.2:3b": {InputPer1M: 0, OutputPer1M: 0},
	})

	tests := map[string]struct {
		model      string
		in, out    int
		wantMicros int64
		wantErr    bool
	}{
		"typical request": {
			model: "gpt-4o", in: 1000, out: 500,
			// (1000*2_500_000 + 500*10_000_000) / 1_000_000 = (2_500_000_000 + 5_000_000_000) / 1_000_000
			wantMicros: 7500,
		},
		"zero usage": {
			model: "gpt-4o", in: 0, out: 0,
			wantMicros: 0,
		},
		"free local model": {
			model: "llama3.2:3b", in: 100_000, out: 100_000,
			wantMicros: 0,
		},
		"sub-microdollar remainder truncates": {
			// 3 input tokens * 2_500_000 = 7_500_000 micros, i.e. exactly
			// 7.5 micros before the output side is added — chosen to prove
			// the division floors rather than rounds.
			model: "gpt-4o", in: 3, out: 0,
			wantMicros: 7, // 7_500_000 / 1_000_000 = 7 (floored), not 8
		},
		"unpriced model errors": {
			model: "claude-opus-9", in: 100, out: 100,
			wantErr: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := calc.Cost(tt.model, tt.in, tt.out)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Cost() error = nil, want an error for unpriced model %q", tt.model)
				}
				return
			}
			if err != nil {
				t.Fatalf("Cost() unexpected error: %v", err)
			}
			if got != tt.wantMicros {
				t.Errorf("Cost(%q, %d, %d) = %d micros, want %d", tt.model, tt.in, tt.out, got, tt.wantMicros)
			}
		})
	}
}

func TestNewCalculatorCopiesPricingMap(t *testing.T) {
	src := map[string]Pricing{"m": {InputPer1M: 1_000_000, OutputPer1M: 1_000_000}}
	calc := NewCalculator(src)

	// Mutate the caller's map after construction; the Calculator must not
	// observe it, since a *Calculator is meant to be handed to a Handler and
	// read concurrently without a lock.
	src["m"] = Pricing{InputPer1M: 999_999_999, OutputPer1M: 999_999_999}

	got, err := calc.Cost("m", 1, 0)
	if err != nil {
		t.Fatalf("Cost() unexpected error: %v", err)
	}
	if got != 1 {
		t.Errorf("Cost() = %d, want 1 (unaffected by the caller's later mutation)", got)
	}
}
