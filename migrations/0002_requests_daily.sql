-- 0002_requests_daily: per-day rollup of request-log rows past retention.
--
-- Retention deletes detail rows after the configured window, but cost and
-- volume history must outlive them: "why did spend jump last quarter" is not
-- answerable from a table that only remembers 30 days. Each retention pass
-- aggregates the rows it is about to delete into this table, in the same
-- transaction, so every row is counted exactly once.
--
-- Counts and sums only. Percentiles are deliberately absent: p95 latency
-- cannot be re-derived from aggregates, and storing an average of percentiles
-- would be a number that looks real and is not.

CREATE TABLE IF NOT EXISTS requests_daily (
    day           date   NOT NULL,
    team_id       text   NOT NULL,

    -- Empty string, not NULL, for a request that never reached a provider:
    -- these are primary key columns, and NULL would split what should be one
    -- group into rows that never match on conflict.
    provider      text   NOT NULL,
    served_model  text   NOT NULL,

    requests      bigint NOT NULL,
    errors        bigint NOT NULL,
    input_tokens  bigint NOT NULL,
    output_tokens bigint NOT NULL,
    cost_micros   bigint NOT NULL,

    PRIMARY KEY (day, team_id, provider, served_model)
);

-- Cost trends scan by time, and by team within a time range.
CREATE INDEX IF NOT EXISTS requests_daily_day_idx ON requests_daily (day DESC);
CREATE INDEX IF NOT EXISTS requests_daily_team_day_idx ON requests_daily (team_id, day DESC);
