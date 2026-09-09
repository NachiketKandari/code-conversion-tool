package main

import "sync"

// runIndexed runs fn(i) for i in [0, n) on a bounded worker pool and waits.
// Callers own their result slices (indexed by i), so output order always
// matches input order — workers=1 stays byte-identical to a sequential run.
// This is the shared fan-out seam of convert's per-service workers and
// batchpy's per-file workers.
func runIndexed(n, workers int, fn func(i int)) {
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
