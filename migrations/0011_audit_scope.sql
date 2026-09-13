-- 0011_audit_scope: attribute audit entries to a user and an organisation
-- (Multi-user, Step 2.5).
--
-- Two changes, both consequences of the admin port becoming session-only in
-- Step 1.5.
--
-- First, the actor. The column was named actor_team_id because a team API key
-- was once what authenticated an admin call. Since Step 1.5 no team key reaches
-- :9090 at all and the handlers have been writing a user ID into it, so the name
-- has been lying about its contents. Renaming it is the honest fix. Rows written
-- before Step 1.5 still hold a team ID; nothing can retroactively resolve those
-- to a person, and they are left as they are rather than guessed at.
--
-- Second, the scope. An audit log that every tenant can read is a disclosure of
-- who else exists, so entries now carry the organisation they belong to and
-- whether a superadmin took the action.
ALTER TABLE audit_log
    RENAME COLUMN actor_team_id TO actor_user_id;

-- Nullable on purpose: an entry written before this migration cannot be
-- attributed to an organisation, and inventing one would be worse than
-- admitting it. A NULL never matches an organisation filter, so those rows are
-- visible to a superadmin only -- the fail-closed direction.
ALTER TABLE audit_log
    ADD COLUMN IF NOT EXISTS organization_id text;

-- Whether the action was taken by a superadmin. Those are operator actions --
-- cross-organisation reads, config reloads, chaos injection -- and they are
-- visible to superadmins only, so this column is half of the read filter rather
-- than decoration. Defaulting existing rows to false is safe: they also have a
-- NULL organization_id, so the filter excludes them regardless.
ALTER TABLE audit_log
    ADD COLUMN IF NOT EXISTS superadmin boolean NOT NULL DEFAULT false;

-- The scoped listing's index. The unscoped superadmin read still uses
-- audit_log_ts_idx from 0007; this one serves the org-filtered read that every
-- normal user now issues, in the same (ts DESC, id DESC) keyset order.
CREATE INDEX IF NOT EXISTS audit_log_org_ts_idx
    ON audit_log (organization_id, ts DESC, id DESC);
