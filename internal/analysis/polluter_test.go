package analysis

import (
	"fmt"
	"strings"
	"testing"

	"github.com/daphnezlin/flakehunt/internal/runner"
)

func outcomes(names ...string) []runner.TestOutcome {
	out := make([]runner.TestOutcome, len(names))
	for i, n := range names {
		out[i] = runner.TestOutcome{Name: n, NodeID: "t.py::" + n}
	}
	return out
}

// fakeSuite fails the victim when every test in breaks ran before it
func fakeSuite(victim string, breaks ...string) prober {
	return func(ids []string) (bool, bool, error) {
		if ids[len(ids)-1] != "t.py::"+victim {
			return false, false, fmt.Errorf("victim should run last")
		}
		ran := strings.Join(ids[:len(ids)-1], " ")
		for _, b := range breaks {
			if !strings.Contains(ran, "t.py::"+b) {
				return false, true, nil
			}
		}
		return len(breaks) > 0, true, nil
	}
}

func TestFindPolluter(t *testing.T) {
	before := outcomes("a", "b", "c", "d", "e", "f", "g", "polluter", "h", "i")
	victim := outcomes("victim")[0]

	res := FindPolluter(victim, before, fakeSuite("victim", "polluter"))
	if res.Polluter != "polluter" || res.PolluterNode != "t.py::polluter" {
		t.Fatalf("got %+v", res)
	}
	// alone + full order + at most 2 per halving
	if res.Probes > 2+2*4 {
		t.Errorf("too many probes: %d", res.Probes)
	}
}

func TestFindPolluterFailsAlone(t *testing.T) {
	alwaysFails := func(ids []string) (bool, bool, error) { return true, true, nil }
	res := FindPolluter(outcomes("victim")[0], outcomes("a", "b"), alwaysFails)
	if !res.FailsAlone || res.Probes != 1 {
		t.Errorf("got %+v", res)
	}
}

func TestFindPolluterNeedsTwo(t *testing.T) {
	// fails only if both "a" and "h" ran first, which halving can't isolate
	res := FindPolluter(outcomes("victim")[0], outcomes("a", "b", "c", "d", "e", "f", "g", "h"),
		fakeSuite("victim", "a", "h"))
	if res.Polluter != "" || !strings.Contains(res.Note, "several") {
		t.Errorf("got %+v", res)
	}
}

func TestFindPolluterCantReproduce(t *testing.T) {
	neverFails := func(ids []string) (bool, bool, error) { return false, true, nil }
	res := FindPolluter(outcomes("victim")[0], outcomes("a", "b"), neverFails)
	if res.FailsAlone || res.Polluter != "" || !strings.Contains(res.Note, "couldn't reproduce") {
		t.Errorf("got %+v", res)
	}
}

func TestFindPolluterNoNodeID(t *testing.T) {
	res := FindPolluter(runner.TestOutcome{Name: "victim"}, nil, nil)
	if res.Note == "" || res.Probes != 0 {
		t.Errorf("got %+v", res)
	}
}
