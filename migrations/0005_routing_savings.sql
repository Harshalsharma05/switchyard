-- 0005_routing_savings: what cost-aware routing saved, per request.
--
-- For a routed request: what the top tier's declared head would have cost for
-- this request's real token usage, minus what the served model actually cost.
-- Zero when routing chose the top tier anyway — routing ran and saved nothing,
-- which is a different fact from routing never having run.
--
-- NULL for a request whose caller named a model, matching routing_tier and
-- routing_reason in 0004 and the not-applicable convention cache_hit uses.
--
-- The baseline is the tier's *declared* head, not whatever health would have
-- picked at the time: a health-dependent baseline makes the headline number
-- move with unrelated provider outages, and a savings figure that changes
-- because Groq had a bad afternoon cannot be compared week to week.

ALTER TABLE requests ADD COLUMN IF NOT EXISTS routing_savings_micros bigint;

-- The rollup carries a summed copy so retention does not lose the figure.
-- NOT NULL with a zero default, same as fallback_cost_delta_micros: an
-- aggregate row always has a number, even when none of its requests routed.
ALTER TABLE requests_daily
    ADD COLUMN IF NOT EXISTS routing_savings_micros bigint NOT NULL DEFAULT 0;
