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
	CPUq      float64 // p-value adjusted for testing many things at once
	NetTested bool
	NetP      float64 // same, for "fails more with more network latency"
	NetQ      float64
	Replays   Count // reruns of a failing seed with no cpu limit
	Polluter  *PolluterResult
	Label     string
	Detail    string
}

// TrendPValue checks if failures go up as conditions get worse (less cpu, more
// latency), using a permutation test. levels must be ordered best to worst.
// The statistic is the sum of "how bad" rank over failing runs (0 for the best
// level, 1 for the next, ...). If the condition doesn't matter, which runs
// failed is basically random, so we shuffle the failures across runs many times
// and see how often a shuffle looks at least as bad as what we observed.
//
// Permutation instead of a chi-square/normal approximation since counts per
// level are small (10-20 runs). One sided since worse conditions making a test
// pass more often isn't something we're looking for.
func TrendPValue(levels []float64, by map[float64]*Count, rng *rand.Rand) float64 {
	if len(levels) < 2 {
		return math.NaN()
	}
	var ranks []int
	observed, f := 0, 0
	for i, l := range levels {
		c := by[l]
		for j := 0; j < c.Runs; j++ {
			ranks = append(ranks, i)
		}
		observed += i * c.Failures
		f += c.Failures
	}
	n := len(ranks)
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

// BenjaminiHochberg adjusts p-values for checking many things at once. At
// alpha 0.05, checking 100 tests that don't really depend on cpu would flag ~5
// by chance without this. Returns q-values in the same order as ps.
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

// Analyze works out a cause for every flaky test. replays and polluters are
// keyed by test name and either can be nil.
func Analyze(agg *Aggregate, replays map[string]*Count, polluters map[string]PolluterResult) []Cause {
	// fixed seed so the same run always gives the same p-values
	rng := rand.New(rand.NewSource(1))

	var causes []Cause
	// every p-value from every test and condition goes through one correction,
	// since together they're all the checks we ran
	var ps []float64
	var setQ []func(q float64)
	for _, s := range agg.Sorted() {
		if !s.Flaky() {
			continue
		}
		c := Cause{Test: s.Name}
		i := len(causes)
		if p := TrendPValue(s.CPULevels(), s.ByCPU, rng); !math.IsNaN(p) {
			c.CPUTested, c.CPUp = true, p
			ps = append(ps, p)
			setQ = append(setQ, func(q float64) { causes[i].CPUq = q })
		}
		if p := TrendPValue(s.LatencyLevels(), s.ByLatency, rng); !math.IsNaN(p) {
			c.NetTested, c.NetP = true, p
			ps = append(ps, p)
			setQ = append(setQ, func(q float64) { causes[i].NetQ = q })
		}
		if r := replays[s.Name]; r != nil {
			c.Replays = *r
		}
		if p, ok := polluters[s.Name]; ok {
			c.Polluter = &p
		}
		causes = append(causes, c)
	}

	for i, q := range BenjaminiHochberg(ps) {
		setQ[i](q)
	}
	for i := range causes {
		causes[i].Label, causes[i].Detail = classify(causes[i])
	}
	return causes
}

func classify(c Cause) (label, detail string) {
	cpu := c.CPUTested && c.CPUq < alpha
	net := c.NetTested && c.NetQ < alpha
	replayed := c.Replays.Runs > 0
	seed := replayed && c.Replays.Failures == c.Replays.Runs

	switch {
	case seed && (cpu || net):
		return "seed + timing", "fails every time with this seed, and also fails more with less cpu or a slower network"
	case seed && c.Polluter != nil:
		return classifySeed(*c.Polluter)
	case seed:
		return "seed-dependent", "fails every time with the same seed, so it depends on test order or random data, not timing"
	case cpu && net:
		return "timing + network", "fails more with less cpu and with a slower network, likely a timeout that's too tight"
	case net:
		return "network", "fails more when the service responds slowly, likely a network timeout that's too tight"
	case cpu:
		return "timing", "fails more with less cpu, likely a race or a timeout that's too tight"
	case replayed:
		return "nondeterministic", "same seed sometimes passes, so not order or random data, and no clear link to cpu or network (try more runs)"
	default:
		return "unknown", "not enough evidence, try more runs or turn on replays"
	}
}

// classifySeed splits seed-dependent tests into random data vs order.
func classifySeed(p PolluterResult) (label, detail string) {
	switch {
	case p.FailsAlone:
		return "random data", "fails even when run on its own with this seed, so it's the random values the seed produces, not other tests"
	case p.Polluter != "":
		return "order-dependent", "fails when it runs after " + p.Polluter + ", which leaves state behind"
	case p.Note != "":
		return "order-dependent", "passes on its own but fails in the full run order (" + p.Note + ")"
	default:
		return "order-dependent", "passes on its own but fails in the full run order"
	}
}
