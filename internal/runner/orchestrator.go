package runner

import (
	"context"
	"math/rand"
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
