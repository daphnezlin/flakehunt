package main

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestCPUSets(t *testing.T) {
	cases := []struct {
		ncpu, workers int
		want          []string
	}{
		{8, 4, []string{"0-1", "2-3", "4-5", "6-7"}},
		{8, 3, []string{"0-1", "2-3", "4-5"}}, // leftover cores stay unused
		{4, 4, []string{"0", "1", "2", "3"}},
		{2, 5, nil}, // not enough cores to pin
	}
	for _, c := range cases {
		if got := CPUSets(c.ncpu, c.workers); !reflect.DeepEqual(got, c.want) {
			t.Errorf("CPUSets(%d, %d) = %v, want %v", c.ncpu, c.workers, got, c.want)
		}
	}
}

func TestMakeTrialsBalanced(t *testing.T) {
	trials := MakeTrials(30, []float64{0, 1, 0.5}, 7)
	counts := map[float64]int{}
	for i, tr := range trials {
		counts[tr.CPUs]++
		if tr.ID != i+1 {
			t.Fatalf("ids should be 1..n, got %d at %d", tr.ID, i)
		}
	}
	for _, l := range []float64{0, 1, 0.5} {
		if counts[l] != 10 {
			t.Errorf("level %v got %d trials, want 10", l, counts[l])
		}
	}
	// same base seed, same run
	if !reflect.DeepEqual(trials, MakeTrials(30, []float64{0, 1, 0.5}, 7)) {
		t.Error("MakeTrials isn't reproducible from the base seed")
	}
}

func TestAggregateByCPU(t *testing.T) {
	agg := NewAggregate()
	add := func(cpus float64, failed bool) {
		agg.Add(TrialResult{Trial: Trial{CPUs: cpus}, Tests: []TestOutcome{{Name: "race", Failed: failed}}})
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

func TestStoreSavesConditions(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	runID, _ := store.CreateRun(RunInfo{StartedAt: time.Now(), Image: "zoo", CPULevels: []float64{0, 0.5}})
	store.SaveTrial(runID, TrialResult{Trial: Trial{ID: 1, CPUs: 0.5}, CPUSet: "2-3",
		Tests: []TestOutcome{{Name: "race", Failed: true}}})

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
