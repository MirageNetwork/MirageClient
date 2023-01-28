// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package tstest

import (
	"bytes"
	"runtime"
	"runtime/pprof"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func ResourceCheck(tb testing.TB) {
	tb.Helper()
	startN, startStacks := goroutines()
	tb.Cleanup(func() {
		if tb.Failed() {
			// Something else went wrong.
			return
		}
		// Goroutines might be still exiting.
		for i := 0; i < 100; i++ {
			if runtime.NumGoroutine() <= startN {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		endN, endStacks := goroutines()
		if endN <= startN {
			return
		}
		tb.Logf("goroutine diff:\n%v\n", cmp.Diff(startStacks, endStacks))
		tb.Fatalf("goroutine count: expected %d, got %d\n", startN, endN)
	})
}

func goroutines() (int, []byte) {
	p := pprof.Lookup("goroutine")
	b := new(bytes.Buffer)
	p.WriteTo(b, 1)
	return p.Count(), b.Bytes()
}
