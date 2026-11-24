package main

import (
	"context"
	"math/rand"
	"sort"
	"sync"
)

// MakeTrials gives each cpu level the same number of trials (as close as
// possible), then shuffles so levels are spread across the whole run instead
// of all the 0.5 trials running back to back. Equal group sizes make the
// per-level comparison fairer.
func MakeTrials(n int, cpuLevels []float64, baseSeed int64) []Trial {
	if len(cpuLevels) == 0 {
		cpuLevels = []float64{0}
	}
	rng := rand.New(rand.NewSource(baseSeed))
	trials := make([]Trial, n)
	for i := range trials {
		trials[i] = Trial{Seed: rng.Int63n(1 << 31), CPUs: cpuLevels[i%len(cpuLevels)]}
	}
	rng.Shuffle(n, func(i, j int) { trials[i].CPUs, trials[j].CPUs = trials[j].CPUs, trials[i].CPUs })
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
	ByCPU   map[float64]*Count
	Failing []Trial // trials where it failed, for repro commands
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
			s = &TestStats{Name: o.Name, ByCPU: map[float64]*Count{}}
			a.Tests[o.Name] = s
		}
		c := s.ByCPU[res.Trial.CPUs]
		if c == nil {
			c = &Count{}
			s.ByCPU[res.Trial.CPUs] = c
		}
		s.Runs++
		c.Runs++
		if o.Failed {
			s.Failures++
			c.Failures++
			s.Failing = append(s.Failing, res.Trial)
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
