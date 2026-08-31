// Package router classifies prompt complexity so Step 8.2 can serve a request
// from the cheapest tier capable of handling it.
package router

import (
	"fmt"
	"strings"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// Complexity is an ordinal, never a tier name: the level -> providers.yaml
// tier mapping is config, so this package knows no provider or model names.
type Complexity int

const (
	Simple Complexity = iota
	Complex
)

func (c Complexity) String() string {
	if c == Complex {
		return "complex"
	}
	return "simple"
}

// Lexicon is a set of lowercase substrings scanned over the prompt.
type Lexicon []string

// Weights scale each [0,1] feature signal into the score. Simple is expected
// to be negative — it pulls trivial lookups back below the threshold.
type Weights struct {
	Length      float64
	Reasoning   float64
	Constraints float64
	Context     float64
	Format      float64
	Simple      float64
}

// Scales are saturation points: the feature value at which a signal reaches
// 1.0 and stops growing, so one very long prompt cannot dominate the score.
type Scales struct {
	LengthTokens      int
	ReasoningMatches  int
	ConstraintMatches int
	FormatMatches     int
}

// ClassifierConfig is everything tunable about classification, loaded from
// configs/router.yaml so weights and vocabulary change without a rebuild.
type ClassifierConfig struct {
	Threshold   float64
	Weights     Weights
	Scales      Scales
	Reasoning   Lexicon
	Constraints Lexicon
	Format      Lexicon
	Simple      Lexicon
}

// Decision is the classifier's answer plus the rationale Step 8.3 surfaces.
// A cost optimiser that hides its decisions is not defensible.
type Decision struct {
	Level   Complexity
	Score   float64
	Signals []string
}

// Reason renders Signals as one header-safe string.
func (d Decision) Reason() string { return strings.Join(d.Signals, " ") }

// Classifier scores prompts against a fixed configuration. It holds no mutable
// state, so one instance is shared across every request.
type Classifier struct{ cfg ClassifierConfig }

func NewClassifier(cfg ClassifierConfig) *Classifier { return &Classifier{cfg: cfg} }

// Classify scores a prompt using lexical features only. No model call: a
// classifier that itself calls an LLM defeats the purpose on cost and latency.
func (c *Classifier) Classify(req provider.Request) Decision {
	scanned, turns, hasCode := promptText(req.Messages)
	lowered := strings.ToLower(scanned)

	tokens := estimateTokens(req.Messages)
	reasoning := countMatches(lowered, c.cfg.Reasoning)
	constraints := countMatches(lowered, c.cfg.Constraints) + countListItems(scanned)
	format := countMatches(lowered, c.cfg.Format)
	simple := countMatches(lowered, c.cfg.Simple)

	// Pasted code is the strongest evidence of supplied context; a prior turn
	// is weaker evidence of the same thing.
	context := 0.0
	switch {
	case hasCode:
		context = 1.0
	case turns > 1:
		context = 0.5
	}

	w, s := c.cfg.Weights, c.cfg.Scales

	var d Decision
	d.Score += w.Length * saturate(float64(tokens), float64(s.LengthTokens))
	d.Score += w.Reasoning * saturate(float64(reasoning), float64(s.ReasoningMatches))
	d.Score += w.Constraints * saturate(float64(constraints), float64(s.ConstraintMatches))
	d.Score += w.Context * context
	d.Score += w.Format * saturate(float64(format), float64(s.FormatMatches))

	// Applied flat rather than saturated: one trivial-lookup phrase is already
	// the whole signal, and matching a second says nothing more.
	if simple > 0 {
		d.Score += w.Simple
	}

	d.Signals = append(d.Signals, fmt.Sprintf("tokens=%d", tokens))
	if reasoning > 0 {
		d.Signals = append(d.Signals, fmt.Sprintf("reasoning=%d", reasoning))
	}
	if constraints > 0 {
		d.Signals = append(d.Signals, fmt.Sprintf("constraints=%d", constraints))
	}
	if context > 0 {
		d.Signals = append(d.Signals, fmt.Sprintf("context=%.1f", context))
	}
	if format > 0 {
		d.Signals = append(d.Signals, fmt.Sprintf("format=%d", format))
	}
	if simple > 0 {
		d.Signals = append(d.Signals, "lookup")
	}
	d.Signals = append(d.Signals, fmt.Sprintf("score=%.2f", d.Score))

	if d.Score >= c.cfg.Threshold {
		d.Level = Complex
	}
	return d
}

// promptText joins the content the lexicons scan, and reports the turn count
// and whether a code block was supplied. Assistant turns are counted but not
// scanned: prior model output is not the request being classified.
func promptText(msgs []provider.Message) (string, int, bool) {
	var b strings.Builder
	turns := 0
	for _, m := range msgs {
		if m.Role != provider.RoleSystem {
			turns++
		}
		if m.Role == provider.RoleAssistant {
			continue
		}
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	text := b.String()
	return text, turns, strings.Contains(text, "```")
}

// estimateTokens reuses the chars/4 heuristic the TPM reservation already
// uses. A real tokenizer would be exact and cost more than this decision.
func estimateTokens(msgs []provider.Message) int {
	total := 0
	for _, m := range msgs {
		total += (len(m.Content)+3)/4 + 4
	}
	return total
}

// countMatches counts distinct patterns present, not total occurrences, so one
// word repeated throughout a long prompt cannot inflate the signal.
func countMatches(lowered string, lex Lexicon) int {
	n := 0
	for _, p := range lex {
		if strings.Contains(lowered, p) {
			n++
		}
	}
	return n
}

// countListItems counts bulleted or enumerated lines, which is how a prompt
// usually spells out several constraints at once.
func countListItems(text string) int {
	n := 0
	for line := range strings.SplitSeq(text, "\n") {
		t := strings.TrimSpace(line)
		if len(t) < 2 {
			continue
		}
		switch {
		case (t[0] == '-' || t[0] == '*') && t[1] == ' ':
			n++
		case t[0] >= '1' && t[0] <= '9' && (t[1] == '.' || t[1] == ')'):
			n++
		}
	}
	return n
}

func saturate(v, scale float64) float64 {
	if scale <= 0 || v <= 0 {
		return 0
	}
	return min(v/scale, 1)
}
