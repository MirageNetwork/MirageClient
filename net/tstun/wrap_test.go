// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package tstun

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unsafe"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/tailscale/wireguard-go/tun/tuntest"
	"go4.org/mem"
	"go4.org/netipx"
	"tailscale.com/disco"
	"tailscale.com/net/connstats"
	"tailscale.com/net/netaddr"
	"tailscale.com/net/packet"
	"tailscale.com/tstest"
	"tailscale.com/tstime/mono"
	"tailscale.com/types/ipproto"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/types/netlogtype"
	"tailscale.com/util/must"
	"tailscale.com/wgengine/filter"
)

func udp4(src, dst string, sport, dport uint16) []byte {
	sip, err := netip.ParseAddr(src)
	if err != nil {
		panic(err)
	}
	dip, err := netip.ParseAddr(dst)
	if err != nil {
		panic(err)
	}
	header := &packet.UDP4Header{
		IP4Header: packet.IP4Header{
			Src:  sip,
			Dst:  dip,
			IPID: 0,
		},
		SrcPort: sport,
		DstPort: dport,
	}
	return packet.Generate(header, []byte("udp_payload"))
}

func tcp4syn(src, dst string, sport, dport uint16) []byte {
	sip, err := netip.ParseAddr(src)
	if err != nil {
		panic(err)
	}
	dip, err := netip.ParseAddr(dst)
	if err != nil {
		panic(err)
	}
	ipHeader := packet.IP4Header{
		IPProto: ipproto.TCP,
		Src:     sip,
		Dst:     dip,
		IPID:    0,
	}
	tcpHeader := make([]byte, 20)
	binary.BigEndian.PutUint16(tcpHeader[0:], sport)
	binary.BigEndian.PutUint16(tcpHeader[2:], dport)
	tcpHeader[13] |= 2 // SYN

	both := packet.Generate(ipHeader, tcpHeader)

	// 20 byte IP4 + 20 byte TCP
	binary.BigEndian.PutUint16(both[2:4], 40)

	return both
}

func nets(nets ...string) (ret []netip.Prefix) {
	for _, s := range nets {
		if i := strings.IndexByte(s, '/'); i == -1 {
			ip, err := netip.ParseAddr(s)
			if err != nil {
				panic(err)
			}
			bits := uint8(32)
			if ip.Is6() {
				bits = 128
			}
			ret = append(ret, netip.PrefixFrom(ip, int(bits)))
		} else {
			pfx, err := netip.ParsePrefix(s)
			if err != nil {
				panic(err)
			}
			ret = append(ret, pfx)
		}
	}
	return ret
}

func ports(s string) filter.PortRange {
	if s == "*" {
		return filter.PortRange{First: 0, Last: 65535}
	}

	var fs, ls string
	i := strings.IndexByte(s, '-')
	if i == -1 {
		fs = s
		ls = fs
	} else {
		fs = s[:i]
		ls = s[i+1:]
	}
	first, err := strconv.ParseInt(fs, 10, 16)
	if err != nil {
		panic(fmt.Sprintf("invalid NetPortRange %q", s))
	}
	last, err := strconv.ParseInt(ls, 10, 16)
	if err != nil {
		panic(fmt.Sprintf("invalid NetPortRange %q", s))
	}
	return filter.PortRange{First: uint16(first), Last: uint16(last)}
}

func netports(netPorts ...string) (ret []filter.NetPortRange) {
	for _, s := range netPorts {
		i := strings.LastIndexByte(s, ':')
		if i == -1 {
			panic(fmt.Sprintf("invalid NetPortRange %q", s))
		}

		npr := filter.NetPortRange{
			Net:   nets(s[:i])[0],
			Ports: ports(s[i+1:]),
		}
		ret = append(ret, npr)
	}
	return ret
}

func setfilter(logf logger.Logf, tun *Wrapper) {
	protos := []ipproto.Proto{
		ipproto.TCP,
		ipproto.UDP,
	}
	matches := []filter.Match{
		{IPProto: protos, Srcs: nets("5.6.7.8"), Dsts: netports("1.2.3.4:89-90")},
		{IPProto: protos, Srcs: nets("1.2.3.4"), Dsts: netports("5.6.7.8:98")},
	}
	var sb netipx.IPSetBuilder
	sb.AddPrefix(netip.MustParsePrefix("1.2.0.0/16"))
	ipSet, _ := sb.IPSet()
	tun.SetFilter(filter.New(matches, ipSet, ipSet, nil, logf))
}

