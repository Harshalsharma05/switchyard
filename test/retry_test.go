// Black-box proof that a retryable upstream failure is retried against the
// real subprocess, and that an unretryable one is not.
//
//go:build integration

package integration

import (
	"net/http"
	"sync/atomic"
	"testing"
)

func TestRetrySucceedsAfterTransientFailures(t *testing.T) {
	providerName := uniqueID("primary")
	model := uniqueID("model")
	upstream := newMockUpstream(t, providerName)

	team := defaultTeam(uniqueID("acme"), "retry-key", []string{providerName}, []string{model})

	gw := startGateway(t, harnessConfig{
		providers: []providerSpec{{name: providerName, url: upstream.URL(), models: []string{model}}},
		teams:     []teamSpec{team},
		env: map[string]string{
			"SWITCHYARD_RETRY_MAX_ATTEMPTS":       "3",
			"SWITCHYARD_RETRY_MAX_TOTAL_ATTEMPTS": "3",
			"SWITCHYARD_RETRY_BASE_DELAY":         "5ms",
		},
	}, upstream)

	var calls int32
	upstream.SetHandler(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			writeErrorJSON(w, http.StatusInternalServerError, "transient upstream failure")
			return
		}
		writeJSONSuccess(w, providerName)
	})

	resp := postChat(t, gw, team.key, chatBody(model))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after retrying past two transient failures", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("upstream received %d calls, want exactly 3 (two failures then a success)", got)
	}
}

func TestUnretryableFailureIsCalledExactlyOnce(t *testing.T) {
	providerName := uniqueID("primary")
	model := uniqueID("model")
	upstream := newMockUpstream(t, providerName)

	team := defaultTeam(uniqueID("acme"), "no-retry-key", []string{providerName}, []string{model})

	gw := startGateway(t, harnessConfig{
		providers: []providerSpec{{name: providerName, url: upstream.URL(), models: []string{model}}},
		teams:     []teamSpec{team},
		env: map[string]string{
			"SWITCHYARD_RETRY_MAX_ATTEMPTS":       "3",
			"SWITCHYARD_RETRY_MAX_TOTAL_ATTEMPTS": "3",
		},
	}, upstream)

	upstream.SetHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"model does not exist","type":"invalid_request_error"}}`))
	})

	resp := postChat(t, gw, team.key, chatBody(model))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 passed through unchanged", resp.StatusCode)
	}
	if got := upstream.Hits(); got != 1 {
		t.Errorf("upstream received %d calls, want exactly 1 — a 400 must never be retried", got)
	}
}
