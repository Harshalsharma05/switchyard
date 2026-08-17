package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestReloadSuccess(t *testing.T) {
	called := false
	reload := func(ctx context.Context) (ReloadSummary, error) {
		called = true
		return ReloadSummary{Providers: 3, Teams: 2}, nil
	}
	srv := newTestAdminServerWithReload(t, testTeamStore(t), &fakeSpendReader{}, fakeProviderLister{}, reload)

	resp, err := http.Post(srv.URL+"/admin/reload", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !called {
		t.Error("the Reloader was never invoked")
	}

	var body reloadResponse
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Status != "reloaded" || body.Providers != 3 || body.Teams != 2 {
		t.Errorf("body = %+v, want {reloaded 3 2}", body)
	}
}

// The Step 4.4 checklist's central claim: a rejected reload must be
// reported to the caller, and — because Reloader's contract is to leave
// the live config untouched on failure — this handler has nothing further
// to roll back itself. That untouched-on-failure guarantee is proven at the
// cmd/gateway level (reload_test.go there), where the actual configStore
// lives; this test only proves the HTTP layer reports the failure correctly.
func TestReloadRejectedIs400(t *testing.T) {
	wantErr := errors.New("providers.yaml:6: unknown type \"bogus\"")
	reload := func(ctx context.Context) (ReloadSummary, error) {
		return ReloadSummary{}, wantErr
	}
	srv := newTestAdminServerWithReload(t, testTeamStore(t), &fakeSpendReader{}, fakeProviderLister{}, reload)

	resp, err := http.Post(srv.URL+"/admin/reload", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	var body errorBody
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Error.Type != "invalid_config" {
		t.Errorf("error.type = %q, want invalid_config", body.Error.Type)
	}
	if !strings.Contains(body.Error.Message, "unknown type") {
		t.Errorf("error.message = %q, want it to include the underlying validation error", body.Error.Message)
	}
}
