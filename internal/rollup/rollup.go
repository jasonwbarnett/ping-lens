// Package rollup accumulates raw samples into per-window aggregates that
// the flusher writes to ping_rollups. One Accumulator instance is shared
// by the buffer fan-out; Flush() drains and returns the closed buckets.
package rollup

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/jasonwbarnett/ping-lens/internal/sample"
)

// Row is one (bucket, source, target) row destined for ping_rollups.
type Row struct {
	Bucket          time.Time
	WindowSeconds   int
	Source          string
	ISPName         string
	Target          string
	TargetGroup     string
	Samples         int
	PingFailures    int
	DNSFailures     int
	LossPct         float64
	DNSFailurePct   float64
	P50MS           float64
	P90MS           float64
	P95MS           float64
	P99MS           float64
	MinMS           float64
	MaxMS           float64
	AvgMS           float64
}

// Accumulator buffers latencies in memory keyed by (bucket, source, target).
type Accumulator struct {
	window time.Duration

	mu      sync.Mutex
	buckets map[key]*agg
}

type key struct {
	bucket time.Time
	source string
	target string
}

type agg struct {
	ispName      string
	targetGroup  string
	samples      int
	pingFails    int
	dnsFails     int
	dnsObserved  int
	latencies    []float64
}

// New returns a fresh accumulator with the given rollup window.
func New(window time.Duration) *Accumulator {
	if window <= 0 {
		window = 30 * time.Minute
	}
	return &Accumulator{
		window:  window,
		buckets: map[key]*agg{},
	}
}

// Consume is the buffer.Sink contract.
func (a *Accumulator) Consume(s sample.Sample) {
	a.mu.Lock()
	defer a.mu.Unlock()
	bk := truncate(s.TS, a.window)
	k := key{bucket: bk, source: s.Source, target: s.Target}
	g, ok := a.buckets[k]
	if !ok {
		g = &agg{ispName: s.ISPName, targetGroup: s.TargetGroup}
		a.buckets[k] = g
	}
	g.samples++
	if s.DNSSuccess != nil {
		g.dnsObserved++
		if !*s.DNSSuccess {
			g.dnsFails++
		}
	}
	if !s.PingSuccess {
		g.pingFails++
		return
	}
	if s.LatencyMS != nil {
		g.latencies = append(g.latencies, *s.LatencyMS)
	}
}

// Flush returns all buckets with start <= cutoff and removes them from the
// accumulator. A bucket is "closed" once its window end is at or before
// cutoff. Pass time.Now() to flush every closed bucket.
func (a *Accumulator) Flush(cutoff time.Time) []Row {
	a.mu.Lock()
	defer a.mu.Unlock()
	var rows []Row
	for k, g := range a.buckets {
		if k.bucket.Add(a.window).After(cutoff) {
			continue
		}
		rows = append(rows, finalize(k, g, int(a.window.Seconds())))
		delete(a.buckets, k)
	}
	return rows
}

// FlushAll drains everything regardless of bucket end time. Use on shutdown.
func (a *Accumulator) FlushAll() []Row {
	a.mu.Lock()
	defer a.mu.Unlock()
	rows := make([]Row, 0, len(a.buckets))
	for k, g := range a.buckets {
		rows = append(rows, finalize(k, g, int(a.window.Seconds())))
		delete(a.buckets, k)
	}
	return rows
}

func finalize(k key, g *agg, windowSeconds int) Row {
	r := Row{
		Bucket:        k.bucket,
		WindowSeconds: windowSeconds,
		Source:        k.source,
		ISPName:       g.ispName,
		Target:        k.target,
		TargetGroup:   g.targetGroup,
		Samples:       g.samples,
		PingFailures:  g.pingFails,
		DNSFailures:   g.dnsFails,
	}
	if g.samples > 0 {
		r.LossPct = pct(g.pingFails, g.samples)
	}
	if g.dnsObserved > 0 {
		r.DNSFailurePct = pct(g.dnsFails, g.dnsObserved)
	}
	if len(g.latencies) > 0 {
		sort.Float64s(g.latencies)
		r.P50MS = percentile(g.latencies, 0.50)
		r.P90MS = percentile(g.latencies, 0.90)
		r.P95MS = percentile(g.latencies, 0.95)
		r.P99MS = percentile(g.latencies, 0.99)
		r.MinMS = g.latencies[0]
		r.MaxMS = g.latencies[len(g.latencies)-1]
		var sum float64
		for _, v := range g.latencies {
			sum += v
		}
		r.AvgMS = sum / float64(len(g.latencies))
	}
	return r
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	// linear interpolation, NIST primary definition
	rank := p * float64(len(sorted)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	frac := rank - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}

func pct(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return 100.0 * float64(num) / float64(den)
}

func truncate(t time.Time, w time.Duration) time.Time {
	return t.UTC().Truncate(w)
}
