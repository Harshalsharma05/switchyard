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

	resp := get(t, srv, "/admin/costs?range=24h&by=provider")
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
	t.Run("a superadmin spans every team", func(t *testing.T) {
		reader := &fakeRequestLogReader{}
		srv := newRequestLogServer(t, reader)

		if resp := get(t, srv, "/admin/costs?by=team"); resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if reader.gotCostQuery.TeamIDs != nil {
			t.Errorf("scope = %v, want nil (all teams)", reader.gotCostQuery.TeamIDs)
		}
	})

	t.Run("a non-superadmin is scoped to their own org, not refused", func(t *testing.T) {
		reader := &fakeRequestLogReader{}
		srv := newRequestLogServerAs(t, reader, testAuth(false))

		if resp := get(t, srv, "/admin/costs?team=globex"); resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		// globex is a different org; the client-supplied ?team= is ignored and
		// the caller's own org (acme) is used instead.
		if want := []string{"acme"}; !slicesEqual(reader.gotCostQuery.TeamIDs, want) {
			t.Errorf("scope = %v, want %v", reader.gotCostQuery.TeamIDs, want)
		}
	})

	t.Run("admin may span all teams and narrow to one", func(t *testing.T) {
		reader := &fakeRequestLogReader{}
		srv := newRequestLogServer(t, reader)

		get(t, srv, "/admin/costs")
		if reader.gotCostQuery.TeamIDs != nil {
			t.Errorf("admin unscoped = %v, want nil", reader.gotCostQuery.TeamIDs)
		}
		get(t, srv, "/admin/costs?team=globex")
		if want := []string{"globex"}; !slicesEqual(reader.gotCostQuery.TeamIDs, want) {
			t.Errorf("admin narrowed = %v, want %v", reader.gotCostQuery.TeamIDs, want)
		}
	})
}

func TestCostsRejectsBadParams(t *testing.T) {
	srv := newRequestLogServer(t, &fakeRequestLogReader{})
	for _, path := range []string{"/admin/costs?range=1y", "/admin/costs?by=day"} {
		if resp := get(t, srv, path); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", path, resp.StatusCode)
		}
	}
}

func TestCostsDisabledWithoutRequestLog(t *testing.T) {
	srv := newRequestLogServer(t, nil)
	if resp := get(t, srv, "/admin/costs"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}
