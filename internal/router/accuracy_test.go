// External test package: internal/config imports internal/router, so an
// in-package test could not load the real configs/router.yaml.
package router_test

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Harshalsharma05/switchyard/internal/config"
	"github.com/Harshalsharma05/switchyard/internal/provider"
	"github.com/Harshalsharma05/switchyard/internal/router"
)

// minHeldoutAccuracy is Step 8.1's bar. V1 is a routing skeleton, not a
// research artifact, so ~80% is sufficient and anything below it is a
// regression worth failing the build over.
const minHeldoutAccuracy = 0.80

// Labelling rule for testdata, applied by hand: `complex` means a small cheap
// model would plausibly give a materially worse answer — multi-step reasoning,
// generation over supplied context, design under constraints. `simple` means
// single-step recall, definition, or a trivial transformation.
type labelled struct {
	Label  string `json:"label"`
	Prompt string `json:"prompt"`
}

func loadSet(t *testing.T, name string) []labelled {
	t.Helper()

	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatalf("opening %s: %v", name, err)
	}
	defer f.Close()

	var out []labelled
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		var l labelled
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		out = append(out, l)
	}
	if err := s.Err(); err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return out
}

func shippedClassifier(t testing.TB) *router.Classifier {
	t.Helper()

	// The shipped config, not test constants: the number this reports has to
	// describe the classifier that actually runs.
	cfg, err := config.LoadRouter("../../configs/router.yaml")
	if err != nil {
		t.Fatalf("loading router config: %v", err)
	}
	if !cfg.Enabled {
		t.Fatal("configs/router.yaml has routing disabled; the accuracy number would be meaningless")
	}
	return router.NewClassifier(cfg.Classifier)
}

// score runs one set and reports accuracy, naming every miss so the dev set is
// actually usable for tuning rather than just a pass/fail gate.
func score(t *testing.T, c *router.Classifier, set []labelled) float64 {
	t.Helper()

	correct := 0
	for _, l := range set {
		d := c.Classify(provider.Request{
			Messages: []provider.Message{{Role: provider.RoleUser, Content: l.Prompt}},
		})
		if d.Level.String() == l.Label {
			correct++
			continue
		}
		t.Logf("MISS want=%s got=%s score=%.2f signals=[%s] prompt=%q",
			l.Label, d.Level, d.Score, d.Reason(), truncate(l.Prompt, 70))
	}

	acc := float64(correct) / float64(len(set))
	t.Logf("accuracy %.1f%% (%d/%d)", acc*100, correct, len(set))
	return acc
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// TestClassifierAccuracy is the committed source of Step 8.1's accuracy
// number. The dev set is where weights were tuned; only heldout gates.
func TestClassifierAccuracy(t *testing.T) {
	c := shippedClassifier(t)

	t.Run("dev", func(t *testing.T) {
		score(t, c, loadSet(t, "dev.jsonl"))
	})

	t.Run("heldout", func(t *testing.T) {
		acc := score(t, c, loadSet(t, "heldout.jsonl"))
		if acc < minHeldoutAccuracy {
			t.Errorf("held-out accuracy %.1f%% is below the %.0f%% bar", acc*100, minHeldoutAccuracy*100)
		}
	})
}

// BenchmarkClassify is the committed source of the "classification latency is
// negligible on the hot path" claim in the Phase 8 checklist.
func BenchmarkClassify(b *testing.B) {
	c := shippedClassifier(b)
	req := provider.Request{Messages: []provider.Message{{
		Role: provider.RoleUser,
		Content: "Design a rate limiter for a multi-tenant API gateway. It must be fair " +
			"across tenants, must survive a Redis outage, and should add under 5ms of " +
			"latency. Explain the trade-offs and return the comparison as a table.",
	}}}

	b.ReportAllocs()
	for b.Loop() {
		_ = c.Classify(req)
	}
}
