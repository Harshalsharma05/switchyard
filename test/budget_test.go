// Black-box proof that a team gets blocked once its monthly budget is
// exhausted, and that a denied request never reaches the provider.
//
//go:build integration

package integration

import (
	"net/http"
	"testing"
)

func TestBudgetBlocksAtExhaustion(t *testing.T) {
	providerName := uniqueID("primary")
	model := uniqueID("model")
	upstream := newMockUpstream(t, providerName)

	team := defaultTeam(uniqueID("acme"), "budget-key", []string{providerName}, []string{model})
	// Priced so real usage (10 prompt + 5 completion tokens per mock reply, at
	// $100000/1M tokens) costs $1.50 per request, and the up-front reservation
	// against default_max_tokens (16) costs more than that again — small
	// enough to allow the first request or two, tight enough to guarantee a
	// 402 well before the loop below runs out of attempts.
	team.monthlyBudgetUSD = 3.0

	gw := startGateway(t, harnessConfig{
		providers: []providerSpec{{name: providerName, url: upstream.URL(), models: []string{model}}},
		teams:     []teamSpec{team},
	}, upstream)

	succeeded := 0
	for i := 0; i < 10; i++ {
		resp := postChat(t, gw, team.key, chatBody(model))
		status := resp.StatusCode
		resp.Body.Close()

		if status == http.StatusOK {
			succeeded++
			continue
		}
		if status != http.StatusPaymentRequired {
			t.Fatalf("request %d: status = %d, want 200 or 402", i, status)
		}

		hitsAtDenial := upstream.Hits()
		if hitsAtDenial != succeeded {
			t.Errorf("upstream hits = %d after denial, want %d (equal to the successful requests) — a 402 must never reach the provider", hitsAtDenial, succeeded)
		}
		if succeeded == 0 {
			t.Error("budget denied the very first request; the fixture's price/cap ratio needs adjusting so at least one request succeeds first")
		}
		return
	}

	t.Fatalf("never saw a 402 after %d requests; budget cap $%.2f is too generous for this fixture's pricing", 10, team.monthlyBudgetUSD)
}
