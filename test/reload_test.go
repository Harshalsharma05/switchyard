// Black-box proof of Step 4.4's promise under Phase 11, as narrowed by Tier 1
// Phase 2: POST /admin/reload swaps in a new providers.yaml without dropping a
// request that is already in flight against the old one, and the new config
// takes effect for the very next request. Teams are no longer part of a reload
// — they live in Postgres — so this exercises the provider half, which is all a
// reload now does.
//
//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestReloadDoesNotDropAnInFlightRequest(t *testing.T) {
	providerName := uniqueID("primary")
	model := uniqueID("model")
	addedModel := uniqueID("added")
	upstream := newMockUpstream(t, providerName)
	upstream.SetHandler(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		writeJSONSuccess(w, providerName)
	})

	// The allowlist names the model the reload will add. Allowlists are team
	// data in Postgres now, not reloadable config, so the team has to be
	// permitted up front for the post-reload request to prove anything.
	team := defaultTeam(uniqueID("acme"), "reload-key", []string{providerName}, []string{model, addedModel})
	team.isAdmin = true // POST /admin/reload is admin-gated
	cfg := harnessConfig{
		providers: []providerSpec{{name: providerName, url: upstream.URL(), models: []string{model}}},
		teams:     []teamSpec{team},
	}
	gw := startGateway(t, cfg, upstream)

	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		body := chatBody(model)
		raw, err := json.Marshal(body)
		if err != nil {
			done <- result{err: err}
			return
		}
		req, err := http.NewRequest(http.MethodPost, gw.BaseURL+"/v1/chat/completions", bytes.NewReader(raw))
		if err != nil {
			done <- result{err: err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+team.key)

		resp, err := gw.Client.Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		done <- result{status: resp.StatusCode}
	}()

	time.Sleep(150 * time.Millisecond) // let the request reach the slow provider call

	updated := cfg
	updated.providers = []providerSpec{{name: providerName, url: upstream.URL(), models: []string{model, addedModel}}}
	if err := os.WriteFile(gw.providersPath, []byte(buildProvidersYAML(updated)), 0o644); err != nil {
		t.Fatalf("rewriting providers.yaml: %v", err)
	}

	reloadReq, err := http.NewRequest(http.MethodPost, gw.AdminURL+"/admin/reload", nil)
	if err != nil {
		t.Fatalf("building reload request: %v", err)
	}
	cookie, csrf := adminSession(t)
	reloadReq.Header.Set("Cookie", cookie)
	reloadReq.Header.Set("X-CSRF-Token", csrf)
	reloadResp, err := gw.Client.Do(reloadReq)
	if err != nil {
		t.Fatalf("POST /admin/reload: %v", err)
	}
	reloadResp.Body.Close()
	if reloadResp.StatusCode != http.StatusOK {
		t.Fatalf("reload status = %d, want 200", reloadResp.StatusCode)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("in-flight request failed: %v", r.err)
		}
		if r.status != http.StatusOK {
			t.Errorf("in-flight request status = %d, want 200 — a reload must not drop a request already being served on the old config", r.status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight request never completed")
	}

	postReload := postChat(t, gw, team.key, chatBody(addedModel))
	defer postReload.Body.Close()
	if postReload.StatusCode != http.StatusOK {
		t.Errorf("post-reload request for the newly added model = %d, want 200 — the reload should have taken effect", postReload.StatusCode)
	}
}
