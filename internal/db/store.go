// Package db owns the Postgres connection lifecycle and bulk-insert paths
// used by the flusher. Reads consumed by the dashboard live in
// internal/db/queries.go.
package db

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jasonwbarnett/ping-lens/internal/outage"
	"github.com/jasonwbarnett/ping-lens/internal/rollup"
	"github.com/jasonwbarnett/ping-lens/internal/sample"
)

//go:embed schema.sql
var embeddedSchema string

// Store is a thin wrapper around a pgxpool. Open one per process.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to Postgres and applies the schema.
func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse db url: %w", err)
	}
	cfg.MaxConns = 4
	cfg.MaxConnLifetime = 30 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Pool exposes the underlying pgxpool for read queries.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases all pooled connections.
func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

// Migrate applies the embedded schema. Idempotent.
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, embeddedSchema)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// InsertSamples bulk-loads raw samples via COPY.
func (s *Store) InsertSamples(ctx context.Context, rows []sample.Sample) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	src := pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
		r := rows[i]
		return []any{
			r.TS,
			r.Source,
			nilIfEmpty(r.ISPName),
			r.Target,
			r.TargetType,
			nilIfEmpty(r.TargetGroup),
			nilIfEmpty(r.TargetIP),
			boolPtrToAny(r.DNSSuccess),
			float64PtrToAny(r.DNSLookupMS),
			r.PingSuccess,
			float64PtrToAny(r.LatencyMS),
			nilIfEmpty(r.Error),
		}, nil
	})
	cols := []string{
		"ts", "source", "isp_name", "target", "target_type", "target_group",
		"target_ip", "dns_success", "dns_lookup_ms", "ping_success",
		"latency_ms", "error",
	}
	return s.pool.CopyFrom(ctx, pgx.Identifier{"ping_samples"}, cols, src)
}

// UpsertRollups writes rollup rows. ON CONFLICT keeps the latest values so a
// re-run after a partial flush is safe.
func (s *Store) UpsertRollups(ctx context.Context, rows []rollup.Row) error {
	if len(rows) == 0 {
		return nil
	}
	const stmt = `
INSERT INTO ping_rollups
  (bucket, window_seconds, source, isp_name, target, target_group,
   samples, ping_failures, dns_failures, loss_pct, dns_failure_pct,
   p50_ms, p90_ms, p95_ms, p99_ms, min_ms, max_ms, avg_ms)
VALUES
  ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
ON CONFLICT (bucket, window_seconds, source, target) DO UPDATE SET
  isp_name = EXCLUDED.isp_name,
  target_group = EXCLUDED.target_group,
  samples = EXCLUDED.samples,
  ping_failures = EXCLUDED.ping_failures,
  dns_failures = EXCLUDED.dns_failures,
  loss_pct = EXCLUDED.loss_pct,
  dns_failure_pct = EXCLUDED.dns_failure_pct,
  p50_ms = EXCLUDED.p50_ms,
  p90_ms = EXCLUDED.p90_ms,
  p95_ms = EXCLUDED.p95_ms,
  p99_ms = EXCLUDED.p99_ms,
  min_ms = EXCLUDED.min_ms,
  max_ms = EXCLUDED.max_ms,
  avg_ms = EXCLUDED.avg_ms
`
	batch := &pgx.Batch{}
	for _, r := range rows {
		batch.Queue(stmt,
			r.Bucket, r.WindowSeconds, r.Source, nilIfEmpty(r.ISPName),
			r.Target, nilIfEmpty(r.TargetGroup),
			r.Samples, r.PingFailures, r.DNSFailures,
			r.LossPct, r.DNSFailurePct,
			r.P50MS, r.P90MS, r.P95MS, r.P99MS, r.MinMS, r.MaxMS, r.AvgMS,
		)
	}
	br := s.pool.SendBatch(ctx, batch)
	defer br.Close()
	for range rows {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("upsert rollup: %w", err)
		}
	}
	return nil
}

// InsertOutages appends finalized outage events.
func (s *Store) InsertOutages(ctx context.Context, events []outage.Event) error {
	if len(events) == 0 {
		return nil
	}
	src := pgx.CopyFromSlice(len(events), func(i int) ([]any, error) {
		e := events[i]
		var ended any
		var dur any
		if !e.EndedAt.IsZero() {
			ended = e.EndedAt
			dur = e.DurationSeconds
		}
		return []any{
			e.Source,
			nilIfEmpty(e.ISPName),
			e.StartedAt,
			ended,
			dur,
			e.Type,
			e.AffectedTargets,
			nilIfEmpty(e.Notes),
		}, nil
	})
	cols := []string{
		"source", "isp_name", "started_at", "ended_at", "duration_seconds",
		"outage_type", "affected_targets", "notes",
	}
	_, err := s.pool.CopyFrom(ctx, pgx.Identifier{"outage_events"}, cols, src)
	return err
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolPtrToAny(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

func float64PtrToAny(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}
