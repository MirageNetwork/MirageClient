// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package flowtrack

import (
	"net/netip"
	"testing"

	"tailscale.com/tstest"
)

func TestCache(t *testing.T) {
	c := &Cache[int]{MaxEntries: 2}

	k1 := Tuple{Src: netip.MustParseAddrPort("1.1.1.1:1"), Dst: netip.MustParseAddrPort("1.1.1.1:1")}
	k2 := Tuple{Src: netip.MustParseAddrPort("1.1.1.1:1"), Dst: netip.MustParseAddrPort("2.2.2.2:2")}
	k3 := Tuple{Src: netip.MustParseAddrPort("1.1.1.1:1"), Dst: netip.MustParseAddrPort("3.3.3.3:3")}
	k4 := Tuple{Src: netip.MustParseAddrPort("1.1.1.1:1"), Dst: netip.MustParseAddrPort("4.4.4.4:4")}

	wantLen := func(want int) {
		t.Helper()
		if got := c.Len(); got != want {
			t.Fatalf("Len = %d; want %d", got, want)
		}
	}
	wantVal := func(key Tuple, want int) {
		t.Helper()
		got, ok := c.Get(key)
		if !ok {
			t.Fatalf("Get(%q) failed; want value %v", key, want)
		}
		if *got != want {
			t.Fatalf("Get(%q) = %v; want %v", key, got, want)
		}
	}
	wantMissing := func(key Tuple) {
		t.Helper()
		if got, ok := c.Get(key); ok {
			t.Fatalf("Get(%q) = %v; want absent from cache", key, got)
		}
	}

	wantLen(0)
	c.RemoveOldest() // shouldn't panic
	c.Remove(k4)     // shouldn't panic

	c.Add(k1, 1)
	wantLen(1)
	c.Add(k2, 2)
	wantLen(2)
	c.Add(k3, 3)
	wantLen(2) // hit the max

	wantMissing(k1)
	c.Remove(k1)
	wantLen(2) // no change; k1 should've been the deleted one per LRU

	wantVal(k3, 3)

	wantVal(k2, 2)
	c.Remove(k2)
	wantLen(1)
	wantMissing(k2)

	c.Add(k3, 30)
	wantVal(k3, 30)
	wantLen(1)

	err := tstest.MinAllocsPerRun(t, 0, func() {
		got, ok := c.Get(k3)
		if !ok {
			t.Fatal("missing k3")
		}
		if *got != 30 {
			t.Fatalf("got = %d; want 30", got)
		}
	})
	if err != nil {
		t.Error(err)
	}
}
