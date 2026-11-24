package main

import "context"

// Trial is one run of the test suite under one set of conditions.
type Trial struct {
	ID   int
	Seed int64 // passed to pytest-randomly, so the run's order and random data are reproducible
}

// TrialResult is what came back from one trial.
type TrialResult struct {
	Trial    Trial
	Tests    []TestOutcome
	TimedOut bool
	Err      error // infrastructure problem (couldn't start, no results file): NOT a test failure
}

// Runner executes a trial somewhere. Phase 1-3: Docker. Phase 4+: Kubernetes.
// The orchestrator only depends on this interface, so adding a backend
// never changes the scheduling or analysis code.
type Runner interface {
	Run(ctx context.Context, t Trial) TrialResult
}
