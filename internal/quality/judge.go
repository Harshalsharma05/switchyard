package quality

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// rubric is the LLM-as-judge instruction. It is deliberately short and scores
// only responsiveness to the request — not length, tone, or hedging — so the
// number stays comparable across models and tiers.
const rubric = `You are a strict evaluator. Score how well an AI assistant's response answers the user's request, from 1 to 5.

5 - Fully correct, complete, and directly responsive.
4 - Correct and responsive, minor omissions or phrasing issues only.
3 - Partially correct, or misses something the request clearly asked for.
2 - A significant error, misunderstanding, or missing the core of the request.
1 - Wrong, off-topic, empty, or an unwarranted refusal.

Judge only against the request. Do not reward length or style.
Reply with one line of JSON and nothing else: {"score": <1-5>, "reason": "<one short sentence>"}`

// minScore and maxScore bound the judge's answer. A model that returns 0 or 7
// is misbehaving, and clamping keeps one bad call from skewing the aggregate.
const (
	minScore = 1.0
	maxScore = 5.0
)

// completer is the slice of provider.Provider the judge needs. Declared here,
// by the consumer, so a test scores against a fake with no HTTP.
type completer interface {
	Complete(ctx context.Context, req provider.Request) (*provider.Response, error)
}

// Verdict is one scored response.
type Verdict struct {
	Score  float64
	Reason string
}

// Judge scores samples by asking a capable model to grade them.
type Judge struct {
	resolve func(model string) (provider.Provider, error)
	model   string
	timeout time.Duration
}

// NewJudge takes a resolver rather than a fixed provider so the judge follows
// a hot reload of configs/providers.yaml, the same way routing does.
func NewJudge(resolve func(string) (provider.Provider, error), model string, timeout time.Duration) *Judge {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &Judge{resolve: resolve, model: model, timeout: timeout}
}

var zeroTemp = float32(0)

// Score grades one sample. Any error means "no score for this request", which
// the worker treats as a dropped sample — never as a request-path failure,
// since the request finished long ago.
func (j *Judge) Score(ctx context.Context, s Sample) (Verdict, error) {
	prov, err := j.resolve(j.model)
	if err != nil {
		return Verdict{}, fmt.Errorf("resolving judge model %q: %w", j.model, err)
	}

	var c completer = prov
	req := provider.Request{
		Model:       j.model,
		Temperature: &zeroTemp,
		MaxTokens:   200,
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: rubric},
			{Role: provider.RoleUser, Content: transcript(s)},
		},
	}

	resp, err := c.Complete(ctx, req)
	if err != nil {
		return Verdict{}, fmt.Errorf("judge call: %w", err)
	}

	v, err := parseVerdict(resp.Content)
	if err != nil {
		return Verdict{}, fmt.Errorf("parsing judge reply %q: %w", truncate(resp.Content, 120), err)
	}
	return v, nil
}

// transcript renders the request and the answer for the judge. Roles are kept
// so the judge can see a system instruction the answer had to obey.
func transcript(s Sample) string {
	var b strings.Builder
	b.WriteString("## Request\n")
	for _, m := range s.Prompt {
		fmt.Fprintf(&b, "[%s] %s\n", m.Role, m.Content)
	}
	b.WriteString("\n## Response\n")
	b.WriteString(s.Response)
	return b.String()
}

// scoreRe matches the instructed "score": N as well as the ways a model
// drifts out of JSON: "Score: 4", "score of 4", "4/5".
var scoreRe = regexp.MustCompile(`(?i)(?:score|rating|rate)\D{0,15}?([0-5](?:\.[0-9]+)?)`)

var outOfFiveRe = regexp.MustCompile(`([0-5](?:\.[0-9]+)?)\s*/\s*5`)

// parseVerdict reads the judge's reply. It tries strict JSON first, then falls
// back to finding a score in free text, because a model told to answer in JSON
// will still occasionally wrap it in prose.
func parseVerdict(raw string) (Verdict, error) {
	raw = strings.TrimSpace(raw)

	var strict struct {
		Score  float64 `json:"score"`
		Reason string  `json:"reason"`
	}
	if start := strings.IndexByte(raw, '{'); start >= 0 {
		if end := strings.LastIndexByte(raw, '}'); end > start {
			if err := json.Unmarshal([]byte(raw[start:end+1]), &strict); err == nil && strict.Score != 0 {
				return Verdict{Score: clamp(strict.Score), Reason: strict.Reason}, nil
			}
		}
	}

	for _, re := range []*regexp.Regexp{scoreRe, outOfFiveRe} {
		if m := re.FindStringSubmatch(raw); m != nil {
			if n, err := strconv.ParseFloat(m[1], 64); err == nil {
				return Verdict{Score: clamp(n), Reason: raw}, nil
			}
		}
	}
	return Verdict{}, fmt.Errorf("no score found")
}

func clamp(n float64) float64 {
	if n < minScore {
		return minScore
	}
	if n > maxScore {
		return maxScore
	}
	return n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
