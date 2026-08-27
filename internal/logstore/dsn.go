package logstore

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DBConfig is the request-log database connection, assembled from its parts
// rather than passed as one opaque URL. The password arrives under its native
// name (POSTGRES_PASSWORD, via env_file) the same way provider API keys do;
// host, user, and database name are not secrets and have working defaults.
type DBConfig struct {
	Host     string // "host:port", e.g. "localhost:5432" or "postgres:5432"
	User     string
	Password string
	Database string
	SSLMode  string // "disable" for local dev and the compose network
}

// DSN renders the config as a postgres:// URL for pgx.Connect. User and
// password are percent-encoded, so a password containing URL metacharacters
// survives the round trip.
func (c DBConfig) DSN() string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.User, c.Password),
		Host:   c.Host,
		Path:   "/" + c.Database,
	}
	q := url.Values{}
	if c.SSLMode != "" {
		q.Set("sslmode", c.SSLMode)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// Redacted is DSN with the password blanked, for logs.
func (c DBConfig) Redacted() string {
	c.Password = ""
	return fmt.Sprintf("postgres://%s@%s/%s?sslmode=%s", c.User, c.Host, c.Database, c.SSLMode)
}

// NewPool builds the connection pool. It does not dial: MinConns stays 0 so
// connections open lazily, which is what lets the gateway boot while Postgres
// is down instead of failing startup over a fail-open dependency.
func NewPool(ctx context.Context, c DBConfig) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(c.DSN())
	if err != nil {
		return nil, fmt.Errorf("parsing postgres dsn: %w", err)
	}
	cfg.MaxConns = 8
	cfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("building postgres pool: %w", err)
	}
	return pool, nil
}
