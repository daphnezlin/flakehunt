package main

import (
	"context"
	"sort"
	"sync"
)

// RunAll executes trials with at most `workers` running at once.
// Results arrive on the returned channel in completion order; the channel
// closes when every trial is done or ctx is cancelled.
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

	// Feed trials, stopping early if cancelled (e.g. Ctrl+C).
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

// TestStats aggregates one test's outcomes across all trials.
type TestStats struct {
	Name         string
	Runs         int
	Failures     int
	FailingSeeds []int64
}

func (s TestStats) Rate() float64 {
	if s.Runs == 0 {
		return 0
	}
	return float64(s.Failures) / float64(s.Runs)
}

// Flaky means it both passed and failed on the same code.
func (s TestStats) Flaky() bool { return s.Failures > 0 && s.Failures < s.Runs }

// Aggregate is the single place results are combined, so no locking is needed:
// only one goroutine (the caller) ever touches the map.
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
			s = &TestStats{Name: o.Name}
			a.Tests[o.Name] = s
		}
		s.Runs++
		if o.Failed {
			s.Failures++
			s.FailingSeeds = append(s.FailingSeeds, res.Trial.Seed)
		}
	}
}

// Sorted returns tests ordered by failure rate, highest first.
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
