package main

import (
	"context"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseJUnitPytest(t *testing.T) {
	xml := `<?xml version="1.0" encoding="utf-8"?>
<testsuites><testsuite name="pytest" tests="3">
  <testcase classname="tests.test_order" name="test_no_users_initially"><failure message="assert"/></testcase>
  <testcase classname="tests.test_stable" name="test_addition" file="tests/test_stable.py" line="3"/>
  <testcase classname="tests.test_stable" name="test_skip"><skipped/></testcase>
</testsuite></testsuites>`
	got, err := ParseJUnit(strings.NewReader(xml))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 cases, got %d", len(got))
	}
	if !got[0].Failed || got[0].Name != "tests.test_order::test_no_users_initially" {
		t.Errorf("bad first case: %+v", got[0])
	}
	if got[1].NodeID != "tests/test_stable.py::test_addition" || got[0].NodeID != "" {
		t.Errorf("node ids: %q %q", got[0].NodeID, got[1].NodeID)
	}
	if got[1].Failed || !got[2].Skip {
		t.Errorf("bad flags: %+v %+v", got[1], got[2])
	}
}

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

// fails m::flaky on odd seeds, tracks max concurrency
type fakeRunner struct{ running, peak atomic.Int32 }

func (f *fakeRunner) Run(ctx context.Context, tr Trial) TrialResult {
	n := f.running.Add(1)
	for {
		p := f.peak.Load()
		if n <= p || f.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(5 * time.Millisecond)
	f.running.Add(-1)
	return TrialResult{Trial: tr, Tests: []TestOutcome{
		{Name: "m::flaky", Failed: tr.Seed%2 == 1},
		{Name: "m::stable"},
	}}
}

func TestRunAllRespectsWorkersAndAggregates(t *testing.T) {
	trials := make([]Trial, 40)
	for i := range trials {
		trials[i] = Trial{ID: i + 1, Seed: int64(i)}
	}
	fr := &fakeRunner{}
	agg := NewAggregate()
	for res := range RunAll(context.Background(), fr, trials, 4) {
		agg.Add(res)
	}
	if agg.Trials != 40 {
		t.Fatalf("want 40 trials, got %d", agg.Trials)
	}
	if p := fr.peak.Load(); p > 4 {
		t.Errorf("peak concurrency %d exceeds 4 workers", p)
	}
	if s := agg.Tests["m::flaky"]; s.Failures != 20 || !s.Flaky() {
		t.Errorf("flaky stats wrong: %+v", s)
	}
	if s := agg.Tests["m::stable"]; s.Flaky() {
		t.Errorf("stable test flagged as flaky")
	}
}

func TestRunAllStopsOnCancel(t *testing.T) {
	trials := make([]Trial, 1000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := 0
	for range RunAll(ctx, &fakeRunner{}, trials, 2) {
		got++
		if got == 5 {
			cancel()
		}
	}
	if got >= 1000 {
		t.Errorf("cancellation didn't stop the run")
	}
}
