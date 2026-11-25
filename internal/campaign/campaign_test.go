package campaign

import (
	"context"
	"math/rand"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/daphnezlin/flakehunt/internal/analysis"
	"github.com/daphnezlin/flakehunt/internal/runner"
	"github.com/daphnezlin/flakehunt/internal/store"
)

// fakeSuite behaves like a small version of flaky-zoo:
//   - race fails more with less cpu, and its seed doesn't decide it
//   - order fails when polluter ran before it
//   - api fails with 150ms latency
//   - polluter and ok never fail
//
// Randomness comes from the trial id, so results don't depend on which
// worker finishes first.
type fakeSuite struct {
	raceRate  map[float64]float64
	aloneBump float64 // extra failure rate for race when it runs on its own

	mu             sync.Mutex
	focused        [][]string // Tests of every focus trial
	focusLatencies []int
}

var nodes = map[string]string{
	"t.py::polluter": "m::polluter", "t.py::order": "m::order", "t.py::race": "m::race",
	"t.py::api": "m::api", "t.py::ok": "m::ok",
}

func (f *fakeSuite) Run(ctx context.Context, t runner.Trial) runner.TrialResult {
	order := t.Tests
	if len(order) == 0 {
		order = []string{"t.py::order", "t.py::polluter", "t.py::race", "t.py::api", "t.py::ok"}
		if t.Seed%2 == 0 {
			order[0], order[1] = order[1], order[0]
		}
	}
	if t.Probe == "focus" {
		f.mu.Lock()
		f.focused = append(f.focused, t.Tests)
		f.focusLatencies = append(f.focusLatencies, t.LatencyMS)
		f.mu.Unlock()
	}

	rng := rand.New(rand.NewSource(int64(t.ID)*7919 + 1))
	polluted := false
	var out []runner.TestOutcome
	for _, id := range order {
		failed := false
		switch id {
		case "t.py::polluter":
			polluted = true
		case "t.py::order":
			failed = polluted
		case "t.py::race":
			rate := f.raceRate[t.CPUs]
			if len(t.Tests) == 1 {
				rate += f.aloneBump
			}
			failed = rng.Float64() < rate
		case "t.py::api":
			failed = t.LatencyMS == 150 && rng.Float64() < 0.9
		}
		out = append(out, runner.TestOutcome{Name: nodes[id], NodeID: id, Failed: failed})
	}
	return runner.TrialResult{Trial: t, Tests: out}
}

