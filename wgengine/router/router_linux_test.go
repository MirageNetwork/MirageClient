// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package router

import (
	"errors"
	"fmt"
	"math/rand"
	"net/netip"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/tailscale/wireguard-go/tun"
	"github.com/vishvananda/netlink"
	"golang.org/x/exp/slices"
	"tailscale.com/net/netmon"
	"tailscale.com/tstest"
	"tailscale.com/types/logger"
)

func TestRouterStates(t *testing.T) {
	basic := `
ip rule add -4 pref 5210 fwmark 0x80000/0xff0000 table main
ip rule add -4 pref 5230 fwmark 0x80000/0xff0000 table default
ip rule add -4 pref 5250 fwmark 0x80000/0xff0000 type unreachable
ip rule add -4 pref 5270 table 52
ip rule add -6 pref 5210 fwmark 0x80000/0xff0000 table main
ip rule add -6 pref 5230 fwmark 0x80000/0xff0000 table default
ip rule add -6 pref 5250 fwmark 0x80000/0xff0000 type unreachable
ip rule add -6 pref 5270 table 52
`
	states := []struct {
		name string
		in   *Config
		want string
	}{
		{
			name: "no config",
			in:   nil,
			want: `
up` + basic,
		},
		{
			name: "local addr only",
			in: &Config{
				LocalAddrs:    mustCIDRs("100.101.102.103/10"),
				NetfilterMode: netfilterOff,
			},
			want: `
up
ip addr add 100.101.102.103/10 dev tailscale0` + basic,
		},

		{
			name: "addr and routes",
			in: &Config{
				LocalAddrs:    mustCIDRs("100.101.102.103/10"),
				Routes:        mustCIDRs("100.100.100.100/32", "192.168.16.0/24"),
				NetfilterMode: netfilterOff,
			},
			want: `
up
ip addr add 100.101.102.103/10 dev tailscale0
ip route add 100.100.100.100/32 dev tailscale0 table 52
ip route add 192.168.16.0/24 dev tailscale0 table 52` + basic,
		},

		{
			name: "addr and routes and subnet routes",
			in: &Config{
				LocalAddrs:    mustCIDRs("100.101.102.103/10"),
				Routes:        mustCIDRs("100.100.100.100/32", "192.168.16.0/24"),
				SubnetRoutes:  mustCIDRs("200.0.0.0/8"),
				NetfilterMode: netfilterOff,
			},
			want: `
up
ip addr add 100.101.102.103/10 dev tailscale0
ip route add 100.100.100.100/32 dev tailscale0 table 52
ip route add 192.168.16.0/24 dev tailscale0 table 52` + basic,
		},

		{
			name: "addr and routes and subnet routes with netfilter",
			in: &Config{
				LocalAddrs:       mustCIDRs("100.101.102.104/10"),
				Routes:           mustCIDRs("100.100.100.100/32", "10.0.0.0/8"),
				SubnetRoutes:     mustCIDRs("200.0.0.0/8"),
				SNATSubnetRoutes: true,
				NetfilterMode:    netfilterOn,
			},
			want: `
up
ip addr add 100.101.102.104/10 dev tailscale0
ip route add 10.0.0.0/8 dev tailscale0 table 52
ip route add 100.100.100.100/32 dev tailscale0 table 52` + basic +
				`v4/filter/FORWARD -j ts-forward
v4/filter/INPUT -j ts-input
v4/filter/ts-forward -i tailscale0 -j MARK --set-mark 0x40000/0xff0000
v4/filter/ts-forward -m mark --mark 0x40000/0xff0000 -j ACCEPT
v4/filter/ts-forward -o tailscale0 -s 100.64.0.0/10 -j DROP
v4/filter/ts-forward -o tailscale0 -j ACCEPT
v4/filter/ts-input -i lo -s 100.101.102.104 -j ACCEPT
v4/filter/ts-input ! -i tailscale0 -s 100.115.92.0/23 -j RETURN
v4/filter/ts-input ! -i tailscale0 -s 100.64.0.0/10 -j DROP
v4/nat/POSTROUTING -j ts-postrouting
v4/nat/ts-postrouting -m mark --mark 0x40000/0xff0000 -j MASQUERADE
v6/filter/FORWARD -j ts-forward
v6/filter/INPUT -j ts-input
v6/filter/ts-forward -i tailscale0 -j MARK --set-mark 0x40000/0xff0000
v6/filter/ts-forward -m mark --mark 0x40000/0xff0000 -j ACCEPT
v6/filter/ts-forward -o tailscale0 -j ACCEPT
v6/nat/POSTROUTING -j ts-postrouting
v6/nat/ts-postrouting -m mark --mark 0x40000/0xff0000 -j MASQUERADE
`,
		},
		{
			name: "addr and routes with netfilter",
			in: &Config{
				LocalAddrs:    mustCIDRs("100.101.102.104/10"),
				Routes:        mustCIDRs("100.100.100.100/32", "10.0.0.0/8"),
				NetfilterMode: netfilterOn,
			},
			want: `
up
ip addr add 100.101.102.104/10 dev tailscale0
ip route add 10.0.0.0/8 dev tailscale0 table 52
ip route add 100.100.100.100/32 dev tailscale0 table 52` + basic +
				`v4/filter/FORWARD -j ts-forward
v4/filter/INPUT -j ts-input
v4/filter/ts-forward -i tailscale0 -j MARK --set-mark 0x40000/0xff0000
v4/filter/ts-forward -m mark --mark 0x40000/0xff0000 -j ACCEPT
v4/filter/ts-forward -o tailscale0 -s 100.64.0.0/10 -j DROP
v4/filter/ts-forward -o tailscale0 -j ACCEPT
v4/filter/ts-input -i lo -s 100.101.102.104 -j ACCEPT
v4/filter/ts-input ! -i tailscale0 -s 100.115.92.0/23 -j RETURN
v4/filter/ts-input ! -i tailscale0 -s 100.64.0.0/10 -j DROP
v4/nat/POSTROUTING -j ts-postrouting
v6/filter/FORWARD -j ts-forward
v6/filter/INPUT -j ts-input
v6/filter/ts-forward -i tailscale0 -j MARK --set-mark 0x40000/0xff0000
v6/filter/ts-forward -m mark --mark 0x40000/0xff0000 -j ACCEPT
v6/filter/ts-forward -o tailscale0 -j ACCEPT
v6/nat/POSTROUTING -j ts-postrouting
`,
		},

		{
			name: "addr and routes and subnet routes with netfilter but no SNAT",
			in: &Config{
				LocalAddrs:       mustCIDRs("100.101.102.104/10"),
				Routes:           mustCIDRs("100.100.100.100/32", "10.0.0.0/8"),
				SubnetRoutes:     mustCIDRs("200.0.0.0/8"),
				SNATSubnetRoutes: false,
				NetfilterMode:    netfilterOn,
			},
			want: `
up
ip addr add 100.101.102.104/10 dev tailscale0
ip route add 10.0.0.0/8 dev tailscale0 table 52
ip route add 100.100.100.100/32 dev tailscale0 table 52` + basic +
				`v4/filter/FORWARD -j ts-forward
v4/filter/INPUT -j ts-input
v4/filter/ts-forward -i tailscale0 -j MARK --set-mark 0x40000/0xff0000
v4/filter/ts-forward -m mark --mark 0x40000/0xff0000 -j ACCEPT
v4/filter/ts-forward -o tailscale0 -s 100.64.0.0/10 -j DROP
v4/filter/ts-forward -o tailscale0 -j ACCEPT
v4/filter/ts-input -i lo -s 100.101.102.104 -j ACCEPT
v4/filter/ts-input ! -i tailscale0 -s 100.115.92.0/23 -j RETURN
v4/filter/ts-input ! -i tailscale0 -s 100.64.0.0/10 -j DROP
v4/nat/POSTROUTING -j ts-postrouting
v6/filter/FORWARD -j ts-forward
v6/filter/INPUT -j ts-input
v6/filter/ts-forward -i tailscale0 -j MARK --set-mark 0x40000/0xff0000
v6/filter/ts-forward -m mark --mark 0x40000/0xff0000 -j ACCEPT
v6/filter/ts-forward -o tailscale0 -j ACCEPT
v6/nat/POSTROUTING -j ts-postrouting
`,
		},
		{
			name: "addr and routes with netfilter",
			in: &Config{
				LocalAddrs:    mustCIDRs("100.101.102.104/10"),
				Routes:        mustCIDRs("100.100.100.100/32", "10.0.0.0/8"),
				NetfilterMode: netfilterOn,
			},
			want: `
up
ip addr add 100.101.102.104/10 dev tailscale0
ip route add 10.0.0.0/8 dev tailscale0 table 52
ip route add 100.100.100.100/32 dev tailscale0 table 52` + basic +
				`v4/filter/FORWARD -j ts-forward
v4/filter/INPUT -j ts-input
v4/filter/ts-forward -i tailscale0 -j MARK --set-mark 0x40000/0xff0000
v4/filter/ts-forward -m mark --mark 0x40000/0xff0000 -j ACCEPT
v4/filter/ts-forward -o tailscale0 -s 100.64.0.0/10 -j DROP
v4/filter/ts-forward -o tailscale0 -j ACCEPT
v4/filter/ts-input -i lo -s 100.101.102.104 -j ACCEPT
v4/filter/ts-input ! -i tailscale0 -s 100.115.92.0/23 -j RETURN
v4/filter/ts-input ! -i tailscale0 -s 100.64.0.0/10 -j DROP
v4/nat/POSTROUTING -j ts-postrouting
v6/filter/FORWARD -j ts-forward
v6/filter/INPUT -j ts-input
v6/filter/ts-forward -i tailscale0 -j MARK --set-mark 0x40000/0xff0000
v6/filter/ts-forward -m mark --mark 0x40000/0xff0000 -j ACCEPT
v6/filter/ts-forward -o tailscale0 -j ACCEPT
v6/nat/POSTROUTING -j ts-postrouting
`,
		},

		{
			name: "addr and routes with half netfilter",
			in: &Config{
				LocalAddrs:    mustCIDRs("100.101.102.104/10"),
				Routes:        mustCIDRs("100.100.100.100/32", "10.0.0.0/8"),
				NetfilterMode: netfilterNoDivert,
			},
			want: `
up
ip addr add 100.101.102.104/10 dev tailscale0
ip route add 10.0.0.0/8 dev tailscale0 table 52
ip route add 100.100.100.100/32 dev tailscale0 table 52` + basic +
				`v4/filter/ts-forward -i tailscale0 -j MARK --set-mark 0x40000/0xff0000
v4/filter/ts-forward -m mark --mark 0x40000/0xff0000 -j ACCEPT
v4/filter/ts-forward -o tailscale0 -s 100.64.0.0/10 -j DROP
v4/filter/ts-forward -o tailscale0 -j ACCEPT
v4/filter/ts-input -i lo -s 100.101.102.104 -j ACCEPT
v4/filter/ts-input ! -i tailscale0 -s 100.115.92.0/23 -j RETURN
v4/filter/ts-input ! -i tailscale0 -s 100.64.0.0/10 -j DROP
v6/filter/ts-forward -i tailscale0 -j MARK --set-mark 0x40000/0xff0000
v6/filter/ts-forward -m mark --mark 0x40000/0xff0000 -j ACCEPT
v6/filter/ts-forward -o tailscale0 -j ACCEPT
`,
		},
		{
			name: "addr and routes with netfilter2",
			in: &Config{
				LocalAddrs:    mustCIDRs("100.101.102.104/10"),
				Routes:        mustCIDRs("100.100.100.100/32", "10.0.0.0/8"),
				NetfilterMode: netfilterOn,
			},
			want: `
up
ip addr add 100.101.102.104/10 dev tailscale0
ip route add 10.0.0.0/8 dev tailscale0 table 52
ip route add 100.100.100.100/32 dev tailscale0 table 52` + basic +
				`v4/filter/FORWARD -j ts-forward
v4/filter/INPUT -j ts-input
v4/filter/ts-forward -i tailscale0 -j MARK --set-mark 0x40000/0xff0000
v4/filter/ts-forward -m mark --mark 0x40000/0xff0000 -j ACCEPT
v4/filter/ts-forward -o tailscale0 -s 100.64.0.0/10 -j DROP
v4/filter/ts-forward -o tailscale0 -j ACCEPT
v4/filter/ts-input -i lo -s 100.101.102.104 -j ACCEPT
v4/filter/ts-input ! -i tailscale0 -s 100.115.92.0/23 -j RETURN
v4/filter/ts-input ! -i tailscale0 -s 100.64.0.0/10 -j DROP
v4/nat/POSTROUTING -j ts-postrouting
v6/filter/FORWARD -j ts-forward
v6/filter/INPUT -j ts-input
v6/filter/ts-forward -i tailscale0 -j MARK --set-mark 0x40000/0xff0000
v6/filter/ts-forward -m mark --mark 0x40000/0xff0000 -j ACCEPT
v6/filter/ts-forward -o tailscale0 -j ACCEPT
v6/nat/POSTROUTING -j ts-postrouting
`,
		},
		{
			name: "addr, routes, and local routes with netfilter",
			in: &Config{
				LocalAddrs:    mustCIDRs("100.101.102.104/10"),
				Routes:        mustCIDRs("100.100.100.100/32", "0.0.0.0/0"),
				LocalRoutes:   mustCIDRs("10.0.0.0/8"),
				NetfilterMode: netfilterOn,
			},
			want: `
up
ip addr add 100.101.102.104/10 dev tailscale0
ip route add 0.0.0.0/0 dev tailscale0 table 52
ip route add 100.100.100.100/32 dev tailscale0 table 52
ip route add throw 10.0.0.0/8 table 52` + basic +
				`v4/filter/FORWARD -j ts-forward
v4/filter/INPUT -j ts-input
v4/filter/ts-forward -i tailscale0 -j MARK --set-mark 0x40000/0xff0000
v4/filter/ts-forward -m mark --mark 0x40000/0xff0000 -j ACCEPT
v4/filter/ts-forward -o tailscale0 -s 100.64.0.0/10 -j DROP
v4/filter/ts-forward -o tailscale0 -j ACCEPT
v4/filter/ts-input -i lo -s 100.101.102.104 -j ACCEPT
v4/filter/ts-input ! -i tailscale0 -s 100.115.92.0/23 -j RETURN
v4/filter/ts-input ! -i tailscale0 -s 100.64.0.0/10 -j DROP
v4/nat/POSTROUTING -j ts-postrouting
v6/filter/FORWARD -j ts-forward
v6/filter/INPUT -j ts-input
v6/filter/ts-forward -i tailscale0 -j MARK --set-mark 0x40000/0xff0000
v6/filter/ts-forward -m mark --mark 0x40000/0xff0000 -j ACCEPT
v6/filter/ts-forward -o tailscale0 -j ACCEPT
v6/nat/POSTROUTING -j ts-postrouting
`,
		},
		{
			name: "addr, routes, and local routes with no netfilter",
			in: &Config{
				LocalAddrs:    mustCIDRs("100.101.102.104/10"),
				Routes:        mustCIDRs("100.100.100.100/32", "0.0.0.0/0"),
				LocalRoutes:   mustCIDRs("10.0.0.0/8", "192.168.0.0/24"),
				NetfilterMode: netfilterOff,
			},
			want: `
up
ip addr add 100.101.102.104/10 dev tailscale0
ip route add 0.0.0.0/0 dev tailscale0 table 52
ip route add 100.100.100.100/32 dev tailscale0 table 52
ip route add throw 10.0.0.0/8 table 52
ip route add throw 192.168.0.0/24 table 52` + basic,
		},
	}

	mon, err := netmon.New(logger.Discard)
	if err != nil {
		t.Fatal(err)
	}
	mon.Start()
	defer mon.Close()

	fake := NewFakeOS(t)
	router, err := newUserspaceRouterAdvanced(t.Logf, "tailscale0", mon, fake.netfilter4, fake.netfilter6, fake, true, true)
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}
	if err := router.Up(); err != nil {
		t.Fatalf("failed to up router: %v", err)
	}

	testState := func(t *testing.T, i int) {
		t.Helper()
		if err := router.Set(states[i].in); err != nil {
			t.Fatalf("failed to set router config: %v", err)
		}
		got := fake.String()
		want := adjustFwmask(t, strings.TrimSpace(states[i].want))
		if diff := cmp.Diff(got, want); diff != "" {
			t.Fatalf("unexpected OS state (-got+want):\n%s", diff)
		}
	}

	for i, state := range states {
		t.Run(state.name, func(t *testing.T) { testState(t, i) })
	}

	// Cycle through a bunch of states in pseudorandom order, to
	// verify that we transition cleanly from state to state no matter
	// the order.
	for randRun := 0; randRun < 5*len(states); randRun++ {
		i := rand.Intn(len(states))
		state := states[i]
		t.Run(state.name, func(t *testing.T) { testState(t, i) })
	}
}

