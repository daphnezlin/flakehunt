package analysis

import "math"

// Wilson returns a 95% Wilson score interval for failures/n.
// Using this instead of the normal approximation since it works better
// for small n and rates near 0 (most flaky tests).
func Wilson(failures, n int) (low, high float64) {
	if n == 0 {
		return 0, 1
	}
	const z = 1.959964
	p := float64(failures) / float64(n)
	nf := float64(n)
	denom := 1 + z*z/nf
	center := (p + z*z/(2*nf)) / denom
	margin := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf)) / denom
	return math.Max(0, center-margin), math.Min(1, center+margin)
}

// CatchProbability is the chance that a test failing at `rate` fails at
// least once in n runs.
func CatchProbability(rate float64, n int) float64 {
	return 1 - math.Pow(1-rate, float64(n))
}

// TrialsToCatch is the fewest runs that give at least `prob` chance of seeing
// a test that fails at `rate` fail at least once. 5% flaky at 95% needs 59 runs,
// 1% flaky needs 299.
func TrialsToCatch(rate, prob float64) int {
	if rate <= 0 || rate >= 1 || prob <= 0 || prob >= 1 {
		return 0
	}
	n := math.Ceil(math.Log(1-prob) / math.Log(1-rate))
	// floating point can land a hair above a whole number
	if CatchProbability(rate, int(n)-1) >= prob {
		n--
	}
	return int(n)
}

// CatchableRate is the lowest failure rate that n runs catch with at least
// `prob` chance. It's also the one-sided upper confidence bound on the failure
// rate of a test that passed all n runs (the "rule of three" says about 3/n).
func CatchableRate(n int, prob float64) float64 {
	if n <= 0 {
		return 1
	}
	return 1 - math.Pow(1-prob, 1/float64(n))
}
