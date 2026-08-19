// Package integration black-box tests the compiled cmd/gateway binary: it
// builds the real binary once, spawns it as a subprocess per test against a
// real Redis and httptest mock upstreams standing in for a provider, and
// drives it purely over HTTP. It never imports internal/proxy — only cmd/ is
// allowed to — so this is the one place that proves the whole wired system
// behaves correctly, not just one package's handlers.
//
//go:build integration

package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var gatewayBin string

func TestMain(m *testing.M) {
	bin, cleanup, err := buildGatewayBinary()
	if err != nil {
		fmt.Fprintln(os.Stderr, "building gateway binary:", err)
		os.Exit(1)
	}
	gatewayBin = bin
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func buildGatewayBinary() (string, func(), error) {
	root, err := repoRoot()
	if err != nil {
		return "", nil, err
	}

	dir, err := os.MkdirTemp("", "switchyard-integration-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }

	name := "switchyard-gateway"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(dir, name)

	cmd := exec.Command("go", "build", "-o", out, "./cmd/gateway")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("go build: %w\n%s", err, output)
	}

	return out, cleanup, nil
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

func testRedisAddr() string {
	if v := os.Getenv("SWITCHYARD_TEST_REDIS_ADDR"); v != "" {
		return v
	}
	return "localhost:6379"
}

func requireRedis(t *testing.T) {
	t.Helper()
	addr := testRedisAddr()
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		t.Skipf("no Redis reachable at %s: %v", addr, err)
	}
	conn.Close()
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocating port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

var uniqueSeq atomic.Int64

// uniqueID returns a name that has never been used against Redis before, in
// this run or any previous one. Rate-limit buckets, budget counters, and
// breaker state are all keyed by team ID or by provider+model and persist in
// Redis with a TTL far longer than a test — a fixed name like "acme" would
// silently inherit another test's (or a previous run's) leftover state.
func uniqueID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), uniqueSeq.Add(1))
}

// mockUpstream stands in for a provider's HTTP API. Its behaviour is swapped
// at runtime via SetHandler, which is what lets one test move a provider from
// "succeeds" to "always 500" to "succeeds again" without restarting anything.
type mockUpstream struct {
	name    string
	srv     *http.Server
	url     string
	handler atomic.Value
	mu      sync.Mutex
	hits    int
}

func newMockUpstream(t *testing.T, name string) *mockUpstream {
	t.Helper()

	m := &mockUpstream{name: name}
	m.handler.Store(http.HandlerFunc(m.defaultHandler))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening for mock upstream %s: %v", name, err)
	}
	m.url = "http://" + ln.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.hits++
		m.mu.Unlock()
		m.handler.Load().(http.HandlerFunc)(w, r)
	})
	m.srv = &http.Server{Handler: mux}

	go m.srv.Serve(ln)
	t.Cleanup(func() { m.srv.Close() })

	return m
}

func (m *mockUpstream) URL() string { return m.url }

func (m *mockUpstream) SetHandler(h http.HandlerFunc) { m.handler.Store(h) }

func (m *mockUpstream) ResetHandler() { m.handler.Store(http.HandlerFunc(m.defaultHandler)) }

func (m *mockUpstream) ResetHits() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hits = 0
}

func (m *mockUpstream) Hits() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits
}

func (m *mockUpstream) defaultHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Stream bool `json:"stream"`
	}
	raw, _ := readAll(r)
	json.Unmarshal(raw, &body)

	if body.Stream {
		writeSSESuccess(w, r, m.name, nil)
		return
	}
	writeJSONSuccess(w, m.name)
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(r.Body)
	return buf.Bytes(), err
}

// writeJSONSuccess omits "model" from the body on purpose: the adapter falls
// back to the requested model whenever the upstream leaves it blank, and the
// requested model is the only one with pricing configured. A hardcoded model
// name here would fail every cost lookup downstream.
func writeJSONSuccess(w http.ResponseWriter, servedBy string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"hello from %s"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, servedBy)
}

