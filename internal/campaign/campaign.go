// Package campaign runs one flakehunt run from start to finish:
//
//  1. detect: full-suite trials spread over every condition, to find flaky tests
//  2. replay: rerun each flaky test's failing seed, to see if the seed alone causes it
//  3. polluters: for seed-dependent tests, find the earlier test that breaks them
//  4. focus: for flaky tests whose cause still isn't clear, run extra trials of
//     only those tests, a batch at a time, until the cause is clear or the
//     budget runs out
//
// Results are only ever handled on the calling goroutine, so it's the only
// writer to the db and the aggregate needs no locks.
package campaign

import (
	"context"
	"fmt"
	"io"

	"github.com/daphnezlin/flakehunt/internal/analysis"
	"github.com/daphnezlin/flakehunt/internal/runner"
	"github.com/daphnezlin/flakehunt/internal/store"
)

type Config struct {
	DetectTrials  int // full-suite trials
	Workers       int
	CPULevels     []float64
	LatencyLevels []int
	BaseSeed      int64
	Replays       int  // reruns per flaky test's failing seed, 0 = off
	FindPolluters bool // polluter search for seed-dependent tests
	FocusBatch    int  // trials per focus batch, 0 = no focus stage
	FocusLooks    int  // most focus batches to run
	Log           io.Writer
}

func (c Config) focusOn() bool { return c.FocusBatch > 0 && c.FocusLooks > 0 }

// Alpha is the threshold for the final verdicts. With focus on, a test can
// stop being focused at the check after detection or after any batch but the
// last, so there are FocusLooks early checks, each using analysis.InterimAlpha.
func (c Config) Alpha() float64 {
	if !c.focusOn() {
		return analysis.DefaultAlpha
	}
	return analysis.FinalAlpha(c.FocusLooks)
}

// networkHint: a raw latency trend p-value below this counts as a hint that a
// test might be network sensitive. It's deliberately loose, only used to decide
// whether focus trials keep varying latency.
const networkHint = 0.5

// Stats is what a run cost.
type Stats struct {
	Detect, Replay, Probe, Focus int // trials per stage
	FocusBatches                 int // focus batches actually run
	TestExecutions               int // tests run across every trial, the real cost on a big suite
}

func (s Stats) Trials() int { return s.Detect + s.Replay + s.Probe + s.Focus }

type Result struct {
	Agg       *analysis.Aggregate
	Replays   map[string]*analysis.Count
	Polluters map[string]analysis.PolluterResult
	Alpha     float64
	Stats     Stats
}

func (r Result) Causes() []analysis.Cause {
	return analysis.AnalyzeAt(r.Agg, r.Replays, r.Polluters, r.Alpha)
}

type stage int

const (
	detect stage = iota
	replay
	probe
	focus
)

type Campaign struct {
	cfg    Config
	db     *store.Store
	runID  int64
	rn     runner.Runner
	lastID int // highest trial id used so far
	res    Result
}

func New(cfg Config, db *store.Store, runID int64, rn runner.Runner) *Campaign {
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	return &Campaign{cfg: cfg, db: db, runID: runID, rn: rn}
}

// Run does every stage. If ctx is cancelled it stops early and returns what
// it has; everything finished so far is already saved.
func (c *Campaign) Run(ctx context.Context) (Result, error) {
	c.res = Result{
		Agg:       analysis.NewAggregate(),
		Replays:   map[string]*analysis.Count{},
		Polluters: map[string]analysis.PolluterResult{},
		Alpha:     c.cfg.Alpha(),
	}

	trials := runner.MakeTrials(c.cfg.DetectTrials, c.cfg.CPULevels, c.cfg.LatencyLevels, c.cfg.BaseSeed)
	c.lastID = len(trials)
	if err := c.runBatch(ctx, trials, detect, "trials"); err != nil {
		return c.res, err
	}

	steps := []func(context.Context) error{}
	if c.cfg.Replays > 0 {
		steps = append(steps, c.replay)
	}
	if c.cfg.FindPolluters {
		steps = append(steps, c.findPolluters)
	}
	if c.cfg.focusOn() {
		steps = append(steps, c.focus)
	}
	for _, step := range steps {
		if ctx.Err() != nil {
			break
		}
		if err := step(ctx); err != nil {
			return c.res, err
		}
	}
	return c.res, nil
}

