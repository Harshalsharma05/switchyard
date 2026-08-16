package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Harshalsharma05/switchyard/internal/auth"
)

// Authenticator is the slice of internal/auth.Registry this package needs.
//
// Declared here, by the consumer, for the same reason Resolver is in
// handler.go: proxy depends on one method, not the concrete registry type,
// which is what lets a test inject a fake without touching internal/auth.
type Authenticator interface {
	Authenticate(key string) (*auth.Team, error)
}

// TeamFrom returns the team attached to a request's context, or nil if Auth
// did not run.
func TeamFrom(ctx context.Context) *auth.Team {
	team, _ := ctx.Value(teamKey).(*auth.Team)
	return team
}

// Auth resolves the caller's bearer token to a team before anything past it
// in the chain runs.
//
// It only answers "who is this" — mapping an unknown or missing key to 401.
// It does not decide what that team is allowed to do; a decision like the
// model allowlist depends on the request body, which nothing before the
// handler has parsed, so that check happens in ChatCompletions once decode
// has run, using the *auth.Team this middleware attaches to the context.
func Auth(authr Authenticator, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := bearerToken(r)
			if !ok {
				writeError(w, log, http.StatusUnauthorized, "invalid_api_key",
					`missing or malformed Authorization header; expected "Bearer <key>"`)
				return
			}

			team, err := authr.Authenticate(key)
			if err != nil {
				if errors.Is(err, auth.ErrUnknownKey) {
					writeError(w, log, http.StatusUnauthorized, "invalid_api_key",
						"the provided API key was not recognized")
					return
				}
				log.ErrorContext(r.Context(), "authenticating request", slog.Any("error", err))
				writeError(w, log, http.StatusInternalServerError, "internal_error",
					"the gateway could not authenticate this request")
				return
			}

			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), teamKey, team)))
		})
	}
}

// bearerToken extracts the credential from "Authorization: Bearer <key>" —
// the same shape OpenAI's own clients already send, so a caller adopting
// SwitchYard learns no new convention.
func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "

	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}

	key := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	if key == "" {
		return "", false
	}
	return key, true
}
