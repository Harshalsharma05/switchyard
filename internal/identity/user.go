// Package identity owns dashboard humans: users, the organisations they own,
// and their sessions. It is reachable only from cmd/ and must never be
// importable from internal/proxy or any provider or resilience package -- the
// gateway's request path authenticates team API keys and knows nothing about
// users.
package identity

import (
	"errors"
	"strings"
	"time"
)

// ErrEmailTaken means the address already belongs to a different account.
// Callers must not reflect it to the browser: doing so turns sign-in into an
// email-enumeration oracle, the same concern that governs login timing.
var ErrEmailTaken = errors.New("email already registered to a different account")

type User struct {
	ID             string
	OrganizationID string
	Email          string
	Name           string
	AvatarURL      string
	GoogleSub      string
	IsSuperadmin   bool
	CreatedAt      time.Time
	LastLoginAt    time.Time
}

type Organization struct {
	ID          string
	Name        string
	OwnerUserID string
	CreatedAt   time.Time
}

// normalizeEmail matches the users_email_check constraint, which requires a
// lowercased address. Normalising in Go rather than relying on the database
// means a lookup and an insert agree on the same key.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// orgNameFor picks a display name for a new user's organisation.
func orgNameFor(name, email string) string {
	if n := strings.TrimSpace(name); n != "" {
		return n
	}
	if local, _, ok := strings.Cut(email, "@"); ok && local != "" {
		return local
	}
	return "Personal"
}
