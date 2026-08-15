package proxy

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

func TestOverheadExcludesProviderTime(t *testing.T) {
	const providerDelay = 120 * time.Millisecond

	mock := &provider.Mock{
		ProviderName: "groq",
		Delay:        providerDelay,
		Response: &provider.Response{
			Content:      "hi",
			FinishReason: provider.FinishStop,
			Model:        "openai/gpt-oss-120b",
			Provider:     "groq",
			// The adapter reports its own round trip; the handler subtracts it.
			Latency: providerDelay,
		},
	}

	srv := newTestServer(t, stubResolver{prov: mock})

	start := time.Now()
	resp := post(t, srv, `{"model":"openai/gpt-oss-120b","messages":[{"role":"user","content":"hi"}]}`)
	wall := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// The request genuinely took the provider delay end to end...
	if wall < providerDelay {
		t.Fatalf("request finished in %v, faster than the %v provider delay", wall, providerDelay)
	}

	// ...but the reported overhead must not include it.
	overhead := parseOverhead(t, resp)
	if overhead >= providerDelay {
		t.Errorf("overhead %v includes the provider's %v; it must be excluded", overhead, providerDelay)
	}
	if overhead > 10*time.Millisecond {
		t.Errorf("overhead %v exceeds the project's 10ms budget", overhead)
	}
}

func TestSwitchyardHeadersOnSuccess(t *testing.T) {
	mock := &provider.Mock{
		ProviderName: "groq",
		Response: &provider.Response{
			Content:      "hi",
			FinishReason: provider.FinishStop,
			// The served model differs from the requested one, which is exactly
			// what these headers exist to make visible.
			Model:    "openai/gpt-oss-120b-0125",
			Provider: "groq",
			Latency:  5 * time.Millisecond,
		},
	}

	srv := newTestServer(t, stubResolver{prov: mock})
	resp := post(t, srv, `{"model":"openai/gpt-oss-120b","messages":[{"role":"user","content":"hi"}]}`)

	tests := map[string]string{
		HeaderProvider: "groq",
		HeaderModel:    "openai/gpt-oss-120b-0125",
	}
	for header, want := range tests {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	if resp.Header.Get(HeaderRequestID) == "" {
		t.Error("request ID header missing")
	}
	if resp.Header.Get(HeaderOverhead) == "" {
		t.Fatal("overhead header missing")
	}

	// The provider's 5ms must have been subtracted out.
	if overhead := parseOverhead(t, resp); overhead >= 5*time.Millisecond {
		t.Errorf("overhead = %v, want the provider's 5ms excluded", overhead)
	}
}

// Every response carries an overhead figure, including the paths that never
// reach a provider — otherwise the number only describes the happy path.
func TestOverheadHeaderOnEveryPath(t *testing.T) {
	tests := map[string]struct {
		body         string
		resolver     Resolver
		wantStatus   int
		wantProvider bool
	}{
		"validation rejection": {
			body:       `{"messages":[]}`,
			resolver:   stubResolver{prov: &provider.Mock{}},
			wantStatus: http.StatusBadRequest,
		},
		"unknown model": {
			body:       `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`,
			resolver:   stubResolver{err: provider.ErrModelNotSupported},
			wantStatus: http.StatusNotFound,
		},
		"provider failure": {
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			resolver: stubResolver{prov: &provider.Mock{
				ProviderName: "groq",
				Err:          &provider.Error{Kind: provider.KindServerError, Provider: "groq", Message: "boom"},
			}},
			wantStatus:   http.StatusBadGateway,
			wantProvider: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			srv := newTestServer(t, tc.resolver)
			resp := post(t, srv, tc.body)

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if resp.Header.Get(HeaderOverhead) == "" {
				t.Error("no overhead header on this path")
			}

			// A provider is only named when one was actually reached.
			gotProvider := resp.Header.Get(HeaderProvider) != ""
			if gotProvider != tc.wantProvider {
				t.Errorf("provider header present = %v, want %v", gotProvider, tc.wantProvider)
			}
		})
	}
}

// The header format has to resolve sub-millisecond values, or the headline
// number reads "0" for every Phase 1 request.
func TestFormatMillisResolvesMicroseconds(t *testing.T) {
	tests := map[time.Duration]string{
		0:                       "0.000",
		1 * time.Microsecond:    "0.001",
		250 * time.Microsecond:  "0.250",
		1 * time.Millisecond:    "1.000",
		1500 * time.Microsecond: "1.500",
		12 * time.Millisecond:   "12.000",
	}

	for in, want := range tests {
		t.Run(in.String(), func(t *testing.T) {
			if got := formatMillis(in); got != want {
				t.Errorf("formatMillis(%v) = %q, want %q", in, got, want)
			}
		})
	}
}

// A provider reporting a longer latency than the whole request took would
// produce a negative headline number. Report zero instead.
func TestOverheadNeverNegative(t *testing.T) {
	m := &requestMetrics{start: time.Now(), providerTime: time.Hour}

	if got := m.overhead(); got != 0 {
		t.Errorf("overhead = %v, want 0 rather than a negative figure", got)
	}
}

// Timing wraps the ResponseWriter, so it faces the same trap the logger does.
func TestTimingPreservesFlusher(t *testing.T) {
	flushable := make(chan bool, 1)

	h := Timing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok := w.(http.Flusher)
		flushable <- ok
		w.Write([]byte("chunk"))
	}))

	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if !<-flushable {
		t.Fatal("http.Flusher was not reachable through the timing wrapper")
	}
	// Headers must have been injected before the first byte went out.
	if resp.Header.Get(HeaderOverhead) == "" {
		t.Error("overhead header was not applied before the first write")
	}
}

// A handler that returns without writing still gets headers, because net/http
// only emits the status line after ServeHTTP returns.
func TestTimingAppliesHeadersWhenHandlerWritesNothing(t *testing.T) {
	srv := httptest.NewServer(Timing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.Header.Get(HeaderOverhead) == "" {
		t.Error("no overhead header on a handler that wrote nothing")
	}
}

func parseOverhead(t *testing.T, resp *http.Response) time.Duration {
	t.Helper()

	raw := resp.Header.Get(HeaderOverhead)
	ms, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("overhead header %q is not a number: %v", raw, err)
	}
	return time.Duration(ms * float64(time.Millisecond))
}
