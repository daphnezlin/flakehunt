package main

import "context"

// Trial is one run of the suite.
type Trial struct {
	ID   int
	Seed int64   // pytest-randomly seed (controls order + random data)
	CPUs float64 // cpu limit for this trial, 0 = no limit

	// set on replay and probe trials: the flaky test this rerun is checking.
	// these don't count towards failure rates
	ReplayFor string
	Probe     string   // "polluter" for polluter search runs
	Tests     []string // node ids to run in this order, empty = whole suite
}

type TrialResult struct {
	Trial    Trial
	CPUSet   string // cores the trial was pinned to, "" if not pinned
	Tests    []TestOutcome
	TimedOut bool
	Err      error // infra problem, not a test failure
}

// Runner runs a trial. Docker for now, k8s later.
type Runner interface {
	Run(ctx context.Context, t Trial) TrialResult
}
