package store

import (
	"database/sql"
	"errors"
	"github.com/daphnezlin/flakehunt/internal/analysis"
	"github.com/daphnezlin/flakehunt/internal/runner"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	runID, err := store.CreateRun(RunInfo{
		StartedAt: time.Now(), Image: "zoo", BaseSeed: 42, Workers: 2, Env: []string{"A=1", "B=2"},
	})
	if err != nil {
		t.Fatal(err)
	}

	trials := []runner.TrialResult{
		{Trial: runner.Trial{ID: 1, Seed: 100}, Tests: []runner.TestOutcome{{Name: "m::flaky", Failed: true}, {Name: "m::ok"}, {Name: "m::skipped", Skip: true}}},
		{Trial: runner.Trial{ID: 2, Seed: 200}, Tests: []runner.TestOutcome{{Name: "m::flaky"}, {Name: "m::ok"}}},
		{Trial: runner.Trial{ID: 3, Seed: 300}, TimedOut: true},
		{Trial: runner.Trial{ID: 4, Seed: 400}, Err: errors.New("no results file")},
	}
	live := analysis.NewAggregate()
	for _, tr := range trials {
		if err := store.SaveTrial(runID, tr); err != nil {
			t.Fatal(err)
		}
		live.Add(tr)
	}
	if err := store.FinishRun(runID); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.LoadAggregate(runID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Trials != live.Trials || loaded.Timeouts != 1 || loaded.InfraFails != 1 {
		t.Errorf("counts: got %+v", loaded)
	}
	flaky := loaded.Tests["m::flaky"]
	if flaky == nil || flaky.Runs != 2 || flaky.Failures != 1 || flaky.Failing[0].Seed != 100 {
		t.Errorf("flaky stats: got %+v", flaky)
	}
	if _, ok := loaded.Tests["m::skipped"]; ok {
		t.Errorf("skipped test shouldn't be counted")
	}

	run, err := store.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if !run.Finished || run.Image != "zoo" || len(run.Env) != 2 || run.BaseSeed != 42 {
		t.Errorf("run info: got %+v", run)
	}

	runs, err := store.ListRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Trials != 4 {
		t.Errorf("list: got %+v", runs)
	}
}

func TestStoreUnknownRun(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.GetRun(99); err == nil {
		t.Error("expected error for missing run")
	}
}

func TestReplaysKeptSeparate(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	runID, _ := store.CreateRun(RunInfo{StartedAt: time.Now(), Image: "zoo"})
	store.SaveTrial(runID, runner.TrialResult{Trial: runner.Trial{ID: 1, Seed: 9},
		Tests: []runner.TestOutcome{{Name: "order", Failed: true}, {Name: "other"}}})
	for i := 2; i <= 4; i++ {
		store.SaveTrial(runID, runner.TrialResult{Trial: runner.Trial{ID: i, Seed: 9, ReplayFor: "order"},
			Tests: []runner.TestOutcome{{Name: "order", Failed: true}, {Name: "other"}}})
	}

	agg, err := store.LoadAggregate(runID)
	if err != nil {
		t.Fatal(err)
	}
	if agg.Trials != 1 || agg.Tests["order"].Runs != 1 {
		t.Errorf("replays leaked into rates: trials=%d runs=%d", agg.Trials, agg.Tests["order"].Runs)
	}
	replays, err := store.LoadReplays(runID)
	if err != nil {
		t.Fatal(err)
	}
	if c := replays["order"]; c == nil || c.Runs != 3 || c.Failures != 3 {
		t.Errorf("replays: %+v", replays)
	}
	if _, ok := replays["other"]; ok {
		t.Error("only the target test should count in a replay")
	}
	runs, _ := store.ListRuns()
	if runs[0].Trials != 1 {
		t.Errorf("run list should not count replays, got %d", runs[0].Trials)
	}
}

func TestPolluterStorage(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	runID, _ := store.CreateRun(RunInfo{StartedAt: time.Now(), Image: "zoo"})
	// a replay and a probe for the same test; only the replay counts as a replay
	store.SaveTrial(runID, runner.TrialResult{Trial: runner.Trial{ID: 1, ReplayFor: "v"}, Tests: []runner.TestOutcome{{Name: "v", Failed: true}}})
	store.SaveTrial(runID, runner.TrialResult{Trial: runner.Trial{ID: 2, ReplayFor: "v", Probe: "polluter"}, Tests: []runner.TestOutcome{{Name: "v"}}})

	want := analysis.PolluterResult{Polluter: "p", PolluterNode: "t.py::p", VictimNode: "t.py::v", Probes: 4}
	if err := store.SavePolluter(runID, "v", want); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadPolluters(runID)
	if err != nil {
		t.Fatal(err)
	}
	if got["v"] != want {
		t.Errorf("got %+v, want %+v", got["v"], want)
	}
	replays, _ := store.LoadReplays(runID)
	if c := replays["v"]; c == nil || c.Runs != 1 {
		t.Errorf("probes leaked into replays: %+v", replays)
	}
}

func TestStoreSavesConditions(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	runID, _ := store.CreateRun(RunInfo{StartedAt: time.Now(), Image: "zoo", CPULevels: []float64{0, 0.5}})
	store.SaveTrial(runID, runner.TrialResult{Trial: runner.Trial{ID: 1, CPUs: 0.5}, CPUSet: "2-3",
		Tests: []runner.TestOutcome{{Name: "race", Failed: true}}})

	agg, err := store.LoadAggregate(runID)
	if err != nil {
		t.Fatal(err)
	}
	if c := agg.Tests["race"].ByCPU[0.5]; c == nil || c.Failures != 1 {
		t.Errorf("cpu level not loaded back: %+v", agg.Tests["race"].ByCPU)
	}
	run, _ := store.GetRun(runID)
	if !reflect.DeepEqual(run.CPULevels, []float64{0, 0.5}) {
		t.Errorf("cpu levels: %v", run.CPULevels)
	}
}

// db files made before migrations existed have the tables but user_version 0
func TestMigratesOldDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO runs (started_at, image, base_seed, workers, cpus, env)
		VALUES ('2026-11-23T00:00:00Z', 'zoo', 1, 5, 0, '')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("opening old db: %v", err)
	}
	defer store.Close()
	runs, err := store.ListRuns()
	if err != nil || len(runs) != 1 {
		t.Fatalf("old run should still be there: %v %v", runs, err)
	}
	var version int
	store.db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != len(migrations) {
		t.Errorf("user_version = %d, want %d", version, len(migrations))
	}
}

func TestStoreSavesLatency(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	runID, _ := store.CreateRun(RunInfo{StartedAt: time.Now(), Image: "zoo", LatencyLevels: []int{0, 150}, Service: "api"})
	store.SaveTrial(runID, runner.TrialResult{Trial: runner.Trial{ID: 1, LatencyMS: 150}, Tests: []runner.TestOutcome{{Name: "api", Failed: true}}})

	agg, err := store.LoadAggregate(runID)
	if err != nil {
		t.Fatal(err)
	}
	if c := agg.Tests["api"].ByLatency[150]; c == nil || c.Failures != 1 {
		t.Errorf("latency not loaded back: %+v", agg.Tests["api"].ByLatency)
	}
	run, _ := store.GetRun(runID)
	if !reflect.DeepEqual(run.LatencyLevels, []int{0, 150}) || run.Service != "api" {
		t.Errorf("run: %+v", run)
	}
}