func newRun(t *testing.T, cfg Config) (*store.Store, int64) {
	db, err := store.OpenStore(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	runID, err := db.CreateRun(store.RunInfo{StartedAt: time.Now(), Image: "fake", Alpha: cfg.Alpha()})
	if err != nil {
		t.Fatal(err)
	}
	return db, runID
}

func baseConfig() Config {
	return Config{
		DetectTrials: 45, Workers: 4, BaseSeed: 1,
		CPULevels: []float64{0, 1, 0.5}, LatencyLevels: []int{0, 50, 150},
		Replays: 5, FindPolluters: true, FocusBatch: 36, FocusLooks: 3,
	}
}

func labels(res Result) map[string]analysis.Cause {
	out := map[string]analysis.Cause{}
	for _, c := range res.Causes() {
		out[c.Test] = c
	}
	return out
}

func TestCampaignFocusesOnlyUnclearTests(t *testing.T) {
	// a weak cpu effect: not enough evidence after 45 trials, so focus has to
	// kick in. race also fails more when it runs alone, like the real zoo did
	suite := &fakeSuite{raceRate: map[float64]float64{0: 0.15, 1: 0.2, 0.5: 0.45}, aloneBump: 0.25}
	cfg := baseConfig()
	db, runID := newRun(t, cfg)

	res, err := New(cfg, db, runID, suite).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := labels(res)
	if got["m::race"].Label != "timing" {
		t.Errorf("race: %+v", got["m::race"])
	}
	if got["m::order"].Label != "order-dependent" || got["m::order"].Polluter.Polluter != "m::polluter" {
		t.Errorf("order: %+v", got["m::order"])
	}
	if got["m::api"].Label != "network" {
		t.Errorf("api: %+v", got["m::api"])
	}
	if _, flaky := got["m::ok"]; flaky {
		t.Error("ok shouldn't be flaky")
	}

	if res.Stats.Focus == 0 {
		t.Fatal("expected focus trials for the weak race effect")
	}
	for _, tests := range suite.focused {
		if len(tests) != 1 || tests[0] != "t.py::race" {
			t.Fatalf("focus trials should only run the race test, got %v", tests)
		}
	}
	// race shows no sign of a network effect, so focus shouldn't vary latency
	for _, l := range suite.focusLatencies {
		if l != 0 {
			t.Fatalf("focus trial with %dms latency for a test with no network hint", l)
		}
	}
}

func TestCampaignSkipsFocusWhenEverythingIsClear(t *testing.T) {
	suite := &fakeSuite{raceRate: map[float64]float64{0: 0.05, 1: 0.1, 0.5: 0.9}}
	cfg := baseConfig()
	db, runID := newRun(t, cfg)

	res, err := New(cfg, db, runID, suite).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Focus != 0 || res.Stats.FocusBatches != 0 {
		t.Errorf("strong effects should be clear after detection, but ran %d focus trials", res.Stats.Focus)
	}
	if labels(res)["m::race"].Label != "timing" {
		t.Errorf("race: %+v", labels(res)["m::race"])
	}
}

func TestAdaptiveCostsLessThanFixed(t *testing.T) {
	rates := map[float64]float64{0: 0.15, 1: 0.2, 0.5: 0.45}

	adaptive := baseConfig()
	db, runID := newRun(t, adaptive)
	a, err := New(adaptive, db, runID, &fakeSuite{raceRate: rates}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	fixed := baseConfig()
	fixed.DetectTrials, fixed.FocusBatch = 135, 0
	db2, runID2 := newRun(t, fixed)
	f, err := New(fixed, db2, runID2, &fakeSuite{raceRate: rates}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if a.Stats.TestExecutions >= f.Stats.TestExecutions {
		t.Errorf("adaptive ran %d test executions, fixed ran %d", a.Stats.TestExecutions, f.Stats.TestExecutions)
	}
	t.Logf("test executions: adaptive %d (%d trials), fixed %d (%d trials)",
		a.Stats.TestExecutions, a.Stats.Trials(), f.Stats.TestExecutions, f.Stats.Trials())
}

func TestSavedRunMatchesLive(t *testing.T) {
	suite := &fakeSuite{raceRate: map[float64]float64{0: 0.15, 1: 0.2, 0.5: 0.45}}
	cfg := baseConfig()
	db, runID := newRun(t, cfg)
	res, err := New(cfg, db, runID, suite).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	run, _ := db.GetRun(runID)
	agg, _ := db.LoadAggregate(runID)
	replays, _ := db.LoadReplays(runID)
	polluters, _ := db.LoadPolluters(runID)
	saved := analysis.AnalyzeAt(agg, replays, polluters, run.Alpha)

	live := res.Causes()
	if len(saved) != len(live) {
		t.Fatalf("saved %d causes, live %d", len(saved), len(live))
	}
	for i := range live {
		if live[i].Label != saved[i].Label || live[i].CPUq != saved[i].CPUq {
			t.Errorf("%s: live %q (q=%v), saved %q (q=%v)", live[i].Test, live[i].Label, live[i].CPUq, saved[i].Label, saved[i].CPUq)
		}
	}
	if run.Alpha != cfg.Alpha() {
		t.Errorf("alpha not saved: %v", run.Alpha)
	}
}

func TestAlpha(t *testing.T) {
	cfg := baseConfig()
	if got, want := cfg.Alpha(), analysis.FinalAlpha(3); got != want {
		t.Errorf("3 focus batches means 3 early checks, want %v got %v", want, got)
	}
	cfg.FocusBatch = 0
	if got := cfg.Alpha(); got != analysis.DefaultAlpha {
		t.Errorf("no focus should use the default, got %v", got)
	}
}
