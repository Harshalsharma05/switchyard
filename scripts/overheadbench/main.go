// Command overheadbench measures gateway overhead against a running SwitchYard.
//
// The p95 < 10ms budget is the number this project is built to defend, and
// every feature since Phase 7 has added work to the hot path. This produces
// that figure reproducibly — per cache outcome, since a semantic miss and an
// exact hit are different amounts of work and averaging them hides both.
//
// Overhead is read from X-Switchyard-Overhead-Ms, measured inside the gateway
// and already excluding provider and embedding time. Client-side timing would
// measure the network and the provider instead.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// subjects vary the prompt enough that two of them are not near neighbours,
// so -mode=miss really does miss rather than quietly measuring the cache.
var subjects = []string{
	"the water cycle", "how a bicycle gear works", "the rules of chess castling",
	"why bread rises", "the structure of a haiku", "how sailboats tack upwind",
	"the layers of the atmosphere", "what a semaphore does", "how compost heats up",
	"the parts of a violin", "why the sky looks red at sunset", "how a lock and key work",
}

type sample struct {
	overheadMS float64
	embedMS    float64
	status     int
	cache      string
	routeTier  string
}

func main() {
	var (
		baseURL = flag.String("base-url", "http://localhost:8080", "gateway public URL")
		model   = flag.String("model", "auto", `model field to send ("auto" exercises routing)`)
		n       = flag.Int("n", 60, "requests to send")
		rpm     = flag.Int("rpm", 50, "pacing, requests per minute; keep under the team's RPM limit")
		mode    = flag.String("mode", "miss", "miss | hit | mixed — whether prompts repeat")
		// Not lower: a response truncated by max_tokens finishes with
		// "length", and cache.Cacheable refuses to store those — so a tight
		// cap silently measures a cache that never stores anything.
		maxTokens = flag.Int("max-tokens", 128, "cap the response so provider time and cost stay small")
		threshold = flag.Float64("threshold", 10, "fail if overall p95 overhead exceeds this, in ms")
		envFile   = flag.String("env-file", ".env", "file to read SWITCHYARD_API_KEY from when unset")
	)
	flag.Parse()

	key := os.Getenv("SWITCHYARD_API_KEY")
	if key == "" {
		key = keyFromFile(*envFile, "SWITCHYARD_API_KEY")
	}
	if key == "" {
		fmt.Fprintln(os.Stderr, "set SWITCHYARD_API_KEY to a team key from configs/teams.yaml")
		os.Exit(1)
	}

	client := &http.Client{Timeout: 120 * time.Second}
	interval := time.Duration(float64(time.Minute) / float64(*rpm))

	fmt.Printf("overheadbench: %d requests, model=%q mode=%s, paced at %d rpm\n\n", *n, *model, *mode, *rpm)

	samples := make([]sample, 0, *n)
	for i := range *n {
		if i > 0 {
			time.Sleep(interval)
		}
		s, err := send(client, *baseURL, key, *model, prompt(*mode, i), *maxTokens)
		if err != nil {
			fmt.Fprintf(os.Stderr, "request %d: %v\n", i, err)
			continue
		}
		samples = append(samples, s)
		fmt.Fprintf(os.Stderr, "\r  %d/%d", len(samples), *n)
	}
	fmt.Fprint(os.Stderr, "\r\033[K")

	if len(samples) == 0 {
		fmt.Fprintln(os.Stderr, "no responses collected")
		os.Exit(1)
	}
	report(samples)

	overall := percentile(overheads(samples), 95)
	fmt.Printf("\np95 overhead %.2fms against a %.0fms budget: ", overall, *threshold)
	if overall > *threshold {
		fmt.Println("OVER BUDGET")
		os.Exit(1)
	}
	fmt.Println("within budget")
}

// prompt decides whether this request should hit the cache. "hit" repeats one
// prompt so every request after the first is served from the exact tier.
func prompt(mode string, i int) string {
	switch mode {
	case "hit":
		return "Explain " + subjects[0] + " in one sentence."
	case "mixed":
		if i%2 == 0 {
			return "Explain " + subjects[0] + " in one sentence."
		}
	}
	return fmt.Sprintf("Explain %s in one sentence. (variation %d)", subjects[i%len(subjects)], i)
}

func send(c *http.Client, baseURL, key, model, text string, maxTokens int) (sample, error) {
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"messages":   []map[string]string{{"role": "user", "content": text}},
	})
	if err != nil {
		return sample{}, err
	}

	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return sample{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := c.Do(req)
	if err != nil {
		return sample{}, err
	}
	defer resp.Body.Close()
	io_copy_discard(resp)

	h := resp.Header
	return sample{
		overheadMS: floatOr(h.Get("X-Switchyard-Overhead-Ms")),
		embedMS:    floatOr(h.Get("X-Switchyard-Embed-Ms")),
		status:     resp.StatusCode,
		cache:      orDash(h.Get("X-Switchyard-Cache")),
		routeTier:  orDash(h.Get("X-Switchyard-Route-Tier")),
	}, nil
}

// report prints the distribution overall and split by cache outcome, because
// an exact hit and a semantic miss are different amounts of gateway work.
func report(samples []sample) {
	byStatus := map[int]int{}
	byTier := map[string]int{}
	byCache := map[string][]float64{}
	var embeds []float64

	for _, s := range samples {
		byStatus[s.status]++
		byTier[s.routeTier]++
		byCache[s.cache] = append(byCache[s.cache], s.overheadMS)
		if s.embedMS > 0 {
			embeds = append(embeds, s.embedMS)
		}
	}

	fmt.Println("gateway overhead, ms")
	printRow("all", overheads(samples))
	for _, tier := range sortedKeys(byCache) {
		printRow("cache="+tier, byCache[tier])
	}
	if len(embeds) > 0 {
		fmt.Println("\nembedding latency, ms (excluded from overhead above)")
		printRow("embed", embeds)
	}

	fmt.Print("\nstatus: ")
	for _, code := range sortedIntKeys(byStatus) {
		fmt.Printf("%d×%d  ", code, byStatus[code])
	}
	fmt.Print("\nrouted:  ")
	for _, tier := range sortedKeys2(byTier) {
		fmt.Printf("%s×%d  ", tier, byTier[tier])
	}
	fmt.Println()
}

func printRow(label string, vs []float64) {
	if len(vs) == 0 {
		return
	}
	fmt.Printf("  %-16s n=%-4d p50 %6.2f  p95 %6.2f  p99 %6.2f  max %6.2f\n",
		label, len(vs), percentile(vs, 50), percentile(vs, 95), percentile(vs, 99), percentile(vs, 100))
}

func overheads(samples []sample) []float64 {
	out := make([]float64, 0, len(samples))
	for _, s := range samples {
		out = append(out, s.overheadMS)
	}
	return out
}

// percentile uses nearest-rank on a copy, so callers keep their ordering.
func percentile(vs []float64, p int) float64 {
	if len(vs) == 0 {
		return 0
	}
	s := append([]float64(nil), vs...)
	sort.Float64s(s)
	i := (p * len(s)) / 100
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i]
}

func floatOr(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func sortedKeys(m map[string][]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys2(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedIntKeys(m map[int]int) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func io_copy_discard(resp *http.Response) {
	buf := make([]byte, 4096)
	for {
		if _, err := resp.Body.Read(buf); err != nil {
			return
		}
	}
}

// keyFromFile reads KEY=value from a dotenv-style file.
func keyFromFile(path, name string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if after, ok := strings.CutPrefix(line, name+"="); ok {
			return strings.Trim(after, `"'`)
		}
	}
	return ""
}
