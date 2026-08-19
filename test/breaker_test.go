// Black-box proof of the circuit breaker's full cycle against the real
// subprocess: enough failures open it, an open breaker rejects without
// calling the provider, and after cooldown a single successful probe closes
// it again.
//
//go:build integration

package integration

import (
	"net/http"
	"testing"
	"time"
)

func TestBreakerOpensRejectsThenRecovers(t *testing.T) {
	providerName := uniqueID("primary")
	model := uniqueID("model")
	upstream := newMockUpstream(t, providerName)
	upstream.SetHandler(func(w http.ResponseWriter, r *http.Request) {
		writeErrorJSON(w, http.StatusInternalServerError, "primary is down")
	})

	team := defaultTeam(uniqueID("acme"), "breaker-key", []string{providerName}, []string{model})

	gw := startGateway(t, harnessConfig{
		providers: []providerSpec{{name: providerName, url: upstream.URL(), models: []string{model}}},
		teams:     []teamSpec{team},
		env: map[string]string{
			"SWITCHYARD_BREAKER_FAILURE_THRESHOLD": "2",
			"SWITCHYARD_BREAKER_WINDOW":            "1m",
			"SWITCHYARD_BREAKER_COOLDOWN_BASE":     "300ms",
			"SWITCHYARD_BREAKER_COOLDOWN_MAX":      "300ms",
			"SWITCHYARD_BREAKER_SUCCESS_THRESHOLD": "1",
			"SWITCHYARD_BREAKER_STATE_CACHE_TTL":   "10ms",
		},
	}, upstream)

	for i := 0; i < 2; i++ {
		resp := postChat(t, gw, team.key, chatBody(model))
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("failure %d: status = %d, want 502 while the breaker is still closed", i, resp.StatusCode)
		}
	}
	if got := upstream.Hits(); got != 2 {
		t.Fatalf("upstream saw %d calls priming the breaker, want exactly 2", got)
	}

	resp := postChat(t, gw, team.key, chatBody(model))
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 once the breaker is open", resp.StatusCode)
	}
	if got := upstream.Hits(); got != 2 {
		t.Errorf("upstream saw %d calls after the breaker opened, want still 2 — an open breaker must skip the call entirely", got)
	}

	time.Sleep(400 * time.Millisecond) // past the 300ms cooldown

	upstream.SetHandler(func(w http.ResponseWriter, r *http.Request) {
		writeJSONSuccess(w, providerName)
	})

	probe := postChat(t, gw, team.key, chatBody(model))
	probe.Body.Close()
	if probe.StatusCode != http.StatusOK {
		t.Fatalf("probe status = %d, want 200 — the half-open probe should reach the now-healthy provider", probe.StatusCode)
	}
	if got := upstream.Hits(); got != 3 {
		t.Fatalf("upstream saw %d calls after the probe, want exactly 3", got)
	}

	after := postChat(t, gw, team.key, chatBody(model))
	after.Body.Close()
	if after.StatusCode != http.StatusOK {
		t.Fatalf("post-recovery status = %d, want 200 — one successful probe should close the breaker at this threshold", after.StatusCode)
	}
	if got := upstream.Hits(); got != 4 {
		t.Errorf("upstream saw %d calls after recovery, want exactly 4", got)
	}
}
