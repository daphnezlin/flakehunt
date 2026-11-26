package analysis

import (
	"github.com/daphnezlin/flakehunt/internal/runner"
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
	trend := func(s *TestStats, _ *rand.Rand) float64 {
		return TrendPValue(s.CPULevels(), CPUScores(s.CPULevels()), []map[float64]*Count{s.ByCPU}, rng)
	}

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
		agg.Add(runner.TrialResult{Trial: runner.Trial{CPUs: cpus}, Tests: []runner.TestOutcome{{Name: name, Failed: failed}}})
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
				agg.Add(runner.TrialResult{Trial: runner.Trial{CPUs: cpu, LatencyMS: lat}, Tests: []runner.TestOutcome{
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

func TestTrendPValueStrata(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	levels := []float64{0, 1, 0.5}
	by := func(fails ...int) map[float64]*Count {
		m := map[float64]*Count{}
		for i, l := range levels {
			if fails[i] >= 0 {
				m[l] = &Count{Runs: 20, Failures: fails[i]}
			}
		}
		return m
	}

	// confounded: the slow-cpu trials all come from a context where the test
	// fails a lot anyway. pooled, that looks like a cpu effect. split by
	// context, each stratum only has one level, so there's nothing to compare
	full := by(2, 2, -1)
	focus := by(-1, -1, 16)
	pooled := map[float64]*Count{0: full[0], 1: full[1], 0.5: focus[0.5]}
	if p := TrendPValue(levels, CPUScores(levels), []map[float64]*Count{pooled}, rng); p > 0.001 {
		t.Fatalf("setup: pooled should look significant, got %v", p)
	}
	if p := TrendPValue(levels, CPUScores(levels), []map[float64]*Count{full, focus}, rng); p < 0.5 {
		t.Errorf("stratified should see no evidence, got %v", p)
	}

	// a real trend inside both contexts, at different base rates
	if p := TrendPValue(levels, CPUScores(levels), []map[float64]*Count{by(2, 4, 10), by(8, 11, 17)}, rng); p > 0.001 {
		t.Errorf("trend in both strata should be significant, got %v", p)
	}
}

func TestFinalAlpha(t *testing.T) {
	if FinalAlpha(0) != DefaultAlpha {
		t.Errorf("no early checks should give the default")
	}
	// interim budget comes off the final threshold so the total stays at 5%
	if got := FinalAlpha(3); math.Abs(got+3*InterimAlpha-DefaultAlpha) > 1e-12 {
		t.Errorf("FinalAlpha(3) = %v", got)
	}
}

func TestSeedBeatsChanceTrend(t *testing.T) {
	// like run 15: the seed reproduced every failure, and the polluter search
	// found the culprit, but the cpu trend came out significant by chance
	c := Cause{CPUTested: true, CPUq: 0.035, Alpha: 0.047, Replays: Count{Runs: 5, Failures: 5},
		Polluter: &PolluterResult{Polluter: "m::test_create"}}
	label, detail := classify(c)
	if label != "order-dependent" || !strings.Contains(detail, "test_create") || !strings.Contains(detail, "likely chance") {
		t.Errorf("got %q: %q", label, detail)
	}
	// without a trend, no note
	c.CPUq = 0.5
	if _, detail := classify(c); strings.Contains(detail, "chance") {
		t.Errorf("unexpected note: %q", detail)
	}
}

func TestHangLabel(t *testing.T) {
	agg := NewAggregate()
	for i := 0; i < 40; i++ {
		agg.Add(runner.TrialResult{Trial: runner.Trial{ID: i, CPUs: []float64{0, 1, 0.5}[i%3]}, Tests: []runner.TestOutcome{
			{Name: "deadlock", Failed: i%4 == 0, Hung: i%4 == 0},
			{Name: "assert", Failed: i%4 == 1},
		}})
	}
	replays := map[string]*Count{"deadlock": {Runs: 5, Failures: 1}, "assert": {Runs: 5, Failures: 2}}
	got := map[string]Cause{}
	for _, c := range Analyze(agg, replays, nil) {
		got[c.Test] = c
	}
	if got["deadlock"].Label != "hang" || got["deadlock"].Hangs != 10 {
		t.Errorf("deadlock: %+v", got["deadlock"])
	}
	if got["assert"].Label == "hang" {
		t.Errorf("a plain failure isn't a hang")
	}
}

func TestSeedNeedsToBeatBaseRate(t *testing.T) {
	five := Count{Runs: 5, Failures: 5}
	// fails half the time anyway: 5 in a row by chance is 3%, so the seed is the cause
	if !(Cause{Replays: five, BaseRate: 0.5}).SeedDetermined() {
		t.Error("50% base rate, 5/5 replays should count")
	}
	// fails 90% of the time anyway: 5 in a row by chance is 59%, proves nothing
	if (Cause{Replays: five, BaseRate: 0.9}).SeedDetermined() {
		t.Error("90% base rate, 5/5 replays shouldn't count")
	}
	// more replays can still settle it
	if !(Cause{Replays: Count{Runs: 30, Failures: 30}, BaseRate: 0.9}).SeedDetermined() {
		t.Error("30/30 replays should beat a 90% base rate")
	}
	if (Cause{Replays: Count{Runs: 5, Failures: 4}, BaseRate: 0.1}).SeedDetermined() {
		t.Error("a replay that passed means it's not the seed")
	}
}

func TestUnreproducedDropsTheSeed(t *testing.T) {
	// like run 20: every replay failed, but rerunning the exact order passed
	c := Cause{Replays: Count{Runs: 5, Failures: 5}, BaseRate: 0.3,
		Polluter: &PolluterResult{Unreproduced: true, Note: "couldn't reproduce"}}
	label, detail := classify(c)
	if label == "order-dependent" || label == "random data" || !strings.Contains(detail, "seed isn't the cause") {
		t.Errorf("got %q: %q", label, detail)
	}
}

func TestSlowdownScoring(t *testing.T) {
	levels := []float64{0, 1, 0.5}
	if got := CPUScores(levels); got[0] != 1 || got[1] != 1 || got[2] != 2 {
		t.Fatalf("scores: %v", got)
	}
	// run 21's time_boundary: no limit and 1 are the same speed for python
	// code, only 0.5 is slower. evenly spaced steps blame the noise between
	// no limit and 1 and miss it, slowdown scoring doesn't
	m := []map[float64]*Count{{0: {Runs: 57, Failures: 13}, 1: {Runs: 57, Failures: 9}, 0.5: {Runs: 54, Failures: 18}}}
	steps := TrendPValue(levels, Ranks(3), m, rand.New(rand.NewSource(1)))
	slow := TrendPValue(levels, CPUScores(levels), m, rand.New(rand.NewSource(1)))
	if !(slow < 0.05 && steps > 0.1) {
		t.Errorf("steps p=%v, slowdown p=%v", steps, slow)
	}
	// limits that are all a full core or more can't slow python down
	if p := TrendPValue([]float64{0, 2, 1}, CPUScores([]float64{0, 2, 1}), m, rand.New(rand.NewSource(1))); !math.IsNaN(p) {
		t.Errorf("no slower level to compare, want NaN, got %v", p)
	}
}
