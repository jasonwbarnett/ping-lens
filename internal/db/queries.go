package db

import (
	"context"
	"time"
)

// RollupRow is a row from ping_rollups used by the dashboard.
type RollupRow struct {
	Bucket        time.Time
	WindowSeconds int
	Source        string
	ISPName       string
	Target        string
	TargetGroup   string
	Samples       int
	PingFailures  int
	DNSFailures   int
	LossPct       float64
	DNSFailurePct float64
	P50MS         float64
	P90MS         float64
	P95MS         float64
	P99MS         float64
	MinMS         float64
	MaxMS         float64
	AvgMS         float64
}

// OutageRow is one outage event for the dashboard.
type OutageRow struct {
	ID              int64
	Source          string
	ISPName         string
	StartedAt       time.Time
	EndedAt         *time.Time
	DurationSeconds *int
	OutageType      string
	AffectedTargets []string
	Notes           string
}

// SourceSummary describes one configured source's metadata as observed in
// the rollup table — used to drive single-vs-multi mode detection.
type SourceSummary struct {
	Source  string
	ISPName string
}

// Sources returns every distinct source that has rollups in the window.
func (s *Store) Sources(ctx context.Context, since time.Time) ([]SourceSummary, error) {
	rows, err := s.pool.Query(ctx, `
SELECT source, COALESCE(MAX(isp_name), '')
FROM ping_rollups
WHERE bucket >= $1
GROUP BY source
ORDER BY source
`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourceSummary
	for rows.Next() {
		var s SourceSummary
		if err := rows.Scan(&s.Source, &s.ISPName); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Rollups returns rollup rows filtered by time window, optionally by source.
func (s *Store) Rollups(ctx context.Context, since, until time.Time, source string) ([]RollupRow, error) {
	q := `
SELECT bucket, window_seconds, source, COALESCE(isp_name,''), target,
       COALESCE(target_group,''), samples, ping_failures, dns_failures,
       loss_pct, COALESCE(dns_failure_pct,0),
       COALESCE(p50_ms,0), COALESCE(p90_ms,0), COALESCE(p95_ms,0),
       COALESCE(p99_ms,0), COALESCE(min_ms,0), COALESCE(max_ms,0),
       COALESCE(avg_ms,0)
FROM ping_rollups
WHERE bucket >= $1 AND bucket < $2
`
	args := []any{since, until}
	if source != "" {
		q += " AND source = $3"
		args = append(args, source)
	}
	q += " ORDER BY bucket ASC, source, target"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RollupRow
	for rows.Next() {
		var r RollupRow
		if err := rows.Scan(
			&r.Bucket, &r.WindowSeconds, &r.Source, &r.ISPName, &r.Target,
			&r.TargetGroup, &r.Samples, &r.PingFailures, &r.DNSFailures,
			&r.LossPct, &r.DNSFailurePct,
			&r.P50MS, &r.P90MS, &r.P95MS, &r.P99MS,
			&r.MinMS, &r.MaxMS, &r.AvgMS,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Outages returns outage events overlapping the window.
func (s *Store) Outages(ctx context.Context, since, until time.Time, source string) ([]OutageRow, error) {
	q := `
SELECT id, source, COALESCE(isp_name,''), started_at, ended_at,
       duration_seconds, outage_type, affected_targets, COALESCE(notes,'')
FROM outage_events
WHERE started_at < $2 AND (ended_at IS NULL OR ended_at >= $1)
`
	args := []any{since, until}
	if source != "" {
		q += " AND source = $3"
		args = append(args, source)
	}
	q += " ORDER BY started_at ASC"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutageRow
	for rows.Next() {
		var o OutageRow
		var ended *time.Time
		var dur *int
		if err := rows.Scan(&o.ID, &o.Source, &o.ISPName, &o.StartedAt,
			&ended, &dur, &o.OutageType, &o.AffectedTargets, &o.Notes); err != nil {
			return nil, err
		}
		o.EndedAt = ended
		o.DurationSeconds = dur
		out = append(out, o)
	}
	return out, rows.Err()
}

