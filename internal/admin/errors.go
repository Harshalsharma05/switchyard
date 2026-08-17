package admin

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Error responses mirror proxy's envelope shape (see proxy/errors.go) so an
// operator scripting against both APIs sees one consistent format. Declared
// separately rather than imported: this package deliberately does not import
// internal/proxy (see router.go's doc comment), so a small duplication here
// is the cost of keeping that boundary real.
type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

// writeError sends an error envelope. It never fails the request further: if
// encoding somehow breaks, the status line has already gone out.
func writeError(w http.ResponseWriter, log *slog.Logger, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	body := errorBody{Error: errorDetail{Message: message, Type: errType}}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Warn("writing admin error response body", slog.Any("error", err))
	}
}

// writeJSON sends a successful JSON response.
func writeJSON(w http.ResponseWriter, log *slog.Logger, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Warn("writing admin response body", slog.Any("error", err))
	}
}