type fakeNetfilter struct {
	t *testing.T
	n map[string][]string
}

func newNetfilter(t *testing.T) *fakeNetfilter {
	return &fakeNetfilter{
		t: t,
		n: map[string][]string{
			"filter/INPUT":    nil,
			"filter/OUTPUT":   nil,
			"filter/FORWARD":  nil,
			"nat/PREROUTING":  nil,
			"nat/OUTPUT":      nil,
			"nat/POSTROUTING": nil,
		},
	}
}

func (n *fakeNetfilter) Insert(table, chain string, pos int, args ...string) error {
	k := table + "/" + chain
	if rules, ok := n.n[k]; ok {
		if pos > len(rules)+1 {
			n.t.Errorf("bad position %d in %s", pos, k)
			return errExec
		}
		rules = append(rules, "")
		copy(rules[pos:], rules[pos-1:])
		rules[pos-1] = strings.Join(args, " ")
		n.n[k] = rules
	} else {
		n.t.Errorf("unknown table/chain %s", k)
		return errExec
	}
	return nil
}

func (n *fakeNetfilter) Append(table, chain string, args ...string) error {
	k := table + "/" + chain
	return n.Insert(table, chain, len(n.n[k])+1, args...)
}

func (n *fakeNetfilter) Exists(table, chain string, args ...string) (bool, error) {
	k := table + "/" + chain
	if rules, ok := n.n[k]; ok {
		for _, rule := range rules {
			if rule == strings.Join(args, " ") {
				return true, nil
			}
		}
		return false, nil
	} else {
		n.t.Errorf("unknown table/chain %s", k)
		return false, errExec
	}
}

