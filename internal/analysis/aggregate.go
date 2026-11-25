package analysis

import (
	"github.com/daphnezlin/flakehunt/internal/runner"
	"sort"
)

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
	Failing   []runner.Trial     // trials where it failed, for repro commands

	// every test's result from the first failing trial, in the order they ran.
	// the polluter search needs to know what ran before it
	FirstFailure []runner.TestOutcome
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

func (a *Aggregate) Add(res runner.TrialResult) {
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
