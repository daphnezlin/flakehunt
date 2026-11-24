package main

import "math"

// Wilson returns the 95% Wilson score interval for a failure rate of
// failures/n. Unlike the simple p ± 1.96·sqrt(p(1-p)/n) interval, it stays
// sensible for small n and for rates near 0, which is exactly where flaky
// tests live (e.g. 1 failure in 200 runs).
func Wilson(failures, n int) (low, high float64) {
	if n == 0 {
		return 0, 1
	}
	const z = 1.959964 // 95% two-sided
	p := float64(failures) / float64(n)
	nf := float64(n)
	denom := 1 + z*z/nf
	center := (p + z*z/(2*nf)) / denom
	margin := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf)) / denom
	return math.Max(0, center-margin), math.Min(1, center+margin)
}