func writeErrorJSON(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"message":%q,"type":"server_error"}}`, message)
}

// writeSSESuccess streams a few small chunks with a real delay between each
// flush, which is what makes a test able to tell a genuinely streamed
// response apart from one that was buffered and sent all at once. cancelled,
// if non-nil, is closed the moment r's context is done before the stream
// finishes — a test's way of observing that a client disconnect actually
// reached the upstream call.
func writeSSESuccess(w http.ResponseWriter, r *http.Request, servedBy string, cancelled chan<- struct{}) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	chunks := []string{"Hello", " from", " " + servedBy}
	for _, c := range chunks {
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", c)
		flusher.Flush()

		select {
		case <-time.After(60 * time.Millisecond):
		case <-r.Context().Done():
			if cancelled != nil {
				close(cancelled)
			}
			return
		}
	}
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	flusher.Flush()
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

type providerSpec struct {
	name      string
	url       string
	models    []string
	pingModel string
}

type tierEntry struct {
	provider string
	model    string
}

type teamSpec struct {
	id               string
	key              string
	allowedProviders []string
	allowedModels    []string
	rpm              int
	tpm              int
	monthlyBudgetUSD float64
	priority         string
}

func defaultTeam(id, key string, allowedProviders, allowedModels []string) teamSpec {
	return teamSpec{
		id:               id,
		key:              key,
		allowedProviders: allowedProviders,
		allowedModels:    allowedModels,
		rpm:              1000,
		tpm:              1_000_000,
		monthlyBudgetUSD: 1000,
		priority:         "realtime",
	}
}

type harnessConfig struct {
	providers []providerSpec
	tiers     map[string][]tierEntry
	teams     []teamSpec
	env       map[string]string
}

func buildProvidersYAML(cfg harnessConfig) string {
	var b strings.Builder
	b.WriteString("providers:\n")
	for _, p := range cfg.providers {
		fmt.Fprintf(&b, "  - name: %s\n", p.name)
		b.WriteString("    type: openai-compatible\n")
		b.WriteString("    enabled: true\n")
		fmt.Fprintf(&b, "    base_url: %s\n", p.url)
		fmt.Fprintf(&b, "    api_key_env: %s\n", apiKeyEnvVar(p.name))
		b.WriteString("    timeout: 5s\n")
		b.WriteString("    default_max_tokens: 16\n")
		if p.pingModel != "" {
			fmt.Fprintf(&b, "    ping_model: %s\n", p.pingModel)
		}
		b.WriteString("    models:\n")
		for _, m := range p.models {
			fmt.Fprintf(&b, "      - name: %s\n        input_per_1m_usd: 100000\n        output_per_1m_usd: 100000\n", m)
		}
	}

	if len(cfg.tiers) > 0 {
		b.WriteString("tiers:\n")
		for name, entries := range cfg.tiers {
			fmt.Fprintf(&b, "  %s:\n", name)
			for _, e := range entries {
				fmt.Fprintf(&b, "    - { provider: %s, model: %s }\n", e.provider, e.model)
			}
		}
	}

	return b.String()
}

func buildTeamsYAML(teams []teamSpec) string {
	var b strings.Builder
	b.WriteString("teams:\n")
	for _, t := range teams {
		fmt.Fprintf(&b, "  - id: %s\n", t.id)
		fmt.Fprintf(&b, "    name: %s\n", t.id)
		fmt.Fprintf(&b, "    api_key_hash: %s\n", sha256Hex(t.key))
		b.WriteString("    allowed_providers:\n")
		for _, p := range t.allowedProviders {
			fmt.Fprintf(&b, "      - %s\n", p)
		}
		b.WriteString("    allowed_models:\n")
		for _, m := range t.allowedModels {
			fmt.Fprintf(&b, "      - %s\n", m)
		}
		fmt.Fprintf(&b, "    rate_limits:\n      rpm: %d\n      tpm: %d\n", t.rpm, t.tpm)
		fmt.Fprintf(&b, "    monthly_budget_usd: %.6f\n", t.monthlyBudgetUSD)
		fmt.Fprintf(&b, "    priority: %s\n", t.priority)
	}
	return b.String()
}

func apiKeyEnvVar(providerName string) string {
	safe := strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(providerName))
	return "SWITCHYARD_TEST_KEY_" + safe
}

type gatewayInstance struct {
	BaseURL  string
	AdminURL string
	Client   *http.Client

	providersPath string
	teamsPath     string
}

// startGateway writes cfg to a temp providers.yaml/teams.yaml, launches the
// real gateway binary against them and a real Redis, and waits for it to
// report healthy. The process is killed on test cleanup.
//
// upstreams lists every mock upstream cfg's providers point at. The health
// checker fires one ping per provider immediately at boot, before any test
// traffic — startGateway waits for that ping to land and then zeroes each
// upstream's hit counter, so a test asserting an exact call count never has
// to account for it.
func startGateway(t *testing.T, cfg harnessConfig, upstreams ...*mockUpstream) *gatewayInstance {
	t.Helper()
	requireRedis(t)

	dir := t.TempDir()
	providersPath := filepath.Join(dir, "providers.yaml")
	teamsPath := filepath.Join(dir, "teams.yaml")

	if err := os.WriteFile(providersPath, []byte(buildProvidersYAML(cfg)), 0o644); err != nil {
		t.Fatalf("writing providers.yaml: %v", err)
	}
	if err := os.WriteFile(teamsPath, []byte(buildTeamsYAML(cfg.teams)), 0o644); err != nil {
		t.Fatalf("writing teams.yaml: %v", err)
	}

	publicPort := freePort(t)
	adminPort := freePort(t)

	env := map[string]string{
		"SWITCHYARD_PROVIDERS_CONFIG": providersPath,
		"SWITCHYARD_TEAMS_CONFIG":     teamsPath,
		"SWITCHYARD_ADDR":             fmt.Sprintf(":%d", publicPort),
		"SWITCHYARD_ADMIN_ADDR":       fmt.Sprintf(":%d", adminPort),
		"SWITCHYARD_REDIS_ADDR":       testRedisAddr(),
		"SWITCHYARD_ENV":              "dev",
		"SWITCHYARD_CHAOS_ENABLED":    "false",
		"SWITCHYARD_LOG_LEVEL":        "error",
		"SWITCHYARD_DRAIN_TIMEOUT":    "2s",
		// Long enough that no test's short lifetime sees a second tick; the
		// checker still fires once immediately at boot, which startGateway
		// accounts for below by resetting every mock's hit counter after
		// the gateway reports healthy.
		"SWITCHYARD_HEALTH_CHECK_INTERVAL":    "1h",
		"SWITCHYARD_RETRY_MAX_ATTEMPTS":       "1",
		"SWITCHYARD_RETRY_BASE_DELAY":         "10ms",
		"SWITCHYARD_RETRY_MAX_TOTAL_ATTEMPTS": "5",
	}
	for _, p := range cfg.providers {
		env[apiKeyEnvVar(p.name)] = "test-key"
	}
	for k, v := range cfg.env {
		env[k] = v
	}

	cmd := exec.Command(gatewayBin)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting gateway: %v", err)
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", publicPort)
	adminURL := fmt.Sprintf("http://127.0.0.1:%d", adminPort)

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("gateway output:\n%s", out.String())
		}
	})

	waitForHealthy(t, baseURL)
	for _, u := range upstreams {
		waitForQuietHits(u)
		u.ResetHits()
	}

	return &gatewayInstance{
		BaseURL:       baseURL,
		AdminURL:      adminURL,
		Client:        &http.Client{},
		providersPath: providersPath,
		teamsPath:     teamsPath,
	}
}

// waitForQuietHits blocks until u's hit count has stopped changing for a
// short stretch, up to a hard cap. It exists only to outlast the boot-time
// health ping's arrival, whenever the scheduler gets around to it — a fixed
// sleep would either race a slow one or waste time waiting past a fast one.
func waitForQuietHits(u *mockUpstream) {
	deadline := time.Now().Add(2 * time.Second)
	last := u.Hits()
	stableSince := time.Now()

	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		if got := u.Hits(); got != last {
			last = got
			stableSince = time.Now()
			continue
		}
		if time.Since(stableSince) >= 150*time.Millisecond {
			return
		}
	}
}

func waitForHealthy(t *testing.T, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("gateway never became healthy: %v", lastErr)
}

type chatRequestBody struct {
	Model     string   `json:"model"`
	Messages  []chatMs `json:"messages"`
	MaxTokens int      `json:"max_tokens,omitempty"`
	Stream    bool     `json:"stream,omitempty"`
}

type chatMs struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func chatBody(model string) chatRequestBody {
	return chatRequestBody{Model: model, Messages: []chatMs{{Role: "user", Content: "hello"}}}
}

func postChat(t *testing.T, gw *gatewayInstance, apiKey string, body chatRequestBody) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshalling chat request: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, gw.BaseURL+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("building chat request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := gw.Client.Do(req)
	if err != nil {
		t.Fatalf("chat request: %v", err)
	}
	return resp
}
