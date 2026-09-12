// Session authentication for the admin port.
package identity

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Attach puts the verified caller into whatever context shape the consuming
// package owns. This package never imports internal/admin and internal/admin
// never imports this one; cmd/ supplies the adapter and is the only place that
// knows about both.
type Attach func(context.Context, Claims) context.Context

// RequireSession rejects any request without a valid session cookie.
//
// The Authorization header is never read here. A team API key presented to the
// admin port is not refused by a rule that could be relaxed -- it is not an
// input to this middleware at all, which is the stronger form of the same
// guarantee.
func (h *Handlers) RequireSession(attach Attach) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(sessionCookie)
			if err != nil || cookie.Value == "" {
				writeJSONError(w, h.log, http.StatusUnauthorized, "no_session",
					"this endpoint requires a signed-in session")
				return
			}

			claims, err := h.signer.verify(cookie.Value, time.Now())
			if err != nil {
				errType := "session_invalid"
				if errors.Is(err, ErrTokenExpired) {
					errType = "session_expired"
				}
				h.log.Debug("rejected admin request", slog.String("reason", errType))
				writeJSONError(w, h.log, http.StatusUnauthorized, errType, "sign in again")
				return
			}

			ctx := r.Context()
			if attach != nil {
				ctx = attach(ctx, claims)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