func newChannelTUN(logf logger.Logf, secure bool) (*tuntest.ChannelTUN, *Wrapper) {
	chtun := tuntest.NewChannelTUN()
	tun := Wrap(logf, chtun.TUN())
	if secure {
		setfilter(logf, tun)
	} else {
		tun.disableFilter = true
	}
	return chtun, tun
}

func newFakeTUN(logf logger.Logf, secure bool) (*fakeTUN, *Wrapper) {
	ftun := NewFake()
	tun := Wrap(logf, ftun)
	if secure {
		setfilter(logf, tun)
	} else {
		tun.disableFilter = true
	}
	return ftun.(*fakeTUN), tun
}

func TestReadAndInject(t *testing.T) {
	chtun, tun := newChannelTUN(t.Logf, false)
	defer tun.Close()

	const size = 2 // all payloads have this size
	written := []string{"w0", "w1"}
	injected := []string{"i0", "i1"}

	go func() {
		for _, packet := range written {
			payload := []byte(packet)
			chtun.Outbound <- payload
		}
	}()

	for _, packet := range injected {
		go func(packet string) {
			payload := []byte(packet)
			err := tun.InjectOutbound(payload)
			if err != nil {
				t.Errorf("%s: error: %v", packet, err)
			}
		}(packet)
	}

	var buf [MaxPacketSize]byte
	var seen = make(map[string]bool)
	sizes := make([]int, 1)
	// We expect the same packets back, in no particular order.
	for i := 0; i < len(written)+len(injected); i++ {
		packet := buf[:]
		buffs := [][]byte{packet}
		numPackets, err := tun.Read(buffs, sizes, 0)
		if err != nil {
			t.Errorf("read %d: error: %v", i, err)
		}
		if numPackets != 1 {
			t.Fatalf("read %d packets, expected %d", numPackets, 1)
		}
		packet = packet[:sizes[0]]
		packetLen := len(packet)
		if packetLen != size {
			t.Errorf("read %d: got size %d; want %d", i, packetLen, size)
		}
		got := string(packet)
		t.Logf("read %d: got %s", i, got)
		seen[got] = true
	}

	for _, packet := range written {
		if !seen[packet] {
			t.Errorf("%s not received", packet)
		}
	}
	for _, packet := range injected {
		if !seen[packet] {
			t.Errorf("%s not received", packet)
		}
	}
}

func TestWriteAndInject(t *testing.T) {
	chtun, tun := newChannelTUN(t.Logf, false)
	defer tun.Close()

	const size = 2 // all payloads have this size
	written := []string{"w0", "w1"}
	injected := []string{"i0", "i1"}

	go func() {
		for _, packet := range written {
			payload := []byte(packet)
			_, err := tun.Write([][]byte{payload}, 0)
			if err != nil {
				t.Errorf("%s: error: %v", packet, err)
			}
		}
	}()

	for _, packet := range injected {
		go func(packet string) {
			payload := []byte(packet)
			err := tun.InjectInboundCopy(payload)
			if err != nil {
				t.Errorf("%s: error: %v", packet, err)
			}
		}(packet)
	}

	seen := make(map[string]bool)
	// We expect the same packets back, in no particular order.
	for i := 0; i < len(written)+len(injected); i++ {
		packet := <-chtun.Inbound
		got := string(packet)
		t.Logf("read %d: got %s", i, got)
		seen[got] = true
	}

	for _, packet := range written {
		if !seen[packet] {
			t.Errorf("%s not received", packet)
		}
	}
	for _, packet := range injected {
		if !seen[packet] {
			t.Errorf("%s not received", packet)
		}
	}
}

// mustHexDecode is like hex.DecodeString, but panics on error
// and ignores whitespace in s.
func mustHexDecode(s string) []byte {
	return must.Get(hex.DecodeString(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)))
}

