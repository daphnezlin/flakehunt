package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScoreAllCorrect(t *testing.T) {
	_, agg, causes := zooRun()
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
	_, agg, causes := zooRun()
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
