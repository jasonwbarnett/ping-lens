// Package grade converts rollup aggregates into plain-language quality
// labels, winner picks for multi-ISP mode, and a confidence band.
package grade

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/jasonwbarnett/ping-lens/internal/config"
	"github.com/jasonwbarnett/ping-lens/internal/db"
)

// Label is one of excellent / good / fair / poor / bad.
type Label string

const (
	Excellent Label = "excellent"
	Good      Label = "good"
	Fair      Label = "fair"
	Poor      Label = "poor"
	Bad       Label = "bad"
)

// Confidence is low / medium / high.
type Confidence string

const (
	ConfLow    Confidence = "low"
	ConfMedium Confidence = "medium"
	ConfHigh   Confidence = "high"
)

// SourceStats is the per-source aggregate derived from a window of rollups.
type SourceStats struct {
	Source        string
	ISPName       string
	Samples       int
	PingFailures  int
	DNSFailures   int
	LossPct       float64
	DNSFailurePct float64
	P50MS         float64
	P90MS         float64
	P95MS         float64
	P99MS         float64
	OutageCount   int
	LongestOutage time.Duration
	Days          float64
}

// PerTargetStats holds per-target aggregates for one source.
type PerTargetStats struct {
	Source  string
	ISPName string
	Target  string
	Group   string
	Samples int
	LossPct float64
	P50MS   float64
	P90MS   float64
	P95MS   float64
	P99MS   float64
	OutageCount int
}

// Summarize collapses a slice of rollup rows + outages into one stats record
// per source, plus per-target aggregates. Latency percentiles are
// sample-weighted averages of the bucket percentiles (good enough for the
// dashboard; the underlying raw samples are still available for drilldown).
func Summarize(rows []db.RollupRow, outages []db.OutageRow) (map[string]*SourceStats, []PerTargetStats) {
	bySource := map[string]*SourceStats{}
	perTargetKey := map[string]*PerTargetStats{}
	bucketSeen := map[string]map[time.Time]struct{}{}

	for _, r := range rows {
		ss, ok := bySource[r.Source]
		if !ok {
			ss = &SourceStats{Source: r.Source, ISPName: r.ISPName}
			bySource[r.Source] = ss
			bucketSeen[r.Source] = map[time.Time]struct{}{}
		}
		bucketSeen[r.Source][r.Bucket] = struct{}{}
		ss.Samples += r.Samples
		ss.PingFailures += r.PingFailures
		ss.DNSFailures += r.DNSFailures
		ss.P50MS += r.P50MS * float64(r.Samples)
		ss.P90MS += r.P90MS * float64(r.Samples)
		ss.P95MS += r.P95MS * float64(r.Samples)
		ss.P99MS += r.P99MS * float64(r.Samples)

		ptKey := r.Source + "|" + r.Target
		pt, ok := perTargetKey[ptKey]
		if !ok {
			pt = &PerTargetStats{Source: r.Source, ISPName: r.ISPName, Target: r.Target, Group: r.TargetGroup}
			perTargetKey[ptKey] = pt
		}
		pt.Samples += r.Samples
		pt.LossPct += r.LossPct * float64(r.Samples)
		pt.P50MS += r.P50MS * float64(r.Samples)
		pt.P90MS += r.P90MS * float64(r.Samples)
		pt.P95MS += r.P95MS * float64(r.Samples)
		pt.P99MS += r.P99MS * float64(r.Samples)
	}

	for _, ss := range bySource {
		if ss.Samples > 0 {
			ss.LossPct = 100.0 * float64(ss.PingFailures) / float64(ss.Samples)
			ss.DNSFailurePct = 100.0 * float64(ss.DNSFailures) / float64(ss.Samples)
			ss.P50MS /= float64(ss.Samples)
			ss.P90MS /= float64(ss.Samples)
			ss.P95MS /= float64(ss.Samples)
			ss.P99MS /= float64(ss.Samples)
		}
		buckets := bucketSeen[ss.Source]
		if n := len(buckets); n > 0 {
			ss.Days = (float64(n) * 30.0) / (60.0 * 24.0)
		}
	}
	for _, pt := range perTargetKey {
		if pt.Samples > 0 {
			pt.LossPct /= float64(pt.Samples)
			pt.P50MS /= float64(pt.Samples)
			pt.P90MS /= float64(pt.Samples)
			pt.P95MS /= float64(pt.Samples)
			pt.P99MS /= float64(pt.Samples)
		}
	}

	// Outage counts + longest by source / target.
	for _, o := range outages {
		ss, ok := bySource[o.Source]
		if !ok {
			ss = &SourceStats{Source: o.Source, ISPName: o.ISPName}
			bySource[o.Source] = ss
		}
		ss.OutageCount++
		if o.DurationSeconds != nil {
			d := time.Duration(*o.DurationSeconds) * time.Second
			if d > ss.LongestOutage {
				ss.LongestOutage = d
			}
		}
		for _, tgt := range o.AffectedTargets {
			key := o.Source + "|" + tgt
			if pt, ok := perTargetKey[key]; ok {
				pt.OutageCount++
			}
		}
	}

	pts := make([]PerTargetStats, 0, len(perTargetKey))
	for _, p := range perTargetKey {
		pts = append(pts, *p)
	}
	sort.Slice(pts, func(i, j int) bool {
		if pts[i].Source != pts[j].Source {
			return pts[i].Source < pts[j].Source
		}
		return pts[i].Target < pts[j].Target
	})
	return bySource, pts
}

