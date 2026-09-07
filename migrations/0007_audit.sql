-- 0007_audit: the operator audit log (Part 2, Step 6.4 backend).
--
-- One row per privileged admin-API mutation: a team limit/budget PATCH, a
-- budget reset, an API key rotate or revoke, a config reload. The row is
-- written BEFORE the mutation is applied, on purpose — an unrecorded
-- credential change is worse than a failed one — so a row here proves an
-- action was attempted, not that it landed. See DECISIONS.md.
--
-- Records THAT a key was rotated for a team, never the key or its hash.
-- `before` and `after` hold field-level deltas only (rpm, tpm, budget,
-- key_source), never a secret and never request content.

CREATE TABLE IF NOT EXISTS audit_log (
    -- Generated 128-bit hex, like a request ID. Not a uuid — nothing here is.
    id             text        PRIMARY KEY,

    ts             timestamptz NOT NULL,

    -- The authenticated admin team that made the call, and its remote address.
    -- "unknown" only in a registry-less test build; a deployed gateway always
    -- has a real team here because requireAdmin resolved one.
    actor_team_id  text        NOT NULL,
    actor_addr     text        NOT NULL,

    -- "team.patch", "team.budget_reset", "key.rotate", "key.revoke",
    -- "config.reload".
    action         text        NOT NULL,

    -- The team the action targeted. NULL for config.reload, which is not
    -- team-scoped.
    target_team_id text,

    before         jsonb       NOT NULL DEFAULT '{}'::jsonb,
    after          jsonb       NOT NULL DEFAULT '{}'::jsonb
);

-- The only query Step 4's audit view runs: newest first, cursor-paginated on
-- (ts, id). Same keyset pattern as requests; there is no team filter yet, so
-- there is no second index to justify.
CREATE INDEX IF NOT EXISTS audit_log_ts_idx ON audit_log (ts DESC, id DESC);
