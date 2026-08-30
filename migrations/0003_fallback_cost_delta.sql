-- 0003_fallback_cost_delta: the per-request cost impact of a fallback.
--
-- Part 1 logs an estimated delta to slog on every fallback transition but
-- keeps nothing durable. Step 6.3's "cost shifted by fallback" panel needs it
-- queryable and attributable per team. This column holds, for a request that
-- a fallback served, the signed difference between what the served model
-- actually cost for this request's real token usage and what the originally
-- requested model would have cost for the same usage. Negative means the
-- fallback was the cheaper option.
--
-- NULL for every request that did not fall back — the same "not applicable,
-- distinct from zero" convention cache_hit and quality_score already use.

ALTER TABLE requests ADD COLUMN IF NOT EXISTS fallback_cost_delta_micros bigint;

-- The daily rollup carries a summed copy so the number is not lost when
-- retention deletes the detail rows. NOT NULL with a zero default: an
-- aggregate row always has a figure even when none of its requests fell back.
ALTER TABLE requests_daily
    ADD COLUMN IF NOT EXISTS fallback_cost_delta_micros bigint NOT NULL DEFAULT 0;