// Grade returns the overall plain-language label for one source.
// Worst sub-grade wins.
func Grade(s *SourceStats, t config.Thresholds) Label {
	labels := []Label{
		bandLossPct(s.LossPct, t.LossPct),
		bandLatencyMS(s.P95MS, t.P95MS),
		bandLatencyMS(s.P99MS, t.P99MS),
		outageGrade(s.OutageCount, s.LongestOutage),
	}
	return worstLabel(labels)
}

// GradeReasons returns each sub-label so the UI can explain the overall grade.
func GradeReasons(s *SourceStats, t config.Thresholds) map[string]Label {
	return map[string]Label{
		"loss":            bandLossPct(s.LossPct, t.LossPct),
		"p95_latency":     bandLatencyMS(s.P95MS, t.P95MS),
		"p99_latency":     bandLatencyMS(s.P99MS, t.P99MS),
		"outages":         outageGrade(s.OutageCount, s.LongestOutage),
	}
}

func bandLossPct(v float64, b config.LabelBands) Label {
	switch {
	case v <= b.Excellent:
		return Excellent
	case v < b.Good:
		return Good
	case v < b.Fair:
		return Fair
	case v < b.Poor:
		return Poor
	default:
		return Bad
	}
}

func bandLatencyMS(v float64, b config.LabelBands) Label {
	switch {
	case v < b.Excellent:
		return Excellent
	case v < b.Good:
		return Good
	case v < b.Fair:
		return Fair
	case v < b.Poor:
		return Poor
	default:
		return Bad
	}
}

func outageGrade(count int, longest time.Duration) Label {
	switch {
	case count == 0:
		return Excellent
	case count == 1 && longest < 60*time.Second:
		return Good
	case count <= 3 && longest < 5*time.Minute:
		return Fair
	case count <= 10:
		return Poor
	default:
		return Bad
	}
}

var labelRank = map[Label]int{
	Excellent: 4, Good: 3, Fair: 2, Poor: 1, Bad: 0,
}

func worstLabel(ls []Label) Label {
	worst := Excellent
	for _, l := range ls {
		if labelRank[l] < labelRank[worst] {
			worst = l
		}
	}
	return worst
}

// Winner picks the better source for multi-ISP mode and explains why.
type Winner struct {
	Source     string
	ISPName    string
	Reasons    []string
	Confidence Confidence
	Tie        bool
}

