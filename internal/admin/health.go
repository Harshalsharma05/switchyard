package admin

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/health"
)

// HealthReader is the slice of health.Monitor this package needs.
type HealthReader interface {
	Snapshots() []health.ProviderHealth
}

// transitionView is one recorded status change, as GET /admin/providers/health
// reports it.
type transitionView struct {
	At     time.Time `json:"at"`
	From   string    `json:"from"`
	To     string    `json:"to"`
	Reason string    `json:"reason"`
}

func newTransitionView(t health.Transition) transitionView {
	return transitionView{At: t.At, From: t.From.String(), To: t.To.String(), Reason: t.Reason}
}

// providerHealthView is GET /admin/providers/health's response shape for one
// provider: Step 5.4's status, error rate, p99, last check, and transition
// history.
type providerHealthView struct {
	Provider         string           `json:"provider"`
	Status           string           `json:"status"`
	ErrorRate        float64          `json:"error_rate"`
	P99LatencyMillis float64          `json:"p99_latency_ms"`
	LastCheckAt      *time.Time       `json:"last_check_at,omitempty"`
	LastTransition   *transitionView  `json:"last_transition,omitempty"`
	History          []transitionView `json:"history"`
}

func newProviderHealthView(h health.ProviderHealth) providerHealthView {
	view := providerHealthView{
		Provider:         h.Provider,
		Status:           h.Status.String(),
		ErrorRate:        h.ErrorRate,
		P99LatencyMillis: float64(h.P99Latency) / float64(time.Millisecond),
		History:          make([]transitionView, 0, len(h.History)),
	}

	// LastCheckAt is the zero time until Observe has run at least once for
	// this provider — omitted rather than serialized as "0001-01-01," which
	// would read as a real, very stale timestamp instead of "no check yet."
	if !h.LastCheckAt.IsZero() {
		t := h.LastCheckAt
		view.LastCheckAt = &t
	}
	if h.LastTransition != nil {
		tv := newTransitionView(*h.LastTransition)
		view.LastTransition = &tv
	}
	for _, t := range h.History {
		view.History = append(view.History, newTransitionView(t))
	}

	return view
}

// listProviderHealth serves GET /admin/providers/health: Step 5.4's status,
// error rate, p99 latency, last active check, and transition history for
// every provider, in configs/providers.yaml order.
func listProviderHealth(reader HealthReader, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snaps := reader.Snapshots()
		views := make([]providerHealthView, 0, len(snaps))
		for _, s := range snaps {
			views = append(views, newProviderHealthView(s))
		}
		writeJSON(w, log, http.StatusOK, views)
	}
}
