package common

import (
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

// TestRunIndexedOrderAndCoverage verifies every index runs exactly once and
// the pool actually bounds concurrency.
func TestRunIndexedOrderAndCoverage(t *testing.T) {
	const n = 50
	seen := make([]int64, n)
	var cur, maxCur int64
	RunIndexed(n, 4, func(i int) {
		c := atomic.AddInt64(&cur, 1)
		for {
			m := atomic.LoadInt64(&maxCur)
			if c <= m || atomic.CompareAndSwapInt64(&maxCur, m, c) {
				break
			}
		}
		defer atomic.AddInt64(&cur, -1)
		atomic.AddInt64(&seen[i], 1)
	})
	for i, c := range seen {
		if c != 1 {
			t.Fatalf("index %d ran %d times", i, c)
		}
	}
	if maxCur > 4 {
		t.Fatalf("concurrency exceeded the bound: %d", maxCur)
	}
}

func TestRunIndexedClamp(t *testing.T) {
	RunIndexed(3, 0, func(i int) {}) // must not panic
	RunIndexed(0, 8, func(i int) {}) // no-op
}

// TestCommonImportsNothingInternal is the package law (AD1, A6.3 guard
// installed early): common is a stdlib-only leaf so the frozen parse stack
// can always depend on it. A function needing an internal import is domain
// knowledge and belongs in its owning package.
func TestCommonImportsNothingInternal(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "github.com/Public/convert-tux-to-go/internal/") &&
			line != "github.com/Public/convert-tux-to-go/internal/common" {
			t.Fatalf("common imports an internal package: %s", line)
		}
	}
}