func TestFilter(t *testing.T) {
	chtun, tun := newChannelTUN(t.Logf, true)
	defer tun.Close()

	type direction int

	const (
		in direction = iota
		out
	)

	tests := []struct {
		name string
		dir  direction
		drop bool
		data []byte
	}{
		{"short_in", in, true, []byte("\x45xxx")},
		{"short_out", out, true, []byte("\x45xxx")},
		{"ip97_out", out, false, mustHexDecode("4500 0019 d186 4000 4061 751d 644a 4603 6449 e549 6865 6c6c 6f")},
		{"bad_port_in", in, true, udp4("5.6.7.8", "1.2.3.4", 22, 22)},
		{"bad_port_out", out, false, udp4("1.2.3.4", "5.6.7.8", 22, 22)},
		{"bad_ip_in", in, true, udp4("8.1.1.1", "1.2.3.4", 89, 89)},
		{"bad_ip_out", out, false, udp4("1.2.3.4", "8.1.1.1", 98, 98)},
		{"good_packet_in", in, false, udp4("5.6.7.8", "1.2.3.4", 89, 89)},
		{"good_packet_out", out, false, udp4("1.2.3.4", "5.6.7.8", 98, 98)},
	}

	// A reader on the other end of the tun.
	go func() {
		var recvbuf []byte
		for {
			select {
			case <-tun.closed:
				return
			case recvbuf = <-chtun.Inbound:
				// continue
			}
			for _, tt := range tests {
				if tt.drop && bytes.Equal(recvbuf, tt.data) {
					t.Errorf("did not drop %s", tt.name)
				}
			}
		}
	}()

	var buf [MaxPacketSize]byte
	stats := connstats.NewStatistics(0, 0, nil)
	defer stats.Shutdown(context.Background())
	tun.SetStatistics(stats)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var n int
			var err error
			var filtered bool
			sizes := make([]int, 1)

			tunStats, _ := stats.TestExtract()
			if len(tunStats) > 0 {
				t.Errorf("connstats.Statistics.Extract = %v, want {}", stats)
			}

			if tt.dir == in {
				// Use the side effect of updating the last
				// activity atomic to determine whether the
				// data was actually filtered.
				// If it stays zero, nothing made it through
				// to the wrapped TUN.
				tun.lastActivityAtomic.StoreAtomic(0)
				_, err = tun.Write([][]byte{tt.data}, 0)
				filtered = tun.lastActivityAtomic.LoadAtomic() == 0
			} else {
				chtun.Outbound <- tt.data
				n, err = tun.Read([][]byte{buf[:]}, sizes, 0)
				// In the read direction, errors are fatal, so we return n = 0 instead.
				filtered = (n == 0)
			}

			if err != nil {
				t.Errorf("got err %v; want nil", err)
			}

			if filtered {
				if !tt.drop {
					t.Errorf("got drop; want accept")
				}
			} else {
				if tt.drop {
					t.Errorf("got accept; want drop")
				}
			}

			got, _ := stats.TestExtract()
			want := map[netlogtype.Connection]netlogtype.Counts{}
			var wasUDP bool
			if !tt.drop {
				var p packet.Parsed
				p.Decode(tt.data)
				wasUDP = p.IPProto == ipproto.UDP
				switch tt.dir {
				case in:
					conn := netlogtype.Connection{Proto: ipproto.UDP, Src: p.Dst, Dst: p.Src}
					want[conn] = netlogtype.Counts{RxPackets: 1, RxBytes: uint64(len(tt.data))}
				case out:
					conn := netlogtype.Connection{Proto: ipproto.UDP, Src: p.Src, Dst: p.Dst}
					want[conn] = netlogtype.Counts{TxPackets: 1, TxBytes: uint64(len(tt.data))}
				}
			}
			if wasUDP {
				if diff := cmp.Diff(got, want, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("stats.TestExtract (-got +want):\n%s", diff)
				}
			}
		})
	}
}

func TestAllocs(t *testing.T) {
	ftun, tun := newFakeTUN(t.Logf, false)
	defer tun.Close()

	buf := [][]byte{{0x00}}
	err := tstest.MinAllocsPerRun(t, 0, func() {
		_, err := ftun.Write(buf, 0)
		if err != nil {
			t.Errorf("write: error: %v", err)
			return
		}
	})

	if err != nil {
		t.Error(err)
	}
}

func TestClose(t *testing.T) {
	ftun, tun := newFakeTUN(t.Logf, false)

	data := [][]byte{udp4("1.2.3.4", "5.6.7.8", 98, 98)}
	_, err := ftun.Write(data, 0)
	if err != nil {
		t.Error(err)
	}

	tun.Close()
	_, err = ftun.Write(data, 0)
	if err == nil {
		t.Error("Expected error from ftun.Write() after Close()")
	}
}

func BenchmarkWrite(b *testing.B) {
	b.ReportAllocs()
	ftun, tun := newFakeTUN(b.Logf, true)
	defer tun.Close()

	packet := [][]byte{udp4("5.6.7.8", "1.2.3.4", 89, 89)}
	for i := 0; i < b.N; i++ {
		_, err := ftun.Write(packet, 0)
		if err != nil {
			b.Errorf("err = %v; want nil", err)
		}
	}
}

