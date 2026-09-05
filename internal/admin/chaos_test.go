package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeChaos is the fake behind ChaosController. available is fixed at
// construction, mirroring the real harness, where it cannot be changed after
// the fact.
type fakeChaos struct {
	available bool
	setErr    error

	mu    sync.Mutex
	rules []ChaosRuleSpec
}

func (f *fakeChaos) Available() bool { return f.available }

func (f *fakeChaos) Rules() []ChaosRuleSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ChaosRuleSpec(nil), f.rules...)
}

func (f *fakeChaos) SetRules(rules []ChaosRuleSpec) error {
	if !f.available {
		return errors.New("chaos harness is not available in this environment")
	}
	if f.setErr != nil {
		return f.setErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append([]ChaosRuleSpec(nil), rules...)
	return nil
}

func (f *fakeChaos) Clear() error {
	if !f.available {
		return errors.New("chaos harness is not available in this environment")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = nil
	return nil
}

func newTestAdminServerWithChaos(t *testing.T, chaos ChaosController) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewRouter(func() bool { return true },
		testTeamStore(t), &fakeSpendReader{}, fakeProviderLister{}, fakeHealthReader{},
		&fakeBreakerController{}, chaos, fakeReloader, nil, nil, nil, nil, nil, nil, QualityFeedbackConfig{}, false, testMetrics(t), discardLogger()))
	t.Cleanup(srv.Close)
	return srv
}

func doChaosRequest(t *testing.T, srv *httptest.Server, method, body string) *http.Response {
	t.Helper()
	var r *http.Request
	var err error
	if body == "" {
		r, err = http.NewRequest(method, srv.URL+"/admin/chaos", nil)
	} else {
		r, err = http.NewRequest(method, srv.URL+"/admin/chaos", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("%s /admin/chaos: %v", method, err)
	}
	return resp
}

// --- the guard ----------------------------------------------------------------

// TestChaosEndpointIs404WhenUnavailable is the plan's "guarded so it can never
// be enabled in a non-dev environment" as the operator experiences it. 404
// rather than 403, so a production gateway is indistinguishable from a build
// that never had the endpoint.
func TestChaosEndpointIs404WhenUnavailable(t *testing.T) {
	srv := newTestAdminServerWithChaos(t, &fakeChaos{available: false})

	tests := map[string]struct {
		method string
		body   string
	}{
		"get":    {method: http.MethodGet},
		"post":   {method: http.MethodPost, body: `{"rules":[{"provider":"groq","mode":"error"}]}`},
		"delete": {method: http.MethodDelete},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			resp := doChaosRequest(t, srv, tt.method, tt.body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404", resp.StatusCode)
			}
		})
	}
}

// TestUnavailableChaosEndpointAppliesNothing proves the 404 is not merely
// cosmetic: a POST against an unavailable harness must leave no rules behind.
func TestUnavailableChaosEndpointAppliesNothing(t *testing.T) {
	chaos := &fakeChaos{available: false}
	srv := newTestAdminServerWithChaos(t, chaos)

	resp := doChaosRequest(t, srv, http.MethodPost, `{"rules":[{"provider":"groq","mode":"error"}]}`)
	resp.Body.Close()

	if got := chaos.Rules(); len(got) != 0 {
		t.Errorf("rules = %+v after a refused POST, want none applied", got)
	}
}

