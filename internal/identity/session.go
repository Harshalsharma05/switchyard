// Refresh sessions: the revocable half of the pair. The access JWT is fast and
// cannot be cancelled; this is slower and can.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrNoSession      = errors.New("no such session")
	ErrSessionExpired = errors.New("session expired")

	// ErrTokenReused means a refresh token was presented after it had already
	// been rotated or revoked. Either a client race or a theft, and the server
	// cannot tell them apart, so it is read as theft.
	ErrTokenReused = errors.New("refresh token replayed")
)

// Session is a newly issued refresh credential. Raw goes to the browser
// exactly once and is never stored.
type Session struct {
	ID  string
	Raw string
}

// queryer is satisfied by both *pgxpool.Pool and pgx.Tx, so the same insert
// serves a fresh login and a rotation inside a transaction.
type queryer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func newRefreshToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating refresh token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// CreateSession issues a refresh credential for a freshly authenticated user.
func (s *Store) CreateSession(ctx context.Context, userID, userAgent, ip string, ttl time.Duration) (Session, error) {
	return s.insertSession(ctx, s.pool, userID, userAgent, ip, ttl)
}

func (s *Store) insertSession(ctx context.Context, q queryer, userID, userAgent, ip string, ttl time.Duration) (Session, error) {
	id, err := newID("ses_")
	if err != nil {
		return Session{}, err
	}
	raw, err := newRefreshToken()
	if err != nil {
		return Session{}, err
	}

	_, err = q.Exec(ctx, `
		INSERT INTO sessions (id, user_id, refresh_token_hash, expires_at, user_agent, ip)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4), $5, $6)`,
		id, userID, hashToken(raw), ttl.Seconds(), truncate(userAgent, 512), ip)
	if err != nil {
		return Session{}, dbErr("creating session", err)
	}
	return Session{ID: id, Raw: raw}, nil
}

// RotateSession exchanges a refresh token for a new one and returns the user
// behind it. The old token is marked rotated, so presenting it again is caught.
func (s *Store) RotateSession(ctx context.Context, raw, userAgent, ip string, ttl time.Duration) (User, Session, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, Session{}, dbErr("beginning refresh transaction", err)
	}
	defer tx.Rollback(ctx)

	var (
		oldID   string
		userID  string
		revoked bool
		rotated bool
		expired bool
	)
	err = tx.QueryRow(ctx, `
		SELECT id, user_id, revoked_at IS NOT NULL, rotated_to IS NOT NULL, expires_at < now()
		  FROM sessions
		 WHERE refresh_token_hash = $1
		   FOR UPDATE`,
		hashToken(raw)).Scan(&oldID, &userID, &revoked, &rotated, &expired)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, Session{}, ErrNoSession
		}
		return User{}, Session{}, dbErr("looking up refresh token", err)
	}

	// A token that has already been spent is replayed or stolen. Kill every
	// session this user has rather than only the one presented: if the token
	// leaked, whoever holds it may hold others from the same chain.
	if revoked || rotated {
		if _, err := tx.Exec(ctx,
			`UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`,
			userID); err != nil {
			return User{}, Session{}, dbErr("revoking replayed session chain", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return User{}, Session{}, dbErr("committing chain revocation", err)
		}
		return User{}, Session{}, ErrTokenReused
	}
	if expired {
		return User{}, Session{}, ErrSessionExpired
	}

	next, err := s.insertSession(ctx, tx, userID, userAgent, ip, ttl)
	if err != nil {
		return User{}, Session{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE sessions SET rotated_to = $2, revoked_at = now() WHERE id = $1`,
		oldID, next.ID); err != nil {
		return User{}, Session{}, dbErr("marking session rotated", err)
	}

	u, err := s.userByID(ctx, tx, userID)
	if err != nil {
		return User{}, Session{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, Session{}, dbErr("committing refresh", err)
	}
	return u, next, nil
}

// RevokeSession ends one session. An unknown token is not an error: logging
// out twice, or with a stale cookie, is not a failure worth surfacing.
func (s *Store) RevokeSession(ctx context.Context, raw string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE refresh_token_hash = $1 AND revoked_at IS NULL`,
		hashToken(raw))
	if err != nil {
		return dbErr("revoking session", err)
	}
	return nil
}

func (s *Store) userByID(ctx context.Context, q queryer, id string) (User, error) {
	var u User
	err := q.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id).Scan(
		&u.ID, &u.OrganizationID, &u.Email, &u.Name, &u.AvatarURL,
		&u.GoogleSub, &u.IsSuperadmin, &u.CreatedAt, &u.LastLoginAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrNoSession
		}
		return User{}, dbErr("loading session user", err)
	}
	return u, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
