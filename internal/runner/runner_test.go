package runner

import (
	"context"
	"reflect"
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

func TestRunAllRespectsWorkers(t *testing.T) {
	trials := make([]Trial, 40)
	for i := range trials {
		trials[i] = Trial{ID: i + 1, Seed: int64(i)}
	}
	fr := &fakeRunner{}
	got, flaky := 0, 0
	for res := range RunAll(context.Background(), fr, trials, 4) {
		got++
		if res.Tests[0].Failed {
			flaky++
		}
	}
	if got != 40 || flaky != 20 {
		t.Fatalf("want 40 results with 20 failures, got %d and %d", got, flaky)
	}
	if p := fr.peak.Load(); p > 4 {
		t.Errorf("peak concurrency %d exceeds 4 workers", p)
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
	trials := MakeTrials(30, []float64{0, 1, 0.5}, nil, 7)
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
	if !reflect.DeepEqual(trials, MakeTrials(30, []float64{0, 1, 0.5}, nil, 7)) {
		t.Error("MakeTrials isn't reproducible from the base seed")
	}
}

func TestMakeTrialsBalancesCombinations(t *testing.T) {
	trials := MakeTrials(90, []float64{0, 1, 0.5}, []int{0, 50, 150}, 3)
	combos := map[[2]float64]int{}
	for _, tr := range trials {
		combos[[2]float64{tr.CPUs, float64(tr.LatencyMS)}]++
	}
	if len(combos) != 9 {
		t.Fatalf("want 9 combinations, got %d", len(combos))
	}
	for k, n := range combos {
		if n != 10 {
			t.Errorf("combination %v got %d trials, want 10", k, n)
		}
	}
}

func TestNodeID(t *testing.T) {
	cases := []struct{ file, class, name, want string }{
		{"tests/test_order.py", "tests.test_order", "test_x", "tests/test_order.py::test_x"},
		{"tests/test_db.py", "tests.test_db.TestUsers", "test_add", "tests/test_db.py::TestUsers::test_add"},
		{"test_a.py", "test_a.TestOuter.TestInner", "test_b[1-2]", "test_a.py::TestOuter::TestInner::test_b[1-2]"},
		{"", "tests.test_x", "test_y", ""},
	}
	for _, c := range cases {
		if got := NodeID(c.file, c.class, c.name); got != c.want {
			t.Errorf("NodeID(%q, %q, %q) = %q, want %q", c.file, c.class, c.name, got, c.want)
		}
	}
}

func TestPytestArgs(t *testing.T) {
	whole := strings.Join(PytestArgs(Trial{Seed: 7}, "/r/j.xml"), " ")
	if !strings.Contains(whole, "--randomly-seed=7") || strings.Contains(whole, "dont-reorganize") {
		t.Errorf("whole suite args: %s", whole)
	}
	picked := PytestArgs(Trial{Seed: 7, Tests: []string{"a.py::x", "a.py::y"}}, "/r/j.xml")
	n := len(picked)
	if picked[n-3] != "--randomly-dont-reorganize" || picked[n-2] != "a.py::x" || picked[n-1] != "a.py::y" {
		t.Errorf("picked args: %v", picked)
	}
}
