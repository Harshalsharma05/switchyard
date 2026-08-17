package admin

import (
	"log/slog"
	"net/http"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// ProviderLister is the slice of provider.Registry this package needs.
type ProviderLister interface {
	Configs() []provider.Config
}

// providerView is GET /admin/providers' response shape for one instance.
//
// APIKey is deliberately absent. provider.Registry.Configs returns it
// unredacted — see that method's doc comment — specifically because
// redaction belongs at the presentation boundary, and this struct is that
// boundary: it is built field by field rather than by embedding
// provider.Config, so there is no field left to accidentally serialize.
type providerView struct {
	Name             string   `json:"name"`
	Type             string   `json:"type"`
	BaseURL          string   `json:"base_url"`
	TimeoutSeconds   float64  `json:"timeout_seconds"`
	Models           []string `json:"models"`
	DefaultMaxTokens int      `json:"default_max_tokens"`
	PingModel        string   `json:"ping_model"`
}

func newProviderView(cfg provider.Config) providerView {
	return providerView{
		Name:             cfg.Name,
		Type:             cfg.Type,
		BaseURL:          cfg.BaseURL,
		TimeoutSeconds:   cfg.Timeout.Seconds(),
		Models:           cfg.Models,
		DefaultMaxTokens: cfg.DefaultMaxTokens,
		PingModel:        cfg.PingModel,
	}
}

// listProviders serves GET /admin/providers. Health status is Phase 5 scope
// — this is static configuration only, the same "declared now, populated
// later" pattern Provider.Ping used from Phase 1 onward.
func listProviders(providers ProviderLister, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfgs := providers.Configs()
		views := make([]providerView, 0, len(cfgs))
		for _, cfg := range cfgs {
			views = append(views, newProviderView(cfg))
		}
		writeJSON(w, log, http.StatusOK, views)
	}
}
