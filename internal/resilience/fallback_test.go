package resilience

import (
	"testing"

	"github.com/Harshalsharma05/switchyard/internal/health"
)

// Fixtures shared by the tests below: a two-model "fast" tier plus the
// requested model that heads it.
var (
	groq   = Candidate{Provider: "groq", Model: "fast-a"}
	gemini = Candidate{Provider: "gemini", Model: "fast-b"}
	ollama = Candidate{Provider: "ollama", Model: "fast-c"}
)

// statuses turns a provider->status map into a StatusOf func, defaulting any
// provider it does not mention to healthy — the same default BuildChain
// applies to a nil func.
func statuses(m map[string]health.Status) func(string) health.Status {
	return func(name string) health.Status {
		if s, ok := m[name]; ok {
			return s
		}
		return health.StatusHealthy
	}
}

// allowOnly permits exactly the named providers, ignoring the model, which is
// enough for the ordering tests. The provider/model pair is exercised
// separately in TestBuildChainAllowlistBeatsAvailability.
func allowOnly(names ...string) func(string, string) bool {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return func(providerName, _ string) bool {
		_, ok := set[providerName]
		return ok
	}
}

func TestBuildChain(t *testing.T) {
	tests := map[string]struct {
		in   ChainInput
		want []Candidate
	}{
		"no tier means no fallback": {
			in:   ChainInput{Requested: groq},
			want: []Candidate{groq},
		},
		"requested model heads its own tier": {
			in: ChainInput{
				Requested: gemini,
				Tier:      []Candidate{groq, gemini, ollama},
			},
			want: []Candidate{gemini, groq, ollama},
		},
		"a down provider is skipped": {
			in: ChainInput{
				Requested: groq,
				Tier:      []Candidate{groq, gemini, ollama},
				StatusOf:  statuses(map[string]health.Status{"gemini": health.StatusDown}),
			},
			want: []Candidate{groq, ollama},
		},
		"a degraded provider sinks below healthy ones": {
			in: ChainInput{
				Requested: groq,
				Tier:      []Candidate{groq, gemini, ollama},
				StatusOf:  statuses(map[string]health.Status{"groq": health.StatusDegraded}),
			},
			want: []Candidate{gemini, ollama, groq},
		},
		"degraded candidates keep their declared order among themselves": {
			in: ChainInput{
				Requested: groq,
				Tier:      []Candidate{groq, gemini, ollama},
				StatusOf: statuses(map[string]health.Status{
					"groq":   health.StatusDegraded,
					"gemini": health.StatusDegraded,
				}),
			},
			want: []Candidate{ollama, groq, gemini},
		},
		"everything down falls back to trying anyway": {
			in: ChainInput{
				Requested: groq,
				Tier:      []Candidate{groq, gemini},
				StatusOf: statuses(map[string]health.Status{
					"groq":   health.StatusDown,
					"gemini": health.StatusDown,
				}),
			},
			want: []Candidate{groq, gemini},
		},
		"a forbidden provider is dropped": {
			in: ChainInput{
				Requested: groq,
				Tier:      []Candidate{groq, gemini, ollama},
				Allowed:   allowOnly("groq", "ollama"),
			},
			want: []Candidate{groq, ollama},
		},
		"an empty tier entry is ignored": {
			in: ChainInput{
				Requested: groq,
				Tier:      []Candidate{{Provider: "gemini"}, {Model: "fast-c"}, ollama},
			},
			want: []Candidate{groq, ollama},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := BuildChain(tc.in)
			if !equalChains(got, tc.want) {
				t.Errorf("BuildChain() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBuildChainAllowlistBeatsAvailability is the compliance case called out
// in Step 6.2: a healthy provider the team may not use is never routed to,
// even when every provider it may use is down. The last-resort rule that
// re-admits down providers must never re-admit a forbidden one.
func TestBuildChainAllowlistBeatsAvailability(t *testing.T) {
	got := BuildChain(ChainInput{
		Requested: groq,
		Tier:      []Candidate{groq, gemini},
		StatusOf:  statuses(map[string]health.Status{"groq": health.StatusDown}),
		Allowed: func(providerName, model string) bool {
			return providerName == groq.Provider && model == groq.Model
		},
	})

	want := []Candidate{groq}
	if !equalChains(got, want) {
		t.Fatalf("BuildChain() = %v, want %v (the healthy but forbidden provider must not appear)", got, want)
	}
}

// TestBuildChainNoPermittedCandidates covers the nil return: the team may use
// nothing in the chain, so the caller has to error rather than route.
func TestBuildChainNoPermittedCandidates(t *testing.T) {
	got := BuildChain(ChainInput{
		Requested: groq,
		Tier:      []Candidate{groq, gemini},
		Allowed:   func(string, string) bool { return false },
	})

	if got != nil {
		t.Fatalf("BuildChain() = %v, want nil", got)
	}
}

func equalChains(got, want []Candidate) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
