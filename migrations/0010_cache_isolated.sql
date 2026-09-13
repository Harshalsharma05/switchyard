-- 0010_cache_isolated: the semantic cache's per-team sharing opt-out
-- (Multi-user, Step 2.3).
--
-- The cache is scoped to an organisation, not a team: one person's several
-- projects asking the same question should not pay for the same answer twice,
-- and an organisation is the outer boundary no entry ever crosses. This column
-- is the opt-out for a team that does not want its answers pooled with its
-- siblings, trading that hit rate for a cache of its own.
--
-- It can only narrow sharing, never widen it. There is no setting that lets an
-- entry cross organisations.
--
-- Defaults false, which is org-shared: the existing single-organisation
-- deployment keeps behaving as it does today.

ALTER TABLE teams
    ADD COLUMN IF NOT EXISTS cache_isolated boolean NOT NULL DEFAULT false;
