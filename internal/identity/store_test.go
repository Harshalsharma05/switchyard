package identity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Harshalsharma05/switchyard/internal/logstore"
	"github.com/Harshalsharma05/switchyard/internal/oauth"
	"github.com/Harshalsharma05/switchyard/migrations"
)

const (
	defaultOrgID   = "personal"
	defaultOrgName = "Personal"
)

func testDSN(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("SWITCHYARD_TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	pw := os.Getenv("POSTGRES_PASSWORD")
	if pw == "" {
		t.Skip("set POSTGRES_PASSWORD (and run Postgres) or SWITCHYARD_TEST_POSTGRES_DSN to run identity tests")
	}
	return logstore.DBConfig{
		Host:     envOrDefault("SWITCHYARD_POSTGRES_HOST", "localhost:5432"),
		User:     envOrDefault("SWITCHYARD_POSTGRES_USER", "switchyard"),
		Password: pw,
		Database: envOrDefault("SWITCHYARD_POSTGRES_DB", "switchyard"),
		SSLMode:  envOrDefault("SWITCHYARD_POSTGRES_SSLMODE", "disable"),
	}.DSN()
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// newStore gives back a store over a migrated throwaway schema, the same
// pattern teamstore's tests use.
func newStore(t *testing.T, superadminEmail string) (*Store, context.Context) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	dsn := testDSN(t)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("no Postgres reachable: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })

	schema := fmt.Sprintf("identity_store_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("creating test schema: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn.Exec(c, "DROP SCHEMA "+schema+" CASCADE")
	})
	if _, err := conn.Exec(ctx, "SET search_path TO "+schema); err != nil {
		t.Fatalf("setting search_path: %v", err)
	}
	if _, err := logstore.Migrate(ctx, conn, migrations.FS); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing pool config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("building pool: %v", err)
	}
	t.Cleanup(pool.Close)

	return NewStore(pool, superadminEmail, defaultOrgID, defaultOrgName, discardLogger()), ctx
}

func profile(sub, email, name string) oauth.Profile {
	return oauth.Profile{Sub: sub, Email: email, EmailVerified: true, Name: name}
}

// First sign-in creates the pair; the second must reuse both. A second
// organisation per login would silently fragment a user's projects.
func TestFindOrCreateIsStableAcrossLogins(t *testing.T) {
	s, ctx := newStore(t, "")

	first, created, err := s.FindOrCreateGoogleUser(ctx, profile("sub-1", "Person@Example.com", "A Person"))
	if err != nil {
		t.Fatalf("first sign-in: %v", err)
	}
	if !created {
		t.Fatal("first sign-in did not report a creation")
	}
	if first.Email != "person@example.com" {
		t.Fatalf("email = %q, want it normalised to lowercase", first.Email)
	}

	second, created, err := s.FindOrCreateGoogleUser(ctx, profile("sub-1", "Person@Example.com", "Renamed Person"))
	if err != nil {
		t.Fatalf("second sign-in: %v", err)
	}
	if created {
		t.Fatal("second sign-in created a new user")
	}
	if second.ID != first.ID || second.OrganizationID != first.OrganizationID {
		t.Fatalf("second sign-in moved the user: %+v then %+v", first, second)
	}
	if second.Name != "Renamed Person" {
		t.Errorf("name = %q, want the profile refreshed from Google", second.Name)
	}

	var orgs int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM organizations").Scan(&orgs); err != nil {
		t.Fatalf("counting organizations: %v", err)
	}
	if orgs != 1 {
		t.Fatalf("organizations = %d, want 1", orgs)
	}
}

// The org must end up owned by the user it was created for; an org with a null
// owner is the broken state the plan warns about.
func TestSignupSetsOrganizationOwner(t *testing.T) {
	s, ctx := newStore(t, "")

	u, _, err := s.FindOrCreateGoogleUser(ctx, profile("sub-1", "person@example.com", "A Person"))
	if err != nil {
		t.Fatalf("sign-in: %v", err)
	}

	var owner string
	err = s.pool.QueryRow(ctx, "SELECT coalesce(owner_user_id, '') FROM organizations WHERE id = $1", u.OrganizationID).Scan(&owner)
	if err != nil {
		t.Fatalf("reading owner: %v", err)
	}
	if owner != u.ID {
		t.Fatalf("owner_user_id = %q, want %q", owner, u.ID)
	}
}

