// System info (Part 2, Step 6.4). GET /admin/system is the read-only half of
// the Settings screen's System panel: what version is running, how long it has
// been up, which config it loaded, and whether its backing services answer.
// Admin-gated by the route group.
package admin

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// SystemReporter is what the gateway process knows about itself. cmd/gateway
// supplies it — version, start time, config fingerprint, and the live
// dependency probes all need collaborators (the Redis client, the pgx pool, the
// summary service) that only main.go wires. The provider list comes from the
// ProviderLister this package already has, not from here.
type SystemReporter interface {
	Version() string
	Uptime() time.Duration
	ConfigHash() string
	// Dependencies probes each backing service and returns its state:
	// "up", "down", or "not_configured". It does live I/O — each probe is
	// given its own short timeout by the implementation — so the handler
	// passes the request context straight through.
	Dependencies(ctx context.Context) map[string]string
}

type systemProviderView struct {
	Name   string   `json:"name"`
	Type   string   `json:"type"`
	Models []string `json:"models"`
}

type systemView struct {
	Version       string               `json:"version"`
	UptimeSeconds int64                `json:"uptime_seconds"`
	ConfigHash    string               `json:"config_hash"`
	GeneratedAt   string               `json:"generated_at"`
	Dependencies  map[string]string    `json:"dependencies"`
	Providers     []systemProviderView `json:"providers"`
}

func handleSystem(sys SystemReporter, providers ProviderLister, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sys == nil {
			writeError(w, log, http.StatusServiceUnavailable, "system_info_unavailable",
				"this gateway does not expose system info")
			return
		}

		provViews := []systemProviderView{}
		if providers != nil {
			for _, c := range providers.Configs() {
				provViews = append(provViews, systemProviderView{
					Name:   c.Name,
					Type:   c.Type,
					Models: c.Models,
				})
			}
		}

		writeJSON(w, log, http.StatusOK, systemView{
			Version:       sys.Version(),
			UptimeSeconds: int64(sys.Uptime().Seconds()),
			ConfigHash:    sys.ConfigHash(),
			GeneratedAt:   time.Now().UTC().Format(time.RFC3339Nano),
			Dependencies:  sys.Dependencies(r.Context()),
			Providers:     provViews,
		})
	}
}
