package analysis

import (
	"math"
	"reflect"
	"testing"

	"github.com/daphnezlin/flakehunt/internal/runner"
)

func TestWilson(t *testing.T) {
	// 1/100 should be ~0.18% to 5.45%
	low, high := Wilson(1, 100)
	if math.Abs(low-0.0018) > 0.0005 || math.Abs(high-0.0545) > 0.0005 {
		t.Errorf("Wilson(1,100) = %.4f, %.4f", low, high)
	}
	// 0 failures shouldn't mean 0 upper bound
	if _, high := Wilson(0, 50); high <= 0 {
		t.Errorf("upper bound for 0/50 should be > 0")
	}
}

func TestAggregateCounts(t *testing.T) {
	agg := NewAggregate()
	for i := 0; i < 40; i++ {
		agg.Add(runner.TrialResult{Trial: runner.Trial{ID: i + 1, Seed: int64(i)}, Tests: []runner.TestOutcome{
			{Name: "m::flaky", Failed: i%2 == 1},
			{Name: "m::stable"},
			{Name: "m::skipped", Skip: true},
		}})
	}
	agg.Add(runner.TrialResult{TimedOut: true})
	if agg.Trials != 41 || agg.Timeouts != 1 {
		t.Fatalf("trials %d timeouts %d", agg.Trials, agg.Timeouts)
	}
	if s := agg.Tests["m::flaky"]; s.Failures != 20 || !s.Flaky() || s.Failing[0].Seed != 1 {
		t.Errorf("flaky stats wrong: %+v", s)
	}
	if s := agg.Tests["m::stable"]; s.Flaky() {
		t.Errorf("stable test flagged as flaky")
	}
	if _, ok := agg.Tests["m::skipped"]; ok {
		t.Errorf("skipped tests shouldn't be counted")
	}
}

func TestAggregateByCPU(t *testing.T) {
	agg := NewAggregate()
	add := func(cpus float64, failed bool) {
		agg.Add(runner.TrialResult{Trial: runner.Trial{CPUs: cpus}, Tests: []runner.TestOutcome{{Name: "race", Failed: failed}}})
	}
	add(0, false)
	add(0, false)
	add(0.5, true)
	add(0.5, false)

	s := agg.Tests["race"]
	if s.ByCPU[0].Failures != 0 || s.ByCPU[0].Runs != 2 || s.ByCPU[0.5].Failures != 1 {
		t.Errorf("by cpu: %+v %+v", *s.ByCPU[0], *s.ByCPU[0.5])
	}
	if got := s.CPULevels(); !reflect.DeepEqual(got, []float64{0, 0.5}) {
		t.Errorf("levels order: %v", got)
	}
}

func TestDetectionPower(t *testing.T) {
	cases := []struct {
		rate float64
		want int
	}{{0.10, 29}, {0.05, 59}, {0.02, 149}, {0.01, 299}}
	for _, c := range cases {
		n := TrialsToCatch(c.rate, 0.95)
		if n != c.want {
			t.Errorf("TrialsToCatch(%v) = %d, want %d", c.rate, n, c.want)
		}
		// n is enough, n-1 isn't
		if CatchProbability(c.rate, n) < 0.95 || CatchProbability(c.rate, n-1) >= 0.95 {
			t.Errorf("rate %v: %d isn't the smallest enough", c.rate, n)
		}
	}
	if p := CatchProbability(0.05, 45); math.Abs(p-0.90) > 0.01 {
		t.Errorf("45 runs at 5%% should catch it ~90%% of the time, got %v", p)
	}
	if r := CatchableRate(60, 0.95); math.Abs(r-0.0487) > 0.001 {
		t.Errorf("CatchableRate(60) = %v", r)
	}
	// at exactly that rate, 60 runs catch it exactly 95% of the time
	if p := CatchProbability(CatchableRate(60, 0.95), 60); math.Abs(p-0.95) > 1e-9 {
		t.Errorf("round trip gave %v", p)
	}
}
