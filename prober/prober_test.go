// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package prober

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"tailscale.com/tstest"
	"tailscale.com/tsweb"
)

const (
	probeInterval        = 8 * time.Second // So expvars that are integer numbers of seconds change
	halfProbeInterval    = probeInterval / 2
	quarterProbeInterval = probeInterval / 4
	convergenceTimeout   = time.Second
	convergenceSleep     = time.Millisecond
	aFewMillis           = 20 * time.Millisecond
)

var epoch = time.Unix(0, 0)

func TestProberTiming(t *testing.T) {
	clk := newFakeTime()
	p := newForTest(clk.Now, clk.NewTicker)

	invoked := make(chan struct{}, 1)

	notCalled := func() {
		t.Helper()
		select {
		case <-invoked:
			t.Fatal("probe was invoked earlier than expected")
		default:
		}
	}
	called := func() {
		t.Helper()
		select {
		case <-invoked:
		case <-time.After(2 * time.Second):
			t.Fatal("probe wasn't invoked as expected")
		}
	}

	p.Run("test-probe", probeInterval, nil, func(context.Context) error {
		invoked <- struct{}{}
		return nil
	})

	waitActiveProbes(t, p, clk, 1)

	called()
	notCalled()
	clk.Advance(probeInterval + halfProbeInterval)
	called()
	notCalled()
	clk.Advance(quarterProbeInterval)
	notCalled()
	clk.Advance(probeInterval)
	called()
	notCalled()
}

func TestProberTimingSpread(t *testing.T) {
	clk := newFakeTime()
	p := newForTest(clk.Now, clk.NewTicker).WithSpread(true)

	invoked := make(chan struct{}, 1)

	notCalled := func() {
		t.Helper()
		select {
		case <-invoked:
			t.Fatal("probe was invoked earlier than expected")
		default:
		}
	}
	called := func() {
		t.Helper()
		select {
		case <-invoked:
		case <-time.After(2 * time.Second):
			t.Fatal("probe wasn't invoked as expected")
		}
	}

	probe := p.Run("test-spread-probe", probeInterval, nil, func(context.Context) error {
		invoked <- struct{}{}
		return nil
	})

	waitActiveProbes(t, p, clk, 1)

	notCalled()
	// Name of the probe (test-spread-probe) has been chosen to ensure that
	// the initial delay is smaller than half of the probe interval.
	clk.Advance(halfProbeInterval)
	called()
	notCalled()

	// We need to wait until the main (non-initial) ticker in Probe.loop is
	// waiting, or we could race and advance the test clock between when
	// the initial delay ticker completes and before the ticker for the
	// main loop is created. In this race, we'd first advance the test
	// clock, then the ticker would be registered, and the test would fail
	// because that ticker would never be fired.
	err := tstest.WaitFor(convergenceTimeout, func() error {
		clk.Lock()
		defer clk.Unlock()
		for _, tick := range clk.tickers {
			tick.Lock()
			stopped, interval := tick.stopped, tick.interval
			tick.Unlock()

			if stopped {
				continue
			}
			// Test for the main loop, not the initialDelay
			if interval == probe.interval {
				return nil
			}
		}

		return fmt.Errorf("no ticker with interval %d found", probe.interval)
	})
	if err != nil {
		t.Fatal(err)
	}

	clk.Advance(quarterProbeInterval)
	notCalled()
	clk.Advance(probeInterval)
	called()
	notCalled()
}

