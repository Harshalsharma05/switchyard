package router

import (
	"strings"
	"testing"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// Covers what the accuracy set structurally cannot: it is all single-message
// prompts, so multi-turn handling and signal saturation go untested there.
func TestClassifyFeatureExtraction(t *testing.T) {
	c := NewClassifier(ClassifierConfig{
		Threshold: 1.0,
		Weights:   Weights{Length: 0.8, Reasoning: 1.0, Context: 0.7},
		Scales:    Scales{LengthTokens: 350, ReasoningMatches: 1, ConstraintMatches: 3, FormatMatches: 1},
		Reasoning: Lexicon{"analyze"},
	})

	user := func(s string) provider.Message { return provider.Message{Role: provider.RoleUser, Content: s} }

	cases := map[string]struct {
		msgs      []provider.Message
		wantLevel Complexity
		wantHas   []string
		wantLacks []string
	}{
		// A verbose model answer must not classify the follow-up turn.
		"assistant content is not scanned": {
			msgs: []provider.Message{
				user("hi"),
				{Role: provider.RoleAssistant, Content: "Let me analyze that in depth."},
				user("thanks"),
			},
			wantLevel: Simple,
			wantHas:   []string{"context=0.5"},
			wantLacks: []string{"reasoning"},
		},
		"user content is scanned": {
			msgs:      []provider.Message{user("analyze this quarter")},
			wantLevel: Complex,
			wantHas:   []string{"reasoning=1"},
		},
		"a code block is full context": {
			msgs:      []provider.Message{user("```\nx := 1\n```")},
			wantLevel: Simple,
			wantHas:   []string{"context=1.0"},
		},
		// Without saturation one pasted document would outweigh every other
		// signal and route everything up.
		"length saturates at its weight": {
			msgs:      []provider.Message{user(strings.Repeat("word ", 50000))},
			wantLevel: Simple,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := c.Classify(provider.Request{Messages: tc.msgs})
			if d.Level != tc.wantLevel {
				t.Errorf("level = %s, want %s (score %.2f, signals %s)", d.Level, tc.wantLevel, d.Score, d.Reason())
			}
			for _, want := range tc.wantHas {
				if !strings.Contains(d.Reason(), want) {
					t.Errorf("signals %q missing %q", d.Reason(), want)
				}
			}
			for _, lack := range tc.wantLacks {
				if strings.Contains(d.Reason(), lack) {
					t.Errorf("signals %q should not contain %q", d.Reason(), lack)
				}
			}
		})
	}
}
