-- ping-lens initial schema.
--
-- Idempotent: this file is applied on startup. CREATE TABLE IF NOT EXISTS
-- everywhere so re-runs are no-ops.

CREATE TABLE IF NOT EXISTS ping_samples (
    ts            timestamptz       NOT NULL,
    source        text              NOT NULL,
    isp_name      text,
    target        text              NOT NULL,
    target_type   text              NOT NULL,
    target_group  text,
    target_ip     inet,
    dns_success   boolean,
    dns_lookup_ms double precision,
    ping_success  boolean           NOT NULL,
    latency_ms    double precision,
    error         text
);

CREATE INDEX IF NOT EXISTS ping_samples_source_target_ts_idx
    ON ping_samples (source, target, ts DESC);

CREATE INDEX IF NOT EXISTS ping_samples_ts_idx
    ON ping_samples (ts DESC);

CREATE TABLE IF NOT EXISTS ping_rollups (
    bucket           timestamptz      NOT NULL,
    window_seconds   integer          NOT NULL,
    source           text             NOT NULL,
    isp_name         text,
    target           text             NOT NULL,
    target_group     text,
    samples          integer          NOT NULL,
    ping_failures    integer          NOT NULL,
    dns_failures     integer          NOT NULL,
    loss_pct         double precision NOT NULL,
    dns_failure_pct  double precision,
    p50_ms           double precision,
    p90_ms           double precision,
    p95_ms           double precision,
    p99_ms           double precision,
    min_ms           double precision,
    max_ms           double precision,
    avg_ms           double precision,
    PRIMARY KEY (bucket, window_seconds, source, target)
);

CREATE INDEX IF NOT EXISTS ping_rollups_source_bucket_idx
    ON ping_rollups (source, bucket DESC);

CREATE TABLE IF NOT EXISTS outage_events (
    id                bigserial PRIMARY KEY,
    source            text        NOT NULL,
    isp_name          text,
    started_at        timestamptz NOT NULL,
    ended_at          timestamptz,
    duration_seconds  integer,
    outage_type       text        NOT NULL,
    affected_targets  text[]      NOT NULL,
    notes             text
);

CREATE INDEX IF NOT EXISTS outage_events_source_started_idx
    ON outage_events (source, started_at DESC);

CREATE TABLE IF NOT EXISTS loaded_latency_tests (
    id              bigserial PRIMARY KEY,
    ts              timestamptz NOT NULL,
    source          text        NOT NULL,
    isp_name        text,
    test_type       text        NOT NULL,
    idle_p50_ms     double precision,
    idle_p95_ms     double precision,
    loaded_p50_ms   double precision,
    loaded_p95_ms   double precision,
    loaded_p99_ms   double precision,
    notes           text
);
