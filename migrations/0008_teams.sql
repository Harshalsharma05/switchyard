-- 0008_teams: tenant storage (Tier 1, Phase 2).
--
-- Moves teams out of configs/teams.yaml and into Postgres so a key rotation,
-- a limit edit, or a newly created team survives a restart and a
-- POST /admin/reload. Only teams move. providers.yaml stays a file — provider
-- config is deployment configuration with no runtime mutation path.
--
-- Same rule as the YAML file and as Part 2: never a plaintext key, only its
-- SHA-256 digest.

-- Exists so teams.organization_id can be a real foreign key today rather than
-- a migration later. No budget, no limits, no settings — all of that is still
-- a team concern, and giving an organisation any of it here would be building
-- multi-org behaviour a phase early.
CREATE TABLE IF NOT EXISTS organizations (
    -- Text, not uuid, for the same reason every other id in this schema is
    -- text: request IDs, team IDs, and audit IDs are all human-readable
    -- strings and a uuid here would be the odd one out. Step 2.2's import
    -- seeds the single default organisation; this migration only shapes it.
    id         text        PRIMARY KEY,
    name       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS teams (
    -- Matches requests.team_id and audit_log.target_team_id, which are plain
    -- text columns with no foreign key to here. That is deliberate: the
    -- request-log writer batches inserts and drops rows rather than ever
    -- pushing back on a request, so a constraint violation that killed a whole
    -- batch would be a new way to lose logs for no gain. Soft delete below is
    -- what actually keeps those rows from being orphaned.
    id              text        PRIMARY KEY,

    organization_id text        NOT NULL REFERENCES organizations (id),
    name            text        NOT NULL,

    -- CHECK rather than a Postgres enum type: adding a third priority later is
    -- an ALTER of this constraint inside a normal transaction, where ALTER TYPE
    -- ... ADD VALUE has historically been the awkward one. Values mirror
    -- auth.Priority.
    priority        text        NOT NULL CHECK (priority IN ('realtime', 'batch')),

    rpm             integer     NOT NULL CHECK (rpm > 0),
    tpm             integer     NOT NULL CHECK (tpm > 0),

    -- Integer micro-dollars, the same int64 Part 1 carries end to end and the
    -- same unit as requests.cost_micros. No float enters the money path.
    monthly_budget_micros bigint NOT NULL CHECK (monthly_budget_micros > 0),

    -- JSONB rather than a join table. Both lists are read whole and replaced
    -- whole, and nothing ever asks the reverse question ("which teams may use
    -- gemini?"), so a join table would add writes and a join to the hot auth
    -- query and buy nothing. The CHECKs restate config.LoadTeams's rule that a
    -- team with an empty allowlist is a misconfiguration, not a lockout.
    allowed_providers jsonb     NOT NULL
        CHECK (jsonb_typeof(allowed_providers) = 'array' AND jsonb_array_length(allowed_providers) > 0),
    allowed_models    jsonb     NOT NULL
        CHECK (jsonb_typeof(allowed_models) = 'array' AND jsonb_array_length(allowed_models) > 0),

    is_admin        boolean     NOT NULL DEFAULT false,

    -- NULL means revoked — no credential resolves to this team until it is
    -- rotated a new one. The same state auth.Registry.RevokeKey produces in
    -- memory today. The CHECK is the isSHA256Hex validation from
    -- internal/config, enforced by the database so a truncated hash or an
    -- accidentally-pasted plaintext key fails the write instead of becoming a
    -- permanent silent 401.
    key_hash        text        CHECK (key_hash ~ '^[0-9a-f]{64}$'),

    -- Where the current key came from. 'created' is for a key minted when the
    -- team itself is created via POST /admin/teams (Step 2.5); the other three
    -- are the existing auth.KeySource* constants.
    key_source      text        NOT NULL
        CHECK (key_source IN ('config', 'rotated', 'revoked', 'created')),

    -- Display-only "sk-…a097", empty for a key this gateway never saw in
    -- plaintext. Never enough to authenticate with.
    key_masked      text        NOT NULL DEFAULT '',
    key_created_at  timestamptz,

    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    -- Soft delete. Request-log and audit rows reference team IDs as free text,
    -- so a hard delete would strand history that the Usage & Cost screens read.
    -- Deleting a team also clears key_hash to NULL, which is what stops a
    -- deleted team's credential authenticating and what keeps the unique index
    -- below from having to care about deleted rows.
    deleted_at      timestamptz
);

-- The auth lookup, and the uniqueness rule behind it, in one index.
--
-- Unique because two teams cannot share a key: every rate limit and budget
-- check above this layer assumes a key identifies exactly one team, and a
-- collision would make two teams' requests indistinguishable everywhere.
-- Partial because a revoked or deleted team has key_hash NULL and several such
-- rows must be able to coexist.
--
-- This is the hot path — one equality lookup per request, behind Step 2.3's
-- cache — so it is the only index this migration adds. Listing teams by
-- organisation is an admin-screen query over a handful of rows and does not
-- earn one.
CREATE UNIQUE INDEX IF NOT EXISTS teams_key_hash_idx
    ON teams (key_hash) WHERE key_hash IS NOT NULL;
