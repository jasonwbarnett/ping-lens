// Package outage classifies failure patterns across the configured target set
// and tracks consecutive failures + outage events in memory.
//
// One Tracker instance lives per agent. The probe loop calls Observe() with
// each tick's batch of samples. The tracker:
//
//   - bumps per-target consecutive-failure counters
//   - opens / closes outage events when failures span targets or persist
//   - emits closed events to the supplied EventSink for DB flushing
//
// The classifier is intentionally simple. Per PRD:
//
//	1. one target down              -> single_target_issue
//	2. several targets down         -> multi_target_issue
//	3. all public targets down + LAN up -> full_isp_outage
//	4. hostname DNS fails + IP works -> dns_issue
//	5. LAN gateway fails             -> local_gateway_issue
package outage

import (
	"sort"
	"sync"
	"time"

	"github.com/jasonwbarnett/ping-lens/internal/sample"
)

const (
	TypeSingleTarget = "single_target_issue"
	TypeMultiTarget  = "multi_target_issue"
	TypeFullISP      = "full_isp_outage"
	TypeDNS          = "dns_issue"
	TypeLocalGateway = "local_gateway_issue"
	TypeUnknown      = "unknown"
)

// Event is a closed outage event ready for the DB.
type Event struct {
	Source          string
	ISPName         string
	StartedAt       time.Time
	EndedAt         time.Time
	DurationSeconds int
	Type            string
	AffectedTargets []string
	Notes           string
}

// EventSink receives finalized outage events.
type EventSink interface {
	RecordOutage(Event)
}

// Tracker accumulates per-target consecutive-failure state and surfaces
// outage events when a tick's failure pattern is classified.
type Tracker struct {
	source        string
	ispName       string
	localGateway  string
	minStreak     int // min consecutive-failure ticks before a target counts as failing

	mu       sync.Mutex
	streaks  map[string]int       // target -> consecutive failures
	open     *Event               // current open event (if any)
	openLast time.Time            // last tick where the open event was still failing
	sink     EventSink
}

// NewTracker constructs a Tracker. minStreak is the number of consecutive
// failed ticks required before a target is considered "failing" for outage
// classification — single dropped packets below this threshold are
// reflected in the streak counter and the rollup loss% but do not open an
// outage event. A value <= 1 disables the threshold (every failed tick
// counts).
func NewTracker(source, ispName, localGateway string, minStreak int, sink EventSink) *Tracker {
	if minStreak < 1 {
		minStreak = 1
	}
	return &Tracker{
		source:       source,
		ispName:      ispName,
		localGateway: localGateway,
		minStreak:    minStreak,
		streaks:      map[string]int{},
		sink:         sink,
	}
}

// Streak returns the current consecutive-failure count for a target.
func (t *Tracker) Streak(target string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.streaks[target]
}

// Observe processes a single tick worth of samples (one per probe target).
// The caller passes the full set so multi-target patterns can be classified.
func (t *Tracker) Observe(tickAt time.Time, samples []sample.Sample) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(samples) == 0 {
		return
	}

	// Update per-target streaks.
	failed := make([]string, 0, len(samples))
	hostFailed := 0
	hostTotal := 0
	ipFailed := 0
	ipTotal := 0
	dnsHostnameFails := 0
	gatewayUp := true
	gatewayObserved := false

	for _, s := range samples {
		isFailing := false
		if !s.PingSuccess {
			t.streaks[s.Target] = t.streaks[s.Target] + 1
			if t.streaks[s.Target] >= t.minStreak {
				isFailing = true
				failed = append(failed, s.Target)
			}
		} else {
			t.streaks[s.Target] = 0
		}
		switch s.TargetType {
		case "ip":
			ipTotal++
			if isFailing {
				ipFailed++
			}
		case "hostname":
			hostTotal++
			if isFailing {
				hostFailed++
			}
			if s.DNSSuccess != nil && !*s.DNSSuccess {
				dnsHostnameFails++
			}
		}
		if t.localGateway != "" && (s.Target == "local_gateway" || s.TargetIP == t.localGateway) {
			gatewayObserved = true
			gatewayUp = !isFailing
		}
	}
	sort.Strings(failed)

	publicTotal := hostTotal + ipTotal
	publicFailed := hostFailed + ipFailed

	var category string
	switch {
	case len(failed) == 0:
		category = ""
	case gatewayObserved && !gatewayUp:
		category = TypeLocalGateway
	case hostTotal > 0 && dnsHostnameFails == hostTotal && ipFailed == 0:
		category = TypeDNS
	case publicTotal > 0 && publicFailed == publicTotal && (!gatewayObserved || gatewayUp):
		category = TypeFullISP
	case len(failed) == 1:
		category = TypeSingleTarget
	case len(failed) > 1:
		category = TypeMultiTarget
	default:
		category = TypeUnknown
	}

	if category == "" {
		// no failures this tick — close any open event
		t.closeOpenLocked(tickAt)
		return
	}

	if t.open == nil {
		t.open = &Event{
			Source:          t.source,
			ISPName:         t.ispName,
			StartedAt:       tickAt,
			Type:            category,
			AffectedTargets: failed,
		}
		t.openLast = tickAt
		return
	}

	// Extend the open event. Upgrade type if the new tick is "worse".
	t.open.AffectedTargets = unionSorted(t.open.AffectedTargets, failed)
	t.open.Type = mergeType(t.open.Type, category)
	t.openLast = tickAt
}

// CloseAll forces the current open event to close as of `at`. Useful on
// shutdown so the in-flight event reaches the sink.
func (t *Tracker) CloseAll(at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closeOpenLocked(at)
}

func (t *Tracker) closeOpenLocked(at time.Time) {
	if t.open == nil {
		return
	}
	end := t.openLast
	if end.Before(t.open.StartedAt) {
		end = t.open.StartedAt
	}
	t.open.EndedAt = end
	t.open.DurationSeconds = int(end.Sub(t.open.StartedAt).Seconds())
	if t.sink != nil {
		t.sink.RecordOutage(*t.open)
	}
	t.open = nil
	t.openLast = time.Time{}
}

func unionSorted(a, b []string) []string {
	seen := map[string]struct{}{}
	for _, x := range a {
		seen[x] = struct{}{}
	}
	for _, x := range b {
		seen[x] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for x := range seen {
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}

// mergeType picks the more severe outage classification when an event spans
// multiple ticks with different patterns. Severity order (largest blast
// radius wins):
//
//	single_target < local_gateway < dns < multi_target < full_isp
func mergeType(a, b string) string {
	rank := map[string]int{
		TypeFullISP:      5,
		TypeMultiTarget:  4,
		TypeDNS:          3,
		TypeLocalGateway: 2,
		TypeSingleTarget: 1,
		TypeUnknown:      0,
	}
	if rank[b] > rank[a] {
		return b
	}
	return a
}
