package main

import (
	"math"
	"math/rand"
	"sort"
)

const (
	permutations = 10000
	alpha        = 0.05 // after Benjamini-Hochberg correction
)

// Cause is the best guess at why a flaky test is flaky, plus the evidence.
type Cause struct {
	Test      string
	CPUTested bool
	CPUp      float64 // raw p-value for "fails more with less cpu"
	CPUq      float64 // p-value adjusted for testing many tests at once
	Replays   Count   // reruns of a failing seed with no cpu limit
	Label     string
	Detail    string
}

// TrendPValue checks if failures go up as cpu goes down, using a permutation
// test. The statistic is the sum of "slowness rank" over failing runs (0 for
// no limit, 1 for the next level down, ...). If cpu doesn't matter, which runs
// failed is basically random, so we shuffle the failures across runs many times
// and see how often a shuffle looks at least as bad as what we observed.
//
// Permutation instead of a chi-square/normal approximation since counts per
// level are small (10-20 runs). One sided since less cpu making a test pass
// more often isn't something we're looking for.
func TrendPValue(s *TestStats, rng *rand.Rand) float64 {
	levels := s.CPULevels() // fastest to slowest
	if len(levels) < 2 {
		return math.NaN()
	}
	var ranks []int
	observed := 0
	for i, l := range levels {
		c := s.ByCPU[l]
		for j := 0; j < c.Runs; j++ {
			ranks = append(ranks, i)
		}
		observed += i * c.Failures
	}
	n, f := len(ranks), s.Failures
	if f == 0 || f == n {
		return 1
	}

	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	atLeast := 0
	for p := 0; p < permutations; p++ {
		// partial Fisher-Yates: pick f random runs to be the failures
		t := 0
		for k := 0; k < f; k++ {
			j := k + rng.Intn(n-k)
			idx[k], idx[j] = idx[j], idx[k]
			t += ranks[idx[k]]
		}
		if t >= observed {
			atLeast++
		}
	}
	// +1 so we never report p = 0 from a finite number of shuffles
	return float64(atLeast+1) / float64(permutations+1)
}

// BenjaminiHochberg adjusts p-values for running many tests at once. At
// alpha 0.05, testing 100 non-flaky-by-cpu tests would flag ~5 by chance
// without this. Returns q-values in the same order as ps.
func BenjaminiHochberg(ps []float64) []float64 {
	m := len(ps)
	order := make([]int, m)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return ps[order[a]] < ps[order[b]] })

	qs := make([]float64, m)
	min := 1.0
	for rank := m; rank >= 1; rank-- {
		i := order[rank-1]
		q := ps[i] * float64(m) / float64(rank)
		if q < min {
			min = q
		}
		qs[i] = min
	}
	return qs
}

// Analyze works out a cause for every flaky test. replays is keyed by test name.
func Analyze(agg *Aggregate, replays map[string]*Count) []Cause {
	// fixed seed so the same run always gives the same p-values
	rng := rand.New(rand.NewSource(1))

	var causes []Cause
	var ps []float64
	var tested []int
	for _, s := range agg.Sorted() {
		if !s.Flaky() {
			continue
		}
		c := Cause{Test: s.Name}
		if p := TrendPValue(s, rng); !math.IsNaN(p) {
			c.CPUTested, c.CPUp = true, p
			tested = append(tested, len(causes))
			ps = append(ps, p)
		}
		if r := replays[s.Name]; r != nil {
			c.Replays = *r
		}
		causes = append(causes, c)
	}

	qs := BenjaminiHochberg(ps)
	for i, ci := range tested {
		causes[ci].CPUq = qs[i]
	}
	for i := range causes {
		causes[i].Label, causes[i].Detail = classify(causes[i])
	}
	return causes
}

func classify(c Cause) (label, detail string) {
	cpu := c.CPUTested && c.CPUq < alpha
	replayed := c.Replays.Runs > 0
	seed := replayed && c.Replays.Failures == c.Replays.Runs

	switch {
	case seed && cpu:
		return "seed + timing", "fails every time with this seed, and also fails more with less cpu"
	case seed:
		return "seed-dependent", "fails every time with the same seed, so it depends on test order or random data, not timing"
	case cpu:
		return "timing", "fails more with less cpu, likely a race or a timeout that's too tight"
	case replayed:
		return "nondeterministic", "same seed sometimes passes, so not order or random data, and no clear link to cpu (try more runs)"
	default:
		return "unknown", "not enough evidence, try more runs or turn on replays"
	}
}
