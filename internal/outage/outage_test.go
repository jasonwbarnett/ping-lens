package outage

import (
	"testing"
	"time"

	"github.com/jasonwbarnett/ping-lens/internal/sample"
)

type recorder struct{ events []Event }

func (r *recorder) RecordOutage(e Event) { r.events = append(r.events, e) }

func tickAt(t time.Time, success ...bool) []sample.Sample {
	out := make([]sample.Sample, len(success))
	for i, s := range success {
		out[i] = sample.Sample{
			TS: t, Source: "pi", Target: "t" + string(rune('A'+i)),
			TargetType: "ip", PingSuccess: s,
		}
	}
	return out
}

func TestTracker_SingleTargetIssue(t *testing.T) {
	r := &recorder{}
	tr := NewTracker("pi", "", "", 1, r)
	base := time.Now()
	tr.Observe(base, tickAt(base, false, true, true))
	tr.Observe(base.Add(5*time.Second), tickAt(base.Add(5*time.Second), true, true, true))
	if len(r.events) != 1 {
		t.Fatalf("want 1 event, got %d", len(r.events))
	}
	if r.events[0].Type != TypeSingleTarget {
		t.Errorf("type = %s want %s", r.events[0].Type, TypeSingleTarget)
	}
}

func TestTracker_FullISPThenRecovery(t *testing.T) {
	r := &recorder{}
	tr := NewTracker("pi", "Quantum", "", 1, r)
	base := time.Now()
	for i := 0; i < 3; i++ {
		ts := base.Add(time.Duration(i*5) * time.Second)
		tr.Observe(ts, tickAt(ts, false, false, false))
	}
	tr.Observe(base.Add(15*time.Second), tickAt(base.Add(15*time.Second), true, true, true))
	if len(r.events) != 1 {
		t.Fatalf("want 1 event, got %d", len(r.events))
	}
	if r.events[0].Type != TypeFullISP {
		t.Errorf("type = %s want %s", r.events[0].Type, TypeFullISP)
	}
	if r.events[0].DurationSeconds != 10 {
		t.Errorf("dur = %d want 10", r.events[0].DurationSeconds)
	}
}

func TestTracker_StreakCounters(t *testing.T) {
	tr := NewTracker("pi", "", "", 1, &recorder{})
	base := time.Now()
	tr.Observe(base, []sample.Sample{
		{TS: base, Source: "pi", Target: "x", TargetType: "ip", PingSuccess: false},
	})
	tr.Observe(base.Add(5*time.Second), []sample.Sample{
		{TS: base.Add(5 * time.Second), Source: "pi", Target: "x", TargetType: "ip", PingSuccess: false},
	})
	if got := tr.Streak("x"); got != 2 {
		t.Errorf("streak = %d want 2", got)
	}
	tr.Observe(base.Add(10*time.Second), []sample.Sample{
		{TS: base.Add(10 * time.Second), Source: "pi", Target: "x", TargetType: "ip", PingSuccess: true},
	})
	if got := tr.Streak("x"); got != 0 {
		t.Errorf("streak = %d want 0", got)
	}
}

// With minStreak=2, a single failed tick must not open an event but the
// streak counter still increments. Two failed ticks in a row open the event.
func TestTracker_MinStreakSuppressesSinglePacketDrops(t *testing.T) {
	r := &recorder{}
	tr := NewTracker("pi", "", "", 2, r)
	base := time.Now()
	// One failed tick — streak=1, below threshold.
	tr.Observe(base, tickAt(base, false, true, true))
	if got := tr.Streak("tA"); got != 1 {
		t.Errorf("streak after 1 failure = %d want 1", got)
	}
	// Recover — no event should ever have been opened.
	tr.Observe(base.Add(5*time.Second), tickAt(base.Add(5*time.Second), true, true, true))
	if len(r.events) != 0 {
		t.Fatalf("single-tick failure opened %d events; want 0", len(r.events))
	}
	// Two consecutive failed ticks now should open + close one event.
	tr.Observe(base.Add(10*time.Second), tickAt(base.Add(10*time.Second), false, true, true))
	tr.Observe(base.Add(15*time.Second), tickAt(base.Add(15*time.Second), false, true, true))
	tr.Observe(base.Add(20*time.Second), tickAt(base.Add(20*time.Second), true, true, true))
	if len(r.events) != 1 {
		t.Fatalf("want 1 event after 2-tick failure, got %d", len(r.events))
	}
	if r.events[0].Type != TypeSingleTarget {
		t.Errorf("type = %s want %s", r.events[0].Type, TypeSingleTarget)
	}
}
