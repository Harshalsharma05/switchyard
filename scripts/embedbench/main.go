// Command embedbench measures hosted embedding latency and semantic separation.
//
// Phase 7 puts an embedding call on the hot path, and the gateway's overhead
// budget is p95 < 10ms. This produces the number that decides whether that is
// survivable, so no figure in DECISIONS.md is hand-typed.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/cache"
)

// probes are prompt pairs chosen to bracket the threshold question: two
// paraphrases that should hit, and an unrelated prompt that must not.
var probes = []struct {
	name string
	text string
}{
	{"paraphrase-a", "What is the capital of France?"},
	{"paraphrase-b", "Which city is the capital city of France?"},
	{"unrelated", "Write a Go function that reverses a linked list."},
	{"long", "Summarise the following incident report in three bullet points. At 14:02 UTC the primary database began returning connection timeouts. The on-call engineer failed over to the replica at 14:11. Traffic recovered by 14:14. Root cause was connection pool exhaustion caused by a deploy that removed a context timeout."},
}

func main() {
	var (
		iterations = flag.Int("n", 30, "requests per probe")
		model      = flag.String("model", "gemini-embedding-001", "embedding model")
		dims       = flag.Int("dims", 768, "output dimensionality")
		baseURL    = flag.String("base-url", "https://generativelanguage.googleapis.com/v1beta", "embedding API base URL")
		envFile    = flag.String("env-file", ".env", "file to read GEMINI_API_KEY from when unset")
	)
	flag.Parse()

	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		key = keyFromFile(*envFile, "GEMINI_API_KEY")
	}
	if key == "" {
		fmt.Fprintln(os.Stderr, "GEMINI_API_KEY is not set")
		os.Exit(1)
	}

	embedder, err := cache.NewGemini(cache.EmbedConfig{
		BaseURL:    *baseURL,
		APIKey:     key,
		Model:      *model,
		Dimensions: *dims,
		Timeout:    30 * time.Second,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	ctx := context.Background()
	fmt.Printf("model=%s dims=%d iterations=%d\n\n", *model, *dims, *iterations)

	vectors := map[string][]float32{}
	var all []time.Duration

	for _, p := range probes {
		samples := make([]time.Duration, 0, *iterations)
		for i := 0; i < *iterations; i++ {
			start := time.Now()
			v, err := embedder.Embed(ctx, p.text)
			elapsed := time.Since(start)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", p.name, err)
				os.Exit(1)
			}
			samples = append(samples, elapsed)
			vectors[p.name] = v
		}
		all = append(all, samples...)
		report(p.name, samples)
	}

	fmt.Println()
	report("ALL", all)

	fmt.Println("\ncosine similarity:")
	for i := 0; i < len(probes); i++ {
		for j := i + 1; j < len(probes); j++ {
			a, b := probes[i].name, probes[j].name
			fmt.Printf("  %-14s vs %-14s  %.4f\n", a, b, cache.Similarity(vectors[a], vectors[b]))
		}
	}

	fmt.Printf("\noverhead budget is p95 < 10ms for the whole gateway; compare against ALL p95 above.\n")
}

func report(name string, samples []time.Duration) {
	if len(samples) == 0 {
		return
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var total time.Duration
	for _, s := range sorted {
		total += s
	}

	fmt.Printf("%-14s n=%-4d min=%-8v mean=%-8v p50=%-8v p95=%-8v p99=%-8v max=%v\n",
		name, len(sorted),
		sorted[0].Round(time.Millisecond),
		(total / time.Duration(len(sorted))).Round(time.Millisecond),
		pct(sorted, 0.50).Round(time.Millisecond),
		pct(sorted, 0.95).Round(time.Millisecond),
		pct(sorted, 0.99).Round(time.Millisecond),
		sorted[len(sorted)-1].Round(time.Millisecond),
	)
}

func pct(sorted []time.Duration, p float64) time.Duration {
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

// keyFromFile reads one KEY=value line so the script runs from PowerShell
// without the caller exporting anything first.
func keyFromFile(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		name, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(name) == key {
			return strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return ""
}