func (n *fakeNetfilter) Delete(table, chain string, args ...string) error {
	k := table + "/" + chain
	if rules, ok := n.n[k]; ok {
		for i, rule := range rules {
			if rule == strings.Join(args, " ") {
				rules = append(rules[:i], rules[i+1:]...)
				n.n[k] = rules
				return nil
			}
		}
		n.t.Errorf("delete of unknown rule %q from %s", strings.Join(args, " "), k)
		return errExec
	} else {
		n.t.Errorf("unknown table/chain %s", k)
		return errExec
	}
}

func (n *fakeNetfilter) ClearChain(table, chain string) error {
	k := table + "/" + chain
	if _, ok := n.n[k]; ok {
		n.n[k] = nil
		return nil
	} else {
		n.t.Logf("note: ClearChain: unknown table/chain %s", k)
		return errors.New("exitcode:1")
	}
}

func (n *fakeNetfilter) NewChain(table, chain string) error {
	k := table + "/" + chain
	if _, ok := n.n[k]; ok {
		n.t.Errorf("table/chain %s already exists", k)
		return errExec
	}
	n.n[k] = nil
	return nil
}

func (n *fakeNetfilter) DeleteChain(table, chain string) error {
	k := table + "/" + chain
	if rules, ok := n.n[k]; ok {
		if len(rules) != 0 {
			n.t.Errorf("%s is not empty", k)
			return errExec
		}
		delete(n.n, k)
		return nil
	} else {
		n.t.Errorf("%s does not exist", k)
		return errExec
	}
}

