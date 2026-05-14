package grade

import (
	"testing"
	"time"

	"github.com/jasonwbarnett/ping-lens/internal/config"
	"github.com/jasonwbarnett/ping-lens/internal/db"
)

func defaultThresholds() config.Thresholds {
	var c config.Config
	// applyDefaults is unexported; emulate the relevant bits here.
	c.Thresholds = config.Thresholds{
		LossPct: config.LabelBands{Excellent: 0.01, Good: 0.05, Fair: 0.25, Poor: 1.00},
		P95MS:   config.LabelBands{Excellent: 25, Good: 50, Fair: 100, Poor: 200},
		P99MS:   config.LabelBands{Excellent: 75, Good: 150, Fair: 300, Poor: 750},
	}
	return c.Thresholds
}

func TestGrade_Excellent(t *testing.T) {
	s := &SourceStats{LossPct: 0.0, P95MS: 20, P99MS: 60, OutageCount: 0}
	if g := Grade(s, defaultThresholds()); g != Excellent {
		t.Errorf("got %s want excellent", g)
	}
}

func TestGrade_BadWorstWins(t *testing.T) {
	s := &SourceStats{LossPct: 0.001, P95MS: 20, P99MS: 60, OutageCount: 50, LongestOutage: time.Hour}
	if g := Grade(s, defaultThresholds()); g != Bad {
		t.Errorf("got %s want bad (outages dominate)", g)
	}
}

func TestSummarize_PerTargetWeighted(t *testing.T) {
	rows := []db.RollupRow{
		{Source: "pi", Target: "1.1.1.1", Samples: 100, P95MS: 10, LossPct: 0},
		{Source: "pi", Target: "1.1.1.1", Samples: 100, P95MS: 30, LossPct: 0},
	}
	_, pt := Summarize(rows, nil)
	if len(pt) != 1 {
		t.Fatalf("want 1 target, got %d", len(pt))
	}
	if pt[0].P95MS != 20 {
		t.Errorf("weighted p95 = %v want 20", pt[0].P95MS)
	}
}

func TestPickWinner_TieReturnsTie(t *testing.T) {
	a := &SourceStats{Source: "a", LossPct: 0.1, P95MS: 30, P99MS: 60}
	b := &SourceStats{Source: "b", LossPct: 0.1, P95MS: 30, P99MS: 60}
	w := PickWinner(a, b, nil)
	if !w.Tie {
		t.Errorf("want tie, got winner %q", w.Source)
	}
}

func TestPickWinner_LowerLossLatencyWins(t *testing.T) {
	a := &SourceStats{Source: "a", ISPName: "A", LossPct: 0.05, P95MS: 30, P99MS: 50}
	b := &SourceStats{Source: "b", ISPName: "B", LossPct: 0.10, P95MS: 50, P99MS: 80}
	w := PickWinner(a, b, nil)
	if w.Tie || w.Source != "a" {
		t.Errorf("want a, got %+v", w)
	}
}