// runBatch runs trials in parallel and records each result as it comes in.
func (c *Campaign) runBatch(ctx context.Context, trials []runner.Trial, st stage, what string) error {
	if len(trials) == 0 {
		return nil
	}
	done := 0
	for res := range runner.RunAll(ctx, c.rn, trials, c.cfg.Workers) {
		if err := c.record(res, st); err != nil {
			return err
		}
		done++
		if res.Err != nil {
			fmt.Fprintf(c.cfg.Log, "\n  trial %d: infrastructure error: %v\n", res.Trial.ID, res.Err)
		}
		fmt.Fprintf(c.cfg.Log, "\r  %d/%d %s done", done, len(trials), what)
	}
	fmt.Fprintln(c.cfg.Log)
	return nil
}

func (c *Campaign) record(res runner.TrialResult, st stage) error {
	if err := c.db.SaveTrial(c.runID, res); err != nil {
		return fmt.Errorf("save trial %d: %w", res.Trial.ID, err)
	}
	s := &c.res.Stats
	s.TestExecutions += len(res.Tests)
	switch st {
	case detect:
		s.Detect++
		c.res.Agg.Add(res)
	case focus:
		s.Focus++
		c.res.Agg.Add(res)
	case replay:
		s.Replay++
		countReplay(c.res.Replays, res)
	case probe:
		s.Probe++
	}
	return nil
}

// countReplay counts how the target test did. Same counting as
// store.LoadReplays so live and saved reports match.
func countReplay(counts map[string]*analysis.Count, res runner.TrialResult) {
	for _, o := range res.Tests {
		if o.Name != res.Trial.ReplayFor || o.Skip {
			continue
		}
		cnt := counts[o.Name]
		if cnt == nil {
			cnt = &analysis.Count{}
			counts[o.Name] = cnt
		}
		cnt.Runs++
		if o.Failed {
			cnt.Failures++
		}
	}
}

// replay reruns each flaky test's first failing seed with no cpu limit or
// latency. If it fails every time, the seed (test order or random data) is
// enough to cause it. If it sometimes passes, something else is involved.
func (c *Campaign) replay(ctx context.Context) error {
	var trials []runner.Trial
	for _, s := range c.res.Agg.Sorted() {
		if !s.Flaky() {
			continue
		}
		for i := 0; i < c.cfg.Replays; i++ {
			c.lastID++
			trials = append(trials, runner.Trial{ID: c.lastID, Seed: s.Failing[0].Seed, ReplayFor: s.Name})
		}
	}
	if len(trials) > 0 {
		fmt.Fprintln(c.cfg.Log, "  replaying failing seeds")
	}
	return c.runBatch(ctx, trials, replay, "replays")
}

// findPolluters runs FindPolluter for every seed-dependent test. Probes run
// one at a time since each step of the search depends on the last one.
func (c *Campaign) findPolluters(ctx context.Context) error {
	for _, cause := range analysis.AnalyzeAt(c.res.Agg, c.res.Replays, nil, c.res.Alpha) {
		if cause.Label != "seed-dependent" {
			continue
		}
		s := c.res.Agg.Tests[cause.Test]
		seed := s.Failing[0].Seed
		victim, before := splitAt(s.FirstFailure, cause.Test)

		fmt.Fprintf(c.cfg.Log, "  searching for what breaks %s\n", cause.Test)
		var saveErr error
		probeFn := func(nodeIDs []string) (bool, bool, error) {
			if ctx.Err() != nil {
				return false, false, ctx.Err()
			}
			c.lastID++
			res := c.rn.Run(ctx, runner.Trial{ID: c.lastID, Seed: seed, ReplayFor: cause.Test, Probe: "polluter", Tests: nodeIDs})
			if err := c.record(res, probe); err != nil {
				saveErr = err
				return false, false, err
			}
			if res.Err != nil {
				return false, false, res.Err
			}
			if res.TimedOut {
				return false, false, fmt.Errorf("timed out")
			}
			for _, o := range res.Tests {
				if o.Name == cause.Test && !o.Skip {
					return o.Failed, true, nil
				}
			}
			return false, false, nil
		}

		result := analysis.FindPolluter(victim, before, probeFn)
		if saveErr != nil {
			return saveErr
		}
		if err := c.db.SavePolluter(c.runID, cause.Test, result); err != nil {
			return err
		}
		c.res.Polluters[cause.Test] = result
	}
	return nil
}

