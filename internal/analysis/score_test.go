package analysis

import (
	"github.com/daphnezlin/flakehunt/internal/runner"
	"os"
	"path/filepath"
	"testing"
)

func TestScoreAllCorrect(t *testing.T) {
	agg, causes := zooResults()
	exp := map[string]Expected{
		"m::test_empty":  {Cause: "order-dependent", Polluter: "m::test_create"},
		"m::test_api":    {Cause: "network"},
		"m::test_create": {Cause: "stable"},
		"m::test_ok":     {Cause: "stable"},
	}
	v := Score(agg, causes, exp)
	for _, x := range v {
		if !x.Correct {
			t.Errorf("%s: got %q want %q", x.Test, x.Got, x.Want)
		}
	}
	s := Summarize(v, exp)
	want := ScoreSummary{FlakyFound: 2, FlakyTotal: 2, Stable: 2, CausesRight: 2, CausesTotal: 2, PollutersRight: 1, Polluted: 1}
	if s != want {
		t.Errorf("got %+v\nwant %+v", s, want)
	}
}

func TestScoreMistakes(t *testing.T) {
	agg, causes := zooResults()
	exp := map[string]Expected{
		"m::test_empty":   {Cause: "order-dependent", Polluter: "m::someone_else"}, // wrong polluter
		"m::test_api":     {Cause: "timing"},                                       // wrong cause
		"m::test_ok":      {Cause: "random data"},                                  // flaky but flakehunt missed it
		"m::test_missing": {Cause: "timing"},                                       // never ran
	}
	v := Score(agg, causes, exp)
	s := Summarize(v, exp)
	if s.CausesRight != 1 || s.CausesTotal != 3 || s.FlakyFound != 2 || s.PollutersRight != 0 || s.NotRun != 1 {
		t.Errorf("got %+v", s)
	}
	for _, x := range v {
		if x.Test == "m::test_empty" && x.Correct {
			t.Error("wrong polluter should count as incorrect")
		}
	}
	// m::test_create isn't in the expected file and is stable, so it's not unexpected
	if s.Unexpected != 0 {
		t.Errorf("unexpected: %d", s.Unexpected)
	}
}

func TestLoadExpected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "expected.json")
	os.WriteFile(path, []byte(`{"a::b": {"cause": "order-dependent", "polluter": "a::c"}, "a::d": {"cause": "stable"}}`), 0o644)
	exp, err := LoadExpected(path)
	if err != nil {
		t.Fatal(err)
	}
	if exp["a::b"].Polluter != "a::c" || exp["a::d"].Cause != "stable" {
		t.Errorf("got %+v", exp)
	}
}

// zooResults is shaped like the flaky zoo: an order-dependent test with a
// known polluter, a network test, and two stable tests.
func zooResults() (*Aggregate, []Cause) {
	agg := NewAggregate()
	for _, cpu := range []float64{0, 0.5} {
		for _, lat := range []int{0, 150} {
			for i := 0; i < 10; i++ {
				agg.Add(runner.TrialResult{Trial: runner.Trial{Seed: int64(i), CPUs: cpu, LatencyMS: lat}, Tests: []runner.TestOutcome{
					{Name: "m::test_create", NodeID: "t.py::test_create"},
					{Name: "m::test_empty", NodeID: "t.py::test_empty", Failed: i%2 == 0},
					{Name: "m::test_api", NodeID: "t.py::test_api", Failed: lat == 150 && i < 8},
					{Name: "m::test_ok", NodeID: "t.py::test_ok"},
				}})
			}
		}
	}
	replays := map[string]*Count{"m::test_empty": {Runs: 5, Failures: 5}, "m::test_api": {Runs: 5}}
	polluters := map[string]PolluterResult{"m::test_empty": {
		Polluter: "m::test_create", PolluterNode: "t.py::test_create", VictimNode: "t.py::test_empty", Probes: 3,
	}}
	return agg, Analyze(agg, replays, polluters)
}
