package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newReconServer(t *testing.T, spend SpendReader, reqLog RequestLogReader) *httptest.Server {
	t.Helper()
	reg := requestLogRegistry(t) // acme = admin, globex = not
	srv := httptest.NewServer(NewRouter(func() bool { return true },
		reg, spend, fakeProviderLister{}, fakeHealthReader{}, &fakeBreakerController{},
		nil, fakeReloader, reqLog, reg, nil, nil, nil, nil, testMetrics(t), discardLogger()))
	t.Cleanup(srv.Close)
	return srv
}

type reconResp struct {
	Reconciled bool `json:"reconciled"`
	Degraded   bool `json:"degraded"`
	Teams      []struct {
		TeamID          string `json:"team_id"`
		RedisMicros     *int64 `json:"redis_micros"`
		LogMicros       int64  `json:"log_micros"`
		DeltaMicros     *int64 `json:"delta_micros"`
		WithinTolerance *bool  `json:"within_tolerance"`
	} `json:"teams"`
}

func getRecon(t *testing.T, srv *httptest.Server, key string) reconResp {
	t.Helper()
	resp := getWithKey(t, srv, "/admin/reconciliation", key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out reconResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return out
}

// The core of the feature: Redis and the request log agree within tolerance, so
// the report reconciles; push one team past tolerance and it does not.
func TestReconciliationFlagsDrift(t *testing.T) {
	t.Run("within tolerance reconciles", func(t *testing.T) {
		spend := &fakeSpendReader{spent: map[string]int64{"acme": 1_000_000, "globex": 500_000}}
		reqLog := &fakeRequestLogReader{spendByTeam: map[string]int64{"acme": 1_000_000, "globex": 495_000}}
		got := getRecon(t, newReconServer(t, spend, reqLog), "acme-key")

		if !got.Reconciled || got.Degraded {
			t.Fatalf("reconciled=%v degraded=%v, want true/false", got.Reconciled, got.Degraded)
		}
		for _, tm := range got.Teams {
			if tm.WithinTolerance == nil || !*tm.WithinTolerance {
				t.Errorf("team %s within_tolerance=%v, want true", tm.TeamID, tm.WithinTolerance)
			}
			if tm.TeamID == "globex" && (tm.DeltaMicros == nil || *tm.DeltaMicros != 5_000) {
				t.Errorf("globex delta = %v, want 5000", tm.DeltaMicros)
			}
		}
	})

	t.Run("drift past tolerance does not reconcile", func(t *testing.T) {
		spend := &fakeSpendReader{spent: map[string]int64{"acme": 1_000_000}}
		reqLog := &fakeRequestLogReader{spendByTeam: map[string]int64{"acme": 900_000}}
		got := getRecon(t, newReconServer(t, spend, reqLog), "acme-key")

		if got.Reconciled {
			t.Fatal("reconciled = true, want false with a 0.10 drift")
		}
		for _, tm := range got.Teams {
			if tm.TeamID == "acme" && (tm.WithinTolerance == nil || *tm.WithinTolerance) {
				t.Errorf("acme within_tolerance = %v, want false", tm.WithinTolerance)
			}
		}
	})
}

// A Redis read failing for a team degrades the report; the log-side numbers are
// still returned rather than the whole endpoint 500-ing.
func TestReconciliationDegradesOnRedisError(t *testing.T) {
	spend := &fakeSpendReader{spentErr: errors.New("redis down")}
	reqLog := &fakeRequestLogReader{spendByTeam: map[string]int64{"acme": 42}}
	got := getRecon(t, newReconServer(t, spend, reqLog), "acme-key")

	if !got.Degraded || got.Reconciled {
		t.Fatalf("degraded=%v reconciled=%v, want true/false", got.Degraded, got.Reconciled)
	}
	for _, tm := range got.Teams {
		if tm.RedisMicros != nil {
			t.Errorf("team %s redis_micros = %d, want null", tm.TeamID, *tm.RedisMicros)
		}
		if tm.TeamID == "acme" && tm.LogMicros != 42 {
			t.Errorf("acme log_micros = %d, want 42", tm.LogMicros)
		}
	}
}

func TestReconciliationAccessControl(t *testing.T) {
	srv := newReconServer(t, &fakeSpendReader{}, &fakeRequestLogReader{})

	t.Run("non-admin key is forbidden", func(t *testing.T) {
		if resp := getWithKey(t, srv, "/admin/reconciliation", "globex-key"); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
	})
	t.Run("no key is unauthorized", func(t *testing.T) {
		if resp := getWithKey(t, srv, "/admin/reconciliation", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})
}

func TestReconciliationDisabledWithoutRequestLog(t *testing.T) {
	srv := newReconServer(t, &fakeSpendReader{}, nil)
	if resp := getWithKey(t, srv, "/admin/reconciliation", "acme-key"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}
