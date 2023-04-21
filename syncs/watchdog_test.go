// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package syncs

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"
)

// Time-based tests are fundamentally flaky.
// We use exaggerated durations in the hopes of minimizing such issues.

func TestWatchUncontended(t *testing.T) {
	mu := new(sync.Mutex)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Once an hour, and now, check whether we can lock mu in under an hour.
	tick := time.Hour
	max := time.Hour
	c := Watch(ctx, mu, tick, max)
	d := <-c
	if d == max {
		t.Errorf("uncontended mutex did not lock in under %v", max)
	}
}

func TestWatchContended(t *testing.T) {
	mu := new(sync.Mutex)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Every hour, and now, check whether we can lock mu in under a millisecond,
	// which is enough time for an uncontended mutex by several orders of magnitude.
	tick := time.Hour
	max := time.Millisecond
	mu.Lock()
	defer mu.Unlock()
	c := Watch(ctx, mu, tick, max)
	d := <-c
	if d != max {
		t.Errorf("contended mutex locked in under %v", max)
	}
}

func TestWatchMultipleValues(t *testing.T) {
	mu := new(sync.Mutex)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // not necessary, but keep vet happy
	// Check the mutex every millisecond.
	// The goal is to see that we get a sufficient number of values out of the channel.
	tick := time.Millisecond
	max := time.Millisecond
	c := Watch(ctx, mu, tick, max)
	start := time.Now()
	n := 0
	for d := range c {
		n++
		if d == max {
			t.Errorf("uncontended mutex did not lock in under %v", max)
		}
		if n == 10 {
			cancel()
		}
	}
	// See https://github.com/golang/go/issues/44343 - on Windows the Go runtime timer resolution is currently too coarse. Allow longer in that case.
	want := 100 * time.Millisecond
	if runtime.GOOS == "windows" {
		want = 500 * time.Millisecond
	}

	if elapsed := time.Since(start); elapsed > want {
		t.Errorf("expected 1 event per millisecond, got only %v events in %v", n, elapsed)
	}
}
