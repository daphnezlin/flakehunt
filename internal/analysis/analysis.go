package analysis

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
)

const (
	permutations = 10000
	// DefaultAlpha is the significance threshold after Benjamini-Hochberg
	// correction, for a run that only checks once at the end
	DefaultAlpha = 0.05
	// InterimAlpha is the bar for an early check to count a cause as found
	// (see FinalAlpha)
	InterimAlpha = 0.001
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
	Hangs     int // failures that hit the per-test timeout
	Failures  int
	BaseRate  float64 // how often it fails in full-suite trials, whatever the seed
	Alpha     float64 // threshold the q-values were compared against
	Label     string
	Detail    string
}

// TrendPValue checks if failures go up as conditions get worse (less cpu, more
// latency), using a permutation test. scores[i] says how bad levels[i] is, and
// the statistic is the sum of scores over failing runs. If the condition doesn't matter, which runs
// failed is basically random, so we shuffle the failures across runs many times
// and see how often a shuffle looks at least as bad as what we observed.
//
// Shuffles only happen within a stratum (e.g. full-suite trials vs focused
// trials of one test). A test can fail at a different base rate when it runs
// alone, and shuffling across both kinds of trial would mix that difference
// into the cpu/latency comparison. Within each stratum, every trial ran in the
// same context, so only the condition differs. With one stratum this is the
// plain permutation test.
//
// Permutation instead of a chi-square/normal approximation since counts per
// level are small (10-20 runs). One sided since worse conditions making a test
// pass more often isn't something we're looking for.
func TrendPValue(levels, scores []float64, strata []map[float64]*Count, rng *rand.Rand) float64 {
	if distinct(scores) < 2 {
		// every level is equally bad, nothing to compare
		return math.NaN()
	}
	type group struct {
		ranks []float64
		idx   []int
		f     int
	}
	var groups []group
	observed, fixed := 0.0, 0.0 // fixed: part of the statistic shuffling can't change
	for _, by := range strata {
		var g group
		for i, l := range levels {
			c := by[l]
			if c == nil {
				continue
			}
			for j := 0; j < c.Runs; j++ {
				g.ranks = append(g.ranks, scores[i])
			}
			observed += scores[i] * float64(c.Failures)
			g.f += c.Failures
		}
		n := len(g.ranks)
		if g.f == 0 || g.f == n {
			// all passed or all failed: every shuffle gives the same sum
			for _, r := range g.ranks {
				if g.f == n {
					fixed += r
				}
			}
			continue
		}
		g.idx = make([]int, n)
		for i := range g.idx {
			g.idx[i] = i
		}
		groups = append(groups, g)
	}
	if len(groups) == 0 {
		return 1
	}

	atLeast := 0
	for p := 0; p < permutations; p++ {
		t := fixed
		for gi := range groups {
			g := &groups[gi]
			n := len(g.ranks)
			// partial Fisher-Yates: pick f random runs in this stratum to be the failures
			for k := 0; k < g.f; k++ {
				j := k + rng.Intn(n-k)
				g.idx[k], g.idx[j] = g.idx[j], g.idx[k]
				t += g.ranks[g.idx[k]]
			}
		}
		if t >= observed-1e-9 {
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
	return AnalyzeAt(agg, replays, polluters, DefaultAlpha)
}

// AnalyzeAt is Analyze with a custom significance threshold. Runs that check
// several times and stop early need a stricter one (see FinalAlpha).
func AnalyzeAt(agg *Aggregate, replays map[string]*Count, polluters map[string]PolluterResult, alpha float64) []Cause {
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
		c := Cause{Test: s.Name, Alpha: alpha, Hangs: s.Hangs, Failures: s.Failures, BaseRate: s.FullSuiteRate()}
		i := len(causes)
		if p := TrendPValue(s.CPULevels(), CPUScores(s.CPULevels()), strata(s.StrataCPU), rng); !math.IsNaN(p) {
			c.CPUTested, c.CPUp = true, p
			ps = append(ps, p)
			setQ = append(setQ, func(q float64) { causes[i].CPUq = q })
		}
		if p := TrendPValue(s.LatencyLevels(), Ranks(len(s.LatencyLevels())), strata(s.StrataLatency), rng); !math.IsNaN(p) {
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
	cpu := c.CPUSensitive()
	net := c.NetworkSensitive()
	replayed := c.Replays.Runs > 0
	seed := c.SeedDetermined()
	// the polluter search reruns the exact failing order. if that passed, the
	// seed can't be what makes it fail, whatever the replays looked like
	unreproduced := seed && c.Polluter != nil && c.Polluter.Unreproduced
	if unreproduced {
		seed = false
	}
	label, detail = classifyOther(c, cpu, net, replayed)
	if unreproduced {
		detail += " (its failing seed failed every replay, but rerunning that exact order passed, so the seed isn't the cause)"
	}
	if !seed {
		return label, detail
	}
	if c.Hung() {
		// timing out on most failures is what the failure looks like, seen
		// directly. the seed and polluter search say what sets it off, which
		// is useful but weaker evidence, so it goes in the detail instead
		return label, detail + hangTrigger(c.Polluter)
	}

	switch {
	case seed && c.Polluter != nil:
		// the seed reproduced every failure with no cpu limit or latency, and
		// conditions are assigned at random, independently of the seed. so a
		// cpu or latency trend on top of that can only be chance: note it,
		// but go with what the replays and polluter search showed
		label, detail := classifySeed(*c.Polluter)
		if cpu || net {
			detail += fmt.Sprintf(" (it also failed more with %s, likely chance since the seed alone reproduces every failure)",
				trendName(cpu, net))
		}
		return label, detail
	case seed && (cpu || net):
		return "seed + timing", "fails every time with this seed, and also fails more with less cpu or a slower network"
	case seed:
		return "seed-dependent", "fails every time with the same seed, so it depends on test order or random data, not timing"
	}
	return label, detail
}

// classifyOther handles tests whose failures the seed doesn't explain.
func classifyOther(c Cause, cpu, net, replayed bool) (label, detail string) {
	switch {
	case c.Hung():
		// checked before cpu and network: a hang is a stronger, more specific
		// signal, and slow conditions can make a deadlock more or less likely
		detail := fmt.Sprintf("never finished in %d of its %d failures (hit the per-test timeout), likely a deadlock "+
			"or waiting on something that doesn't come", c.Hangs, c.Failures)
		if cpu || net {
			detail += fmt.Sprintf(", and happened more with %s", trendName(cpu, net))
		}
		return "hang", detail
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

// hangTrigger says what the seed evidence adds to a hang.
func hangTrigger(p *PolluterResult) string {
	switch {
	case p == nil:
		return " (its failing seed also failed every replay)"
	case p.FailsAlone:
		return " (its failing seed makes it fail even when run on its own)"
	case p.Polluter != "":
		return " (it hangs when it runs after " + p.Polluter + ")"
	default:
		return " (its failing seed failed every replay but it passes on its own, so earlier tests may make the hang more likely)"
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

// Hung is true when most of a test's failures were the per-test timeout.
func (c Cause) Hung() bool { return c.Hangs > 0 && 2*c.Hangs >= c.Failures }

// SeedP is the chance of the replays all failing if the seed had nothing to do
// with it: a test that fails at BaseRate anyway fails n replays in a row with
// probability BaseRate^n. A test that fails 90% of the time fails 5 replays in
// a row 59% of the time, so "failed every replay" alone proves nothing for it.
func (c Cause) SeedP() float64 {
	return math.Pow(c.BaseRate, float64(c.Replays.Runs))
}

// SeedDetermined is true when rerunning the failing seed failed every time,
// i.e. test order or random data alone is enough to make it fail.
func (c Cause) SeedDetermined() bool {
	return c.Replays.Runs > 0 && c.Replays.Failures == c.Replays.Runs && c.SeedP() < DefaultAlpha
}

func trendName(cpu, net bool) string {
	switch {
	case cpu && net:
		return "less cpu and more latency"
	case cpu:
		return "less cpu"
	default:
		return "more latency"
	}
}

// CPUSensitive and NetworkSensitive are true when the trend test found a real effect.
func (c Cause) CPUSensitive() bool     { return c.CPUTested && c.CPUq < c.threshold() }
func (c Cause) NetworkSensitive() bool { return c.NetTested && c.NetQ < c.threshold() }

func (c Cause) threshold() float64 {
	if c.Alpha == 0 {
		return DefaultAlpha
	}
	return c.Alpha
}

// FinalAlpha is the threshold for the last check of a run that also checked
// `interim` times before it and could have stopped at any of them.
//
// Checking after every batch and stopping as soon as something looks
// significant gives chance more opportunities to produce a false positive.
// This uses a Haybittle-Peto boundary: early checks need very strong evidence
// (InterimAlpha) to stop, so almost all of the error budget is left for the
// final check. Taking the interim budget off the final threshold keeps the
// overall false positive rate at or below DefaultAlpha (union bound).
//
// Splitting alpha evenly across checks (Bonferroni) also works, but it made
// the final check so strict that a real cpu effect was missed in testing.
func FinalAlpha(interim int) float64 {
	if interim < 0 {
		interim = 0
	}
	return DefaultAlpha - InterimAlpha*float64(interim)
}

// strata turns per-stratum counts into a list with a stable order, so the
// same data always gives the same p-value.
func strata(m map[string]map[float64]*Count) []map[float64]*Count {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]map[float64]*Count, len(keys))
	for i, k := range keys {
		out[i] = m[k]
	}
	return out
}

// CausesByName indexes causes by test name.
func CausesByName(causes []Cause) map[string]*Cause {
	m := map[string]*Cause{}
	for i := range causes {
		m[causes[i].Test] = &causes[i]
	}
	return m
}

// Slowdown is how much slower a cpu limit makes a test compared to one full
// core: 2 for 0.5, 4 for 0.25. Limits of 1 or more, and no limit (0), are all
// 1. Python test code holds the GIL, so it can't run on more than one core at
// a time, and extra cores don't make it any faster. Below one core it gets
// throttled, so it takes 1/cpus as long.
//
// Scoring cpu levels this way instead of as evenly spaced steps matches how
// timing bugs behave: a test that fails when its work takes too long fails
// about twice as often at 0.5, and about the same with no limit or 1.
func Slowdown(cpus float64) float64 {
	if cpus <= 0 || cpus >= 1 {
		return 1
	}
	return 1 / cpus
}

// CPUScores scores cpu levels by Slowdown.
func CPUScores(levels []float64) []float64 {
	scores := make([]float64, len(levels))
	for i, l := range levels {
		scores[i] = Slowdown(l)
	}
	return scores
}

// Ranks scores n levels as evenly spaced steps 0, 1, 2, ... Latency uses this:
// there's no general rule for how latency turns into failures, a timeout can
// fail at 150ms and never at 50ms.
func Ranks(n int) []float64 {
	r := make([]float64, n)
	for i := range r {
		r[i] = float64(i)
	}
	return r
}

func distinct(xs []float64) int {
	seen := map[float64]bool{}
	for _, x := range xs {
		seen[x] = true
	}
	return len(seen)
}
