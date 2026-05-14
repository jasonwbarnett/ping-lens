package rollup

import (
	"math"
	"testing"
	"time"

	"github.com/jasonwbarnett/ping-lens/internal/sample"
)

func TestAccumulator_Percentiles(t *testing.T) {
	acc := New(30 * time.Minute)
	base := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 100; i++ {
		ms := float64(i)
		acc.Consume(sample.Sample{
			TS:          base.Add(time.Duration(i) * time.Second),
			Source:      "pi", Target: "1.1.1.1",
			PingSuccess: true,
			LatencyMS:   &ms,
		})
	}
	rows := acc.FlushAll()
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.Samples != 100 {
		t.Fatalf("samples = %d", r.Samples)
	}
	if math.Abs(r.P50MS-50.5) > 0.001 {
		t.Errorf("p50 = %v want ~50.5", r.P50MS)
	}
	if math.Abs(r.P95MS-95.05) > 0.001 {
		t.Errorf("p95 = %v want ~95.05", r.P95MS)
	}
	if math.Abs(r.P99MS-99.01) > 0.001 {
		t.Errorf("p99 = %v want ~99.01", r.P99MS)
	}
	if r.MinMS != 1 || r.MaxMS != 100 {
		t.Errorf("min/max = %v/%v", r.MinMS, r.MaxMS)
	}
}

func TestAccumulator_LossPct(t *testing.T) {
	acc := New(30 * time.Minute)
	base := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	// 90 success + 10 failure for one target
	ms := 5.0
	for i := 0; i < 90; i++ {
		acc.Consume(sample.Sample{
			TS:          base.Add(time.Duration(i) * time.Second),
			Source:      "pi", Target: "1.1.1.1",
			PingSuccess: true, LatencyMS: &ms,
		})
	}
	for i := 0; i < 10; i++ {
		acc.Consume(sample.Sample{
			TS:          base.Add(time.Duration(i+90) * time.Second),
			Source:      "pi", Target: "1.1.1.1",
			PingSuccess: false,
		})
	}
	rows := acc.FlushAll()
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].LossPct != 10.0 {
		t.Errorf("loss = %v want 10", rows[0].LossPct)
	}
	if rows[0].PingFailures != 10 {
		t.Errorf("failures = %v", rows[0].PingFailures)
	}
}

func TestAccumulator_FlushClosedOnly(t *testing.T) {
	acc := New(30 * time.Minute)
	// 12:15 is mid-bucket for window 12:00-12:30. The earlier 11:55 sample
	// lives in the closed 11:30-12:00 bucket.
	now := time.Date(2026, 5, 14, 12, 15, 0, 0, time.UTC)
	closedSample := time.Date(2026, 5, 14, 11, 55, 0, 0, time.UTC)
	openSample := time.Date(2026, 5, 14, 12, 10, 0, 0, time.UTC)
	ms := 5.0
	acc.Consume(sample.Sample{TS: closedSample, Source: "pi", Target: "x", PingSuccess: true, LatencyMS: &ms})
	acc.Consume(sample.Sample{TS: openSample, Source: "pi", Target: "x", PingSuccess: true, LatencyMS: &ms})

	rows := acc.Flush(now)
	if len(rows) != 1 {
		t.Fatalf("want 1 closed row, got %d", len(rows))
	}
	if rows[0].Bucket.Minute() != 30 || rows[0].Bucket.Hour() != 11 {
		t.Errorf("flushed wrong bucket: %v", rows[0].Bucket)
	}
}
