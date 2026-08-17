package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// fakeProviderLister is the fake behind ProviderLister.
type fakeProviderLister struct {
	configs []provider.Config
}

func (f fakeProviderLister) Configs() []provider.Config {
	return f.configs
}

func TestListProvidersOmitsAPIKey(t *testing.T) {
	lister := fakeProviderLister{configs: []provider.Config{
		{
			Name: "groq", Type: provider.TypeOpenAICompatible,
			BaseURL: "https://api.groq.com/openai/v1", APIKey: "gsk-super-secret",
			Timeout: 30 * time.Second, Models: []string{"openai/gpt-oss-120b"},
			DefaultMaxTokens: 1024, PingModel: "openai/gpt-oss-120b",
		},
		{
			Name: "ollama", Type: provider.TypeOllama,
			BaseURL: "http://localhost:11434", Timeout: 120 * time.Second,
			Models: []string{"llama3.2:3b"}, DefaultMaxTokens: 1024,
		},
	}}
	srv := newTestAdminServer(t, testTeamStore(t), &fakeSpendReader{}, lister)

	resp, err := http.Get(srv.URL + "/admin/providers")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if strings.Contains(string(raw), "gsk-super-secret") || strings.Contains(string(raw), "api_key") {
		t.Errorf("response body leaked the API key: %s", raw)
	}

	var views []providerView
	if err := json.Unmarshal(raw, &views); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("got %d providers, want 2", len(views))
	}
	if views[0].Name != "groq" || views[0].TimeoutSeconds != 30 {
		t.Errorf("views[0] = %+v, want groq/30s", views[0])
	}
	if views[1].Name != "ollama" || views[1].TimeoutSeconds != 120 {
		t.Errorf("views[1] = %+v, want ollama/120s", views[1])
	}
}

func TestListProvidersPreservesConfigOrder(t *testing.T) {
	lister := fakeProviderLister{configs: []provider.Config{
		{Name: "z-provider", Type: provider.TypeOllama, BaseURL: "http://z", Timeout: time.Second, Models: []string{"m"}, DefaultMaxTokens: 1},
		{Name: "a-provider", Type: provider.TypeOllama, BaseURL: "http://a", Timeout: time.Second, Models: []string{"m"}, DefaultMaxTokens: 1},
	}}
	srv := newTestAdminServer(t, testTeamStore(t), &fakeSpendReader{}, lister)

	resp, err := http.Get(srv.URL + "/admin/providers")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var views []providerView
	json.NewDecoder(resp.Body).Decode(&views)
	if len(views) != 2 || views[0].Name != "z-provider" || views[1].Name != "a-provider" {
		t.Errorf("order = %v, want [z-provider a-provider] (config order, not sorted)", names(views))
	}
}

func names(views []providerView) []string {
	out := make([]string, len(views))
	for i, v := range views {
		out[i] = v.Name
	}
	return out
}
