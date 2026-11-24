package main

import (
	"context"
	"math/rand"
	"sort"
	"sync"
)

// MakeTrials spreads trials evenly over every (cpu, latency) combination,
// then shuffles so combinations are mixed across the whole run instead of all
// the 0.5 cpu trials running back to back.
//
// Every combination gets the same count, so cpu and latency are independent
// of each other. That's what lets us test each one on its own: the slow cpu
// trials have the same mix of latencies as the fast ones.
func MakeTrials(n int, cpuLevels []float64, latencyLevels []int, baseSeed int64) []Trial {
	if len(cpuLevels) == 0 {
		cpuLevels = []float64{0}
	}
	if len(latencyLevels) == 0 {
		latencyLevels = []int{0}
	}
	type combo struct {
		cpus    float64
		latency int
	}
	var combos []combo
	for _, c := range cpuLevels {
		for _, l := range latencyLevels {
			combos = append(combos, combo{c, l})
		}
	}

	rng := rand.New(rand.NewSource(baseSeed))
	trials := make([]Trial, n)
	for i := range trials {
		c := combos[i%len(combos)]
		trials[i] = Trial{Seed: rng.Int63n(1 << 31), CPUs: c.cpus, LatencyMS: c.latency}
	}
	rng.Shuffle(n, func(i, j int) {
		trials[i].CPUs, trials[j].CPUs = trials[j].CPUs, trials[i].CPUs
		trials[i].LatencyMS, trials[j].LatencyMS = trials[j].LatencyMS, trials[i].LatencyMS
	})
	for i := range trials {
		trials[i].ID = i + 1
	}
	return trials
}

// RunAll runs trials with at most `workers` at once. Results come back in
// completion order and the channel closes when everything's done or ctx is cancelled.
func RunAll(ctx context.Context, r Runner, trials []Trial, workers int) <-chan TrialResult {
	jobs := make(chan Trial)
	results := make(chan TrialResult)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				res := r.Run(ctx, t)
				select {
				case results <- res:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, t := range trials {
			select {
			case jobs <- t:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()
	return results
}

type Count struct {
	Runs     int
	Failures int
}

func (c Count) Rate() float64 {
	if c.Runs == 0 {
		return 0
	}
	return float64(c.Failures) / float64(c.Runs)
}

type TestStats struct {
	Name string
	Count
	ByCPU     map[float64]*Count
	ByLatency map[float64]*Count // keyed by latency ms
	Failing   []Trial            // trials where it failed, for repro commands

	// every test's result from the first failing trial, in the order they ran.
	// the polluter search needs to know what ran before it
	FirstFailure []TestOutcome
}

// passed and failed on the same code
func (s TestStats) Flaky() bool { return s.Failures > 0 && s.Failures < s.Runs }

// Aggregate is only touched by one goroutine, so no locks.
type Aggregate struct {
	Tests      map[string]*TestStats
	Trials     int
	InfraFails int
	Timeouts   int
}

func NewAggregate() *Aggregate { return &Aggregate{Tests: map[string]*TestStats{}} }

func (a *Aggregate) Add(res TrialResult) {
	a.Trials++
	switch {
	case res.TimedOut:
		a.Timeouts++
		return
	case res.Err != nil:
		a.InfraFails++
		return
	}
	for _, o := range res.Tests {
		if o.Skip {
			continue
		}
		s := a.Tests[o.Name]
		if s == nil {
			s = &TestStats{Name: o.Name, ByCPU: map[float64]*Count{}, ByLatency: map[float64]*Count{}}
			a.Tests[o.Name] = s
		}
		cpu := countFor(s.ByCPU, res.Trial.CPUs)
		lat := countFor(s.ByLatency, float64(res.Trial.LatencyMS))
		s.Runs++
		cpu.Runs++
		lat.Runs++
		if o.Failed {
			s.Failures++
			cpu.Failures++
			lat.Failures++
			s.Failing = append(s.Failing, res.Trial)
			if s.FirstFailure == nil {
				s.FirstFailure = res.Tests
			}
		}
	}
}

// Sorted: highest failure rate first.
func (a *Aggregate) Sorted() []*TestStats {
	out := make([]*TestStats, 0, len(a.Tests))
	for _, s := range a.Tests {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rate() != out[j].Rate() {
			return out[i].Rate() > out[j].Rate()
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// CPULevels returns the levels seen for a test, unlimited first then highest to lowest.
func (s TestStats) CPULevels() []float64 {
	levels := make([]float64, 0, len(s.ByCPU))
	for l := range s.ByCPU {
		levels = append(levels, l)
	}
	sort.Slice(levels, func(i, j int) bool {
		if levels[i] == 0 || levels[j] == 0 {
			return levels[i] == 0 && levels[j] != 0
		}
		return levels[i] > levels[j]
	})
	return levels
}

// LatencyLevels returns the latencies seen for a test, lowest (best) first.
func (s TestStats) LatencyLevels() []float64 {
	levels := make([]float64, 0, len(s.ByLatency))
	for l := range s.ByLatency {
		levels = append(levels, l)
	}
	sort.Float64s(levels)
	return levels
}

func countFor(m map[float64]*Count, level float64) *Count {
	c := m[level]
	if c == nil {
		c = &Count{}
		m[level] = c
	}
	return c
}
