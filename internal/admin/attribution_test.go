package admin

import (
	"encoding/json"
	"net/http"
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
