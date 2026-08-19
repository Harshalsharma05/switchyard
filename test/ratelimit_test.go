// Black-box proof that RPM rate limiting is exact under real concurrency
// against the real subprocess, not just the in-process handler.
//
//go:build integration

package integration

import (
	"net/http"
	"sync"
	"testing"
)

func TestRateLimitIsExactUnderConcurrency(t *testing.T) {
	providerName := uniqueID("primary")
	model := uniqueID("model")
	upstream := newMockUpstream(t, providerName)

	const rpm = 5
	const fired = 25

	team := defaultTeam(uniqueID("acme"), "concurrency-key", []string{providerName}, []string{model})
	team.rpm = rpm

	gw := startGateway(t, harnessConfig{
		providers: []providerSpec{{name: providerName, url: upstream.URL(), models: []string{model}}},
		teams:     []teamSpec{team},
	}, upstream)

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed, denied := 0, 0

	for i := 0; i < fired; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := postChat(t, gw, team.key, chatBody(model))
			defer resp.Body.Close()

			mu.Lock()
			defer mu.Unlock()
			switch resp.StatusCode {
			case http.StatusOK:
				allowed++
			case http.StatusTooManyRequests:
				denied++
			default:
				t.Errorf("unexpected status %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()

	if allowed != rpm {
		t.Errorf("allowed = %d, want exactly %d", allowed, rpm)
	}
	if allowed+denied != fired {
		t.Errorf("allowed+denied = %d, want %d", allowed+denied, fired)
	}
	if got := upstream.Hits(); got != rpm {
		t.Errorf("upstream saw %d requests, want exactly %d — a denied request must never reach the provider", got, rpm)
	}
}
