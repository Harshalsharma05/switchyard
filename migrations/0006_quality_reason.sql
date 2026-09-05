-- 0006_quality_reason: why Phase 9 sampled the request it scored.
--
-- quality_score alone says "this response scored 3"; it does not say whether
-- that 3 is a near-threshold cache hit (evidence the similarity threshold is
-- too permissive) or a downgraded-tier response (a candidate classifier
-- mislabel). Step 9.3's two feedback loops are exactly that distinction, so
-- the sampling reason is stored next to the score.
--
-- NULL until the async worker writes a score — the same not-applicable
-- convention quality_score itself uses. It is a categorical label derived from
-- routing and cache signals, never prompt content, matching routing_reason's
-- boundary in 0004.
--
-- Not added to requests_daily: it is a per-row category, not a sum, and the
-- rollup already does not carry quality (a known Phase 9 gap).

ALTER TABLE requests ADD COLUMN IF NOT EXISTS quality_sample_reason text;
