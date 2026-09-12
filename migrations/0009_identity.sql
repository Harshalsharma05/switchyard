-- 0009_identity: dashboard users, the organisations they own, and their refresh
-- sessions (Multi-user, Phase 1). Humans only. Team API keys and the :8080 auth
-- path are untouched by this migration.

ALTER TABLE organizations
    ADD COLUMN IF NOT EXISTS owner_user_id text;

CREATE TABLE IF NOT EXISTS users (
    id              text        PRIMARY KEY,
    organization_id text        NOT NULL REFERENCES organizations (id),

    email             text      NOT NULL CHECK (email <> '' AND email = lower(email)),
    email_verified_at timestamptz,

    name       text NOT NULL DEFAULT '',
    avatar_url text NOT NULL DEFAULT '',

    -- Google's stable subject claim, never the email: an address can be
    -- reassigned to a different person, a sub cannot.
    google_sub text UNIQUE,

    -- Nullable and unwritten this phase -- Google is the only credential. It
    -- ships now so adding password sign-in later is a code change, not a
    -- migration.
    password_hash text,

    is_superadmin boolean NOT NULL DEFAULT false,

    created_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz,

    CONSTRAINT users_has_credential
        CHECK (google_sub IS NOT NULL OR password_hash IS NOT NULL)
);

CREATE UNIQUE INDEX IF NOT EXISTS users_email_idx ON users (email);

-- Added after users exists because the two tables reference each other. Signup
-- inserts the organisation, then the user, then sets the owner, in one
-- transaction.
ALTER TABLE organizations
    ADD CONSTRAINT organizations_owner_user_id_fkey
    FOREIGN KEY (owner_user_id) REFERENCES users (id);

CREATE TABLE IF NOT EXISTS sessions (
    id      text PRIMARY KEY,
    user_id text NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- SHA-256 of a 256-bit random token, same rule as teams.key_hash: the raw
    -- token is returned to the browser once and never stored.
    refresh_token_hash text NOT NULL UNIQUE
        CHECK (refresh_token_hash ~ '^[0-9a-f]{64}$'),

    issued_at  timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,

    -- Set when this token is rotated. Presenting a token that already has a
    -- successor is a replay, and revokes every session for the user.
    rotated_to text REFERENCES sessions (id),

    user_agent text NOT NULL DEFAULT '',
    ip         text NOT NULL DEFAULT ''
);

-- Revoking every session for one user -- logout everywhere, and what reuse
-- detection does. The refresh lookup is served by the unique index above.
CREATE INDEX IF NOT EXISTS sessions_user_id_idx ON sessions (user_id);