// PickWinner compares two sources and returns the apparent winner. Tie if
// neither leads on the majority of axes.
func PickWinner(a, b *SourceStats, perTarget []PerTargetStats) Winner {
	if a == nil || b == nil {
		return Winner{Tie: true, Confidence: ConfLow}
	}
	// Score over four major axes.
	scoreA, scoreB := 0, 0
	reasons := []string{}

	cmpLower := func(av, bv float64, label string) {
		// minimum-meaningful-difference filter: 0.5% loss or 1ms latency
		if math.Abs(av-bv) < 0.0001 {
			return
		}
		if av < bv {
			scoreA++
			reasons = append(reasons, fmt.Sprintf("%s lower for %s (%.2f vs %.2f)", label, a.Source, av, bv))
		} else {
			scoreB++
			reasons = append(reasons, fmt.Sprintf("%s lower for %s (%.2f vs %.2f)", label, b.Source, bv, av))
		}
	}
	cmpLower(a.LossPct, b.LossPct, "Packet loss")
	cmpLower(a.P95MS, b.P95MS, "p95 latency")
	cmpLower(a.P99MS, b.P99MS, "p99 latency")
	cmpLower(float64(a.OutageCount), float64(b.OutageCount), "Outage count")

	// Per-target lead counts.
	aLeads, bLeads, totalTargets := perTargetLeads(a.Source, b.Source, perTarget)
	if aLeads > bLeads {
		scoreA++
		reasons = append(reasons, fmt.Sprintf("Lower p95 on %d of %d targets for %s", aLeads, totalTargets, a.Source))
	} else if bLeads > aLeads {
		scoreB++
		reasons = append(reasons, fmt.Sprintf("Lower p95 on %d of %d targets for %s", bLeads, totalTargets, b.Source))
	}

	w := Winner{}
	switch {
	case scoreA == scoreB:
		w.Tie = true
	case scoreA > scoreB:
		w.Source, w.ISPName = a.Source, a.ISPName
	default:
		w.Source, w.ISPName = b.Source, b.ISPName
	}
	w.Reasons = reasons
	w.Confidence = scoreConfidence(a, b, scoreA, scoreB)
	return w
}

func perTargetLeads(srcA, srcB string, rows []PerTargetStats) (int, int, int) {
	byTarget := map[string]map[string]PerTargetStats{}
	for _, r := range rows {
		if _, ok := byTarget[r.Target]; !ok {
			byTarget[r.Target] = map[string]PerTargetStats{}
		}
		byTarget[r.Target][r.Source] = r
	}
	aLeads, bLeads, total := 0, 0, 0
	for _, srcs := range byTarget {
		ra, okA := srcs[srcA]
		rb, okB := srcs[srcB]
		if !okA || !okB {
			continue
		}
		total++
		if ra.P95MS < rb.P95MS {
			aLeads++
		} else if rb.P95MS < ra.P95MS {
			bLeads++
		}
	}
	return aLeads, bLeads, total
}

func scoreConfidence(a, b *SourceStats, scoreA, scoreB int) Confidence {
	minDays := math.Min(a.Days, b.Days)
	minSamples := a.Samples
	if b.Samples < minSamples {
		minSamples = b.Samples
	}
	lead := scoreA - scoreB
	if lead < 0 {
		lead = -lead
	}
	switch {
	case minDays >= 7 && minSamples >= 100000 && lead >= 3:
		return ConfHigh
	case minDays >= 2 && minSamples >= 10000 && lead >= 2:
		return ConfMedium
	default:
		return ConfLow
	}
}

// SingleConfidence rates a single-ISP grade by data volume only.
func SingleConfidence(s *SourceStats) Confidence {
	switch {
	case s.Days >= 7 && s.Samples >= 100000:
		return ConfHigh
	case s.Days >= 2 && s.Samples >= 10000:
		return ConfMedium
	default:
		return ConfLow
	}
}
