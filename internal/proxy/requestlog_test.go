package proxy

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/auth"
	"github.com/Harshalsharma05/switchyard/internal/logstore"
	"github.com/Harshalsharma05/switchyard/internal/ratelimit"
)

// fakeRequestLogger captures what the middleware would have persisted.
type fakeRequestLogger struct {
	mu      sync.Mutex
	records []logstore.Record
}

func (f *fakeRequestLogger) Write(rec logstore.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, rec)
}

func (f *fakeRequestLogger) all() []logstore.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]logstore.Record(nil), f.records...)
}

func newLoggedServer(t *testing.T, resolver Resolver, authr Authenticator, limiter RateLimiter, calc CostCalculator) (*httptest.Server, *fakeRequestLogger) {
	t.Helper()
	fake := &fakeRequestLogger{}
	srv := httptest.NewServer(NewRouter(resolver, authr, limiter, stubBudgetTracker{}, calc,
		stubHealthRecorder{}, nil, nil, nil, noRetryConfig(t), nil, fake,
		discardLogger(), func() bool { return true }))
	t.Cleanup(srv.Close)
	return srv, fake
}

// The stored cost must equal what the caller was told, or Usage & Cost in
// Phase 6 reconciles against a number nobody was ever charged.
func TestRequestLogMatchesResponseHeaders(t *testing.T) {
	resolver := stubResolver{prov: okMock("groq", "openai/gpt-oss-120b")}
	srv, fake := newLoggedServer(t, resolver, stubAuthenticator{team: defaultTestTeam()},
		stubRateLimiter{}, stubCostCalculator{costMicros: 4321})

	resp := post(t, srv, `{"model":"openai/gpt-oss-120b","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()

	records := fake.all()
	if len(records) != 1 {
		t.Fatalf("wrote %d rows, want exactly 1", len(records))
	}
	got := records[0]

	if got.ID != resp.Header.Get(HeaderRequestID) {
		t.Errorf("row id = %q, want %q", got.ID, resp.Header.Get(HeaderRequestID))
	}
	if got.StatusCode != http.StatusOK {
		t.Errorf("status_code = %d, want 200", got.StatusCode)
	}
	if got.CostMicros != 4321 {
		t.Errorf("cost_micros = %d, want 4321", got.CostMicros)
	}
	if got.Provider != resp.Header.Get(HeaderProvider) {
		t.Errorf("provider = %q, want %q", got.Provider, resp.Header.Get(HeaderProvider))
	}
	if got.ServedModel != resp.Header.Get(HeaderServedModel) {
		t.Errorf("served_model = %q, want %q", got.ServedModel, resp.Header.Get(HeaderServedModel))
	}
	wantOverhead, err := strconv.ParseFloat(resp.Header.Get(HeaderOverhead), 64)
	if err != nil {
		t.Fatalf("parsing overhead header: %v", err)
	}
	// The header is applied at first write and the row is built after the
	// handler returns, so the row's overhead can only be the larger of the two
	// — give or take the half-digit the header's 3-decimal rounding can add.
	if got.OverheadMS < wantOverhead-0.0005 {
		t.Errorf("overhead_ms = %v, want at least the header's %v", got.OverheadMS, wantOverhead)
	}
	if got.CacheHit != nil || got.QualityScore != nil {
		t.Errorf("cache_hit/quality_score = %v/%v, want NULL until Phases 7 and 9", got.CacheHit, got.QualityScore)
	}
}

// Rejections are logged too — a request log that only holds successes is
// useless for the failure investigation it exists to support.
func TestRequestLogRecordsRejections(t *testing.T) {
	denied := ratelimit.Result{Allowed: false, Remaining: 0, RetryAfter: time.Second}
	srv, fake := newLoggedServer(t, stubResolver{prov: okMock("groq", "openai/gpt-oss-120b")},
		stubAuthenticator{team: defaultTestTeam()},
		stubRateLimiter{consumeResult: &denied}, stubCostCalculator{})

	resp := post(t, srv, `{"model":"openai/gpt-oss-120b","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}

	records := fake.all()
	if len(records) != 1 {
		t.Fatalf("wrote %d rows for a 429, want exactly 1", len(records))
	}
	if records[0].StatusCode != http.StatusTooManyRequests {
		t.Errorf("status_code = %d, want 429", records[0].StatusCode)
	}
	if records[0].TeamID == "" {
		t.Error("team_id is empty; a rejected request is still attributable to a team")
	}
	if records[0].Provider != "" {
		t.Errorf("provider = %q, want empty: no provider was reached", records[0].Provider)
	}
}

// An unauthenticated request has no team to attribute a row to, and logging
// every probe from a scanner would fill the table with unattributable noise.
func TestRequestLogSkipsUnauthenticated(t *testing.T) {
	srv, fake := newLoggedServer(t, stubResolver{}, stubAuthenticator{err: auth.ErrUnknownKey},
		stubRateLimiter{}, stubCostCalculator{})

	resp := postWithAuth(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if got := fake.all(); len(got) != 0 {
		t.Errorf("wrote %d rows for a 401, want none", len(got))
	}
}
