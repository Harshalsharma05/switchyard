-- 0001_requests: the durable request-log table (Part 2, Phase 1).
--
-- One row per request that reached authentication — successes and the
-- deliberate 429/402/403/503 responses alike. Redis holds current state and
-- Prometheus holds aggregates; this table is the only queryable history.
--
-- Hard boundary: no prompt or response content, ever. Same rule as Part 1
-- tracing — this is metadata about requests, never their contents.

CREATE TABLE IF NOT EXISTS requests (
    -- The Part 1 request ID: a generated 128-bit hex string, or a caller's
    -- X-Request-ID (up to 64 chars, [A-Za-z0-9_-]). Not a uuid — inbound IDs
    -- are not guaranteed to be one.
    id               text        PRIMARY KEY,

    -- When the gateway received the request (requestMetrics.start).
    ts               timestamptz NOT NULL,

    team_id          text        NOT NULL,

    -- Null on rejections that happen before the body is decoded — an RPM-limit
    -- 429 is written by middleware ahead of model resolution.
    requested_model  text,

    -- Null when no provider was reached (validation error, budget 402,
    -- allowlist 403, every-candidate 503).
    served_model     text,
    provider         text,

    status_code      smallint    NOT NULL,

    input_tokens     integer     NOT NULL DEFAULT 0,
    output_tokens    integer     NOT NULL DEFAULT 0,

    -- Integer micro-dollars, the same int64 Part 1 carries end to end. Matches
    -- the X-Switchyard cost header exactly.
    cost_micros      bigint      NOT NULL DEFAULT 0,

    -- numeric, not float: Part 1 reports overhead to microsecond resolution and
    -- this table must not round it away. Millisecond unit, 3 decimal places.
    latency_ms       numeric(12,3) NOT NULL,
    overhead_ms      numeric(10,3) NOT NULL,

    fallback         boolean     NOT NULL DEFAULT false,

    -- Nullable until Phase 7 wires the semantic cache: null means "cache not
    -- consulted", distinct from a real false.
    cache_hit        boolean,

    -- Nullable until Phase 9's async quality worker scores sampled responses.
    quality_score    numeric(3,2),

    -- OpenTelemetry trace ID (32 hex chars). Null when the request was not
    -- sampled or tracing is disabled.
    trace_id         text
);

-- Indexes for the query patterns in Step 1.3, and no others. Status, model,
-- and cache-status filters ride on top of an already team- and time-bounded
-- scan; Postgres filters those cheaply without a dedicated index at 30-day
-- retention scale.

-- The dominant query: a team's own rows, newest first, cursor-paginated on
-- (ts, id). Covers the team filter and the ordering in one index.
CREATE INDEX IF NOT EXISTS requests_team_ts_idx
    ON requests (team_id, ts DESC, id DESC);

-- The admin cross-team list, same ordering without the team predicate.
CREATE INDEX IF NOT EXISTS requests_ts_idx
    ON requests (ts DESC, id DESC);

-- Provider-scoped cost and ops analytics (Phases 4 and 6) aggregate across all
-- teams by provider over a time window.
CREATE INDEX IF NOT EXISTS requests_provider_ts_idx
    ON requests (provider, ts DESC);
