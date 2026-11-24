package main

import (
	"math"
	"math/rand"
	"strings"
	"testing"
)

// stats builds TestStats from failures/runs per level, fastest level first.
func stats(levels []float64, fails, runs []int) *TestStats {
	s := &TestStats{Name: "t", ByCPU: map[float64]*Count{}}
	for i, l := range levels {
		s.ByCPU[l] = &Count{Runs: runs[i], Failures: fails[i]}
		s.Runs += runs[i]
		s.Failures += fails[i]
	}
	return s
}

func TestTrendPValue(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	levels := []float64{0, 1, 0.5}
	trend := func(s *TestStats, _ *rand.Rand) float64 { return TrendPValue(s.CPULevels(), s.ByCPU, rng) }

	// clear trend, like the race test
	if p := trend(stats(levels, []int{2, 8, 16}, []int{20, 20, 20}), rng); p > 0.001 {
		t.Errorf("strong trend should have tiny p, got %v", p)
	}
	// flat, like the order test
	if p := trend(stats(levels, []int{10, 10, 10}, []int{20, 20, 20}), rng); p < 0.3 {
		t.Errorf("flat rates should have large p, got %v", p)
	}
	// 0/1/2 out of 20 looks like a trend but is too few failures to mean much
	if p := trend(stats(levels, []int{0, 1, 2}, []int{20, 20, 20}), rng); p < 0.05 {
		t.Errorf("tiny counts shouldn't be significant, got %v", p)
	}
	// opposite direction isn't what we test for
	if p := trend(stats(levels, []int{16, 8, 2}, []int{20, 20, 20}), rng); p < 0.9 {
		t.Errorf("reverse trend should not be significant, got %v", p)
	}
	// one level, nothing to compare
	if p := trend(stats([]float64{0}, []int{5}, []int{20}), rng); !math.IsNaN(p) {
		t.Errorf("single level should be NaN, got %v", p)
	}
}

func TestBenjaminiHochberg(t *testing.T) {
	ps := []float64{0.01, 0.04, 0.03, 0.20}
	want := []float64{0.04, 0.0533, 0.0533, 0.20}
	got := BenjaminiHochberg(ps)
	for i := range want {
		if math.Abs(got[i]-want[i]) > 0.001 {
			t.Errorf("q[%d] = %.4f, want %.4f", i, got[i], want[i])
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name  string
		c     Cause
		label string
	}{
		{"timing", Cause{CPUTested: true, CPUq: 0.001, Replays: Count{Runs: 5, Failures: 2}}, "timing"},
		{"seed", Cause{CPUTested: true, CPUq: 0.8, Replays: Count{Runs: 5, Failures: 5}}, "seed-dependent"},
		{"both", Cause{CPUTested: true, CPUq: 0.001, Replays: Count{Runs: 5, Failures: 5}}, "seed + timing"},
		{"unclear", Cause{CPUTested: true, CPUq: 0.5, Replays: Count{Runs: 5, Failures: 1}}, "nondeterministic"},
		{"no data", Cause{}, "unknown"},
		{"network", Cause{CPUTested: true, CPUq: 0.7, NetTested: true, NetQ: 0.001, Replays: Count{Runs: 5}}, "network"},
		{"cpu and network", Cause{CPUTested: true, CPUq: 0.01, NetTested: true, NetQ: 0.001, Replays: Count{Runs: 5}}, "timing + network"},
	}
	for _, c := range cases {
		if got, _ := classify(c.c); got != c.label {
			t.Errorf("%s: got %q, want %q", c.name, got, c.label)
		}
	}
}

func TestAnalyzeZooShape(t *testing.T) {
	agg := NewAggregate()
	add := func(name string, cpus float64, failed bool) {
		agg.Add(TrialResult{Trial: Trial{CPUs: cpus}, Tests: []TestOutcome{{Name: name, Failed: failed}}})
	}
	for _, l := range []float64{0, 1, 0.5} {
		race := map[float64]int{0: 2, 1: 8, 0.5: 16}[l]
		for i := 0; i < 20; i++ {
			add("race", l, i < race)
			add("order", l, i < 10)
		}
	}
	replays := map[string]*Count{
		"race":  {Runs: 5, Failures: 1},
		"order": {Runs: 5, Failures: 5},
	}
	got := map[string]string{}
	for _, c := range Analyze(agg, replays, nil) {
		got[c.Test] = c.Label
	}
	if got["race"] != "timing" || got["order"] != "seed-dependent" {
		t.Errorf("got %v", got)
	}
}

func TestClassifySeed(t *testing.T) {
	seed := Cause{CPUTested: true, CPUq: 0.9, Replays: Count{Runs: 5, Failures: 5}}

	alone := seed
	alone.Polluter = &PolluterResult{FailsAlone: true}
	if got, _ := classify(alone); got != "random data" {
		t.Errorf("fails alone: got %q", got)
	}
	order := seed
	order.Polluter = &PolluterResult{Polluter: "tests.m::test_create"}
	if got, detail := classify(order); got != "order-dependent" || !strings.Contains(detail, "test_create") {
		t.Errorf("polluter: got %q %q", got, detail)
	}
}

func TestAnalyzeSeparatesCPUAndNetwork(t *testing.T) {
	agg := NewAggregate()
	// 3 cpu x 3 latency, 10 trials per combination. "slow api" only cares
	// about latency, "race" only about cpu
	for _, cpu := range []float64{0, 1, 0.5} {
		for _, lat := range []int{0, 50, 150} {
			for i := 0; i < 10; i++ {
				agg.Add(TrialResult{Trial: Trial{CPUs: cpu, LatencyMS: lat}, Tests: []TestOutcome{
					{Name: "slow api", Failed: lat == 150 && i < 7},
					{Name: "race", Failed: cpu == 0.5 && i < 6},
				}})
			}
		}
	}
	replays := map[string]*Count{"slow api": {Runs: 5}, "race": {Runs: 5, Failures: 1}}
	got := map[string]string{}
	for _, c := range Analyze(agg, replays, nil) {
		got[c.Test] = c.Label
	}
	if got["slow api"] != "network" || got["race"] != "timing" {
		t.Errorf("got %v", got)
	}
}
