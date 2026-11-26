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

// hintP: a raw trend p-value below this counts as a hint that a test might be
// cpu or network sensitive. It's deliberately loose, since it's never used for
// a verdict, only to decide what focus trials vary and which tests are still
// worth focusing on.
const hintP = 0.5

// minFocusBatches: a test gets at least this many focus batches before it can
// be dropped for showing no hint of an effect. Right after detection there are
// only about 20 trials per cpu level, and a real but small effect often doesn't
// show yet: in simulations, dropping tests straight away lost about a third of
// the 19% vs 33% effects that a few more batches would have caught.
const minFocusBatches = 2

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

	focused map[string]int  // focus batches each test has been in
	dropped map[string]bool // tests no longer focused for showing no effect
}

func New(cfg Config, db *store.Store, runID int64, rn runner.Runner) *Campaign {
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	return &Campaign{cfg: cfg, db: db, runID: runID, rn: rn,
		focused: map[string]int{}, dropped: map[string]bool{}}
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

// replayRounds is how many rounds of replays a test can get at most.
const replayRounds = 3

// replay reruns each flaky test's first failing seed with no cpu limit or
// latency. If it fails every time, the seed (test order or random data) may be
// enough to cause it. If it sometimes passes, something else is involved.
//
// "Failed every replay" only means something if the test usually passes: one
// that fails 90% of the time anyway fails 5 replays in a row 59% of the time.
// So when every replay so far failed but that's not convincing yet given how
// often the test fails, it gets another round, up to replayRounds. A test the
// seed really breaks keeps failing and the evidence adds up quickly.
func (c *Campaign) replay(ctx context.Context) error {
	var targets []string
	for _, s := range c.res.Agg.Sorted() {
		if s.Flaky() {
			targets = append(targets, s.Name)
		}
	}

	for round := 1; round <= replayRounds && len(targets) > 0 && ctx.Err() == nil; round++ {
		var trials []runner.Trial
		for _, name := range targets {
			seed := c.res.Agg.Tests[name].Failing[0].Seed
			for i := 0; i < c.cfg.Replays; i++ {
				c.lastID++
				trials = append(trials, runner.Trial{ID: c.lastID, Seed: seed, ReplayFor: name})
			}
		}
		if round == 1 {
			fmt.Fprintln(c.cfg.Log, "  replaying failing seeds")
		} else {
			fmt.Fprintf(c.cfg.Log, "  more replays for %d test(s) that failed every replay but fail often anyway\n", len(targets))
		}
		if err := c.runBatch(ctx, trials, replay, "replays"); err != nil {
			return err
		}

		targets = nil
		for _, cause := range analysis.AnalyzeAt(c.res.Agg, c.res.Replays, nil, c.res.Alpha) {
			r := cause.Replays
			if r.Runs > 0 && r.Failures == r.Runs && !cause.SeedDetermined() {
				targets = append(targets, cause.Test)
			}
		}
	}
	return nil
}

// findPolluters runs FindPolluter for every seed-dependent test. Probes run
// one at a time since each step of the search depends on the last one.
func (c *Campaign) findPolluters(ctx context.Context) error {
	// every test whose failing seed failed every replay, even if a trend test
	// also flagged cpu or latency: a chance trend shouldn't stop us finding the
	// real cause and a minimal repro
	for _, cause := range analysis.AnalyzeAt(c.res.Agg, c.res.Replays, nil, c.res.Alpha) {
		if !cause.SeedDetermined() {
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
// Focused trials only use the mildest and harshest cpu limit (and latency, if
// varied). That's the most efficient way to detect a trend: the middle levels
// add trials but barely any evidence, since the difference between the ends is
// what the test picks up.
//
// Tests that still show no hint of a cpu or network effect after
// minFocusBatches are dropped, so a test that's flaky for some other reason
// doesn't use up the whole budget. Dropping a test early can only make a false
// "timing" or "network" less likely, so the alpha from Config.Alpha still holds.
//
// Order-dependent and random-data tests never get here: replays and the
// polluter search already explain them, and running them without the rest of
// the suite would hide order problems anyway.
func (c *Campaign) focus(ctx context.Context) error {
	cpus := cpuExtremes(c.cfg.CPULevels)
	for batch := 1; batch <= c.cfg.FocusLooks; batch++ {
		groups, dropped := c.focusTargets()
		for _, name := range dropped {
			fmt.Fprintf(c.cfg.Log, "  stopped focusing on %s: no sign of a cpu or network effect after %d batches\n",
				name, c.focused[name])
		}
		if len(groups) == 0 || ctx.Err() != nil {
			return nil
		}

		var trials []runner.Trial
		var n, noNet int
		for gi, g := range groups {
			// each group gets a full batch, so splitting tests up doesn't
			// leave any of them with fewer trials than before
			ts := runner.MakeTrials(c.cfg.FocusBatch, cpus, g.latencies, c.cfg.BaseSeed+int64(batch)*100+int64(gi))
			for i := range ts {
				c.lastID++
				ts[i].ID = c.lastID
				ts[i].Tests = g.nodeIDs
				ts[i].Probe = "focus"
			}
			trials = append(trials, ts...)
			n += len(g.names)
			if len(g.latencies) < 2 {
				noNet += len(g.names)
			}
		}
		note := ""
		if noNet > 0 && len(c.cfg.LatencyLevels) > 1 {
			note = fmt.Sprintf(", %d without latency (no sign of a network effect)", noNet)
		}
		fmt.Fprintf(c.cfg.Log, "  focus batch %d/%d on %d unclear test(s)%s\n", batch, c.cfg.FocusLooks, n, note)
		if err := c.runBatch(ctx, trials, focus, "focused trials"); err != nil {
			return err
		}
		c.res.Stats.FocusBatches = batch
		for _, g := range groups {
			for _, name := range g.names {
				c.focused[name]++
			}
		}
	}
	return nil
}

// focusGroup is a set of tests that share focus trials.
type focusGroup struct {
	names, nodeIDs []string
	latencies      []int
}

// focusTargets is every flaky test that's still "nondeterministic" or
// "unknown" at this check, that we can rerun on its own, and that's still
// worth focusing on. A cause only counts as found at an early check if it
// clears analysis.InterimAlpha.
//
// Tests with a hint of a network effect go in one group, with trials at no
// latency and the highest latency. The rest go in another with no latency at
// all, so their trials skip toxiproxy (one less container each) and all go to
// the cpu comparison. dropped is the tests dropped at this check.
func (c *Campaign) focusTargets() (groups []focusGroup, dropped []string) {
	net := focusGroup{latencies: latencyExtremes(c.cfg.LatencyLevels)}
	still := focusGroup{latencies: []int{0}}
	if len(c.cfg.LatencyLevels) == 0 {
		still.latencies = nil
	}

	causes := analysis.AnalyzeAt(c.res.Agg, c.res.Replays, c.res.Polluters, analysis.InterimAlpha)
	for _, cause := range causes {
		if cause.Label != "nondeterministic" && cause.Label != "unknown" {
			continue
		}
		if c.dropped[cause.Test] {
			continue
		}
		victim, _ := splitAt(c.res.Agg.Tests[cause.Test].FirstFailure, cause.Test)
		if victim.NodeID == "" {
			continue
		}
		cpuHint := cause.CPUTested && cause.CPUp < hintP
		netHint := cause.NetTested && cause.NetP < hintP
		if !cpuHint && !netHint && c.focused[cause.Test] >= minFocusBatches {
			c.dropped[cause.Test] = true
			dropped = append(dropped, cause.Test)
			continue
		}
		g := &still
		if len(c.cfg.LatencyLevels) > 1 && (netHint || !cause.NetTested) {
			g = &net
		}
		g.names = append(g.names, cause.Test)
		g.nodeIDs = append(g.nodeIDs, victim.NodeID)
	}
	for _, g := range []focusGroup{net, still} {
		if len(g.names) > 0 {
			groups = append(groups, g)
		}
	}
	return groups, dropped
}

// cpuExtremes is the cpu levels that slow tests down the least and the most,
// in config order. If every level is equally slow, all of them.
func cpuExtremes(levels []float64) []float64 {
	if len(levels) < 2 {
		return levels
	}
	lo, hi := levels[0], levels[0]
	for _, l := range levels {
		if analysis.Slowdown(l) < analysis.Slowdown(lo) {
			lo = l
		}
		if analysis.Slowdown(l) > analysis.Slowdown(hi) {
			hi = l
		}
	}
	if analysis.Slowdown(lo) == analysis.Slowdown(hi) {
		return levels
	}
	return []float64{lo, hi}
}

// latencyExtremes is the lowest and highest latency.
func latencyExtremes(levels []int) []int {
	if len(levels) < 2 {
		return levels
	}
	lo, hi := levels[0], levels[0]
	for _, l := range levels {
		lo, hi = min(lo, l), max(hi, l)
	}
	return []int{lo, hi}
}

func splitAt(outcomes []runner.TestOutcome, name string) (runner.TestOutcome, []runner.TestOutcome) {
	for i, o := range outcomes {
		if o.Name == name {
			return o, outcomes[:i]
		}
	}
	return runner.TestOutcome{Name: name}, nil
}
