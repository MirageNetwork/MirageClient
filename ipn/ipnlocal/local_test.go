// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package ipnlocal

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"go4.org/netipx"
	"tailscale.com/control/controlclient"
	"tailscale.com/ipn"
	"tailscale.com/ipn/store/mem"
	"tailscale.com/net/interfaces"
	"tailscale.com/net/tsaddr"
	"tailscale.com/tailcfg"
	"tailscale.com/tsd"
	"tailscale.com/tstest"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/types/logid"
	"tailscale.com/types/netmap"
	"tailscale.com/wgengine"
	"tailscale.com/wgengine/filter"
	"tailscale.com/wgengine/wgcfg"
)

func TestNetworkMapCompare(t *testing.T) {
	prefix1, err := netip.ParsePrefix("192.168.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	node1 := &tailcfg.Node{Addresses: []netip.Prefix{prefix1}}

	prefix2, err := netip.ParsePrefix("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	node2 := &tailcfg.Node{Addresses: []netip.Prefix{prefix2}}

	tests := []struct {
		name string
		a, b *netmap.NetworkMap
		want bool
	}{
		{
			"both nil",
			nil,
			nil,
			true,
		},
		{
			"b nil",
			&netmap.NetworkMap{},
			nil,
			false,
		},
		{
			"a nil",
			nil,
			&netmap.NetworkMap{},
			false,
		},
		{
			"both default",
			&netmap.NetworkMap{},
			&netmap.NetworkMap{},
			true,
		},
		{
			"names identical",
			&netmap.NetworkMap{Name: "map1"},
			&netmap.NetworkMap{Name: "map1"},
			true,
		},
		{
			"names differ",
			&netmap.NetworkMap{Name: "map1"},
			&netmap.NetworkMap{Name: "map2"},
			false,
		},
		{
			"Peers identical",
			&netmap.NetworkMap{Peers: []*tailcfg.Node{}},
			&netmap.NetworkMap{Peers: []*tailcfg.Node{}},
			true,
		},
		{
			"Peer list length",
			// length of Peers list differs
			&netmap.NetworkMap{Peers: []*tailcfg.Node{{}}},
			&netmap.NetworkMap{Peers: []*tailcfg.Node{}},
			false,
		},
		{
			"Node names identical",
			&netmap.NetworkMap{Peers: []*tailcfg.Node{{Name: "A"}}},
			&netmap.NetworkMap{Peers: []*tailcfg.Node{{Name: "A"}}},
			true,
		},
		{
			"Node names differ",
			&netmap.NetworkMap{Peers: []*tailcfg.Node{{Name: "A"}}},
			&netmap.NetworkMap{Peers: []*tailcfg.Node{{Name: "B"}}},
			false,
		},
		{
			"Node lists identical",
			&netmap.NetworkMap{Peers: []*tailcfg.Node{node1, node1}},
			&netmap.NetworkMap{Peers: []*tailcfg.Node{node1, node1}},
			true,
		},
		{
			"Node lists differ",
			&netmap.NetworkMap{Peers: []*tailcfg.Node{node1, node1}},
			&netmap.NetworkMap{Peers: []*tailcfg.Node{node1, node2}},
			false,
		},
		{
			"Node Users differ",
			// User field is not checked.
			&netmap.NetworkMap{Peers: []*tailcfg.Node{{User: 0}}},
			&netmap.NetworkMap{Peers: []*tailcfg.Node{{User: 1}}},
			true,
		},
	}
	for _, tt := range tests {
		got := dnsMapsEqual(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("%s: Equal = %v; want %v", tt.name, got, tt.want)
		}
	}
}

func inRemove(ip netip.Addr) bool {
	for _, pfx := range removeFromDefaultRoute {
		if pfx.Contains(ip) {
			return true
		}
	}
	return false
}

func TestShrinkDefaultRoute(t *testing.T) {
	tests := []struct {
		route     string
		in        []string
		out       []string
		localIPFn func(netip.Addr) bool // true if this machine's local IP address should be "in" after shrinking.
	}{
		{
			route: "0.0.0.0/0",
			in:    []string{"1.2.3.4", "25.0.0.1"},
			out: []string{
				"10.0.0.1",
				"10.255.255.255",
				"192.168.0.1",
				"192.168.255.255",
				"172.16.0.1",
				"172.31.255.255",
				"100.101.102.103",
				"224.0.0.1",
				"169.254.169.254",
				// Some random IPv6 stuff that shouldn't be in a v4
				// default route.
				"fe80::",
				"2601::1",
			},
			localIPFn: func(ip netip.Addr) bool { return !inRemove(ip) && ip.Is4() },
		},
		{
			route: "::/0",
			in:    []string{"::1", "2601::1"},
			out: []string{
				"fe80::1",
				"ff00::1",
				tsaddr.TailscaleULARange().Addr().String(),
			},
			localIPFn: func(ip netip.Addr) bool { return !inRemove(ip) && ip.Is6() },
		},
	}

	// Construct a fake local network environment to make this test hermetic.
	// localInterfaceRoutes and hostIPs would normally come from calling interfaceRoutes,
	// and localAddresses would normally come from calling interfaces.LocalAddresses.
	var b netipx.IPSetBuilder
	for _, c := range []string{"127.0.0.0/8", "192.168.9.0/24", "fe80::/32"} {
		p := netip.MustParsePrefix(c)
		b.AddPrefix(p)
	}
	localInterfaceRoutes, err := b.IPSet()
	if err != nil {
		t.Fatal(err)
	}
	hostIPs := []netip.Addr{
		netip.MustParseAddr("127.0.0.1"),
		netip.MustParseAddr("192.168.9.39"),
		netip.MustParseAddr("fe80::1"),
		netip.MustParseAddr("fe80::437d:feff:feca:49a7"),
	}
	localAddresses := []netip.Addr{
		netip.MustParseAddr("192.168.9.39"),
	}

	for _, test := range tests {
		def := netip.MustParsePrefix(test.route)
		got, err := shrinkDefaultRoute(def, localInterfaceRoutes, hostIPs)
		if err != nil {
			t.Fatalf("shrinkDefaultRoute(%q): %v", test.route, err)
		}
		for _, ip := range test.in {
			if !got.Contains(netip.MustParseAddr(ip)) {
				t.Errorf("shrink(%q).Contains(%v) = false, want true", test.route, ip)
			}
		}
		for _, ip := range test.out {
			if got.Contains(netip.MustParseAddr(ip)) {
				t.Errorf("shrink(%q).Contains(%v) = true, want false", test.route, ip)
			}
		}
		for _, ip := range localAddresses {
			want := test.localIPFn(ip)
			if gotContains := got.Contains(ip); gotContains != want {
				t.Errorf("shrink(%q).Contains(%v) = %v, want %v", test.route, ip, gotContains, want)
			}
		}
	}
}

func TestPeerRoutes(t *testing.T) {
	pp := netip.MustParsePrefix
	tests := []struct {
		name  string
		peers []wgcfg.Peer
		want  []netip.Prefix
	}{
		{
			name: "small_v4",
			peers: []wgcfg.Peer{
				{
					AllowedIPs: []netip.Prefix{
						pp("100.101.102.103/32"),
					},
				},
			},
			want: []netip.Prefix{
				pp("100.101.102.103/32"),
			},
		},
		{
			name: "big_v4",
			peers: []wgcfg.Peer{
				{
					AllowedIPs: []netip.Prefix{
						pp("100.101.102.103/32"),
						pp("100.101.102.104/32"),
						pp("100.101.102.105/32"),
					},
				},
			},
			want: []netip.Prefix{
				pp("100.64.0.0/10"),
			},
		},
		{
			name: "has_1_v6",
			peers: []wgcfg.Peer{
				{
					AllowedIPs: []netip.Prefix{
						pp("fd7a:115c:a1e0:ab12:4843:cd96:6258:b240/128"),
					},
				},
			},
			want: []netip.Prefix{
				pp("fd7a:115c:a1e0::/48"),
			},
		},
		{
			name: "has_2_v6",
			peers: []wgcfg.Peer{
				{
					AllowedIPs: []netip.Prefix{
						pp("fd7a:115c:a1e0:ab12:4843:cd96:6258:b240/128"),
						pp("fd7a:115c:a1e0:ab12:4843:cd96:6258:b241/128"),
					},
				},
			},
			want: []netip.Prefix{
				pp("fd7a:115c:a1e0::/48"),
			},
		},
		{
			name: "big_v4_big_v6",
			peers: []wgcfg.Peer{
				{
					AllowedIPs: []netip.Prefix{
						pp("100.101.102.103/32"),
						pp("100.101.102.104/32"),
						pp("100.101.102.105/32"),
						pp("fd7a:115c:a1e0:ab12:4843:cd96:6258:b240/128"),
						pp("fd7a:115c:a1e0:ab12:4843:cd96:6258:b241/128"),
					},
				},
			},
			want: []netip.Prefix{
				pp("100.64.0.0/10"),
				pp("fd7a:115c:a1e0::/48"),
			},
		},
		{
			name: "output-should-be-sorted",
			peers: []wgcfg.Peer{
				{
					AllowedIPs: []netip.Prefix{
						pp("100.64.0.2/32"),
						pp("10.0.0.0/16"),
					},
				},
				{
					AllowedIPs: []netip.Prefix{
						pp("100.64.0.1/32"),
						pp("10.0.0.0/8"),
					},
				},
			},
			want: []netip.Prefix{
				pp("10.0.0.0/8"),
				pp("10.0.0.0/16"),
				pp("100.64.0.1/32"),
				pp("100.64.0.2/32"),
			},
		},
		{
			name: "skip-unmasked-prefixes",
			peers: []wgcfg.Peer{
				{
					PublicKey: key.NewNode().Public(),
					AllowedIPs: []netip.Prefix{
						pp("100.64.0.2/32"),
						pp("10.0.0.100/16"),
					},
				},
			},
			want: []netip.Prefix{
				pp("100.64.0.2/32"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := peerRoutes(t.Logf, tt.peers, 2)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got = %v; want %v", got, tt.want)
			}
		})
	}

}

func TestPeerAPIBase(t *testing.T) {
	tests := []struct {
		name string
		nm   *netmap.NetworkMap
		peer *tailcfg.Node
		want string
	}{
		{
			name: "nil_netmap",
			peer: new(tailcfg.Node),
			want: "",
		},
		{
			name: "nil_peer",
			nm:   new(netmap.NetworkMap),
			want: "",
		},
		{
			name: "self_only_4_them_both",
			nm: &netmap.NetworkMap{
				Addresses: []netip.Prefix{
					netip.MustParsePrefix("100.64.1.1/32"),
				},
			},
			peer: &tailcfg.Node{
				Addresses: []netip.Prefix{
					netip.MustParsePrefix("100.64.1.2/32"),
					netip.MustParsePrefix("fe70::2/128"),
				},
				Hostinfo: (&tailcfg.Hostinfo{
					Services: []tailcfg.Service{
						{Proto: "peerapi4", Port: 444},
						{Proto: "peerapi6", Port: 666},
					},
				}).View(),
			},
			want: "http://100.64.1.2:444",
		},
		{
			name: "self_only_6_them_both",
			nm: &netmap.NetworkMap{
				Addresses: []netip.Prefix{
					netip.MustParsePrefix("fe70::1/128"),
				},
			},
			peer: &tailcfg.Node{
				Addresses: []netip.Prefix{
					netip.MustParsePrefix("100.64.1.2/32"),
					netip.MustParsePrefix("fe70::2/128"),
				},
				Hostinfo: (&tailcfg.Hostinfo{
					Services: []tailcfg.Service{
						{Proto: "peerapi4", Port: 444},
						{Proto: "peerapi6", Port: 666},
					},
				}).View(),
			},
			want: "http://[fe70::2]:666",
		},
		{
			name: "self_both_them_only_4",
			nm: &netmap.NetworkMap{
				Addresses: []netip.Prefix{
					netip.MustParsePrefix("100.64.1.1/32"),
					netip.MustParsePrefix("fe70::1/128"),
				},
			},
			peer: &tailcfg.Node{
				Addresses: []netip.Prefix{
					netip.MustParsePrefix("100.64.1.2/32"),
					netip.MustParsePrefix("fe70::2/128"),
				},
				Hostinfo: (&tailcfg.Hostinfo{
					Services: []tailcfg.Service{
						{Proto: "peerapi4", Port: 444},
					},
				}).View(),
			},
			want: "http://100.64.1.2:444",
		},
		{
			name: "self_both_them_only_6",
			nm: &netmap.NetworkMap{
				Addresses: []netip.Prefix{
					netip.MustParsePrefix("100.64.1.1/32"),
					netip.MustParsePrefix("fe70::1/128"),
				},
			},
			peer: &tailcfg.Node{
				Addresses: []netip.Prefix{
					netip.MustParsePrefix("100.64.1.2/32"),
					netip.MustParsePrefix("fe70::2/128"),
				},
				Hostinfo: (&tailcfg.Hostinfo{
					Services: []tailcfg.Service{
						{Proto: "peerapi6", Port: 666},
					},
				}).View(),
			},
			want: "http://[fe70::2]:666",
		},
		{
			name: "self_both_them_no_peerapi_service",
			nm: &netmap.NetworkMap{
				Addresses: []netip.Prefix{
					netip.MustParsePrefix("100.64.1.1/32"),
					netip.MustParsePrefix("fe70::1/128"),
				},
			},
			peer: &tailcfg.Node{
				Addresses: []netip.Prefix{
					netip.MustParsePrefix("100.64.1.2/32"),
					netip.MustParsePrefix("fe70::2/128"),
				},
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := peerAPIBase(tt.nm, tt.peer)
			if got != tt.want {
				t.Errorf("got %q; want %q", got, tt.want)
			}
		})
	}
}

type panicOnUseTransport struct{}

func (panicOnUseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	panic("unexpected HTTP request")
}

// Issue 1573: don't generate a machine key if we don't want to be running.
func TestLazyMachineKeyGeneration(t *testing.T) {
	tstest.Replace(t, &panicOnMachineKeyGeneration, func() bool { return true })

	var logf logger.Logf = logger.Discard
	sys := new(tsd.System)
	store := new(mem.Store)
	sys.Set(store)
	eng, err := wgengine.NewFakeUserspaceEngine(logf, sys.Set)
	if err != nil {
		t.Fatalf("NewFakeUserspaceEngine: %v", err)
	}
	t.Cleanup(eng.Close)
	sys.Set(eng)
	lb, err := NewLocalBackend(logf, logid.PublicID{}, sys, 0)
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}

	lb.SetHTTPTestClient(&http.Client{
		Transport: panicOnUseTransport{}, // validate we don't send HTTP requests
	})

	if err := lb.Start(ipn.Options{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Give the controlclient package goroutines (if they're
	// accidentally started) extra time to schedule and run (and thus
	// hit panicOnUseTransport).
	time.Sleep(500 * time.Millisecond)
}

func TestFileTargets(t *testing.T) {
	b := new(LocalBackend)
	_, err := b.FileTargets()
	if got, want := fmt.Sprint(err), "not connected to the tailnet"; got != want {
		t.Errorf("before connect: got %q; want %q", got, want)
	}

	b.netMap = new(netmap.NetworkMap)
	_, err = b.FileTargets()
	if got, want := fmt.Sprint(err), "not connected to the tailnet"; got != want {
		t.Errorf("non-running netmap: got %q; want %q", got, want)
	}

	b.state = ipn.Running
	_, err = b.FileTargets()
	if got, want := fmt.Sprint(err), "file sharing not enabled by Tailscale admin"; got != want {
		t.Errorf("without cap: got %q; want %q", got, want)
	}

	b.capFileSharing = true
	got, err := b.FileTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("unexpected %d peers", len(got))
	}
	// (other cases handled by TestPeerAPIBase above)
}

func TestInternalAndExternalInterfaces(t *testing.T) {
	type interfacePrefix struct {
		i   interfaces.Interface
		pfx netip.Prefix
	}

	masked := func(ips ...interfacePrefix) (pfxs []netip.Prefix) {
		for _, ip := range ips {
			pfxs = append(pfxs, ip.pfx.Masked())
		}
		return pfxs
	}
	iList := func(ips ...interfacePrefix) (il interfaces.List) {
		for _, ip := range ips {
			il = append(il, ip.i)
		}
		return il
	}
	newInterface := func(name, pfx string, wsl2, loopback bool) interfacePrefix {
		ippfx := netip.MustParsePrefix(pfx)
		ip := interfaces.Interface{
			Interface: &net.Interface{},
			AltAddrs: []net.Addr{
				netipx.PrefixIPNet(ippfx),
			},
		}
		if loopback {
			ip.Flags = net.FlagLoopback
		}
		if wsl2 {
			ip.HardwareAddr = []byte{0x00, 0x15, 0x5d, 0x00, 0x00, 0x00}
		}
		return interfacePrefix{i: ip, pfx: ippfx}
	}
	var (
		en0      = newInterface("en0", "10.20.2.5/16", false, false)
		en1      = newInterface("en1", "192.168.1.237/24", false, false)
		wsl      = newInterface("wsl", "192.168.5.34/24", true, false)
		loopback = newInterface("lo0", "127.0.0.1/8", false, true)
	)

	tests := []struct {
		name    string
		goos    string
		il      interfaces.List
		wantInt []netip.Prefix
		wantExt []netip.Prefix
	}{
		{
			name: "single-interface",
			goos: "linux",
			il: iList(
				en0,
				loopback,
			),
			wantInt: masked(loopback),
			wantExt: masked(en0),
		},
		{
			name: "multiple-interfaces",
			goos: "linux",
			il: iList(
				en0,
				en1,
				wsl,
				loopback,
			),
			wantInt: masked(loopback),
			wantExt: masked(en0, en1, wsl),
		},
		{
			name: "wsl2",
			goos: "windows",
			il: iList(
				en0,
				en1,
				wsl,
				loopback,
			),
			wantInt: masked(loopback, wsl),
			wantExt: masked(en0, en1),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotInt, gotExt, err := internalAndExternalInterfacesFrom(tc.il, tc.goos)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotInt, tc.wantInt) {
				t.Errorf("unexpected internal prefixes\ngot %v\nwant %v", gotInt, tc.wantInt)
			}
			if !reflect.DeepEqual(gotExt, tc.wantExt) {
				t.Errorf("unexpected external prefixes\ngot %v\nwant %v", gotExt, tc.wantExt)
			}
		})
	}
}

func TestPacketFilterPermitsUnlockedNodes(t *testing.T) {
	tests := []struct {
		name   string
		peers  []*tailcfg.Node
		filter []filter.Match
		want   bool
	}{
		{
			name: "empty",
			want: false,
		},
		{
			name: "no-unsigned",
			peers: []*tailcfg.Node{
				{ID: 1},
			},
			want: false,
		},
		{
			name: "unsigned-good",
			peers: []*tailcfg.Node{
				{ID: 1, UnsignedPeerAPIOnly: true},
			},
			want: false,
		},
		{
			name: "unsigned-bad",
			peers: []*tailcfg.Node{
				{
					ID:                  1,
					UnsignedPeerAPIOnly: true,
					AllowedIPs: []netip.Prefix{
						netip.MustParsePrefix("100.64.0.0/32"),
					},
				},
			},
			filter: []filter.Match{
				{
					Srcs: []netip.Prefix{netip.MustParsePrefix("100.64.0.0/32")},
					Dsts: []filter.NetPortRange{
						{
							Net: netip.MustParsePrefix("100.99.0.0/32"),
						},
					},
				},
			},
			want: true,
		},
		{
			name: "unsigned-bad-src-is-superset",
			peers: []*tailcfg.Node{
				{
					ID:                  1,
					UnsignedPeerAPIOnly: true,
					AllowedIPs: []netip.Prefix{
						netip.MustParsePrefix("100.64.0.0/32"),
					},
				},
			},
			filter: []filter.Match{
				{
					Srcs: []netip.Prefix{netip.MustParsePrefix("100.64.0.0/24")},
					Dsts: []filter.NetPortRange{
						{
							Net: netip.MustParsePrefix("100.99.0.0/32"),
						},
					},
				},
			},
			want: true,
		},
		{
			name: "unsigned-okay-because-no-dsts",
			peers: []*tailcfg.Node{
				{
					ID:                  1,
					UnsignedPeerAPIOnly: true,
					AllowedIPs: []netip.Prefix{
						netip.MustParsePrefix("100.64.0.0/32"),
					},
				},
			},
			filter: []filter.Match{
				{
					Srcs: []netip.Prefix{netip.MustParsePrefix("100.64.0.0/32")},
					Caps: []filter.CapMatch{
						{
							Dst: netip.MustParsePrefix("100.99.0.0/32"),
							Cap: "foo",
						},
					},
				},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := packetFilterPermitsUnlockedNodes(tt.peers, tt.filter); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}

}

func TestStatusWithoutPeers(t *testing.T) {
	logf := tstest.WhileTestRunningLogger(t)
	store := new(testStateStorage)
	sys := new(tsd.System)
	sys.Set(store)
	e, err := wgengine.NewFakeUserspaceEngine(logf, sys.Set)
	if err != nil {
		t.Fatalf("NewFakeUserspaceEngine: %v", err)
	}
	sys.Set(e)
	t.Cleanup(e.Close)

	b, err := NewLocalBackend(logf, logid.PublicID{}, sys, 0)
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	var cc *mockControl
	b.SetControlClientGetterForTesting(func(opts controlclient.Options) (controlclient.Client, error) {
		cc = newClient(t, opts)

		t.Logf("ccGen: new mockControl.")
		cc.called("New")
		return cc, nil
	})
	b.Start(ipn.Options{})
	b.Login(nil)
	cc.send(nil, "", false, &netmap.NetworkMap{
		MachineStatus: tailcfg.MachineAuthorized,
		Addresses:     ipps("100.101.101.101"),
		SelfNode: &tailcfg.Node{
			Addresses: ipps("100.101.101.101"),
		},
	})
	got := b.StatusWithoutPeers()
	if got.TailscaleIPs == nil {
		t.Errorf("got nil, expected TailscaleIPs value to not be nil")
	}
	if !reflect.DeepEqual(got.TailscaleIPs, got.Self.TailscaleIPs) {
		t.Errorf("got %v, expected %v", got.TailscaleIPs, got.Self.TailscaleIPs)
	}
}

// legacyBackend was the interface between Tailscale frontends
// (e.g. cmd/tailscale, iOS/MacOS/Windows GUIs) and the tailscale
// backend (e.g. cmd/tailscaled) running on the same machine.
// (It has nothing to do with the interface between the backends
// and the cloud control plane.)
type legacyBackend interface {
	// SetNotifyCallback sets the callback to be called on updates
	// from the backend to the client.
	SetNotifyCallback(func(ipn.Notify))
	// Start starts or restarts the backend, typically when a
	// frontend client connects.
	Start(ipn.Options) error
	// StartLoginInteractive requests to start a new interactive login
	// flow. This should trigger a new BrowseToURL notification
	// eventually.
	StartLoginInteractive()
	// Login logs in with an OAuth2 token.
	Login(token *tailcfg.Oauth2Token)
	// Logout terminates the current login session and stops the
	// wireguard engine.
	Logout()
	// SetPrefs installs a new set of user preferences, including
	// WantRunning. This may cause the wireguard engine to
	// reconfigure or stop.
	SetPrefs(*ipn.Prefs)
	// RequestEngineStatus polls for an update from the wireguard
	// engine. Only needed if you want to display byte
	// counts. Connection events are emitted automatically without
	// polling.
	RequestEngineStatus()
}

// Verify that LocalBackend still implements the legacyBackend interface
// for now, at least until the macOS and iOS clients move off of it.
var _ legacyBackend = (*LocalBackend)(nil)

func TestWatchNotificationsCallbacks(t *testing.T) {
	b := new(LocalBackend)
	n := new(ipn.Notify)
	b.WatchNotifications(context.Background(), 0, func() {
		b.mu.Lock()
		defer b.mu.Unlock()

		// Ensure a watcher has been installed.
		if len(b.notifyWatchers) != 1 {
			t.Fatalf("unexpected number of watchers in new LocalBackend, want: 1 got: %v", len(b.notifyWatchers))
		}
		// Send a notification. Range over notifyWatchers to get the channel
		// because WatchNotifications doesn't expose the handle for it.
		for _, c := range b.notifyWatchers {
			select {
			case c <- n:
			default:
				t.Fatalf("could not send notification")
			}
		}
	}, func(roNotify *ipn.Notify) bool {
		if roNotify != n {
			t.Fatalf("unexpected notification received. want: %v got: %v", n, roNotify)
		}
		return false
	})

	// Ensure watchers have been cleaned up.
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.notifyWatchers) != 0 {
		t.Fatalf("unexpected number of watchers in new LocalBackend, want: 0 got: %v", len(b.notifyWatchers))
	}
}