// fakeOS implements commandRunner and provides v4 and v6
// netfilterRunners, but captures changes without touching the OS.
type fakeOS struct {
	t          *testing.T
	up         bool
	ips        []string
	routes     []string
	rules      []string
	netfilter4 *fakeNetfilter
	netfilter6 *fakeNetfilter
}

func NewFakeOS(t *testing.T) *fakeOS {
	return &fakeOS{
		t:          t,
		netfilter4: newNetfilter(t),
		netfilter6: newNetfilter(t),
	}
}

var errExec = errors.New("execution failed")

func (o *fakeOS) String() string {
	var b strings.Builder
	if o.up {
		b.WriteString("up\n")
	} else {
		b.WriteString("down\n")
	}

	for _, ip := range o.ips {
		fmt.Fprintf(&b, "ip addr add %s\n", ip)
	}

	for _, route := range o.routes {
		fmt.Fprintf(&b, "ip route add %s\n", route)
	}

	for _, rule := range o.rules {
		fmt.Fprintf(&b, "ip rule add %s\n", rule)
	}

	var chains []string
	for chain := range o.netfilter4.n {
		chains = append(chains, chain)
	}
	sort.Strings(chains)
	for _, chain := range chains {
		for _, rule := range o.netfilter4.n[chain] {
			fmt.Fprintf(&b, "v4/%s %s\n", chain, rule)
		}
	}

	chains = nil
	for chain := range o.netfilter6.n {
		chains = append(chains, chain)
	}
	sort.Strings(chains)
	for _, chain := range chains {
		for _, rule := range o.netfilter6.n[chain] {
			fmt.Fprintf(&b, "v6/%s %s\n", chain, rule)
		}
	}

	return b.String()[:len(b.String())-1]
}