func TestAtomic64Alignment(t *testing.T) {
	off := unsafe.Offsetof(Wrapper{}.lastActivityAtomic)
	if off%8 != 0 {
		t.Errorf("offset %v not 8-byte aligned", off)
	}

	c := new(Wrapper)
	c.lastActivityAtomic.StoreAtomic(mono.Now())
}

func TestPeerAPIBypass(t *testing.T) {
	wrapperWithPeerAPI := &Wrapper{
		PeerAPIPort: func(ip netip.Addr) (port uint16, ok bool) {
			if ip == netip.MustParseAddr("100.64.1.2") {
				return 60000, true
			}
			return
		},
	}

	tests := []struct {
		name   string
		w      *Wrapper
		filter *filter.Filter
		pkt    []byte
		want   filter.Response
	}{
		{
			name: "reject_nil_filter",
			w: &Wrapper{
				PeerAPIPort: func(netip.Addr) (port uint16, ok bool) {
					return 60000, true
				},
			},
			pkt:  tcp4syn("1.2.3.4", "100.64.1.2", 1234, 60000),
			want: filter.Drop,
		},
		{
			name:   "reject_with_filter",
			w:      &Wrapper{},
			filter: filter.NewAllowNone(logger.Discard, new(netipx.IPSet)),
			pkt:    tcp4syn("1.2.3.4", "100.64.1.2", 1234, 60000),
			want:   filter.Drop,
		},
		{
			name:   "peerapi_bypass_filter",
			w:      wrapperWithPeerAPI,
			filter: filter.NewAllowNone(logger.Discard, new(netipx.IPSet)),
			pkt:    tcp4syn("1.2.3.4", "100.64.1.2", 1234, 60000),
			want:   filter.Accept,
		},
		{
			name:   "peerapi_dont_bypass_filter_wrong_port",
			w:      wrapperWithPeerAPI,
			filter: filter.NewAllowNone(logger.Discard, new(netipx.IPSet)),
			pkt:    tcp4syn("1.2.3.4", "100.64.1.2", 1234, 60001),
			want:   filter.Drop,
		},
		{
			name:   "peerapi_dont_bypass_filter_wrong_dst_ip",
			w:      wrapperWithPeerAPI,
			filter: filter.NewAllowNone(logger.Discard, new(netipx.IPSet)),
			pkt:    tcp4syn("1.2.3.4", "100.64.1.3", 1234, 60000),
			want:   filter.Drop,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := new(packet.Parsed)
			p.Decode(tt.pkt)
			tt.w.SetFilter(tt.filter)
			tt.w.disableTSMPRejected = true
			tt.w.logf = t.Logf
			if got := tt.w.filterIn(p); got != tt.want {
				t.Errorf("got = %v; want %v", got, tt.want)
			}
		})
	}
}

// Issue 1526: drop disco frames from ourselves.
func TestFilterDiscoLoop(t *testing.T) {
	var memLog tstest.MemLogger
	discoPub := key.DiscoPublicFromRaw32(mem.B([]byte{1: 1, 2: 2, 31: 0}))
	tw := &Wrapper{logf: memLog.Logf, limitedLogf: memLog.Logf}
	tw.SetDiscoKey(discoPub)
	uh := packet.UDP4Header{
		IP4Header: packet.IP4Header{
			IPProto: ipproto.UDP,
			Src:     netaddr.IPv4(1, 2, 3, 4),
			Dst:     netaddr.IPv4(5, 6, 7, 8),
		},
		SrcPort: 9,
		DstPort: 10,
	}
	discobs := discoPub.Raw32()
	discoPayload := fmt.Sprintf("%s%s%s", disco.Magic, discobs[:], [disco.NonceLen]byte{})
	pkt := make([]byte, uh.Len()+len(discoPayload))
	uh.Marshal(pkt)
	copy(pkt[uh.Len():], discoPayload)

	p := new(packet.Parsed)
	p.Decode(pkt)
	got := tw.filterIn(p)
	if got != filter.DropSilently {
		t.Errorf("got %v; want DropSilently", got)
	}
	if got, want := memLog.String(), "[unexpected] received self disco in packet over tstun; dropping\n"; got != want {
		t.Errorf("log output mismatch\n got: %q\nwant: %q\n", got, want)
	}

	memLog.Reset()
	pp := new(packet.Parsed)
	pp.Decode(pkt)
	got = tw.filterOut(pp)
	if got != filter.DropSilently {
		t.Errorf("got %v; want DropSilently", got)
	}
	if got, want := memLog.String(), "[unexpected] received self disco out packet over tstun; dropping\n"; got != want {
		t.Errorf("log output mismatch\n got: %q\nwant: %q\n", got, want)
	}
}
