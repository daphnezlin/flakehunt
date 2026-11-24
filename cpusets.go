package main

import (
	"fmt"
	"strconv"
)

// CPUSets splits ncpu cores into one equal, non-overlapping set per worker,
// e.g. CPUSets(8, 3) = ["0-1", "2-3", "4-5"]. Returns nil if there aren't
// enough cores to give every worker at least one.
func CPUSets(ncpu, workers int) []string {
	if workers < 1 || ncpu < workers {
		return nil
	}
	per := ncpu / workers
	sets := make([]string, workers)
	for i := range sets {
		first := i * per
		if per == 1 {
			sets[i] = strconv.Itoa(first)
		} else {
			sets[i] = fmt.Sprintf("%d-%d", first, first+per-1)
		}
	}
	return sets
}