func TestProberRun(t *testing.T) {
	clk := newFakeTime()
	p := newForTest(clk.Now, clk.NewTicker)

	var (
		mu  sync.Mutex
		cnt int
	)

	const startingProbes = 100
	var probes []*Probe

	for i := 0; i < startingProbes; i++ {
		probes = append(probes, p.Run(fmt.Sprintf("probe%d", i), probeInterval, nil, func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			cnt++
			return nil
		}))
	}

	checkCnt := func(want int) {
		t.Helper()
		err := tstest.WaitFor(convergenceTimeout, func() error {
			mu.Lock()
			defer mu.Unlock()
			if cnt == want {
				cnt = 0
				return nil
			}
			return fmt.Errorf("wrong number of probe counter increments, got %d want %d", cnt, want)
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	waitActiveProbes(t, p, clk, startingProbes)
	checkCnt(startingProbes)
	clk.Advance(probeInterval + halfProbeInterval)
	checkCnt(startingProbes)

	keep := startingProbes / 2

	for i := keep; i < startingProbes; i++ {
		probes[i].Close()
	}
	waitActiveProbes(t, p, clk, keep)

	clk.Advance(probeInterval)
	checkCnt(keep)
}

func TestExpvar(t *testing.T) {
	clk := newFakeTime()
	p := newForTest(clk.Now, clk.NewTicker)

	var succeed atomic.Bool
	p.Run("probe", probeInterval, map[string]string{"label": "value"}, func(context.Context) error {
		clk.Advance(aFewMillis)
		if succeed.Load() {
			return nil
		}
		return errors.New("failing, as instructed by test")
	})

	waitActiveProbes(t, p, clk, 1)

	check := func(name string, want ProbeInfo) {
		t.Helper()
		err := tstest.WaitFor(convergenceTimeout, func() error {
			vars := probeExpvar(t, p)
			if got, want := len(vars), 1; got != want {
				return fmt.Errorf("wrong probe count in expvar, got %d want %d", got, want)
			}
			for k, v := range vars {
				if k != name {
					return fmt.Errorf("wrong probe name in expvar, got %q want %q", k, name)
				}
				if diff := cmp.Diff(v, &want); diff != "" {
					return fmt.Errorf("wrong probe stats (-got+want):\n%s", diff)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	check("probe", ProbeInfo{
		Labels:  map[string]string{"label": "value"},
		Start:   epoch,
		End:     epoch.Add(aFewMillis),
		Latency: aFewMillis.String(),
		Result:  false,
		Error:   "failing, as instructed by test",
	})

	succeed.Store(true)
	clk.Advance(probeInterval + halfProbeInterval)

	st := epoch.Add(probeInterval + halfProbeInterval + aFewMillis)
	check("probe", ProbeInfo{
		Labels:  map[string]string{"label": "value"},
		Start:   st,
		End:     st.Add(aFewMillis),
		Latency: aFewMillis.String(),
		Result:  true,
	})
}

func TestPrometheus(t *testing.T) {
	clk := newFakeTime()
	p := newForTest(clk.Now, clk.NewTicker)

	var succeed atomic.Bool
	p.Run("testprobe", probeInterval, map[string]string{"label": "value"}, func(context.Context) error {
		clk.Advance(aFewMillis)
		if succeed.Load() {
			return nil
		}
		return errors.New("failing, as instructed by test")
	})

	waitActiveProbes(t, p, clk, 1)

	err := tstest.WaitFor(convergenceTimeout, func() error {
		var b bytes.Buffer
		p.Expvar().(tsweb.PrometheusVar).WritePrometheus(&b, "probe")
		want := strings.TrimSpace(fmt.Sprintf(`
probe_interval_secs{name="testprobe",label="value"} %f
probe_start_secs{name="testprobe",label="value"} %d
probe_end_secs{name="testprobe",label="value"} %d
probe_latency_millis{name="testprobe",label="value"} %d
probe_result{name="testprobe",label="value"} 0
`, probeInterval.Seconds(), epoch.Unix(), epoch.Add(aFewMillis).Unix(), aFewMillis.Milliseconds()))
		if diff := cmp.Diff(strings.TrimSpace(b.String()), want); diff != "" {
			return fmt.Errorf("wrong probe stats (-got+want):\n%s", diff)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	succeed.Store(true)
	clk.Advance(probeInterval + halfProbeInterval)

	err = tstest.WaitFor(convergenceTimeout, func() error {
		var b bytes.Buffer
		p.Expvar().(tsweb.PrometheusVar).WritePrometheus(&b, "probe")
		start := epoch.Add(probeInterval + halfProbeInterval)
		end := start.Add(aFewMillis)
		want := strings.TrimSpace(fmt.Sprintf(`
probe_interval_secs{name="testprobe",label="value"} %f
probe_start_secs{name="testprobe",label="value"} %d
probe_end_secs{name="testprobe",label="value"} %d
probe_latency_millis{name="testprobe",label="value"} %d
probe_result{name="testprobe",label="value"} 1
`, probeInterval.Seconds(), start.Unix(), end.Unix(), aFewMillis.Milliseconds()))
		if diff := cmp.Diff(strings.TrimSpace(b.String()), want); diff != "" {
			return fmt.Errorf("wrong probe stats (-got+want):\n%s", diff)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOnceMode(t *testing.T) {
	clk := newFakeTime()
	p := newForTest(clk.Now, clk.NewTicker).WithOnce(true)

	p.Run("probe1", probeInterval, nil, func(context.Context) error { return nil })
	p.Run("probe2", probeInterval, nil, func(context.Context) error { return fmt.Errorf("error2") })
	p.Run("probe3", probeInterval, nil, func(context.Context) error {
		p.Run("probe4", probeInterval, nil, func(context.Context) error {
			return fmt.Errorf("error4")
		})
		return nil
	})

	p.Wait()
	info := p.ProbeInfo()
	if len(info) != 4 {
		t.Errorf("expected 4 probe results, got %+v", info)
	}
	for _, p := range info {
		if p.End.IsZero() {
			t.Errorf("expected all probes to finish; got %+v", p)
		}
	}
}

type fakeTicker struct {
	ch       chan time.Time
	interval time.Duration

	sync.Mutex
	next    time.Time
	stopped bool
}

func (t *fakeTicker) Chan() <-chan time.Time {
	return t.ch
}

func (t *fakeTicker) Stop() {
	t.Lock()
	defer t.Unlock()
	t.stopped = true
}

func (t *fakeTicker) fire(now time.Time) {
	t.Lock()
	defer t.Unlock()
	// Slight deviation from the stdlib ticker: time.Ticker will
	// adjust t.next to make up for missed ticks, whereas we tick on a
	// fixed interval regardless of receiver behavior. In our case
	// this is fine, since we're using the ticker as a wakeup
	// mechanism and not a precise timekeeping system.
	select {
	case t.ch <- now:
	default:
	}
	for now.After(t.next) {
		t.next = t.next.Add(t.interval)
	}
}

type fakeTime struct {
	sync.Mutex
	*sync.Cond
	curTime time.Time
	tickers []*fakeTicker
}

func newFakeTime() *fakeTime {
	ret := &fakeTime{
		curTime: epoch,
	}
	ret.Cond = &sync.Cond{L: &ret.Mutex}
	return ret
}

func (t *fakeTime) Now() time.Time {
	t.Lock()
	defer t.Unlock()
	ret := t.curTime
	return ret
}

func (t *fakeTime) NewTicker(d time.Duration) ticker {
	t.Lock()
	defer t.Unlock()
	ret := &fakeTicker{
		ch:       make(chan time.Time, 1),
		interval: d,
		next:     t.curTime.Add(d),
	}
	t.tickers = append(t.tickers, ret)
	t.Cond.Broadcast()
	return ret
}

func (t *fakeTime) Advance(d time.Duration) {
	t.Lock()
	defer t.Unlock()
	t.curTime = t.curTime.Add(d)
	for _, tick := range t.tickers {
		if t.curTime.After(tick.next) {
			tick.fire(t.curTime)
		}
	}
}

func (t *fakeTime) activeTickers() (count int) {
	t.Lock()
	defer t.Unlock()
	for _, tick := range t.tickers {
		if !tick.stopped {
			count += 1
		}
	}
	return
}

func probeExpvar(t *testing.T, p *Prober) map[string]*ProbeInfo {
	t.Helper()
	s := p.Expvar().String()
	ret := map[string]*ProbeInfo{}
	if err := json.Unmarshal([]byte(s), &ret); err != nil {
		t.Fatalf("expvar json decode failed: %v", err)
	}
	return ret
}

func waitActiveProbes(t *testing.T, p *Prober, clk *fakeTime, want int) {
	t.Helper()
	err := tstest.WaitFor(convergenceTimeout, func() error {
		if got := p.activeProbes(); got != want {
			return fmt.Errorf("installed probe count is %d, want %d", got, want)
		}
		if got := clk.activeTickers(); got != want {
			return fmt.Errorf("active ticker count is %d, want %d", got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
