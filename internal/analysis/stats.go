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