func (o *fakeOS) run(args ...string) error {
	unexpected := func() error {
		o.t.Errorf("unexpected invocation %q", strings.Join(args, " "))
		return errors.New("unrecognized invocation")
	}
	if args[0] != "ip" {
		return unexpected()
	}

	if len(args) == 2 && args[1] == "rule" {
		// naked invocation of `ip rule` is a feature test. Return
		// successfully.
		return nil
	}

	family := ""
	rest := strings.Join(args[3:], " ")
	if args[1] == "-4" || args[1] == "-6" {
		family = args[1]
		copy(args[1:], args[2:])
		args = args[:len(args)-1]
		rest = family + " " + strings.Join(args[3:], " ")
	}

	var l *[]string
	switch args[1] {
	case "link":
		got := strings.Join(args[2:], " ")
		switch got {
		case "set dev tailscale0 up":
			o.up = true
		case "set dev tailscale0 down":
			o.up = false
		default:
			return unexpected()
		}
		return nil
	case "addr":
		l = &o.ips
	case "route":
		l = &o.routes
	case "rule":
		l = &o.rules
	default:
		return unexpected()
	}

	switch args[2] {
	case "add":
		for _, el := range *l {
			if el == rest {
				o.t.Errorf("can't add %q, already present", rest)
				return errors.New("already exists")
			}
		}
		*l = append(*l, rest)
		sort.Strings(*l)
	case "del":
		found := false
		for i, el := range *l {
			if el == rest {
				found = true
				*l = append((*l)[:i], (*l)[i+1:]...)
				break
			}
		}
		if !found {
			o.t.Logf("note: can't delete %q, not present", rest)
			// 'ip rule del' exits with code 2 when a row is
			// missing. We don't want to consider that an error,
			// for cleanup purposes.

			// TODO(apenwarr): this is a hack.
			// I'd like to return an exec.ExitError(2) here, but
			// I can't, because the ExitCode is implemented in
			// os.ProcessState, which is an opaque object I can't
			// instantiate or modify. Go's 75 levels of abstraction
			// between me and an 8-bit int are really paying off
			// here, as you can see.
			return errors.New("exitcode:2")
		}
	default:
		return unexpected()
	}

	return nil
}

