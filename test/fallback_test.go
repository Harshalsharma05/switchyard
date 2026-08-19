// Black-box proof that a failing primary falls back to the next healthy
// candidate in its tier, and that a team's allowlist beats availability —
// it never gets routed to a provider it isn't permitted to use, even when
// that provider is the only healthy one left.
//
//go:build integration

package integration

import (
	"net/http"
	"testing"
)

func TestFallbackServesFromTheSecondCandidate(t *testing.T) {
	primaryName, fallbackName := uniqueID("primary"), uniqueID("fallback")
	modelA, modelB := uniqueID("model-a"), uniqueID("model-b")

	primary := newMockUpstream(t, primaryName)
	fallback := newMockUpstream(t, fallbackName)

	primary.SetHandler(func(w http.ResponseWriter, r *http.Request) {
		writeErrorJSON(w, http.StatusInternalServerError, "primary is down")
	})

	team := defaultTeam(uniqueID("acme"), "fallback-key", []string{primaryName, fallbackName}, []string{modelA, modelB})

	gw := startGateway(t, harnessConfig{
		providers: []providerSpec{
			{name: primaryName, url: primary.URL(), models: []string{modelA}},
			{name: fallbackName, url: fallback.URL(), models: []string{modelB}},
		},
		tiers: map[string][]tierEntry{
			"fast": {
				{provider: primaryName, model: modelA},
				{provider: fallbackName, model: modelB},
			},
		},
		teams: []teamSpec{team},
	}, primary, fallback)

	resp := postChat(t, gw, team.key, chatBody(modelA))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 served by the fallback", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Switchyard-Fallback"); got != "true" {
		t.Errorf("X-Switchyard-Fallback = %q, want %q", got, "true")
	}
	if got := resp.Header.Get("X-Switchyard-Served-Model"); got != modelB {
		t.Errorf("X-Switchyard-Served-Model = %q, want %q", got, modelB)
	}
	if primary.Hits() != 1 {
		t.Errorf("primary received %d hits, want exactly 1", primary.Hits())
	}
	if fallback.Hits() != 1 {
		t.Errorf("fallback received %d hits, want exactly 1", fallback.Hits())
	}
}

func TestFallbackRespectsTeamAllowlist(t *testing.T) {
	primaryName, fallbackName := uniqueID("primary"), uniqueID("fallback")
	modelA, modelB := uniqueID("model-a"), uniqueID("model-b")

	primary := newMockUpstream(t, primaryName)
	fallback := newMockUpstream(t, fallbackName)

	primary.SetHandler(func(w http.ResponseWriter, r *http.Request) {
		writeErrorJSON(w, http.StatusInternalServerError, "primary is down")
	})

	// Deliberately missing fallbackName: the compliance point from Step 6.2 —
	// this team must get an error, never a silent route to a provider it
	// isn't permitted to use, even though fallback is healthy.
	team := defaultTeam(uniqueID("acme"), "no-fallback-allowlist-key", []string{primaryName}, []string{modelA})

	gw := startGateway(t, harnessConfig{
		providers: []providerSpec{
			{name: primaryName, url: primary.URL(), models: []string{modelA}},
			{name: fallbackName, url: fallback.URL(), models: []string{modelB}},
		},
		tiers: map[string][]tierEntry{
			"fast": {
				{provider: primaryName, model: modelA},
				{provider: fallbackName, model: modelB},
			},
		},
		teams: []teamSpec{team},
	}, primary, fallback)

	resp := postChat(t, gw, team.key, chatBody(modelA))
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Fatal("request succeeded; it must fail rather than fall back to a provider outside the team's allowlist")
	}
	if got := resp.Header.Get("X-Switchyard-Fallback"); got == "true" {
		t.Error("X-Switchyard-Fallback = true, want no fallback to an unauthorized provider")
	}
	if fallback.Hits() != 0 {
		t.Errorf("fallback received %d hits, want 0 — it is outside this team's allowlist", fallback.Hits())
	}
}
