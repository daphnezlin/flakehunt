package analysis

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// Expected is the known true answer for one test, e.g. from flaky-zoo/expected.json.
type Expected struct {
	Cause    string `json:"cause"`              // stable, timing, network, order-dependent, random data, ...
	Polluter string `json:"polluter,omitempty"` // for order-dependent tests
}

func LoadExpected(path string) (map[string]Expected, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var exp map[string]Expected
	if err := json.Unmarshal(b, &exp); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return exp, nil
}

// Verdict compares what flakehunt said about one test to the expected answer.
type Verdict struct {
	Test     string
	Got      string // "stable", "always fails", or a cause label. "not run" if missing
	Want     string // "" if the test isn't in the expected file
	Polluter string // what the polluter search found, if anything
	Correct  bool
}

// Score checks every test in the run and in the expected file.
func Score(agg *Aggregate, causes []Cause, expected map[string]Expected) []Verdict {
	byName := CausesByName(causes)
	names := map[string]bool{}
	for n := range agg.Tests {
		names[n] = true
	}
	for n := range expected {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	var out []Verdict
	for _, name := range sorted {
		v := Verdict{Test: name, Got: "not run"}
		if s := agg.Tests[name]; s != nil {
			switch {
			case s.Flaky():
				v.Got = "unknown"
				if c := byName[name]; c != nil {
					v.Got = c.Label
					if c.Polluter != nil {
						v.Polluter = c.Polluter.Polluter
					}
				}
			case s.Failures == s.Runs:
				v.Got = "always fails"
			default:
				v.Got = "stable"
			}
		}
		if e, ok := expected[name]; ok {
			v.Want = e.Cause
			v.Correct = v.Got == e.Cause && (e.Polluter == "" || v.Polluter == e.Polluter)
		}
		out = append(out, v)
	}
	return out
}

// ScoreSummary is the headline numbers across one or more runs.
type ScoreSummary struct {
	FlakyFound, FlakyTotal   int // truly flaky tests that were flagged flaky
	FalsePositives, Stable   int // truly stable tests that were flagged flaky
	CausesRight, CausesTotal int // truly flaky tests with the exact right cause
	PollutersRight, Polluted int // order-dependent tests where the right polluter was named
	Unexpected, NotRun       int // flaky tests missing from the expected file, expected tests that didn't run
}

func Summarize(verdicts []Verdict, expected map[string]Expected) ScoreSummary {
	var s ScoreSummary
	for _, v := range verdicts {
		flagged := v.Got != "stable" && v.Got != "always fails" && v.Got != "not run"
		switch {
		case v.Got == "not run":
			s.NotRun++
		case v.Want == "":
			if flagged {
				s.Unexpected++
			}
		case v.Want == "stable":
			s.Stable++
			if flagged {
				s.FalsePositives++
			}
		default:
			s.FlakyTotal++
			s.CausesTotal++
			if flagged {
				s.FlakyFound++
			}
			if v.Got == v.Want {
				s.CausesRight++
			}
			if p := expected[v.Test].Polluter; p != "" {
				s.Polluted++
				if v.Polluter == p {
					s.PollutersRight++
				}
			}
		}
	}
	return s
}

func (s *ScoreSummary) Add(o ScoreSummary) {
	s.FlakyFound += o.FlakyFound
	s.FlakyTotal += o.FlakyTotal
	s.FalsePositives += o.FalsePositives
	s.Stable += o.Stable
	s.CausesRight += o.CausesRight
	s.CausesTotal += o.CausesTotal
	s.PollutersRight += o.PollutersRight
	s.Polluted += o.Polluted
	s.Unexpected += o.Unexpected
	s.NotRun += o.NotRun
}
