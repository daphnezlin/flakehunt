package main

import (
	"errors"
	"path/filepath"
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

	trials := []TrialResult{
		{Trial: Trial{ID: 1, Seed: 100}, Tests: []TestOutcome{{Name: "m::flaky", Failed: true}, {Name: "m::ok"}, {Name: "m::skipped", Skip: true}}},
		{Trial: Trial{ID: 2, Seed: 200}, Tests: []TestOutcome{{Name: "m::flaky"}, {Name: "m::ok"}}},
		{Trial: Trial{ID: 3, Seed: 300}, TimedOut: true},
		{Trial: Trial{ID: 4, Seed: 400}, Err: errors.New("no results file")},
	}
	live := NewAggregate()
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
	if flaky == nil || flaky.Runs != 2 || flaky.Failures != 1 || flaky.FailingSeeds[0] != 100 {
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