func (o *fakeOS) output(args ...string) ([]byte, error) {
	want := "ip rule list priority 10000"
	got := strings.Join(args, " ")
	if got != want {
		o.t.Errorf("unexpected command that wants output: %v", got)
		return nil, errExec
	}

	var ret []string
	for _, rule := range o.rules {
		if strings.Contains(rule, "10000") {
			ret = append(ret, rule)
		}
	}
	return []byte(strings.Join(ret, "\n")), nil
}

var tunTestNum int64

func createTestTUN(t *testing.T) tun.Device {
	const minimalMTU = 1280
	tunName := fmt.Sprintf("tuntest%d", atomic.AddInt64(&tunTestNum, 1))
	tun, err := tun.CreateTUN(tunName, minimalMTU)
	if err != nil {
		t.Fatalf("CreateTUN(%q): %v", tunName, err)
	}
	return tun
}

type linuxTest struct {
	tun       tun.Device
	mon       *netmon.Monitor
	r         *linuxRouter
	logOutput tstest.MemLogger
}

func (lt *linuxTest) Close() error {
	if lt.tun != nil {
		lt.tun.Close()
	}
	if lt.mon != nil {
		lt.mon.Close()
	}
	return nil
}

func newLinuxRootTest(t *testing.T) *linuxTest {
	if os.Getuid() != 0 {
		t.Skip("test requires root")
	}

	lt := new(linuxTest)
	lt.tun = createTestTUN(t)

	logf := lt.logOutput.Logf

	mon, err := netmon.New(logger.Discard)
	if err != nil {
		lt.Close()
		t.Fatal(err)
	}
	mon.Start()
	lt.mon = mon

	r, err := newUserspaceRouter(logf, lt.tun, mon)
	if err != nil {
		lt.Close()
		t.Fatal(err)
	}
	lr := r.(*linuxRouter)
	if err := lr.upInterface(); err != nil {
		lt.Close()
		t.Fatal(err)
	}
	lt.r = lr
	return lt
}

