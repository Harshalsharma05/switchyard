package admin

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/logstore"
)

type costsResp struct {
	Range  string `json:"range"`
	Bucket string `json:"bucket"`
	By     string `json:"by"`
	Keys   []string
	Series []struct {
		T           string           `json:"t"`
		TotalMicros int64            `json:"total_micros"`
		Breakdown   map[string]int64 `json:"breakdown"`
	} `json:"series"`
}

func TestCostsAssemblesBucketsAndKeys(t *testing.T) {
	h0 := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	h1 := h0.Add(time.Hour)
	reader := &fakeRequestLogReader{costCells: []logstore.CostCell{
		{Bucket: h0, Key: "groq", Micros: 1000},
		{Bucket: h0, Key: "gemini", Micros: 500},
		{Bucket: h1, Key: "groq", Micros: 2000},
		{Bucket: h1, Key: "", Micros: 0}, // a 402 that never reached a provider
	}}
	srv := newRequestLogServer(t, reader)

	resp := getWithKey(t, srv, "/admin/costs?range=24h&by=provider", "acme-key")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got costsResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.Bucket != "hour" || got.By != "provider" {
		t.Errorf("bucket/by = %q/%q", got.Bucket, got.By)
	}
	if len(got.Series) != 2 {
		t.Fatalf("series has %d points, want 2", len(got.Series))
	}
	if got.Series[0].TotalMicros != 1500 || got.Series[1].TotalMicros != 2000 {
		t.Errorf("totals = %d, %d; want 1500, 2000", got.Series[0].TotalMicros, got.Series[1].TotalMicros)
	}
	if got.Series[1].Breakdown["groq"] != 2000 {
		t.Errorf("h1 groq = %d, want 2000", got.Series[1].Breakdown["groq"])
	}
	// The zero-cost empty-provider cell must not create a key or a series entry.
	if want := []string{"gemini", "groq"}; len(got.Keys) != 2 || got.Keys[0] != want[0] || got.Keys[1] != want[1] {
		t.Errorf("keys = %v, want %v", got.Keys, want)
	}
}

func TestCostsScoping(t *testing.T) {
	t.Run("non-admin is pinned to its own team", func(t *testing.T) {
		reader := &fakeRequestLogReader{}
		srv := newRequestLogServer(t, reader)

		if resp := getWithKey(t, srv, "/admin/costs?by=team", "globex-key"); resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if reader.gotCostQuery.TeamID != "globex" {
			t.Errorf("scope = %q, want globex", reader.gotCostQuery.TeamID)
		}
	})

	t.Run("non-admin naming another team is refused", func(t *testing.T) {
		reader := &fakeRequestLogReader{}
		srv := newRequestLogServer(t, reader)

		if resp := getWithKey(t, srv, "/admin/costs?team=acme", "globex-key"); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		if reader.gotCostQuery.Dimension != "" {
			t.Error("the query ran despite being refused")
		}
	})

	t.Run("admin may span all teams and narrow to one", func(t *testing.T) {
		reader := &fakeRequestLogReader{}
		srv := newRequestLogServer(t, reader)

		getWithKey(t, srv, "/admin/costs", "acme-key")
		if reader.gotCostQuery.TeamID != "" {
			t.Errorf("admin unscoped = %q, want empty", reader.gotCostQuery.TeamID)
		}
		getWithKey(t, srv, "/admin/costs?team=globex", "acme-key")
		if reader.gotCostQuery.TeamID != "globex" {
			t.Errorf("admin narrowed = %q, want globex", reader.gotCostQuery.TeamID)
		}
	})
}

func TestCostsRejectsBadParams(t *testing.T) {
	srv := newRequestLogServer(t, &fakeRequestLogReader{})
	for _, path := range []string{"/admin/costs?range=1y", "/admin/costs?by=day"} {
		if resp := getWithKey(t, srv, path, "acme-key"); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", path, resp.StatusCode)
		}
	}
}

func TestCostsDisabledWithoutRequestLog(t *testing.T) {
	srv := newRequestLogServer(t, nil)
	if resp := getWithKey(t, srv, "/admin/costs", "acme-key"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}