// TestNilChaosControllerIs404 covers a gateway wired with no harness at all.
func TestNilChaosControllerIs404(t *testing.T) {
	srv := newTestAdminServerWithChaos(t, nil)

	resp := doChaosRequest(t, srv, http.MethodGet, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// --- the happy path -------------------------------------------------------------

func TestSetAndGetChaosRules(t *testing.T) {
	chaos := &fakeChaos{available: true}
	srv := newTestAdminServerWithChaos(t, chaos)

	body := `{"rules":[
		{"provider":"groq","model":"gpt-oss-120b","mode":"error"},
		{"provider":"ollama","mode":"latency","latency_ms":5000}
	]}`
	resp := doChaosRequest(t, srv, http.MethodPost, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST status = %d, want 200", resp.StatusCode)
	}

	got := chaos.Rules()
	if len(got) != 2 {
		t.Fatalf("applied %d rules, want 2", len(got))
	}
	if got[0] != (ChaosRuleSpec{Provider: "groq", Model: "gpt-oss-120b", Mode: "error"}) {
		t.Errorf("rule 0 = %+v", got[0])
	}
	if got[1] != (ChaosRuleSpec{Provider: "ollama", Mode: "latency", LatencyMS: 5000}) {
		t.Errorf("rule 1 = %+v", got[1])
	}

	readBack := doChaosRequest(t, srv, http.MethodGet, "")
	defer readBack.Body.Close()

	var view chaosView
	if err := json.NewDecoder(readBack.Body).Decode(&view); err != nil {
		t.Fatalf("decoding GET: %v", err)
	}
	if !view.Available {
		t.Errorf("available = false, want true")
	}
	if len(view.Rules) != 2 {
		t.Fatalf("GET returned %d rules, want 2", len(view.Rules))
	}
	if view.Rules[1].LatencyMS != 5000 {
		t.Errorf("latency_ms = %d, want 5000", view.Rules[1].LatencyMS)
	}
}

func TestDeleteChaosClearsRules(t *testing.T) {
	chaos := &fakeChaos{available: true, rules: []ChaosRuleSpec{{Provider: "groq", Mode: "error"}}}
	srv := newTestAdminServerWithChaos(t, chaos)

	resp := doChaosRequest(t, srv, http.MethodDelete, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := chaos.Rules(); len(got) != 0 {
		t.Errorf("rules = %+v after DELETE, want none", got)
	}
}

// TestSetChaosRejectsAnInvalidRule proves validation failures from the
// harness surface as a 400 naming the problem, not a silent success.
func TestSetChaosRejectsAnInvalidRule(t *testing.T) {
	chaos := &fakeChaos{available: true, setErr: errors.New("chaos rule 0: unknown chaos mode \"explode\"")}
	srv := newTestAdminServerWithChaos(t, chaos)

	resp := doChaosRequest(t, srv, http.MethodPost, `{"rules":[{"provider":"groq","mode":"explode"}]}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	var body errorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !strings.Contains(body.Error.Message, "explode") {
		t.Errorf("message = %q, want it to name the offending mode", body.Error.Message)
	}
}

func TestSetChaosRejectsMalformedJSON(t *testing.T) {
	srv := newTestAdminServerWithChaos(t, &fakeChaos{available: true})

	resp := doChaosRequest(t, srv, http.MethodPost, `{"rules":[{"provider":`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestSetChaosRejectsUnknownFields catches a misspelled key rather than
// silently ignoring it — the same DisallowUnknownFields reasoning the public
// handler uses for max_tokens.
func TestSetChaosRejectsUnknownFields(t *testing.T) {
	srv := newTestAdminServerWithChaos(t, &fakeChaos{available: true})

	resp := doChaosRequest(t, srv, http.MethodPost, `{"rules":[{"provider":"groq","mode":"latency","latency":5000}]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a misspelled field", resp.StatusCode)
	}
}

// TestEmptyRuleSetIsAValidReset proves POSTing no rules is the same as
// clearing, so a demo script can drive the gateway to a known state with one
// call shape throughout.
func TestEmptyRuleSetIsAValidReset(t *testing.T) {
	chaos := &fakeChaos{available: true, rules: []ChaosRuleSpec{{Provider: "groq", Mode: "error"}}}
	srv := newTestAdminServerWithChaos(t, chaos)

	resp := doChaosRequest(t, srv, http.MethodPost, `{"rules":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := chaos.Rules(); len(got) != 0 {
		t.Errorf("rules = %+v, want the set replaced with nothing", got)
	}
}