// The bootstrap operator adopts the organisation the YAML import already
// created, so the teams imported in Tier 1 are not orphaned.
func TestSuperadminAdoptsDefaultOrganization(t *testing.T) {
	s, ctx := newStore(t, "Boss@Example.com")

	if _, err := s.pool.Exec(ctx, "INSERT INTO organizations (id, name) VALUES ($1, $2)", defaultOrgID, defaultOrgName); err != nil {
		t.Fatalf("seeding default org: %v", err)
	}

	boss, _, err := s.FindOrCreateGoogleUser(ctx, profile("sub-boss", "boss@example.com", "The Boss"))
	if err != nil {
		t.Fatalf("superadmin sign-in: %v", err)
	}
	if !boss.IsSuperadmin {
		t.Error("bootstrap address did not produce a superadmin")
	}
	if boss.OrganizationID != defaultOrgID {
		t.Fatalf("organization = %q, want the existing %q", boss.OrganizationID, defaultOrgID)
	}

	other, _, err := s.FindOrCreateGoogleUser(ctx, profile("sub-2", "someone@example.com", "Someone"))
	if err != nil {
		t.Fatalf("ordinary sign-in: %v", err)
	}
	if other.IsSuperadmin {
		t.Error("an ordinary address became superadmin")
	}
	if other.OrganizationID == defaultOrgID {
		t.Error("an ordinary user joined the default organisation instead of getting their own")
	}
}

// Unreachable under Google-only sign-in, but the branch exists so it cannot
// become an accidental account takeover if a second credential type returns.
func TestEmailBelongingToAnotherAccountIsRejected(t *testing.T) {
	s, ctx := newStore(t, "")

	if _, _, err := s.FindOrCreateGoogleUser(ctx, profile("sub-1", "person@example.com", "A Person")); err != nil {
		t.Fatalf("first sign-in: %v", err)
	}

	_, _, err := s.FindOrCreateGoogleUser(ctx, profile("sub-2", "person@example.com", "An Impostor"))
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("error = %v, want ErrEmailTaken", err)
	}

	var users int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&users); err != nil {
		t.Fatalf("counting users: %v", err)
	}
	if users != 1 {
		t.Fatalf("users = %d, want the second sign-in to have created nothing", users)
	}
}

// A failure partway through signup must leave no organisation behind: an org
// with no user is exactly the orphan the transaction exists to prevent.
func TestFailedSignupLeavesNoOrphanOrganization(t *testing.T) {
	s, ctx := newStore(t, "")

	// An unverified profile is refused before any write; a profile whose email
	// violates the column CHECK fails mid-transaction, after the organisation
	// insert. The second is the one that would orphan a row.
	p := oauth.Profile{Sub: "sub-1", Email: "NotLowercased@Example.com", EmailVerified: true}
	s2 := NewStore(s.pool, "", defaultOrgID, defaultOrgName, discardLogger())

	// Bypass normalizeEmail by writing through the same transaction shape the
	// store uses, with an address the constraint rejects.
	tx, err := s2.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, err = s2.create(ctx, tx, p, p.Email)
	if err == nil {
		t.Fatal("expected the email CHECK to reject the insert")
	}
	tx.Rollback(ctx)

	var orgs int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM organizations").Scan(&orgs); err != nil {
		t.Fatalf("counting organizations: %v", err)
	}
	if orgs != 0 {
		t.Fatalf("organizations = %d, want 0 after a failed signup", orgs)
	}
}

// The Detail field of a Postgres error carries every column of the rejected
// row. It must not survive into an error a caller might log.
func TestDBErrorOmitsFailingRow(t *testing.T) {
	s, ctx := newStore(t, "")

	_, err := s.pool.Exec(ctx,
		`INSERT INTO users (id, organization_id, email, google_sub) VALUES ($1, $2, $3, $4)`,
		"usr_x", "org_missing", "leaky@example.com", "sub-secret-value")
	if err == nil {
		t.Fatal("expected a foreign key violation")
	}

	wrapped := dbErr("creating user", err)
	for _, leak := range []string{"leaky@example.com", "sub-secret-value"} {
		if contains(wrapped.Error(), leak) {
			t.Fatalf("wrapped error leaked %q: %s", leak, wrapped)
		}
	}
	if !contains(wrapped.Error(), "users_organization_id_fkey") {
		t.Errorf("wrapped error lost the constraint name: %s", wrapped)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