func TestDelRouteIdempotent(t *testing.T) {
	lt := newLinuxRootTest(t)
	defer lt.Close()

	for _, s := range []string{
		"192.0.2.0/24",  // RFC 5737
		"2001:DB8::/32", // RFC 3849
	} {
		cidr := netip.MustParsePrefix(s)
		if err := lt.r.addRoute(cidr); err != nil {
			t.Error(err)
			continue
		}
		for i := 0; i < 2; i++ {
			if err := lt.r.delRoute(cidr); err != nil {
				t.Errorf("delRoute(i=%d): %v", i, err)
			}
		}
	}

	if t.Failed() {
		out := lt.logOutput.String()
		t.Logf("Log output:\n%s", out)
	}
}

func TestAddRemoveRules(t *testing.T) {
	lt := newLinuxRootTest(t)
	defer lt.Close()
	r := lt.r

	step := func(name string, f func() error) {
		t.Logf("Doing %v ...", name)
		if err := f(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		rules, err := netlink.RuleList(netlink.FAMILY_ALL)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rules {
			if r.Priority >= 5000 && r.Priority <= 5999 {
				t.Logf("Rule: %+v", r)
			}
		}

	}

	step("init_del_and_add", r.addIPRules)
	step("dup_add", r.justAddIPRules)
	step("del", r.delIPRules)
	step("dup_del", r.delIPRules)

}

func TestDebugListLinks(t *testing.T) {
	links, err := netlink.LinkList()
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range links {
		t.Logf("Link: %+v", ln)
	}
}

func TestDebugListRoutes(t *testing.T) {
	// We need to pass a non-nil route to RouteListFiltered, along
	// with the netlink.RT_FILTER_TABLE bit set in the filter
	// mask, otherwise it ignores non-main routes.
	filter := &netlink.Route{}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, filter, netlink.RT_FILTER_TABLE)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routes {
		t.Logf("Route: %+v", r)
	}
}

var famName = map[int]string{
	netlink.FAMILY_ALL: "all",
	netlink.FAMILY_V4:  "v4",
	netlink.FAMILY_V6:  "v6",
}

func TestDebugListRules(t *testing.T) {
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6, netlink.FAMILY_ALL} {
		t.Run(famName[fam], func(t *testing.T) {
			rules, err := netlink.RuleList(fam)
			if err != nil {
				t.Skipf("skip; RuleList fails with: %v", err)
			}
			for _, r := range rules {
				t.Logf("Rule: %+v", r)
			}
		})
	}
}

func TestCheckIPRuleSupportsV6(t *testing.T) {
	err := checkIPRuleSupportsV6(t.Logf)
	if err != nil && os.Getuid() != 0 {
		t.Skipf("skipping, error when not root: %v", err)
	}
	// Just log it. For interactive testing only.
	// Some machines running our tests might not have IPv6.
	t.Logf("Got: %v", err)
}

