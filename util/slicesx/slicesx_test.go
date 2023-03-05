// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package slicesx

import (
	"reflect"
	"testing"

	"golang.org/x/exp/slices"
)

func TestInterleave(t *testing.T) {
	testCases := []struct {
		name string
		a, b []int
		want []int
	}{
		{name: "equal", a: []int{1, 3, 5}, b: []int{2, 4, 6}, want: []int{1, 2, 3, 4, 5, 6}},
		{name: "short_b", a: []int{1, 3, 5}, b: []int{2, 4}, want: []int{1, 2, 3, 4, 5}},
		{name: "short_a", a: []int{1, 3}, b: []int{2, 4, 6}, want: []int{1, 2, 3, 4, 6}},
		{name: "len_1", a: []int{1}, b: []int{2, 4, 6}, want: []int{1, 2, 4, 6}},
		{name: "nil_a", a: nil, b: []int{2, 4, 6}, want: []int{2, 4, 6}},
		{name: "nil_all", a: nil, b: nil, want: nil},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			merged := Interleave(tc.a, tc.b)
			if !reflect.DeepEqual(merged, tc.want) {
				t.Errorf("got %v; want %v", merged, tc.want)
			}
		})
	}
}

func BenchmarkInterleave(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Interleave(
			[]int{1, 2, 3},
			[]int{9, 8, 7},
		)
	}
}
func TestShuffle(t *testing.T) {
	var sl []int
	for i := 0; i < 100; i++ {
		sl = append(sl, i)
	}

	var wasShuffled bool
	for try := 0; try < 10; try++ {
		shuffled := slices.Clone(sl)
		Shuffle(shuffled)
		if !reflect.DeepEqual(shuffled, sl) {
			wasShuffled = true
			break
		}
	}

	if !wasShuffled {
		t.Errorf("expected shuffle after 10 tries")
	}
}
