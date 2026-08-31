package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Harshalsharma05/switchyard/internal/logstore"
)

type attrResp struct {
	Range    string `json:"range"`
	Fallback struct {
		ExtraMicros int64   `json:"extra_micros"`
		SavedMicros int64   `json:"saved_micros"`
		NetMicros   int64   `json:"net_micros"`
		NetUSD      float64 `json:"net_usd"`
	} `json:"fallback"`
	Cache json.RawMessage `json:"cache"`
}

func TestAttributionReportsFallbackDelta(t *testing.T) {
	reader := &fakeRequestLogReader{
		fallbackAttr: logstore.FallbackAttribution{ExtraMicros: 2_500_000, SavedMicros: 500_000},
	}
	srv := newRequestLogServer(t, reader)

	resp := getWithKey(t, srv, "/admin/attribution?range=7d", "acme-key")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got attrResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.Fallback.NetMicros != 2_000_000 || got.Fallback.NetUSD != 2 {
		t.Errorf("net = %d micros / $%v, want 2000000 / $2", got.Fallback.NetMicros, got.Fallback.NetUSD)
	}
	if string(got.Cache) != "null" {
		t.Errorf("cache = %s, want null (Phase 7)", got.Cache)
	}
}

func TestAttributionScopingAndParams(t *testing.T) {
	t.Run("non-admin is scoped to its own team", func(t *testing.T) {
		reader := &fakeRequestLogReader{}
		srv := newRequestLogServer(t, reader)
		if resp := getWithKey(t, srv, "/admin/attribution", "globex-key"); resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if reader.gotTeamID != "globex" {
			t.Errorf("scope = %q, want globex", reader.gotTeamID)
		}
	})

	t.Run("non-admin naming another team is refused", func(t *testing.T) {
		srv := newRequestLogServer(t, &fakeRequestLogReader{})
		if resp := getWithKey(t, srv, "/admin/attribution?team=acme", "globex-key"); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("bad range is 400", func(t *testing.T) {
		srv := newRequestLogServer(t, &fakeRequestLogReader{})
		if resp := getWithKey(t, srv, "/admin/attribution?range=1y", "acme-key"); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})
}

func TestAttributionDisabledWithoutRequestLog(t *testing.T) {
	srv := newRequestLogServer(t, nil)
	if resp := getWithKey(t, srv, "/admin/attribution", "acme-key"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

// fakeCostCalculator prices at exactly 1 micro-dollar per input token and 2 per
// output token, so the expected savings are arithmetic rather than a fixture.
type fakeCostCalculator struct{ unpriced string }

func (c fakeCostCalculator) Cost(model string, in, out int) (int64, error) {
	if model == c.unpriced {
		return 0, errors.New("no pricing for model")
	}
	return int64(in) + 2*int64(out), nil
}

// Savings must come from the real token counts on cache-hit rows, priced at the
// served model's own rate — an estimate would make the headline number fiction.
func TestCacheAttributionPricesRealTokens(t *testing.T) {
	reader := &fakeRequestLogReader{
		cacheSavings: logstore.CacheSavings{
			Hits:   3,
			Misses: 1,
			Groups: []logstore.CacheSavingsGroup{
				{Model: "gpt-oss-20b", Hits: 2, InputTokens: 100, OutputTokens: 50},
				{Model: "gemini-flash", Hits: 1, InputTokens: 10, OutputTokens: 5},
			},
		},
	}

	got := getCacheAttribution(t, reader, fakeCostCalculator{})

	// (100 + 2*50) + (10 + 2*5) = 200 + 20
	if got.SavedMicros != 220 {
		t.Fatalf("saved_micros = %d, want 220", got.SavedMicros)
	}
	if got.Hits != 3 || got.Misses != 1 {
		t.Fatalf("hits/misses = %d/%d, want 3/1", got.Hits, got.Misses)
	}
	if got.HitRate != 0.75 {
		t.Fatalf("hit_rate = %v, want 0.75", got.HitRate)
	}
}

// A model that has left configs/providers.yaml has no price. Understating the
// savings is the safe direction; failing the whole report is not.
func TestCacheAttributionSkipsUnpricedModel(t *testing.T) {
	reader := &fakeRequestLogReader{
		cacheSavings: logstore.CacheSavings{
			Hits: 2,
			Groups: []logstore.CacheSavingsGroup{
				{Model: "gpt-oss-20b", Hits: 1, InputTokens: 100, OutputTokens: 0},
				{Model: "retired-model", Hits: 1, InputTokens: 999, OutputTokens: 999},
			},
		},
	}

	got := getCacheAttribution(t, reader, fakeCostCalculator{unpriced: "retired-model"})

	if got.SavedMicros != 100 {
		t.Fatalf("saved_micros = %d, want 100 (retired model skipped)", got.SavedMicros)
	}
}

// With no pricing table the panel stays null, so Usage & Cost renders its empty
// state rather than a confident zero.
func TestCacheAttributionNullWithoutCalculator(t *testing.T) {
	body := attributionBody(t, &fakeRequestLogReader{}, nil)
	if body.Cache != nil {
		t.Fatalf("cache = %+v, want null without a cost calculator", body.Cache)
	}
}

func getCacheAttribution(t *testing.T, reader RequestLogReader, calc CostCalculator) cacheAttrView {
	t.Helper()
	body := attributionBody(t, reader, calc)
	if body.Cache == nil {
		t.Fatal("cache attribution is null")
	}
	return *body.Cache
}

func attributionBody(t *testing.T, reader RequestLogReader, calc CostCalculator) attributionView {
	t.Helper()
	srv := httptest.NewServer(NewRouter(func() bool { return true },
		testTeamStore(t), &fakeSpendReader{}, fakeProviderLister{}, fakeHealthReader{},
		&fakeBreakerController{}, nil, fakeReloader, reader, requestLogRegistry(t),
		nil, nil, calc, testMetrics(t), discardLogger()))
	t.Cleanup(srv.Close)

	resp := getWithKey(t, srv, "/admin/attribution?range=24h", "acme-key")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var body attributionView
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding attribution: %v", err)
	}
	return body
}