// focus runs batches of trials containing only the tests whose cause is
// still unclear, checking after each batch and stopping as soon as every one
// is explained or the batch budget runs out. Each focused trial runs 1 or 2
// tests instead of the whole suite, which is where the savings come from on a
// real project.
//
// Order-dependent and random-data tests never get here: replays and the
// polluter search already explain them, and running them without the rest of
// the suite would hide order problems anyway.
func (c *Campaign) focus(ctx context.Context) error {
	for batch := 1; batch <= c.cfg.FocusLooks; batch++ {
		names, nodeIDs, latencies := c.focusTargets()
		if len(names) == 0 || ctx.Err() != nil {
			return nil
		}
		trials := runner.MakeTrials(c.cfg.FocusBatch, c.cfg.CPULevels, latencies, c.cfg.BaseSeed+int64(batch))
		for i := range trials {
			c.lastID++
			trials[i].ID = c.lastID
			trials[i].Tests = nodeIDs
			trials[i].Probe = "focus"
		}
		note := ""
		if len(latencies) < len(c.cfg.LatencyLevels) {
			note = ", no latency (no sign of a network effect)"
		}
		fmt.Fprintf(c.cfg.Log, "  focus batch %d/%d on %d unclear test(s)%s\n", batch, c.cfg.FocusLooks, len(names), note)
		if err := c.runBatch(ctx, trials, focus, "focused trials"); err != nil {
			return err
		}
		c.res.Stats.FocusBatches = batch
	}
	return nil
}

// focusTargets is every flaky test that's still "nondeterministic" or
// "unknown" at this check, and that we can rerun on its own. A cause only
// counts as found at an early check if it clears analysis.InterimAlpha.
//
// latencies is the latency levels the next batch should use. If none of the
// targets shows even a hint of a network effect, latency is dropped, so focus
// trials skip toxiproxy (one less container each) and every trial goes to
// the cpu comparison.
func (c *Campaign) focusTargets() (names, nodeIDs []string, latencies []int) {
	latencies = []int{0}
	if len(c.cfg.LatencyLevels) == 0 {
		latencies = nil
	}
	causes := analysis.AnalyzeAt(c.res.Agg, c.res.Replays, c.res.Polluters, analysis.InterimAlpha)
	for _, cause := range causes {
		if cause.Label != "nondeterministic" && cause.Label != "unknown" {
			continue
		}
		victim, _ := splitAt(c.res.Agg.Tests[cause.Test].FirstFailure, cause.Test)
		if victim.NodeID == "" {
			continue
		}
		names = append(names, cause.Test)
		nodeIDs = append(nodeIDs, victim.NodeID)
		if !cause.NetTested || cause.NetP < networkHint {
			latencies = c.cfg.LatencyLevels
		}
	}
	return names, nodeIDs, latencies
}

// splitAt finds name in a trial's results and returns it plus everything that ran before it.
func splitAt(outcomes []runner.TestOutcome, name string) (runner.TestOutcome, []runner.TestOutcome) {
	for i, o := range outcomes {
		if o.Name == name {
			return o, outcomes[:i]
		}
	}
	return runner.TestOutcome{Name: name}, nil
}
