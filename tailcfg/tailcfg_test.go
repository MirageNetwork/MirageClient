// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package tailcfg_test

import (
	"encoding"
	"encoding/json"
	"net/netip"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	. "tailscale.com/tailcfg"
	"tailscale.com/tstest"
	"tailscale.com/types/key"
	"tailscale.com/types/ptr"
	"tailscale.com/util/must"
	"tailscale.com/version"
)

func fieldsOf(t reflect.Type) (fields []string) {
	for i := 0; i < t.NumField(); i++ {
		fields = append(fields, t.Field(i).Name)
	}
	return
}

func TestHostinfoEqual(t *testing.T) {
	hiHandles := []string{
		"IPNVersion",
		"FrontendLogID",
		"BackendLogID",
		"OS",
		"OSVersion",
		"Container",
		"Env",
		"Distro",
		"DistroVersion",
		"DistroCodeName",
		"App",
		"Desktop",
		"Package",
		"DeviceModel",
		"PushDeviceToken",
		"Hostname",
		"ShieldsUp",
		"ShareeNode",
		"NoLogsNoSupport",
		"WireIngress",
		"AllowsUpdate",
		"Machine",
		"GoArch",
		"GoArchVar",
		"GoVersion",
		"RoutableIPs",
		"RequestTags",
		"Services",
		"NetInfo",
		"SSH_HostKeys",
		"Cloud",
		"Userspace",
		"UserspaceRouter",
	}
	if have := fieldsOf(reflect.TypeOf(Hostinfo{})); !reflect.DeepEqual(have, hiHandles) {
		t.Errorf("Hostinfo.Equal check might be out of sync\nfields: %q\nhandled: %q\n",
			have, hiHandles)
	}

	nets := func(strs ...string) (ns []netip.Prefix) {
		for _, s := range strs {
			n, err := netip.ParsePrefix(s)
			if err != nil {
				panic(err)
			}
			ns = append(ns, n)
		}
		return ns
	}
	tests := []struct {
		a, b *Hostinfo
		want bool
	}{
		{
			nil,
			nil,
			true,
		},
		{
			&Hostinfo{},
			nil,
			false,
		},
		{
			nil,
			&Hostinfo{},
			false,
		},
		{
			&Hostinfo{},
			&Hostinfo{},
			true,
		},

		{
			&Hostinfo{IPNVersion: "1"},
			&Hostinfo{IPNVersion: "2"},
			false,
		},
		{
			&Hostinfo{IPNVersion: "2"},
			&Hostinfo{IPNVersion: "2"},
			true,
		},

		{
			&Hostinfo{FrontendLogID: "1"},
			&Hostinfo{FrontendLogID: "2"},
			false,
		},
		{
			&Hostinfo{FrontendLogID: "2"},
			&Hostinfo{FrontendLogID: "2"},
			true,
		},

		{
			&Hostinfo{BackendLogID: "1"},
			&Hostinfo{BackendLogID: "2"},
			false,
		},
		{
			&Hostinfo{BackendLogID: "2"},
			&Hostinfo{BackendLogID: "2"},
			true,
		},

		{
			&Hostinfo{OS: "windows"},
			&Hostinfo{OS: "linux"},
			false,
		},
		{
			&Hostinfo{OS: "windows"},
			&Hostinfo{OS: "windows"},
			true,
		},

		{
			&Hostinfo{Hostname: "vega"},
			&Hostinfo{Hostname: "iris"},
			false,
		},
		{
			&Hostinfo{Hostname: "vega"},
			&Hostinfo{Hostname: "vega"},
			true,
		},

		{
			&Hostinfo{RoutableIPs: nil},
			&Hostinfo{RoutableIPs: nets("10.0.0.0/16")},
			false,
		},
		{
			&Hostinfo{RoutableIPs: nets("10.1.0.0/16", "192.168.1.0/24")},
			&Hostinfo{RoutableIPs: nets("10.2.0.0/16", "192.168.2.0/24")},
			false,
		},
		{
			&Hostinfo{RoutableIPs: nets("10.1.0.0/16", "192.168.1.0/24")},
			&Hostinfo{RoutableIPs: nets("10.1.0.0/16", "192.168.2.0/24")},
			false,
		},
		{
			&Hostinfo{RoutableIPs: nets("10.1.0.0/16", "192.168.1.0/24")},
			&Hostinfo{RoutableIPs: nets("10.1.0.0/16", "192.168.1.0/24")},
			true,
		},

		{
			&Hostinfo{RequestTags: []string{"abc", "def"}},
			&Hostinfo{RequestTags: []string{"abc", "def"}},
			true,
		},
		{
			&Hostinfo{RequestTags: []string{"abc", "def"}},
			&Hostinfo{RequestTags: []string{"abc", "123"}},
			false,
		},
		{
			&Hostinfo{RequestTags: []string{}},
			&Hostinfo{RequestTags: []string{"abc"}},
			false,
		},

		{
			&Hostinfo{Services: []Service{{Proto: TCP, Port: 1234, Description: "foo"}}},
			&Hostinfo{Services: []Service{{Proto: UDP, Port: 2345, Description: "bar"}}},
			false,
		},
		{
			&Hostinfo{Services: []Service{{Proto: TCP, Port: 1234, Description: "foo"}}},
			&Hostinfo{Services: []Service{{Proto: TCP, Port: 1234, Description: "foo"}}},
			true,
		},
		{
			&Hostinfo{ShareeNode: true},
			&Hostinfo{},
			false,
		},
		{
			&Hostinfo{SSH_HostKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIO.... root@bar"}},
			&Hostinfo{},
			false,
		},
		{
			&Hostinfo{App: "golink"},
			&Hostinfo{App: "abc"},
			false,
		},
		{
			&Hostinfo{App: "golink"},
			&Hostinfo{App: "golink"},
			true,
		},
	}
	for i, tt := range tests {
		got := tt.a.Equal(tt.b)
		if got != tt.want {
			t.Errorf("%d. Equal = %v; want %v", i, got, tt.want)
		}
	}
}

func TestHostinfoHowEqual(t *testing.T) {
	tests := []struct {
		a, b *Hostinfo
		want []string
	}{
		{
			a:    nil,
			b:    nil,
			want: nil,
		},
		{
			a:    new(Hostinfo),
			b:    nil,
			want: []string{"nil"},
		},
		{
			a:    nil,
			b:    new(Hostinfo),
			want: []string{"nil"},
		},
		{
			a:    new(Hostinfo),
			b:    new(Hostinfo),
			want: nil,
		},
		{
			a: &Hostinfo{
				IPNVersion:  "1",
				ShieldsUp:   false,
				RoutableIPs: []netip.Prefix{netip.MustParsePrefix("1.2.3.0/24")},
			},
			b: &Hostinfo{
				IPNVersion:  "2",
				ShieldsUp:   true,
				RoutableIPs: []netip.Prefix{netip.MustParsePrefix("1.2.3.0/25")},
			},
			want: []string{"IPNVersion", "ShieldsUp", "RoutableIPs"},
		},
		{
			a: &Hostinfo{
				IPNVersion: "1",
			},
			b: &Hostinfo{
				IPNVersion: "2",
				NetInfo:    new(NetInfo),
			},
			want: []string{"IPNVersion", "NetInfo.nil"},
		},
		{
			a: &Hostinfo{
				IPNVersion: "1",
				NetInfo: &NetInfo{
					WorkingIPv6:   "true",
					HavePortMap:   true,
					LinkType:      "foo",
					PreferredDERP: 123,
					DERPLatency: map[string]float64{
						"foo": 1.0,
					},
				},
			},
			b: &Hostinfo{
				IPNVersion: "2",
				NetInfo:    &NetInfo{},
			},
			want: []string{"IPNVersion", "NetInfo.WorkingIPv6", "NetInfo.HavePortMap", "NetInfo.PreferredDERP", "NetInfo.LinkType", "NetInfo.DERPLatency"},
		},
	}
	for i, tt := range tests {
		got := tt.a.HowUnequal(tt.b)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%d. got %q; want %q", i, got, tt.want)
		}
	}
}

