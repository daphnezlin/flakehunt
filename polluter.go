package main

import "fmt"

// PolluterResult is what the polluter search found for one seed-dependent test.
type PolluterResult struct {
	FailsAlone   bool   // fails with only itself run, so it's not other tests
	Polluter     string // test name, "" if none found
	PolluterNode string
	VictimNode   string
	Note         string // why the search stopped early, if it did
	Probes       int    // pytest runs used
}

// prober runs exactly these tests in this order with the failing seed and
// reports whether the victim failed. ran is false if the victim had no result.
type prober func(nodeIDs []string) (failed, ran bool, err error)

// FindPolluter works out why victim fails with a given seed. before is every
// test that ran before it in the failing run, in order.
//
//  1. run the victim alone. failing here means other tests aren't the cause
//  2. run everything before it + victim, to make sure the failure reproduces
//  3. binary search: keep whichever half of the earlier tests still makes it fail
//
// Assumes one test is enough to pollute. If neither half fails on its own,
// it needs several tests together and we stop and say so.
func FindPolluter(victim TestOutcome, before []TestOutcome, probe prober) PolluterResult {
	res := PolluterResult{VictimNode: victim.NodeID}
	if victim.NodeID == "" {
		res.Note = "no file path in results, can't rerun single tests"
		return res
	}

	run := func(tests []TestOutcome) (bool, bool) {
		ids := make([]string, 0, len(tests)+1)
		for _, t := range tests {
			ids = append(ids, t.NodeID)
		}
		ids = append(ids, victim.NodeID)
		res.Probes++
		failed, ran, err := probe(ids)
		if err != nil {
			res.Note = fmt.Sprintf("probe run failed: %v", err)
			return false, false
		}
		if !ran {
			res.Note = "test didn't run in a probe, can't continue"
		}
		return failed, ran
	}

	failed, ok := run(nil)
	if !ok {
		return res
	}
	if failed {
		res.FailsAlone = true
		return res
	}

	var cands []TestOutcome
	for _, t := range before {
		if t.NodeID != "" {
			cands = append(cands, t)
		}
	}
	if len(cands) == 0 {
		res.Note = "passes alone but nothing ran before it"
		return res
	}

	failed, ok = run(cands)
	if !ok {
		return res
	}
	if !failed {
		res.Note = "passed when rerun in the same order, couldn't reproduce"
		return res
	}

	for len(cands) > 1 {
		half := len(cands) / 2
		first, second := cands[:half], cands[half:]
		if failed, ok = run(first); !ok {
			return res
		} else if failed {
			cands = first
			continue
		}
		if failed, ok = run(second); !ok {
			return res
		} else if failed {
			cands = second
			continue
		}
		res.Note = "fails only when several earlier tests run together"
		return res
	}

	res.Polluter = cands[0].Name
	res.PolluterNode = cands[0].NodeID
	return res
}
