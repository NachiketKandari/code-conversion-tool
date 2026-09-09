// Package conc is the concurrency kernel: the bounded-index fan-out pool
// shared by every fan-out site (convert's DB renders and per-service dir
// workers, batchpy's per-file workers, gentest's per-function pool).
package conc

import "sync"

// RunIndexed runs fn(i) for i in [0, n) on a bounded worker pool and waits.
// Callers own their result slices (indexed by i), so output order always
// matches input order — workers=1 stays byte-identical to a sequential run.
func RunIndexed(n, workers int, fn func(i int)) {
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}