func TestHostinfoTailscaleSSHEnabled(t *testing.T) {
	tests := []struct {
		hi   *Hostinfo
		want bool
	}{
		{
			nil,
			false,
		},
		{
			&Hostinfo{},
			false,
		},
		{
			&Hostinfo{SSH_HostKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIO.... root@bar"}},
			true,
		},
	}

	for i, tt := range tests {
		got := tt.hi.TailscaleSSHEnabled()
		if got != tt.want {
			t.Errorf("%d. got %v; want %v", i, got, tt.want)
		}
	}
}

func TestNodeEqual(t *testing.T) {
	nodeHandles := []string{
		"ID", "StableID", "Name", "User", "Sharer",
		"Key", "KeyExpiry", "KeySignature", "Machine", "DiscoKey",
		"Addresses", "AllowedIPs", "Endpoints", "DERP", "Hostinfo",
		"Created", "Cap", "Tags", "PrimaryRoutes",
		"LastSeen", "Online", "KeepAlive", "MachineAuthorized",
		"Capabilities",
		"UnsignedPeerAPIOnly",
		"ComputedName", "computedHostIfDifferent", "ComputedNameWithHost",
		"DataPlaneAuditLogID", "Expired", "SelfNodeV4MasqAddrForThisPeer",
		"IsWireGuardOnly",
	}
	if have := fieldsOf(reflect.TypeOf(Node{})); !reflect.DeepEqual(have, nodeHandles) {
		t.Errorf("Node.Equal check might be out of sync\nfields: %q\nhandled: %q\n",
			have, nodeHandles)
	}

	n1 := key.NewNode().Public()
	m1 := key.NewMachine().Public()
	now := time.Now()

	tests := []struct {
		a, b *Node
		want bool
	}{
		{
			&Node{},
			nil,
			false,
		},
		{
			nil,
			&Node{},
			false,
		},
		{
			&Node{},
			&Node{},
			true,
		},
		{
			&Node{},
			&Node{},
			true,
		},
		{
			&Node{ID: 1},
			&Node{},
			false,
		},
		{
			&Node{ID: 1},
			&Node{ID: 1},
			true,
		},
		{
			&Node{StableID: "node-abcd"},
			&Node{},
			false,
		},
		{
			&Node{StableID: "node-abcd"},
			&Node{StableID: "node-abcd"},
			true,
		},
		{
			&Node{User: 0},
			&Node{User: 1},
			false,
		},
		{
			&Node{User: 1},
			&Node{User: 1},
			true,
		},
		{
			&Node{Key: n1},
			&Node{Key: key.NewNode().Public()},
			false,
		},
		{
			&Node{Key: n1},
			&Node{Key: n1},
			true,
		},
		{
			&Node{KeyExpiry: now},
			&Node{KeyExpiry: now.Add(60 * time.Second)},
			false,
		},
		{
			&Node{KeyExpiry: now},
			&Node{KeyExpiry: now},
			true,
		},
		{
			&Node{Machine: m1},
			&Node{Machine: key.NewMachine().Public()},
			false,
		},
		{
			&Node{Machine: m1},
			&Node{Machine: m1},
			true,
		},
		{
			&Node{Addresses: []netip.Prefix{}},
			&Node{Addresses: nil},
			false,
		},
		{
			&Node{Addresses: []netip.Prefix{}},
			&Node{Addresses: []netip.Prefix{}},
			true,
		},
		{
			&Node{AllowedIPs: []netip.Prefix{}},
			&Node{AllowedIPs: nil},
			false,
		},
		{
			&Node{Addresses: []netip.Prefix{}},
			&Node{Addresses: []netip.Prefix{}},
			true,
		},
		{
			&Node{Endpoints: []string{}},
			&Node{Endpoints: nil},
			false,
		},
		{
			&Node{Endpoints: []string{}},
			&Node{Endpoints: []string{}},
			true,
		},
		{
			&Node{Hostinfo: (&Hostinfo{Hostname: "alice"}).View()},
			&Node{Hostinfo: (&Hostinfo{Hostname: "bob"}).View()},
			false,
		},
		{
			&Node{Hostinfo: (&Hostinfo{}).View()},
			&Node{Hostinfo: (&Hostinfo{}).View()},
			true,
		},
		{
			&Node{Created: now},
			&Node{Created: now.Add(60 * time.Second)},
			false,
		},
		{
			&Node{Created: now},
			&Node{Created: now},
			true,
		},
		{
			&Node{LastSeen: &now},
			&Node{LastSeen: nil},
			false,
		},
		{
			&Node{LastSeen: &now},
			&Node{LastSeen: &now},
			true,
		},
		{
			&Node{DERP: "foo"},
			&Node{DERP: "bar"},
			false,
		},
		{
			&Node{Tags: []string{"tag:foo"}},
			&Node{Tags: []string{"tag:foo"}},
			true,
		},
		{
			&Node{Tags: []string{"tag:foo", "tag:bar"}},
			&Node{Tags: []string{"tag:bar"}},
			false,
		},
		{
			&Node{Tags: []string{"tag:foo"}},
			&Node{Tags: []string{"tag:bar"}},
			false,
		},
		{
			&Node{Tags: []string{"tag:foo"}},
			&Node{},
			false,
		},
		{
			&Node{Expired: true},
			&Node{},
			false,
		},
		{
			&Node{},
			&Node{SelfNodeV4MasqAddrForThisPeer: ptr.To(netip.MustParseAddr("100.64.0.1"))},
			false,
		},
		{
			&Node{SelfNodeV4MasqAddrForThisPeer: ptr.To(netip.MustParseAddr("100.64.0.1"))},
			&Node{SelfNodeV4MasqAddrForThisPeer: ptr.To(netip.MustParseAddr("100.64.0.1"))},
			true,
		},
	}
	for i, tt := range tests {
		got := tt.a.Equal(tt.b)
		if got != tt.want {
			t.Errorf("%d. Equal = %v; want %v", i, got, tt.want)
		}
	}
}

func TestNetInfoFields(t *testing.T) {
	handled := []string{
		"MappingVariesByDestIP",
		"HairPinning",
		"WorkingIPv6",
		"OSHasIPv6",
		"WorkingUDP",
		"WorkingICMPv4",
		"HavePortMap",
		"UPnP",
		"PMP",
		"PCP",
		"PreferredDERP",
		"LinkType",
		"DERPLatency",
	}
	if have := fieldsOf(reflect.TypeOf(NetInfo{})); !reflect.DeepEqual(have, handled) {
		t.Errorf("NetInfo.Clone/BasicallyEqually check might be out of sync\nfields: %q\nhandled: %q\n",
			have, handled)
	}
}

type keyIn interface {
	String() string
	MarshalText() ([]byte, error)
}

func testKey(t *testing.T, prefix string, in keyIn, out encoding.TextUnmarshaler) {
	got, err := in.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	if err := out.UnmarshalText(got); err != nil {
		t.Fatal(err)
	}
	if s := in.String(); string(got) != s {
		t.Errorf("MarshalText = %q != String %q", got, s)
	}
	if !strings.HasPrefix(string(got), prefix) {
		t.Errorf("%q didn't start with prefix %q", got, prefix)
	}
	if reflect.ValueOf(out).Elem().Interface() != in {
		t.Errorf("mismatch after unmarshal")
	}
}

func TestCloneUser(t *testing.T) {
	tests := []struct {
		name string
		u    *User
	}{
		{"nil_logins", &User{}},
		{"zero_logins", &User{Logins: make([]LoginID, 0)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u2 := tt.u.Clone()
			if !reflect.DeepEqual(tt.u, u2) {
				t.Errorf("not equal")
			}
		})
	}
}

func TestCloneNode(t *testing.T) {
	tests := []struct {
		name string
		v    *Node
	}{
		{"nil_fields", &Node{}},
		{"zero_fields", &Node{
			Addresses:  make([]netip.Prefix, 0),
			AllowedIPs: make([]netip.Prefix, 0),
			Endpoints:  make([]string, 0),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v2 := tt.v.Clone()
			if !reflect.DeepEqual(tt.v, v2) {
				t.Errorf("not equal")
			}
		})
	}
}

func TestUserProfileJSONMarshalForMac(t *testing.T) {
	// Old macOS clients had a bug where they required
	// UserProfile.Roles to be non-null. Lock that in
	// 1.0.x/1.2.x clients are gone in the wild.
	// See mac commit 0242c08a2ca496958027db1208f44251bff8488b (Sep 30).
	// It was fixed in at least 1.4.x, and perhaps 1.2.x.
	j, err := json.Marshal(UserProfile{})
	if err != nil {
		t.Fatal(err)
	}
	const wantSub = `"Roles":[]`
	if !strings.Contains(string(j), wantSub) {
		t.Fatalf("didn't contain %#q; got: %s", wantSub, j)
	}

	// And back:
	var up UserProfile
	if err := json.Unmarshal(j, &up); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
}

func TestEndpointTypeMarshal(t *testing.T) {
	eps := []EndpointType{
		EndpointUnknownType,
		EndpointLocal,
		EndpointSTUN,
		EndpointPortmapped,
		EndpointSTUN4LocalPort,
	}
	got, err := json.Marshal(eps)
	if err != nil {
		t.Fatal(err)
	}
	const want = `[0,1,2,3,4]`
	if string(got) != want {
		t.Errorf("got %s; want %s", got, want)
	}
}

var sinkBytes []byte

func BenchmarkKeyMarshalText(b *testing.B) {
	b.ReportAllocs()
	var k [32]byte
	for i := 0; i < b.N; i++ {
		sinkBytes = ExportKeyMarshalText("prefix", k)
	}
}

func TestAppendKeyAllocs(t *testing.T) {
	if version.IsRace() {
		t.Skip("skipping in race detector") // append(b, make([]byte, N)...) not optimized in compiler with race
	}
	var k [32]byte
	err := tstest.MinAllocsPerRun(t, 1, func() {
		sinkBytes = ExportKeyMarshalText("prefix", k)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRegisterRequestNilClone(t *testing.T) {
	var nilReq *RegisterRequest
	got := nilReq.Clone()
	if got != nil {
		t.Errorf("got = %v; want nil", got)
	}
}

// Tests that CurrentCapabilityVersion is bumped when the comment block above it gets bumped.
// We've screwed this up several times.
func TestCurrentCapabilityVersion(t *testing.T) {
	f := must.Get(os.ReadFile("tailcfg.go"))
	matches := regexp.MustCompile(`(?m)^//[\s-]+(\d+): \d\d\d\d-\d\d-\d\d: `).FindAllStringSubmatch(string(f), -1)
	max := 0
	for _, m := range matches {
		n := must.Get(strconv.Atoi(m[1]))
		if n > max {
			max = n
		}
	}
	if CapabilityVersion(max) != CurrentCapabilityVersion {
		t.Errorf("CurrentCapabilityVersion = %d; want %d", CurrentCapabilityVersion, max)
	}
}