func TestBusyboxParseVersion(t *testing.T) {
	input := `BusyBox v1.34.1 (2022-09-01 16:10:29 UTC) multi-call binary.
BusyBox is copyrighted by many authors between 1998-2015.
Licensed under GPLv2. See source distribution for detailed
copyright notices.

Usage: busybox [function [arguments]...]
   or: busybox --list[-full]
   or: busybox --show SCRIPT
   or: busybox --install [-s] [DIR]
   or: function [arguments]...

	BusyBox is a multi-call binary that combines many common Unix
	utilities into a single executable.  Most people will create a
	link to busybox for each function they wish to use and BusyBox
	will act like whatever it was invoked as.
`

	v1, v2, v3, err := busyboxParseVersion(input)
	if err != nil {
		t.Fatalf("busyboxParseVersion() failed: %v", err)
	}

	if got, want := fmt.Sprintf("%d.%d.%d", v1, v2, v3), "1.34.1"; got != want {
		t.Errorf("version = %q, want %q", got, want)
	}
}

func TestCIDRDiff(t *testing.T) {
	pfx := func(p ...string) []netip.Prefix {
		var ret []netip.Prefix
		for _, s := range p {
			ret = append(ret, netip.MustParsePrefix(s))
		}
		return ret
	}
	tests := []struct {
		old     []netip.Prefix
		new     []netip.Prefix
		wantAdd []netip.Prefix
		wantDel []netip.Prefix
		final   []netip.Prefix
	}{
		{
			old:     nil,
			new:     pfx("1.1.1.1/32"),
			wantAdd: pfx("1.1.1.1/32"),
			final:   pfx("1.1.1.1/32"),
		},
		{
			old:   pfx("1.1.1.1/32"),
			new:   pfx("1.1.1.1/32"),
			final: pfx("1.1.1.1/32"),
		},
		{
			old:     pfx("1.1.1.1/32", "2.3.4.5/32"),
			new:     pfx("1.1.1.1/32"),
			wantDel: pfx("2.3.4.5/32"),
			final:   pfx("1.1.1.1/32"),
		},
		{
			old:     pfx("1.1.1.1/32", "2.3.4.5/32"),
			new:     pfx("1.0.0.0/32", "3.4.5.6/32"),
			wantDel: pfx("1.1.1.1/32", "2.3.4.5/32"),
			wantAdd: pfx("1.0.0.0/32", "3.4.5.6/32"),
			final:   pfx("1.0.0.0/32", "3.4.5.6/32"),
		},
	}
	for _, tc := range tests {
		om := make(map[netip.Prefix]bool)
		for _, p := range tc.old {
			om[p] = true
		}
		var added []netip.Prefix
		var deleted []netip.Prefix
		fm, err := cidrDiff("test", om, tc.new, func(p netip.Prefix) error {
			if len(deleted) > 0 {
				t.Error("delete called before add")
			}
			added = append(added, p)
			return nil
		}, func(p netip.Prefix) error {
			deleted = append(deleted, p)
			return nil
		}, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		slices.SortFunc(added, func(a, b netip.Prefix) bool { return a.Addr().Less(b.Addr()) })
		slices.SortFunc(deleted, func(a, b netip.Prefix) bool { return a.Addr().Less(b.Addr()) })
		if !reflect.DeepEqual(added, tc.wantAdd) {
			t.Errorf("added = %v, want %v", added, tc.wantAdd)
		}
		if !reflect.DeepEqual(deleted, tc.wantDel) {
			t.Errorf("deleted = %v, want %v", deleted, tc.wantDel)
		}

		// Check that the final state is correct.
		if len(fm) != len(tc.final) {
			t.Fatalf("final state = %v, want %v", fm, tc.final)
		}
		for _, p := range tc.final {
			if !fm[p] {
				t.Errorf("final state = %v, want %v", fm, tc.final)
			}
		}
	}
}

var (
	fwmaskSupported     bool
	fwmaskSupportedOnce sync.Once
	fwmaskAdjustRe      = regexp.MustCompile(`(?m)(fwmark 0x[0-9a-f]+)/0x[0-9a-f]+`)
)

// adjustFwmask removes the "/0xmask" string from fwmask stanzas if the
// installed 'ip' binary does not support that format.
func adjustFwmask(t *testing.T, s string) string {
	t.Helper()
	fwmaskSupportedOnce.Do(func() {
		fwmaskSupported, _ = ipCmdSupportsFwmask()
	})
	if fwmaskSupported {
		return s
	}

	return fwmaskAdjustRe.ReplaceAllString(s, "$1")
}
