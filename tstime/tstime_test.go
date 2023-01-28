// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package tstime

import (
	"testing"
	"time"

	"go4.org/mem"
)

func TestParse3339(t *testing.T) {
	tests := []string{
		"2020-04-05T15:56:00Z",
		"2020-04-05T15:56:00.1234Z",
		"2020-04-05T15:56:00+08:00",

		"2020-04-05T15:56:00.1+08:00",
		"2020-04-05T15:56:00.12+08:00",
		"2020-04-05T15:56:00.012+08:00",
		"2020-04-05T15:56:00.0012+08:00",
		"2020-04-05T15:56:00.148487491+08:00",

		"2020x04-05T15:56:00.1234+08:00",
		"2020-04x05T15:56:00.1234+08:00",
		"2020-04-05x15:56:00.1234+08:00",
		"2020-04-05T15x56:00.1234+08:00",
		"2020-04-05T15:56x00.1234+08:00",
		"2020-04-05T15:56:00x1234+08:00",
	}
	for _, s := range tests {
		t.Run(s, func(t *testing.T) {
			goTime, goErr := time.Parse(time.RFC3339Nano, s)

			Parse3339(s) // prime the tz cache so next parse use fast path
			got, err := Parse3339(s)

			if (err == nil) != (goErr == nil) {
				t.Fatalf("for %q, go err = %v; our err = %v", s, goErr, err)
			}
			if err != nil {
				return
			}
			if !goTime.Equal(got) {
				t.Errorf("for %q, times not Equal: we got %v, want go's %v", s, got, goTime)
			}
			if goStr, ourStr := goTime.Format(time.RFC3339Nano), got.Format(time.RFC3339Nano); goStr != ourStr {
				t.Errorf("for %q, strings not equal: we got %q, want go's %q", s, ourStr, goStr)
			}

		})
	}

}

func TestZoneOf(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"2020-04-05T15:56:00+08:00", "+08:00"},
		{"2020-04-05T15:56:00-08:00", "-08:00"},
		{"2020-04-05T15:56:00.12345-08:00", "-08:00"},
		{"2020-04-05T15:56:00.12345-08:00", "-08:00"},
		{"2020-04-05T15:56:00.12345-08:30", "-08:30"},
		{"2020-04-05T15:56:00.12345-08:15", "-08:15"},
		{"2020-04-05T15:56:00.12345-08:17", ""}, // don't cache weird offsets
		{"2020-04-05T15:56:00.12345Z", "Z"},
		{"2020-04-05T15:56:00Z", "Z"},
		{"123+08:00", ""}, // too short
		{"+08:00", ""},    // too short
	}
	for _, tt := range tests {
		if got := zoneOf(mem.S(tt.in)); !got.EqualString(tt.want) {
			t.Errorf("zoneOf(%q) = %q; want %q", tt.in, got.StringCopy(), tt.want)
		}
	}
}

func TestParseInt(t *testing.T) {
	tests := []struct {
		in   string
		want int
		ret  bool
	}{
		{"", 0, false},
		{"1", 1, true},
		{"999999999", 999999999, true},
		{"123456789", 123456789, true},
		{"000000", 0, true},
		{"bork", 0, false},
		{"123bork", 0, false},
	}

	for _, tt := range tests {
		var got int
		gotRet := parseInt(mem.S(tt.in), &got)
		if gotRet != tt.ret || got != tt.want {
			t.Errorf("parseInt(%q) = %v, %d; want %v, %d", tt.in, gotRet, got, tt.ret, tt.want)
		}
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"1h", time.Hour},
		{"1d", 24 * time.Hour},
		{"365d", 365 * 24 * time.Hour},
		{"12345d", 12345 * 24 * time.Hour},
		{"67890d", 67890 * 24 * time.Hour},
		{"100d", 100 * 24 * time.Hour},
		{"1d1d", 48 * time.Hour},
		{"1h1d", 25 * time.Hour},
		{"1d1h", 25 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
		{"1w1d1h", 8*24*time.Hour + time.Hour},
		{"1w1d1h", 8*24*time.Hour + time.Hour},
		{"1y", 0},
		{"", 0},
	}
	for _, tt := range tests {
		if got, _ := ParseDuration(tt.in); got != tt.want {
			t.Errorf("ParseDuration(%q) = %d; want %d", tt.in, got, tt.want)
		}
	}
}

func BenchmarkGoParse3339(b *testing.B) {
	run := func(in string) func(*testing.B) {
		return func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, err := time.Parse(time.RFC3339Nano, in)
				if err != nil {
					b.Fatal(err)
				}
			}
		}
	}
	b.Run("Z", run("2020-04-05T15:56:00.148487491Z"))
	b.Run("TZ", run("2020-04-05T15:56:00.148487491+08:00"))
}

func BenchmarkGoParse3339InLocation(b *testing.B) {
	b.ReportAllocs()
	const in = `2020-04-05T15:56:00.148487491+08:00`

	t, err := time.Parse(time.RFC3339Nano, in)
	if err != nil {
		b.Fatal(err)
	}
	loc := t.Location()

	t2, err := time.ParseInLocation(time.RFC3339Nano, in, loc)
	if err != nil {
		b.Fatal(err)
	}
	if !t.Equal(t2) {
		b.Fatal("not equal")
	}
	if s1, s2 := t.Format(time.RFC3339Nano), t2.Format(time.RFC3339Nano); s1 != s2 {
		b.Fatalf("times don't stringify the same: %q vs %q", s1, s2)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := time.ParseInLocation(time.RFC3339Nano, in, loc)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParse3339(b *testing.B) {
	run := func(in string) func(*testing.B) {
		return func(b *testing.B) {
			b.ReportAllocs()
			t, err := time.Parse(time.RFC3339Nano, in)
			if err != nil {
				b.Fatal(err)
			}

			t2, err := Parse3339(in)
			if err != nil {
				b.Fatal(err)
			}
			if !t.Equal(t2) {
				b.Fatal("not equal")
			}
			if s1, s2 := t.Format(time.RFC3339Nano), t2.Format(time.RFC3339Nano); s1 != s2 {
				b.Fatalf("times don't stringify the same: %q vs %q", s1, s2)
			}

			for i := 0; i < b.N; i++ {
				_, err := Parse3339(in)
				if err != nil {
					b.Fatal(err)
				}
			}
		}
	}
	b.Run("Z", run("2020-04-05T15:56:00.148487491Z"))
	b.Run("TZ", run("2020-04-05T15:56:00.148487491+08:00"))
}

func BenchmarkParseInt(b *testing.B) {
	b.ReportAllocs()
	var out int
	for i := 0; i < b.N; i++ {
		parseInt(mem.S("148487491"), &out)
	}
}
