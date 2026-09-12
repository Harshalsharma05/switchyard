// The Postgres home for users, organisations, and sessions.
package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Harshalsharma05/switchyard/internal/oauth"
)

// Store reads and writes the identity tables. It is only ever called from the
// admin port, so a query per request is affordable here in a way it would
// never be on the gateway's hot path.
type Store struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	// superadminEmail bootstraps the first operator: the Google account that
	// signs in with this address becomes superadmin and adopts defaultOrgID
	// rather than getting an organisation of its own. Empty disables the
	// branch entirely, and no superadmin is ever created.
	superadminEmail string
	defaultOrgID    string
	defaultOrgName  string
}

func NewStore(pool *pgxpool.Pool, superadminEmail, defaultOrgID, defaultOrgName string, log *slog.Logger) *Store {
	return &Store{
		pool:            pool,
		log:             log,
		superadminEmail: normalizeEmail(superadminEmail),
		defaultOrgID:    defaultOrgID,
		defaultOrgName:  defaultOrgName,
	}
}

const userColumns = `id, organization_id, email, name, avatar_url,
	coalesce(google_sub, ''), is_superadmin, created_at, coalesce(last_login_at, created_at)`

// FindOrCreateGoogleUser resolves a Google profile to a user, creating the
// user and their organisation in one transaction on first sign-in. The bool
// reports whether this call created them.
//
// Lookup is by Google's sub, never by email: an address can be reassigned to a
// different person, a sub cannot.
func (s *Store) FindOrCreateGoogleUser(ctx context.Context, p oauth.Profile) (User, bool, error) {
	if p.Sub == "" || !p.EmailVerified {
		return User{}, false, errors.New("refusing to resolve an unverified google profile")
	}
	email := normalizeEmail(p.Email)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, false, dbErr("beginning identity transaction", err)
	}
	defer tx.Rollback(ctx)

	u, err := s.signInExisting(ctx, tx, p)
	switch {
	case err == nil:
		if err := tx.Commit(ctx); err != nil {
			return User{}, false, dbErr("committing sign-in", err)
		}
		return u, false, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return User{}, false, err
	}

	// Unreachable under Google-only sign-in, which is exactly why it gets an
	// explicit branch rather than an assumption.
	var existing string
	switch err := tx.QueryRow(ctx, `SELECT id FROM users WHERE email = $1`, email).Scan(&existing); {
	case err == nil:
		return User{}, false, ErrEmailTaken
	case !errors.Is(err, pgx.ErrNoRows):
		return User{}, false, dbErr("checking email", err)
	}

	created, err := s.create(ctx, tx, p, email)
	if err != nil {
		return User{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, false, dbErr("committing signup", err)
	}
	return created, true, nil
}

// signInExisting records the sign-in and returns the refreshed user in one
// statement. Name and avatar live on Google's side and change there; this is
// the only moment we hear about it, and reading the row before the update
// would hand the caller a stale profile.
func (s *Store) signInExisting(ctx context.Context, tx pgx.Tx, p oauth.Profile) (User, error) {
	var u User
	err := tx.QueryRow(ctx, `
		UPDATE users
		   SET last_login_at = now(),
		       email_verified_at = coalesce(email_verified_at, now()),
		       name = $2,
		       avatar_url = $3
		 WHERE google_sub = $1
		RETURNING `+userColumns,
		p.Sub, p.Name, p.Picture,
	).Scan(
		&u.ID, &u.OrganizationID, &u.Email, &u.Name, &u.AvatarURL,
		&u.GoogleSub, &u.IsSuperadmin, &u.CreatedAt, &u.LastLoginAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, err
		}
		return User{}, dbErr("recording sign-in", err)
	}
	return u, nil
}

// create inserts the user and the organisation that owns them.
//
// The two tables reference each other, so the order is fixed: organisation
// first with a null owner, then the user, then the owner back-reference.
func (s *Store) create(ctx context.Context, tx pgx.Tx, p oauth.Profile, email string) (User, error) {
	userID, err := newID("usr_")
	if err != nil {
		return User{}, err
	}

	orgID, orgName := "", ""
	superadmin := s.superadminEmail != "" && email == s.superadminEmail
	if superadmin {
		// The bootstrap operator adopts the organisation the YAML import
		// already created, so the teams imported in Tier 1 are not orphaned.
		// Created here if it is missing, which makes a fresh database and a
		// seeded one behave identically.
		orgID, orgName = s.defaultOrgID, s.defaultOrgName
	} else {
		if orgID, err = newID("org_"); err != nil {
			return User{}, err
		}
		orgName = orgNameFor(p.Name, email)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO organizations (id, name) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
		orgID, orgName); err != nil {
		return User{}, dbErr("creating organization", err)
	}

	var u User
	err = tx.QueryRow(ctx, `
		INSERT INTO users (id, organization_id, email, email_verified_at, name, avatar_url,
		                   google_sub, is_superadmin, last_login_at)
		VALUES ($1, $2, $3, now(), $4, $5, $6, $7, now())
		RETURNING `+userColumns,
		userID, orgID, email, p.Name, p.Picture, p.Sub, superadmin,
	).Scan(
		&u.ID, &u.OrganizationID, &u.Email, &u.Name, &u.AvatarURL,
		&u.GoogleSub, &u.IsSuperadmin, &u.CreatedAt, &u.LastLoginAt,
	)
	if err != nil {
		return User{}, dbErr("creating user", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE organizations SET owner_user_id = $2 WHERE id = $1 AND owner_user_id IS NULL`,
		orgID, userID); err != nil {
		return User{}, dbErr("setting organization owner", err)
	}

	return u, nil
}

func newID(prefix string) (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating %sid: %w", prefix, err)
	}
	return prefix + hex.EncodeToString(b), nil
}

// dbErr wraps a Postgres error without its Detail field.
//
// PgError.Detail carries every column value of the rejected row -- for these
// tables that means email, IP, and token hashes -- and %w would carry it into
// any log line that prints the error. The constraint name and SQLSTATE are
// what actually identify the failure.
func dbErr(op string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.ConstraintName != "" {
			return fmt.Errorf("%s: constraint %s (%s)", op, pgErr.ConstraintName, pgErr.Code)
		}
		return fmt.Errorf("%s: postgres %s", op, pgErr.Code)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// Ping reports whether the identity tables are reachable.
func (s *Store) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		return dbErr("pinging identity store", err)
	}
	return nil
}

// SuperadminConfigured reports whether a bootstrap address is set, for the
// boot-time warning in cmd/.
func (s *Store) SuperadminConfigured() bool { return s.superadminEmail != "" }
