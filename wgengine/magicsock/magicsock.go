// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package magicsock implements a socket that can change its communication path while
// in use, actively searching for the best way to communicate.
package magicsock

import (
	"bufio"
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"math/rand"
	"net"
	"net/netip"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tailscale/wireguard-go/conn"
	"go4.org/mem"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"tailscale.com/control/controlclient"
	"tailscale.com/derp"
	"tailscale.com/derp/derphttp"
	"tailscale.com/disco"
	"tailscale.com/envknob"
	"tailscale.com/health"
	"tailscale.com/hostinfo"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/logtail/backoff"
	"tailscale.com/net/connstats"
	"tailscale.com/net/dnscache"
	"tailscale.com/net/interfaces"
	"tailscale.com/net/netaddr"
	"tailscale.com/net/netcheck"
	"tailscale.com/net/neterror"
	"tailscale.com/net/netmon"
	"tailscale.com/net/netns"
	"tailscale.com/net/packet"
	"tailscale.com/net/ping"
	"tailscale.com/net/portmapper"
	"tailscale.com/net/sockstats"
	"tailscale.com/net/stun"
	"tailscale.com/net/tsaddr"
	"tailscale.com/syncs"
	"tailscale.com/tailcfg"
	"tailscale.com/tstime"
	"tailscale.com/tstime/mono"
	"tailscale.com/types/key"
	"tailscale.com/types/lazy"
	"tailscale.com/types/logger"
	"tailscale.com/types/netmap"
	"tailscale.com/types/nettype"
	"tailscale.com/util/clientmetric"
	"tailscale.com/util/mak"
	"tailscale.com/util/ringbuffer"
	"tailscale.com/util/set"
	"tailscale.com/util/sysresources"
	"tailscale.com/util/uniq"
	"tailscale.com/version"
	"tailscale.com/wgengine/capture"
)

const (
	// These are disco.Magic in big-endian form, 4 then 2 bytes. The
	// BPF filters need the magic in this format to match on it. Used
	// only in magicsock_linux.go, but defined here so that the test
	// which verifies this is the correct magic doesn't also need a
	// _linux variant.
	discoMagic1 = 0x5453f09f
	discoMagic2 = 0x92ac

	// UDP socket read/write buffer size (7MB). The value of 7MB is chosen as it
	// is the max supported by a default configuration of macOS. Some platforms
	// will silently clamp the value.
	socketBufferSize = 7 << 20
)

// useDerpRoute reports whether magicsock should enable the DERP
// return path optimization (Issue 150).
func useDerpRoute() bool {
	if b, ok := debugUseDerpRoute().Get(); ok {
		return b
	}
	ob := controlclient.DERPRouteFlag()
	if v, ok := ob.Get(); ok {
		return v
	}
	return true // as of 1.21.x
}

// peerInfo is all the information magicsock tracks about a particular
// peer.
type peerInfo struct {
	ep *endpoint // always non-nil.
	// ipPorts is an inverted version of peerMap.byIPPort (below), so
	// that when we're deleting this node, we can rapidly find out the
	// keys that need deleting from peerMap.byIPPort without having to
	// iterate over every IPPort known for any peer.
	ipPorts map[netip.AddrPort]bool
}

func newPeerInfo(ep *endpoint) *peerInfo {
	return &peerInfo{
		ep:      ep,
		ipPorts: map[netip.AddrPort]bool{},
	}
}

// peerMap is an index of peerInfos by node (WireGuard) key, disco
// key, and discovered ip:port endpoints.
//
// Doesn't do any locking, all access must be done with Conn.mu held.
type peerMap struct {
	byNodeKey map[key.NodePublic]*peerInfo
	byIPPort  map[netip.AddrPort]*peerInfo

	// nodesOfDisco contains the set of nodes that are using a
	// DiscoKey. Usually those sets will be just one node.
	nodesOfDisco map[key.DiscoPublic]map[key.NodePublic]bool
}

func newPeerMap() peerMap {
	return peerMap{
		byNodeKey:    map[key.NodePublic]*peerInfo{},
		byIPPort:     map[netip.AddrPort]*peerInfo{},
		nodesOfDisco: map[key.DiscoPublic]map[key.NodePublic]bool{},
	}
}

// nodeCount returns the number of nodes currently in m.
func (m *peerMap) nodeCount() int {
	return len(m.byNodeKey)
}

// anyEndpointForDiscoKey reports whether there exists any
// peers in the netmap with dk as their DiscoKey.
func (m *peerMap) anyEndpointForDiscoKey(dk key.DiscoPublic) bool {
	return len(m.nodesOfDisco[dk]) > 0
}

// endpointForNodeKey returns the endpoint for nk, or nil if
// nk is not known to us.
func (m *peerMap) endpointForNodeKey(nk key.NodePublic) (ep *endpoint, ok bool) {
	if nk.IsZero() {
		return nil, false
	}
	if info, ok := m.byNodeKey[nk]; ok {
		return info.ep, true
	}
	return nil, false
}

// endpointForIPPort returns the endpoint for the peer we
// believe to be at ipp, or nil if we don't know of any such peer.
func (m *peerMap) endpointForIPPort(ipp netip.AddrPort) (ep *endpoint, ok bool) {
	if info, ok := m.byIPPort[ipp]; ok {
		return info.ep, true
	}
	return nil, false
}

// forEachEndpoint invokes f on every endpoint in m.
func (m *peerMap) forEachEndpoint(f func(ep *endpoint)) {
	for _, pi := range m.byNodeKey {
		f(pi.ep)
	}
}

// forEachEndpointWithDiscoKey invokes f on every endpoint in m that has the
// provided DiscoKey until f returns false or there are no endpoints left to
// iterate.
func (m *peerMap) forEachEndpointWithDiscoKey(dk key.DiscoPublic, f func(*endpoint) (keepGoing bool)) {
	for nk := range m.nodesOfDisco[dk] {
		pi, ok := m.byNodeKey[nk]
		if !ok {
			// Unexpected. Data structures would have to
			// be out of sync.  But we don't have a logger
			// here to log [unexpected], so just skip.
			// Maybe log later once peerMap is merged back
			// into Conn.
			continue
		}
		if !f(pi.ep) {
			return
		}
	}
}

// upsertEndpoint stores endpoint in the peerInfo for
// ep.publicKey, and updates indexes. m must already have a
// tailcfg.Node for ep.publicKey.
func (m *peerMap) upsertEndpoint(ep *endpoint, oldDiscoKey key.DiscoPublic) {
	if m.byNodeKey[ep.publicKey] == nil {
		m.byNodeKey[ep.publicKey] = newPeerInfo(ep)
	}
	epDisco := ep.disco.Load()
	if epDisco == nil || oldDiscoKey != epDisco.key {
		delete(m.nodesOfDisco[oldDiscoKey], ep.publicKey)
	}
	if ep.isWireguardOnly {
		// If the peer is a WireGuard only peer, add all of its endpoints.

		// TODO(raggi,catzkorn): this could mean that if a "isWireguardOnly"
		// peer has, say, 192.168.0.2 and so does a tailscale peer, the
		// wireguard one will win. That may not be the outcome that we want -
		// perhaps we should prefer bestAddr.AddrPort if it is set?
		// see tailscale/tailscale#7994
		for ipp := range ep.endpointState {
			m.setNodeKeyForIPPort(ipp, ep.publicKey)
		}

		return
	}
	set := m.nodesOfDisco[epDisco.key]
	if set == nil {
		set = map[key.NodePublic]bool{}
		m.nodesOfDisco[epDisco.key] = set
	}
	set[ep.publicKey] = true
}

// setNodeKeyForIPPort makes future peer lookups by ipp return the
// same endpoint as a lookup by nk.
//
// This should only be called with a fully verified mapping of ipp to
// nk, because calling this function defines the endpoint we hand to
// WireGuard for packets received from ipp.
func (m *peerMap) setNodeKeyForIPPort(ipp netip.AddrPort, nk key.NodePublic) {
	if pi := m.byIPPort[ipp]; pi != nil {
		delete(pi.ipPorts, ipp)
		delete(m.byIPPort, ipp)
	}
	if pi, ok := m.byNodeKey[nk]; ok {
		pi.ipPorts[ipp] = true
		m.byIPPort[ipp] = pi
	}
}

// deleteEndpoint deletes the peerInfo associated with ep, and
// updates indexes.
func (m *peerMap) deleteEndpoint(ep *endpoint) {
	if ep == nil {
		return
	}
	ep.stopAndReset()

	epDisco := ep.disco.Load()

	pi := m.byNodeKey[ep.publicKey]
	if epDisco != nil {
		delete(m.nodesOfDisco[epDisco.key], ep.publicKey)
	}
	delete(m.byNodeKey, ep.publicKey)
	if pi == nil {
		// Kneejerk paranoia from earlier issue 2801.
		// Unexpected. But no logger plumbed here to log so.
		return
	}
	for ip := range pi.ipPorts {
		delete(m.byIPPort, ip)
	}
}

// A Conn routes UDP packets and actively manages a list of its endpoints.
type Conn struct {
	// This block mirrors the contents and field order of the Options
	// struct. Initialized once at construction, then constant.

	logf                   logger.Logf
	epFunc                 func([]tailcfg.Endpoint)
	derpActiveFunc         func()
	idleFunc               func() time.Duration // nil means unknown
	testOnlyPacketListener nettype.PacketListener
	noteRecvActivity       func(key.NodePublic) // or nil, see Options.NoteRecvActivity
	netMon                 *netmon.Monitor      // or nil

	// ================================================================
	// No locking required to access these fields, either because
	// they're static after construction, or are wholly owned by a
	// single goroutine.

	connCtx       context.Context // closed on Conn.Close
	connCtxCancel func()          // closes connCtx
	donec         <-chan struct{} // connCtx.Done()'s to avoid context.cancelCtx.Done()'s mutex per call

	// pconn4 and pconn6 are the underlying UDP sockets used to
	// send/receive packets for wireguard and other magicsock
	// protocols.
	pconn4 RebindingUDPConn
	pconn6 RebindingUDPConn

	receiveBatchPool sync.Pool

	// closeDisco4 and closeDisco6 are io.Closers to shut down the raw
	// disco packet receivers. If nil, no raw disco receiver is
	// running for the given family.
	closeDisco4 io.Closer
	closeDisco6 io.Closer

	// netChecker is the prober that discovers local network
	// conditions, including the closest DERP relay and NAT mappings.
	netChecker *netcheck.Client

	// portMapper is the NAT-PMP/PCP/UPnP prober/client, for requesting
	// port mappings from NAT devices.
	portMapper *portmapper.Client

	// stunReceiveFunc holds the current STUN packet processing func.
	// Its Loaded value is always non-nil.
	stunReceiveFunc syncs.AtomicValue[func(p []byte, fromAddr netip.AddrPort)]

	// derpRecvCh is used by receiveDERP to read DERP messages.
	// It must have buffer size > 0; see issue 3736.
	derpRecvCh chan derpReadResult

	// bind is the wireguard-go conn.Bind for Conn.
	bind *connBind

	// ============================================================
	// Fields that must be accessed via atomic load/stores.

	// noV4 and noV6 are whether IPv4 and IPv6 are known to be
	// missing.  They're only used to suppress log spam. The name
	// is named negatively because in early start-up, we don't yet
	// necessarily have a netcheck.Report and don't want to skip
	// logging.
	noV4, noV6 atomic.Bool

	// noV4Send is whether IPv4 UDP is known to be unable to transmit
	// at all. This could happen if the socket is in an invalid state
	// (as can happen on darwin after a network link status change).
	noV4Send atomic.Bool

	// networkUp is whether the network is up (some interface is up
	// with IPv4 or IPv6). It's used to suppress log spam and prevent
	// new connection that'll fail.
	networkUp atomic.Bool

	// Whether debugging logging is enabled.
	debugLogging atomic.Bool

	// havePrivateKey is whether privateKey is non-zero.
	havePrivateKey  atomic.Bool
	publicKeyAtomic syncs.AtomicValue[key.NodePublic] // or NodeKey zero value if !havePrivateKey

	// derpMapAtomic is the same as derpMap, but without requiring
	// sync.Mutex. For use with NewRegionClient's callback, to avoid
	// lock ordering deadlocks. See issue 3726 and mu field docs.
	derpMapAtomic atomic.Pointer[tailcfg.DERPMap]

	lastNetCheckReport atomic.Pointer[netcheck.Report]

	// port is the preferred port from opts.Port; 0 means auto.
	port atomic.Uint32

	// stats maintains per-connection counters.
	stats atomic.Pointer[connstats.Statistics]

	// captureHook, if non-nil, is the pcap logging callback when capturing.
	captureHook syncs.AtomicValue[capture.Callback]

	// discoPrivate is the private naclbox key used for active
	// discovery traffic. It is always present, and immutable.
	discoPrivate key.DiscoPrivate
	// public of discoPrivate. It is always present and immutable.
	discoPublic key.DiscoPublic
	// ShortString of discoPublic (to save logging work later). It is always
	// present and immutable.
	discoShort string

	// ============================================================
	// mu guards all following fields; see userspaceEngine lock
	// ordering rules against the engine. For derphttp, mu must
	// be held before derphttp.Client.mu.
	mu     sync.Mutex
	muCond *sync.Cond

	closed  bool        // Close was called
	closing atomic.Bool // Close is in progress (or done)

	// derpCleanupTimer is the timer that fires to occasionally clean
	// up idle DERP connections. It's only used when there is a non-home
	// DERP connection in use.
	derpCleanupTimer *time.Timer

	// derpCleanupTimerArmed is whether derpCleanupTimer is
	// scheduled to fire within derpCleanStaleInterval.
	derpCleanupTimerArmed bool

	// periodicReSTUNTimer, when non-nil, is an AfterFunc timer
	// that will call Conn.doPeriodicSTUN.
	periodicReSTUNTimer *time.Timer

	// endpointsUpdateActive indicates that updateEndpoints is
	// currently running. It's used to deduplicate concurrent endpoint
	// update requests.
	endpointsUpdateActive bool
	// wantEndpointsUpdate, if non-empty, means that a new endpoints
	// update should begin immediately after the currently-running one
	// completes. It can only be non-empty if
	// endpointsUpdateActive==true.
	wantEndpointsUpdate string // true if non-empty; string is reason
	// lastEndpoints records the endpoints found during the previous
	// endpoint discovery. It's used to avoid duplicate endpoint
	// change notifications.
	lastEndpoints []tailcfg.Endpoint

	// lastEndpointsTime is the last time the endpoints were updated,
	// even if there was no change.
	lastEndpointsTime time.Time

	// onEndpointRefreshed are funcs to run (in their own goroutines)
	// when endpoints are refreshed.
	onEndpointRefreshed map[*endpoint]func()

	// endpointTracker tracks the set of cached endpoints that we advertise
	// for a period of time before withdrawing them.
	endpointTracker endpointTracker

	// peerSet is the set of peers that are currently configured in
	// WireGuard. These are not used to filter inbound or outbound
	// traffic at all, but only to track what state can be cleaned up
	// in other maps below that are keyed by peer public key.
	peerSet map[key.NodePublic]struct{}

	// nodeOfDisco tracks the networkmap Node entity for each peer
	// discovery key.
	peerMap peerMap

	// discoInfo is the state for an active DiscoKey.
	discoInfo map[key.DiscoPublic]*discoInfo

	// netInfoFunc is a callback that provides a tailcfg.NetInfo when
	// discovered network conditions change.
	//
	// TODO(danderson): why can't it be set at construction time?
	// There seem to be a few natural places in ipn/local.go to
	// swallow untimely invocations.
	netInfoFunc func(*tailcfg.NetInfo) // nil until set
	// netInfoLast is the NetInfo provided in the last call to
	// netInfoFunc. It's used to deduplicate calls to netInfoFunc.
	//
	// TODO(danderson): should all the deduping happen in
	// ipn/local.go? We seem to be doing dedupe at several layers, and
	// magicsock could do with any complexity reduction it can get.
	netInfoLast *tailcfg.NetInfo

	derpMap     *tailcfg.DERPMap // nil (or zero regions/nodes) means DERP is disabled
	netMap      *netmap.NetworkMap
	privateKey  key.NodePrivate    // WireGuard private key for this node
	everHadKey  bool               // whether we ever had a non-zero private key
	myDerp      int                // nearest DERP region ID; 0 means none/unknown
	derpStarted chan struct{}      // closed on first connection to DERP; for tests & cleaner Close
	activeDerp  map[int]activeDerp // DERP regionID -> connection to a node in that region
	prevDerp    map[int]*syncs.WaitGroupChan

	// derpRoute contains optional alternate routes to use as an
	// optimization instead of contacting a peer via their home
	// DERP connection.  If they sent us a message on a different
	// DERP connection (which should really only be on our DERP
	// home connection, or what was once our home), then we
	// remember that route here to optimistically use instead of
	// creating a new DERP connection back to their home.
	derpRoute map[key.NodePublic]derpRoute

	// peerLastDerp tracks which DERP node we last used to speak with a
	// peer. It's only used to quiet logging, so we only log on change.
	peerLastDerp map[key.NodePublic]int

	// wgPinger is the WireGuard only pinger used for latency measurements.
	wgPinger lazy.SyncValue[*ping.Pinger]
}

// SetDebugLoggingEnabled controls whether spammy debug logging is enabled.
//
// Note that this is currently independent from the log levels, even though
// they're pretty correlated: debugging logs should be [v1] (or higher), but
// some non-debug logs may also still have a [vN] annotation. The [vN] level
// controls which gets shown in stderr. The dlogf method, on the other hand,
// controls which gets even printed or uploaded at any level.
func (c *Conn) SetDebugLoggingEnabled(v bool) {
	c.debugLogging.Store(v)
}

// dlogf logs a debug message if debug logging is enabled via SetDebugLoggingEnabled.
func (c *Conn) dlogf(format string, a ...any) {
	if c.debugLogging.Load() {
		c.logf(format, a...)
	}
}

// derpRoute is a route entry for a public key, saying that a certain
// peer should be available at DERP node derpID, as long as the
// current connection for that derpID is dc. (but dc should not be
// used to write directly; it's owned by the read/write loops)
type derpRoute struct {
	derpID int
	dc     *derphttp.Client // don't use directly; see comment above
}

// removeDerpPeerRoute removes a DERP route entry previously added by addDerpPeerRoute.
func (c *Conn) removeDerpPeerRoute(peer key.NodePublic, derpID int, dc *derphttp.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r2 := derpRoute{derpID, dc}
	if r, ok := c.derpRoute[peer]; ok && r == r2 {
		delete(c.derpRoute, peer)
	}
}

// addDerpPeerRoute adds a DERP route entry, noting that peer was seen
// on DERP node derpID, at least on the connection identified by dc.
// See issue 150 for details.
func (c *Conn) addDerpPeerRoute(peer key.NodePublic, derpID int, dc *derphttp.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	mak.Set(&c.derpRoute, peer, derpRoute{derpID, dc})
}

var derpMagicIPAddr = netip.MustParseAddr(tailcfg.DerpMagicIP)

// activeDerp contains fields for an active DERP connection.
type activeDerp struct {
	c       *derphttp.Client
	cancel  context.CancelFunc
	writeCh chan<- derpWriteRequest
	// lastWrite is the time of the last request for its write
	// channel (currently even if there was no write).
	// It is always non-nil and initialized to a non-zero Time.
	lastWrite  *time.Time
	createTime time.Time
}

// Options contains options for Listen.
type Options struct {
	// Logf optionally provides a log function to use.
	// Must not be nil.
	Logf logger.Logf

	// Port is the port to listen on.
	// Zero means to pick one automatically.
	Port uint16

	// EndpointsFunc optionally provides a func to be called when
	// endpoints change. The called func does not own the slice.
	EndpointsFunc func([]tailcfg.Endpoint)

	// DERPActiveFunc optionally provides a func to be called when
	// a connection is made to a DERP server.
	DERPActiveFunc func()

	// IdleFunc optionally provides a func to return how long
	// it's been since a TUN packet was sent or received.
	IdleFunc func() time.Duration

	// TestOnlyPacketListener optionally specifies how to create PacketConns.
	// Only used by tests.
	TestOnlyPacketListener nettype.PacketListener

	// NoteRecvActivity, if provided, is a func for magicsock to call
	// whenever it receives a packet from a a peer if it's been more
	// than ~10 seconds since the last one. (10 seconds is somewhat
	// arbitrary; the sole user just doesn't need or want it called on
	// every packet, just every minute or two for WireGuard timeouts,
	// and 10 seconds seems like a good trade-off between often enough
	// and not too often.)
	// The provided func is likely to call back into
	// Conn.ParseEndpoint, which acquires Conn.mu. As such, you should
	// not hold Conn.mu while calling it.
	NoteRecvActivity func(key.NodePublic)

	// NetMon is the network monitor to use.
	// With one, the portmapper won't be used.
	NetMon *netmon.Monitor
}

func (o *Options) logf() logger.Logf {
	if o.Logf == nil {
		panic("must provide magicsock.Options.logf")
	}
	return o.Logf
}

func (o *Options) endpointsFunc() func([]tailcfg.Endpoint) {
	if o == nil || o.EndpointsFunc == nil {
		return func([]tailcfg.Endpoint) {}
	}
	return o.EndpointsFunc
}

func (o *Options) derpActiveFunc() func() {
	if o == nil || o.DERPActiveFunc == nil {
		return func() {}
	}
	return o.DERPActiveFunc
}

// newConn is the error-free, network-listening-side-effect-free based
// of NewConn. Mostly for tests.
func newConn() *Conn {
	discoPrivate := key.NewDisco()
	c := &Conn{
		derpRecvCh:   make(chan derpReadResult, 1), // must be buffered, see issue 3736
		derpStarted:  make(chan struct{}),
		peerLastDerp: make(map[key.NodePublic]int),
		peerMap:      newPeerMap(),
		discoInfo:    make(map[key.DiscoPublic]*discoInfo),
		discoPrivate: discoPrivate,
		discoPublic:  discoPrivate.Public(),
	}
	c.discoShort = c.discoPublic.ShortString()
	c.bind = &connBind{Conn: c, closed: true}
	c.receiveBatchPool = sync.Pool{New: func() any {
		msgs := make([]ipv6.Message, c.bind.BatchSize())
		for i := range msgs {
			msgs[i].Buffers = make([][]byte, 1)
			msgs[i].OOB = make([]byte, controlMessageSize)
		}
		batch := &receiveBatch{
			msgs: msgs,
		}
		return batch
	}}
	c.muCond = sync.NewCond(&c.mu)
	c.networkUp.Store(true) // assume up until told otherwise
	return c
}

// NewConn creates a magic Conn listening on opts.Port.
// As the set of possible endpoints for a Conn changes, the
// callback opts.EndpointsFunc is called.
func NewConn(opts Options) (*Conn, error) {
	c := newConn()
	c.port.Store(uint32(opts.Port))
	c.logf = opts.logf()
	c.epFunc = opts.endpointsFunc()
	c.derpActiveFunc = opts.derpActiveFunc()
	c.idleFunc = opts.IdleFunc
	c.testOnlyPacketListener = opts.TestOnlyPacketListener
	c.noteRecvActivity = opts.NoteRecvActivity
	c.portMapper = portmapper.NewClient(logger.WithPrefix(c.logf, "portmapper: "), opts.NetMon, nil, c.onPortMapChanged)
	if opts.NetMon != nil {
		c.portMapper.SetGatewayLookupFunc(opts.NetMon.GatewayAndSelfIP)
	}
	c.netMon = opts.NetMon

	if err := c.rebind(keepCurrentPort); err != nil {
		return nil, err
	}

	c.connCtx, c.connCtxCancel = context.WithCancel(context.Background())
	c.donec = c.connCtx.Done()
	c.netChecker = &netcheck.Client{
		Logf:                logger.WithPrefix(c.logf, "netcheck: "),
		NetMon:              c.netMon,
		GetSTUNConn4:        func() netcheck.STUNConn { return &c.pconn4 },
		GetSTUNConn6:        func() netcheck.STUNConn { return &c.pconn6 },
		SkipExternalNetwork: inTest(),
		PortMapper:          c.portMapper,
		UseDNSCache:         true,
	}

	c.ignoreSTUNPackets()

	if d4, err := c.listenRawDisco("ip4"); err == nil {
		c.logf("[v1] using BPF disco receiver for IPv4")
		c.closeDisco4 = d4
	} else {
		c.logf("[v1] couldn't create raw v4 disco listener, using regular listener instead: %v", err)
	}
	if d6, err := c.listenRawDisco("ip6"); err == nil {
		c.logf("[v1] using BPF disco receiver for IPv6")
		c.closeDisco6 = d6
	} else {
		c.logf("[v1] couldn't create raw v6 disco listener, using regular listener instead: %v", err)
	}

	c.logf("magicsock: disco key = %v", c.discoShort)
	return c, nil
}

// InstallCaptureHook installs a callback which is called to
// log debug information into the pcap stream. This function
// can be called with a nil argument to uninstall the capture
// hook.
func (c *Conn) InstallCaptureHook(cb capture.Callback) {
	c.captureHook.Store(cb)
}

// ignoreSTUNPackets sets a STUN packet processing func that does nothing.
func (c *Conn) ignoreSTUNPackets() {
	c.stunReceiveFunc.Store(func([]byte, netip.AddrPort) {})
}

// doPeriodicSTUN is called (in a new goroutine) by
// periodicReSTUNTimer when periodic STUNs are active.
func (c *Conn) doPeriodicSTUN() { c.ReSTUN("periodic") }

func (c *Conn) stopPeriodicReSTUNTimerLocked() {
	if t := c.periodicReSTUNTimer; t != nil {
		t.Stop()
		c.periodicReSTUNTimer = nil
	}
}

// c.mu must NOT be held.
func (c *Conn) updateEndpoints(why string) {
	metricUpdateEndpoints.Add(1)
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		why := c.wantEndpointsUpdate
		c.wantEndpointsUpdate = ""
		if !c.closed {
			if why != "" {
				go c.updateEndpoints(why)
				return
			}
			if c.shouldDoPeriodicReSTUNLocked() {
				// Pick a random duration between 20
				// and 26 seconds (just under 30s, a
				// common UDP NAT timeout on Linux,
				// etc)
				d := tstime.RandomDurationBetween(20*time.Second, 26*time.Second)
				if t := c.periodicReSTUNTimer; t != nil {
					if debugReSTUNStopOnIdle() {
						c.logf("resetting existing periodicSTUN to run in %v", d)
					}
					t.Reset(d)
				} else {
					if debugReSTUNStopOnIdle() {
						c.logf("scheduling periodicSTUN to run in %v", d)
					}
					c.periodicReSTUNTimer = time.AfterFunc(d, c.doPeriodicSTUN)
				}
			} else {
				if debugReSTUNStopOnIdle() {
					c.logf("periodic STUN idle")
				}
				c.stopPeriodicReSTUNTimerLocked()
			}
		}
		c.endpointsUpdateActive = false
		c.muCond.Broadcast()
	}()
	c.dlogf("[v1] magicsock: starting endpoint update (%s)", why)
	if c.noV4Send.Load() && runtime.GOOS != "js" {
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		if !closed {
			c.logf("magicsock: last netcheck reported send error. Rebinding.")
			c.Rebind()
		}
	}

	endpoints, err := c.determineEndpoints(c.connCtx)
	if err != nil {
		c.logf("magicsock: endpoint update (%s) failed: %v", why, err)
		// TODO(crawshaw): are there any conditions under which
		// we should trigger a retry based on the error here?
		return
	}

	if c.setEndpoints(endpoints) {
		c.logEndpointChange(endpoints)
		c.epFunc(endpoints)
	}
}

// setEndpoints records the new endpoints, reporting whether they're changed.
// It takes ownership of the slice.
func (c *Conn) setEndpoints(endpoints []tailcfg.Endpoint) (changed bool) {
	anySTUN := false
	for _, ep := range endpoints {
		if ep.Type == tailcfg.EndpointSTUN {
			anySTUN = true
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if !anySTUN && c.derpMap == nil && !inTest() {
		// Don't bother storing or reporting this yet. We
		// don't have a DERP map or any STUN entries, so we're
		// just starting up. A DERP map should arrive shortly
		// and then we'll have more interesting endpoints to
		// report. This saves a map update.
		// TODO(bradfitz): this optimization is currently
		// skipped during the e2e tests because they depend
		// too much on the exact sequence of updates.  Fix the
		// tests. But a protocol rewrite might happen first.
		c.dlogf("[v1] magicsock: ignoring pre-DERP map, STUN-less endpoint update: %v", endpoints)
		return false
	}

	c.lastEndpointsTime = time.Now()
	for de, fn := range c.onEndpointRefreshed {
		go fn()
		delete(c.onEndpointRefreshed, de)
	}

	if endpointSetsEqual(endpoints, c.lastEndpoints) {
		return false
	}
	c.lastEndpoints = endpoints
	return true
}

// setNetInfoHavePortMap updates NetInfo.HavePortMap to true.
func (c *Conn) setNetInfoHavePortMap() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.netInfoLast == nil {
		// No NetInfo yet. Nothing to update.
		return
	}
	if c.netInfoLast.HavePortMap {
		// No change.
		return
	}
	ni := c.netInfoLast.Clone()
	ni.HavePortMap = true
	c.callNetInfoCallbackLocked(ni)
}

func (c *Conn) updateNetInfo(ctx context.Context) (*netcheck.Report, error) {
	c.mu.Lock()
	dm := c.derpMap
	c.mu.Unlock()

	if dm == nil || c.networkDown() {
		return new(netcheck.Report), nil
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	c.stunReceiveFunc.Store(c.netChecker.ReceiveSTUNPacket)
	defer c.ignoreSTUNPackets()

	report, err := c.netChecker.GetReport(ctx, dm)
	if err != nil {
		return nil, err
	}

	c.lastNetCheckReport.Store(report)
	c.noV4.Store(!report.IPv4)
	c.noV6.Store(!report.IPv6)
	c.noV4Send.Store(!report.IPv4CanSend)

	ni := &tailcfg.NetInfo{
		DERPLatency:           map[string]float64{},
		MappingVariesByDestIP: report.MappingVariesByDestIP,
		HairPinning:           report.HairPinning,
		UPnP:                  report.UPnP,
		PMP:                   report.PMP,
		PCP:                   report.PCP,
		HavePortMap:           c.portMapper.HaveMapping(),
	}
	for rid, d := range report.RegionV4Latency {
		ni.DERPLatency[fmt.Sprintf("%d-v4", rid)] = d.Seconds()
	}
	for rid, d := range report.RegionV6Latency {
		ni.DERPLatency[fmt.Sprintf("%d-v6", rid)] = d.Seconds()
	}
	ni.WorkingIPv6.Set(report.IPv6)
	ni.OSHasIPv6.Set(report.OSHasIPv6)
	ni.WorkingUDP.Set(report.UDP)
	ni.WorkingICMPv4.Set(report.ICMPv4)
	ni.PreferredDERP = report.PreferredDERP

	if ni.PreferredDERP == 0 {
		// Perhaps UDP is blocked. Pick a deterministic but arbitrary
		// one.
		ni.PreferredDERP = c.pickDERPFallback()
	}
	if !c.setNearestDERP(ni.PreferredDERP) {
		ni.PreferredDERP = 0
	}

	// TODO: set link type

	c.callNetInfoCallback(ni)
	return report, nil
}

var processStartUnixNano = time.Now().UnixNano()

// pickDERPFallback returns a non-zero but deterministic DERP node to
// connect to.  This is only used if netcheck couldn't find the
// nearest one (for instance, if UDP is blocked and thus STUN latency
// checks aren't working).
//
// c.mu must NOT be held.
func (c *Conn) pickDERPFallback() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.wantDerpLocked() {
		return 0
	}
	ids := c.derpMap.RegionIDs()
	if len(ids) == 0 {
		// No DERP regions in non-nil map.
		return 0
	}

	// TODO: figure out which DERP region most of our peers are using,
	// and use that region as our fallback.
	//
	// If we already had selected something in the past and it has any
	// peers, we want to stay on it. If there are no peers at all,
	// stay on whatever DERP we previously picked. If we need to pick
	// one and have no peer info, pick a region randomly.
	//
	// We used to do the above for legacy clients, but never updated
	// it for disco.

	if c.myDerp != 0 {
		return c.myDerp
	}

	h := fnv.New64()
	fmt.Fprintf(h, "%p/%d", c, processStartUnixNano) // arbitrary
	return ids[rand.New(rand.NewSource(int64(h.Sum64()))).Intn(len(ids))]
}

// callNetInfoCallback calls the NetInfo callback (if previously
// registered with SetNetInfoCallback) if ni has substantially changed
// since the last state.
//
// callNetInfoCallback takes ownership of ni.
//
// c.mu must NOT be held.
func (c *Conn) callNetInfoCallback(ni *tailcfg.NetInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ni.BasicallyEqual(c.netInfoLast) {
		return
	}
	c.callNetInfoCallbackLocked(ni)
}

func (c *Conn) callNetInfoCallbackLocked(ni *tailcfg.NetInfo) {
	c.netInfoLast = ni
	if c.netInfoFunc != nil {
		c.dlogf("[v1] magicsock: netInfo update: %+v", ni)
		go c.netInfoFunc(ni)
	}
}

// addValidDiscoPathForTest makes addr a validated disco address for
// discoKey. It's used in tests to enable receiving of packets from
// addr without having to spin up the entire active discovery
// machinery.
func (c *Conn) addValidDiscoPathForTest(nodeKey key.NodePublic, addr netip.AddrPort) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.peerMap.setNodeKeyForIPPort(addr, nodeKey)
}

func (c *Conn) SetNetInfoCallback(fn func(*tailcfg.NetInfo)) {
	if fn == nil {
		panic("nil NetInfoCallback")
	}
	c.mu.Lock()
	last := c.netInfoLast
	c.netInfoFunc = fn
	c.mu.Unlock()

	if last != nil {
		fn(last)
	}
}

// LastRecvActivityOfNodeKey describes the time we last got traffic from
// this endpoint (updated every ~10 seconds).
func (c *Conn) LastRecvActivityOfNodeKey(nk key.NodePublic) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	de, ok := c.peerMap.endpointForNodeKey(nk)
	if !ok {
		return "never"
	}
	saw := de.lastRecv.LoadAtomic()
	if saw == 0 {
		return "never"
	}
	return mono.Since(saw).Round(time.Second).String()
}

// Ping handles a "tailscale ping" CLI query.
func (c *Conn) Ping(peer *tailcfg.Node, res *ipnstate.PingResult, cb func(*ipnstate.PingResult)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.privateKey.IsZero() {
		res.Err = "local tailscaled stopped"
		cb(res)
		return
	}
	if len(peer.Addresses) > 0 {
		res.NodeIP = peer.Addresses[0].Addr().String()
	}
	res.NodeName = peer.Name // prefer DNS name
	if res.NodeName == "" {
		res.NodeName = peer.Hostinfo.Hostname() // else hostname
	} else {
		res.NodeName, _, _ = strings.Cut(res.NodeName, ".")
	}

	ep, ok := c.peerMap.endpointForNodeKey(peer.Key)
	if !ok {
		res.Err = "unknown peer"
		cb(res)
		return
	}
	ep.cliPing(res, cb)
}

// c.mu must be held
func (c *Conn) populateCLIPingResponseLocked(res *ipnstate.PingResult, latency time.Duration, ep netip.AddrPort) {
	res.LatencySeconds = latency.Seconds()
	if ep.Addr() != derpMagicIPAddr {
		res.Endpoint = ep.String()
		return
	}
	regionID := int(ep.Port())
	res.DERPRegionID = regionID
	res.DERPRegionCode = c.derpRegionCodeLocked(regionID)
}

// GetEndpointChanges returns the most recent changes for a particular
// endpoint. The returned EndpointChange structs are for debug use only and
// there are no guarantees about order, size, or content.
func (c *Conn) GetEndpointChanges(peer *tailcfg.Node) ([]EndpointChange, error) {
	c.mu.Lock()
	if c.privateKey.IsZero() {
		c.mu.Unlock()
		return nil, fmt.Errorf("tailscaled stopped")
	}
	ep, ok := c.peerMap.endpointForNodeKey(peer.Key)
	c.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("unknown peer")
	}

	return ep.debugUpdates.GetAll(), nil
}

func (c *Conn) derpRegionCodeLocked(regionID int) string {
	if c.derpMap == nil {
		return ""
	}
	if dr, ok := c.derpMap.Regions[regionID]; ok {
		return dr.RegionCode
	}
	return ""
}

// DiscoPublicKey returns the discovery public key.
func (c *Conn) DiscoPublicKey() key.DiscoPublic {
	return c.discoPublic
}

// c.mu must NOT be held.
func (c *Conn) setNearestDERP(derpNum int) (wantDERP bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.wantDerpLocked() {
		c.myDerp = 0
		health.SetMagicSockDERPHome(0)
		return false
	}
	if derpNum == c.myDerp {
		// No change.
		return true
	}
	if c.myDerp != 0 && derpNum != 0 {
		metricDERPHomeChange.Add(1)
	}
	c.myDerp = derpNum
	health.SetMagicSockDERPHome(derpNum)

	if c.privateKey.IsZero() {
		// No private key yet, so DERP connections won't come up anyway.
		// Return early rather than ultimately log a couple lines of noise.
		return true
	}

	// On change, notify all currently connected DERP servers and
	// start connecting to our home DERP if we are not already.
	dr := c.derpMap.Regions[derpNum]
	if dr == nil {
		c.logf("[unexpected] magicsock: derpMap.Regions[%v] is nil", derpNum)
	} else {
		c.logf("magicsock: home is now derp-%v (%v)", derpNum, c.derpMap.Regions[derpNum].RegionCode)
	}
	for i, ad := range c.activeDerp {
		go ad.c.NotePreferred(i == c.myDerp)
	}
	c.goDerpConnect(derpNum)
	return true
}

// startDerpHomeConnectLocked starts connecting to our DERP home, if any.
//
// c.mu must be held.
func (c *Conn) startDerpHomeConnectLocked() {
	c.goDerpConnect(c.myDerp)
}

// goDerpConnect starts a goroutine to start connecting to the given
// DERP node.
//
// c.mu may be held, but does not need to be.
func (c *Conn) goDerpConnect(node int) {
	if node == 0 {
		return
	}
	go c.derpWriteChanOfAddr(netip.AddrPortFrom(derpMagicIPAddr, uint16(node)), key.NodePublic{})
}

// determineEndpoints returns the machine's endpoint addresses. It
// does a STUN lookup (via netcheck) to determine its public address.
//
// c.mu must NOT be held.
func (c *Conn) determineEndpoints(ctx context.Context) ([]tailcfg.Endpoint, error) {
	var havePortmap bool
	var portmapExt netip.AddrPort
	if runtime.GOOS != "js" {
		portmapExt, havePortmap = c.portMapper.GetCachedMappingOrStartCreatingOne()
	}

	nr, err := c.updateNetInfo(ctx)
	if err != nil {
		c.logf("magicsock.Conn.determineEndpoints: updateNetInfo: %v", err)
		return nil, err
	}

	if runtime.GOOS == "js" {
		// TODO(bradfitz): why does control require an
		// endpoint? Otherwise it doesn't stream map responses
		// back.
		return []tailcfg.Endpoint{
			{
				Addr: netip.MustParseAddrPort("[fe80:123:456:789::1]:12345"),
				Type: tailcfg.EndpointLocal,
			},
		}, nil
	}

	var already map[netip.AddrPort]tailcfg.EndpointType // endpoint -> how it was found
	var eps []tailcfg.Endpoint                          // unique endpoints

	ipp := func(s string) (ipp netip.AddrPort) {
		ipp, _ = netip.ParseAddrPort(s)
		return
	}
	addAddr := func(ipp netip.AddrPort, et tailcfg.EndpointType) {
		if !ipp.IsValid() || (debugOmitLocalAddresses() && et == tailcfg.EndpointLocal) {
			return
		}
		if _, ok := already[ipp]; !ok {
			mak.Set(&already, ipp, et)
			eps = append(eps, tailcfg.Endpoint{Addr: ipp, Type: et})
		}
	}

	// If we didn't have a portmap earlier, maybe it's done by now.
	if !havePortmap {
		portmapExt, havePortmap = c.portMapper.GetCachedMappingOrStartCreatingOne()
	}
	if havePortmap {
		addAddr(portmapExt, tailcfg.EndpointPortmapped)
		c.setNetInfoHavePortMap()
	}

	if nr.GlobalV4 != "" {
		addAddr(ipp(nr.GlobalV4), tailcfg.EndpointSTUN)

		// If they're behind a hard NAT and are using a fixed
		// port locally, assume they might've added a static
		// port mapping on their router to the same explicit
		// port that tailscaled is running with. Worst case
		// it's an invalid candidate mapping.
		if port := c.port.Load(); nr.MappingVariesByDestIP.EqualBool(true) && port != 0 {
			if ip, _, err := net.SplitHostPort(nr.GlobalV4); err == nil {
				addAddr(ipp(net.JoinHostPort(ip, strconv.Itoa(int(port)))), tailcfg.EndpointSTUN4LocalPort)
			}
		}
	}
	if nr.GlobalV6 != "" {
		addAddr(ipp(nr.GlobalV6), tailcfg.EndpointSTUN)
	}

	c.ignoreSTUNPackets()

	// Update our set of endpoints by adding any endpoints that we
	// previously found but haven't expired yet. This also updates the
	// cache with the set of endpoints discovered in this function.
	//
	// NOTE: we do this here and not below so that we don't cache local
	// endpoints; we know that the local endpoints we discover are all
	// possible local endpoints since we determine them by looking at the
	// set of addresses on our local interfaces.
	//
	// TODO(andrew): If we pull in any cached endpoints, we should probably
	// do something to ensure we're propagating the removal of those cached
	// endpoints if they do actually time out without being rediscovered.
	// For now, though, rely on a minor LinkChange event causing this to
	// re-run.
	eps = c.endpointTracker.update(time.Now(), eps)

	if localAddr := c.pconn4.LocalAddr(); localAddr.IP.IsUnspecified() {
		ips, loopback, err := interfaces.LocalAddresses()
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 && len(eps) == 0 {
			// Only include loopback addresses if we have no
			// interfaces at all to use as endpoints and don't
			// have a public IPv4 or IPv6 address. This allows
			// for localhost testing when you're on a plane and
			// offline, for example.
			ips = loopback
		}
		for _, ip := range ips {
			addAddr(netip.AddrPortFrom(ip, uint16(localAddr.Port)), tailcfg.EndpointLocal)
		}
	} else {
		// Our local endpoint is bound to a particular address.
		// Do not offer addresses on other local interfaces.
		addAddr(ipp(localAddr.String()), tailcfg.EndpointLocal)
	}

	// Note: the endpoints are intentionally returned in priority order,
	// from "farthest but most reliable" to "closest but least
	// reliable." Addresses returned from STUN should be globally
	// addressable, but might go farther on the network than necessary.
	// Local interface addresses might have lower latency, but not be
	// globally addressable.
	//
	// The STUN address(es) are always first so that legacy wireguard
	// can use eps[0] as its only known endpoint address (although that's
	// obviously non-ideal).
	//
	// Despite this sorting, though, clients since 0.100 haven't relied
	// on the sorting order for any decisions.
	return eps, nil
}

// endpointSetsEqual reports whether x and y represent the same set of
// endpoints. The order doesn't matter.
//
// It does not mutate the slices.
func endpointSetsEqual(x, y []tailcfg.Endpoint) bool {
	if len(x) == len(y) {
		orderMatches := true
		for i := range x {
			if x[i] != y[i] {
				orderMatches = false
				break
			}
		}
		if orderMatches {
			return true
		}
	}
	m := map[tailcfg.Endpoint]int{}
	for _, v := range x {
		m[v] |= 1
	}
	for _, v := range y {
		m[v] |= 2
	}
	for _, n := range m {
		if n != 3 {
			return false
		}
	}
	return true
}

// LocalPort returns the current IPv4 listener's port number.
func (c *Conn) LocalPort() uint16 {
	if runtime.GOOS == "js" {
		return 12345
	}
	laddr := c.pconn4.LocalAddr()
	return uint16(laddr.Port)
}

var errNetworkDown = errors.New("magicsock: network down")

func (c *Conn) networkDown() bool { return !c.networkUp.Load() }

// Send implements conn.Bind.
//
// See https://pkg.go.dev/golang.zx2c4.com/wireguard/conn#Bind.Send
func (c *Conn) Send(buffs [][]byte, ep conn.Endpoint) error {
	n := int64(len(buffs))
	metricSendData.Add(n)
	if c.networkDown() {
		metricSendDataNetworkDown.Add(n)
		return errNetworkDown
	}
	return ep.(*endpoint).send(buffs)
}

var errConnClosed = errors.New("Conn closed")

var errDropDerpPacket = errors.New("too many DERP packets queued; dropping")

var errNoUDP = errors.New("no UDP available on platform")

var (
	// This acts as a compile-time check for our usage of ipv6.Message in
	// batchingUDPConn for both IPv6 and IPv4 operations.
	_ ipv6.Message = ipv4.Message{}
)

func (c *Conn) sendUDPBatch(addr netip.AddrPort, buffs [][]byte) (sent bool, err error) {
	isIPv6 := false
	switch {
	case addr.Addr().Is4():
	case addr.Addr().Is6():
		isIPv6 = true
	default:
		panic("bogus sendUDPBatch addr type")
	}
	if isIPv6 {
		err = c.pconn6.WriteBatchTo(buffs, addr)
	} else {
		err = c.pconn4.WriteBatchTo(buffs, addr)
	}
	if err != nil {
		var errGSO neterror.ErrUDPGSODisabled
		if errors.As(err, &errGSO) {
			c.logf("magicsock: %s", errGSO.Error())
			err = errGSO.RetryErr
		}
	}
	return err == nil, err
}

// sendUDP sends UDP packet b to ipp.
// See sendAddr's docs on the return value meanings.
func (c *Conn) sendUDP(ipp netip.AddrPort, b []byte) (sent bool, err error) {
	if runtime.GOOS == "js" {
		return false, errNoUDP
	}
	sent, err = c.sendUDPStd(ipp, b)
	if err != nil {
		metricSendUDPError.Add(1)
	} else {
		if sent {
			metricSendUDP.Add(1)
		}
	}
	return
}

// sendUDP sends UDP packet b to addr.
// See sendAddr's docs on the return value meanings.
func (c *Conn) sendUDPStd(addr netip.AddrPort, b []byte) (sent bool, err error) {
	switch {
	case addr.Addr().Is4():
		_, err = c.pconn4.WriteToUDPAddrPort(b, addr)
		if err != nil && (c.noV4.Load() || neterror.TreatAsLostUDP(err)) {
			return false, nil
		}
	case addr.Addr().Is6():
		_, err = c.pconn6.WriteToUDPAddrPort(b, addr)
		if err != nil && (c.noV6.Load() || neterror.TreatAsLostUDP(err)) {
			return false, nil
		}
	default:
		panic("bogus sendUDPStd addr type")
	}
	return err == nil, err
}

// sendAddr sends packet b to addr, which is either a real UDP address
// or a fake UDP address representing a DERP server (see derpmap.go).
// The provided public key identifies the recipient.
//
// The returned err is whether there was an error writing when it
// should've worked.
// The returned sent is whether a packet went out at all.
// An example of when they might be different: sending to an
// IPv6 address when the local machine doesn't have IPv6 support
// returns (false, nil); it's not an error, but nothing was sent.
func (c *Conn) sendAddr(addr netip.AddrPort, pubKey key.NodePublic, b []byte) (sent bool, err error) {
	if addr.Addr() != derpMagicIPAddr {
		return c.sendUDP(addr, b)
	}

	ch := c.derpWriteChanOfAddr(addr, pubKey)
	if ch == nil {
		metricSendDERPErrorChan.Add(1)
		return false, nil
	}

	// TODO(bradfitz): this makes garbage for now; we could use a
	// buffer pool later.  Previously we passed ownership of this
	// to derpWriteRequest and waited for derphttp.Client.Send to
	// complete, but that's too slow while holding wireguard-go
	// internal locks.
	pkt := make([]byte, len(b))
	copy(pkt, b)

	select {
	case <-c.donec:
		metricSendDERPErrorClosed.Add(1)
		return false, errConnClosed
	case ch <- derpWriteRequest{addr, pubKey, pkt}:
		metricSendDERPQueued.Add(1)
		return true, nil
	default:
		metricSendDERPErrorQueue.Add(1)
		// Too many writes queued. Drop packet.
		return false, errDropDerpPacket
	}
}

var (
	bufferedDerpWrites     int
	bufferedDerpWritesOnce sync.Once
)

// bufferedDerpWritesBeforeDrop returns how many packets writes can be queued
// up the DERP client to write on the wire before we start dropping.
func bufferedDerpWritesBeforeDrop() int {
	// For mobile devices, always return the previous minimum value of 32;
	// we can do this outside the sync.Once to avoid that overhead.
	if runtime.GOOS == "ios" || runtime.GOOS == "android" {
		return 32
	}

	bufferedDerpWritesOnce.Do(func() {
		// Some rough sizing: for the previous fixed value of 32, the
		// total consumed memory can be:
		// = numDerpRegions * messages/region * sizeof(message)
		//
		// For sake of this calculation, assume 100 DERP regions; at
		// time of writing (2023-04-03), we have 24.
		//
		// A reasonable upper bound for the worst-case average size of
		// a message is a *disco.CallMeMaybe message with 16 endpoints;
		// since sizeof(netip.AddrPort) = 32, that's 512 bytes. Thus:
		// = 100 * 32 * 512
		// = 1638400 (1.6MiB)
		//
		// On a reasonably-small node with 4GiB of memory that's
		// connected to each region and handling a lot of load, 1.6MiB
		// is about 0.04% of the total system memory.
		//
		// For sake of this calculation, then, let's double that memory
		// usage to 0.08% and scale based on total system memory.
		//
		// For a 16GiB Linux box, this should buffer just over 256
		// messages.
		systemMemory := sysresources.TotalMemory()
		memoryUsable := float64(systemMemory) * 0.0008

		const (
			theoreticalDERPRegions  = 100
			messageMaximumSizeBytes = 512
		)
		bufferedDerpWrites = int(memoryUsable / (theoreticalDERPRegions * messageMaximumSizeBytes))

		// Never drop below the previous minimum value.
		if bufferedDerpWrites < 32 {
			bufferedDerpWrites = 32
		}
	})
	return bufferedDerpWrites
}

// derpWriteChanOfAddr returns a DERP client for fake UDP addresses that
// represent DERP servers, creating them as necessary. For real UDP
// addresses, it returns nil.
//
// If peer is non-zero, it can be used to find an active reverse
// path, without using addr.
func (c *Conn) derpWriteChanOfAddr(addr netip.AddrPort, peer key.NodePublic) chan<- derpWriteRequest {
	if addr.Addr() != derpMagicIPAddr {
		return nil
	}
	regionID := int(addr.Port())

	if c.networkDown() {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.wantDerpLocked() || c.closed {
		return nil
	}
	if c.derpMap == nil || c.derpMap.Regions[regionID] == nil {
		return nil
	}
	if c.privateKey.IsZero() {
		c.logf("magicsock: DERP lookup of %v with no private key; ignoring", addr)
		return nil
	}

	// See if we have a connection open to that DERP node ID
	// first. If so, might as well use it. (It's a little
	// arbitrary whether we use this one vs. the reverse route
	// below when we have both.)
	ad, ok := c.activeDerp[regionID]
	if ok {
		*ad.lastWrite = time.Now()
		c.setPeerLastDerpLocked(peer, regionID, regionID)
		return ad.writeCh
	}

	// If we don't have an open connection to the peer's home DERP
	// node, see if we have an open connection to a DERP node
	// where we'd heard from that peer already. For instance,
	// perhaps peer's home is Frankfurt, but they dialed our home DERP
	// node in SF to reach us, so we can reply to them using our
	// SF connection rather than dialing Frankfurt. (Issue 150)
	if !peer.IsZero() && useDerpRoute() {
		if r, ok := c.derpRoute[peer]; ok {
			if ad, ok := c.activeDerp[r.derpID]; ok && ad.c == r.dc {
				c.setPeerLastDerpLocked(peer, r.derpID, regionID)
				*ad.lastWrite = time.Now()
				return ad.writeCh
			}
		}
	}

	why := "home-keep-alive"
	if !peer.IsZero() {
		why = peer.ShortString()
	}
	c.logf("magicsock: adding connection to derp-%v for %v", regionID, why)

	firstDerp := false
	if c.activeDerp == nil {
		firstDerp = true
		c.activeDerp = make(map[int]activeDerp)
		c.prevDerp = make(map[int]*syncs.WaitGroupChan)
	}

	// Note that derphttp.NewRegionClient does not dial the server
	// (it doesn't block) so it is safe to do under the c.mu lock.
	dc := derphttp.NewRegionClient(c.privateKey, c.logf, c.netMon, func() *tailcfg.DERPRegion {
		// Warning: it is not legal to acquire
		// magicsock.Conn.mu from this callback.
		// It's run from derphttp.Client.connect (via Send, etc)
		// and the lock ordering rules are that magicsock.Conn.mu
		// must be acquired before derphttp.Client.mu.
		// See https://github.com/tailscale/tailscale/issues/3726
		if c.connCtx.Err() != nil {
			// We're closing anyway; return nil to stop dialing.
			return nil
		}
		derpMap := c.derpMapAtomic.Load()
		if derpMap == nil {
			return nil
		}
		return derpMap.Regions[regionID]
	})

	dc.SetCanAckPings(true)
	dc.NotePreferred(c.myDerp == regionID)
	dc.SetAddressFamilySelector(derpAddrFamSelector{c})
	dc.DNSCache = dnscache.Get()

	ctx, cancel := context.WithCancel(c.connCtx)
	ch := make(chan derpWriteRequest, bufferedDerpWritesBeforeDrop())

	ad.c = dc
	ad.writeCh = ch
	ad.cancel = cancel
	ad.lastWrite = new(time.Time)
	*ad.lastWrite = time.Now()
	ad.createTime = time.Now()
	c.activeDerp[regionID] = ad
	metricNumDERPConns.Set(int64(len(c.activeDerp)))
	c.logActiveDerpLocked()
	c.setPeerLastDerpLocked(peer, regionID, regionID)
	c.scheduleCleanStaleDerpLocked()

	// Build a startGate for the derp reader+writer
	// goroutines, so they don't start running until any
	// previous generation is closed.
	startGate := syncs.ClosedChan()
	if prev := c.prevDerp[regionID]; prev != nil {
		startGate = prev.DoneChan()
	}
	// And register a WaitGroup(Chan) for this generation.
	wg := syncs.NewWaitGroupChan()
	wg.Add(2)
	c.prevDerp[regionID] = wg

	if firstDerp {
		startGate = c.derpStarted
		go func() {
			dc.Connect(ctx)
			close(c.derpStarted)
			c.muCond.Broadcast()
		}()
	}

	go c.runDerpReader(ctx, addr, dc, wg, startGate)
	go c.runDerpWriter(ctx, dc, ch, wg, startGate)
	go c.derpActiveFunc()

	return ad.writeCh
}

// setPeerLastDerpLocked notes that peer is now being written to via
// the provided DERP regionID, and that the peer advertises a DERP
// home region ID of homeID.
//
// If there's any change, it logs.
//
// c.mu must be held.
func (c *Conn) setPeerLastDerpLocked(peer key.NodePublic, regionID, homeID int) {
	if peer.IsZero() {
		return
	}
	old := c.peerLastDerp[peer]
	if old == regionID {
		return
	}
	c.peerLastDerp[peer] = regionID

	var newDesc string
	switch {
	case regionID == homeID && regionID == c.myDerp:
		newDesc = "shared home"
	case regionID == homeID:
		newDesc = "their home"
	case regionID == c.myDerp:
		newDesc = "our home"
	case regionID != homeID:
		newDesc = "alt"
	}
	if old == 0 {
		c.logf("[v1] magicsock: derp route for %s set to derp-%d (%s)", peer.ShortString(), regionID, newDesc)
	} else {
		c.logf("[v1] magicsock: derp route for %s changed from derp-%d => derp-%d (%s)", peer.ShortString(), old, regionID, newDesc)
	}
}

// derpReadResult is the type sent by runDerpClient to ReceiveIPv4
// when a DERP packet is available.
//
// Notably, it doesn't include the derp.ReceivedPacket because we
// don't want to give the receiver access to the aliased []byte.  To
// get at the packet contents they need to call copyBuf to copy it
// out, which also releases the buffer.
type derpReadResult struct {
	regionID int
	n        int // length of data received
	src      key.NodePublic
	// copyBuf is called to copy the data to dst.  It returns how
	// much data was copied, which will be n if dst is large
	// enough. copyBuf can only be called once.
	// If copyBuf is nil, that's a signal from the sender to ignore
	// this message.
	copyBuf func(dst []byte) int
}

// runDerpReader runs in a goroutine for the life of a DERP
// connection, handling received packets.
func (c *Conn) runDerpReader(ctx context.Context, derpFakeAddr netip.AddrPort, dc *derphttp.Client, wg *syncs.WaitGroupChan, startGate <-chan struct{}) {
	defer wg.Decr()
	defer dc.Close()

	select {
	case <-startGate:
	case <-ctx.Done():
		return
	}

	didCopy := make(chan struct{}, 1)
	regionID := int(derpFakeAddr.Port())
	res := derpReadResult{regionID: regionID}
	var pkt derp.ReceivedPacket
	res.copyBuf = func(dst []byte) int {
		n := copy(dst, pkt.Data)
		didCopy <- struct{}{}
		return n
	}

	defer health.SetDERPRegionConnectedState(regionID, false)
	defer health.SetDERPRegionHealth(regionID, "")

	// peerPresent is the set of senders we know are present on this
	// connection, based on messages we've received from the server.
	peerPresent := map[key.NodePublic]bool{}
	bo := backoff.NewBackoff(fmt.Sprintf("derp-%d", regionID), c.logf, 5*time.Second)
	var lastPacketTime time.Time
	var lastPacketSrc key.NodePublic

	for {
		msg, connGen, err := dc.RecvDetail()
		if err != nil {
			health.SetDERPRegionConnectedState(regionID, false)
			// Forget that all these peers have routes.
			for peer := range peerPresent {
				delete(peerPresent, peer)
				c.removeDerpPeerRoute(peer, regionID, dc)
			}
			if err == derphttp.ErrClientClosed {
				return
			}
			if c.networkDown() {
				c.logf("[v1] magicsock: derp.Recv(derp-%d): network down, closing", regionID)
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
			}

			c.logf("magicsock: [%p] derp.Recv(derp-%d): %v", dc, regionID, err)

			// If our DERP connection broke, it might be because our network
			// conditions changed. Start that check.
			c.ReSTUN("derp-recv-error")

			// Back off a bit before reconnecting.
			bo.BackOff(ctx, err)
			select {
			case <-ctx.Done():
				return
			default:
			}
			continue
		}
		bo.BackOff(ctx, nil) // reset

		now := time.Now()
		if lastPacketTime.IsZero() || now.Sub(lastPacketTime) > 5*time.Second {
			health.NoteDERPRegionReceivedFrame(regionID)
			lastPacketTime = now
		}

		switch m := msg.(type) {
		case derp.ServerInfoMessage:
			health.SetDERPRegionConnectedState(regionID, true)
			health.SetDERPRegionHealth(regionID, "") // until declared otherwise
			c.logf("magicsock: derp-%d connected; connGen=%v", regionID, connGen)
			continue
		case derp.ReceivedPacket:
			pkt = m
			res.n = len(m.Data)
			res.src = m.Source
			if logDerpVerbose() {
				c.logf("magicsock: got derp-%v packet: %q", regionID, m.Data)
			}
			// If this is a new sender we hadn't seen before, remember it and
			// register a route for this peer.
			if res.src != lastPacketSrc { // avoid map lookup w/ high throughput single peer
				lastPacketSrc = res.src
				if _, ok := peerPresent[res.src]; !ok {
					peerPresent[res.src] = true
					c.addDerpPeerRoute(res.src, regionID, dc)
				}
			}
		case derp.PingMessage:
			// Best effort reply to the ping.
			pingData := [8]byte(m)
			go func() {
				if err := dc.SendPong(pingData); err != nil {
					c.logf("magicsock: derp-%d SendPong error: %v", regionID, err)
				}
			}()
			continue
		case derp.HealthMessage:
			health.SetDERPRegionHealth(regionID, m.Problem)
		case derp.PeerGoneMessage:
			switch m.Reason {
			case derp.PeerGoneReasonDisconnected:
				// Do nothing.
			case derp.PeerGoneReasonNotHere:
				metricRecvDiscoDERPPeerNotHere.Add(1)
				c.logf("[unexpected] magicsock: derp-%d does not know about peer %s, removing route",
					regionID, key.NodePublic(m.Peer).ShortString())
			default:
				metricRecvDiscoDERPPeerGoneUnknown.Add(1)
				c.logf("[unexpected] magicsock: derp-%d peer %s gone, reason %v, removing route",
					regionID, key.NodePublic(m.Peer).ShortString(), m.Reason)
			}
			c.removeDerpPeerRoute(key.NodePublic(m.Peer), regionID, dc)
		default:
			// Ignore.
			continue
		}

		select {
		case <-ctx.Done():
			return
		case c.derpRecvCh <- res:
		}

		select {
		case <-ctx.Done():
			return
		case <-didCopy:
			continue
		}
	}
}

type derpWriteRequest struct {
	addr   netip.AddrPort
	pubKey key.NodePublic
	b      []byte // copied; ownership passed to receiver
}

// runDerpWriter runs in a goroutine for the life of a DERP
// connection, handling received packets.
func (c *Conn) runDerpWriter(ctx context.Context, dc *derphttp.Client, ch <-chan derpWriteRequest, wg *syncs.WaitGroupChan, startGate <-chan struct{}) {
	defer wg.Decr()
	select {
	case <-startGate:
	case <-ctx.Done():
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case wr := <-ch:
			err := dc.Send(wr.pubKey, wr.b)
			if err != nil {
				c.logf("magicsock: derp.Send(%v): %v", wr.addr, err)
				metricSendDERPError.Add(1)
			} else {
				metricSendDERP.Add(1)
			}
		}
	}
}

type receiveBatch struct {
	msgs []ipv6.Message
}

func (c *Conn) getReceiveBatchForBuffs(buffs [][]byte) *receiveBatch {
	batch := c.receiveBatchPool.Get().(*receiveBatch)
	for i := range buffs {
		batch.msgs[i].Buffers[0] = buffs[i]
		batch.msgs[i].OOB = batch.msgs[i].OOB[:cap(batch.msgs[i].OOB)]
	}
	return batch
}

func (c *Conn) putReceiveBatch(batch *receiveBatch) {
	for i := range batch.msgs {
		batch.msgs[i] = ipv6.Message{Buffers: batch.msgs[i].Buffers, OOB: batch.msgs[i].OOB}
	}
	c.receiveBatchPool.Put(batch)
}

// receiveIPv4 creates an IPv4 ReceiveFunc reading from c.pconn4.
func (c *Conn) receiveIPv4() conn.ReceiveFunc {
	return c.mkReceiveFunc(&c.pconn4, &health.ReceiveIPv4, metricRecvDataIPv4)
}

// receiveIPv6 creates an IPv6 ReceiveFunc reading from c.pconn6.
func (c *Conn) receiveIPv6() conn.ReceiveFunc {
	return c.mkReceiveFunc(&c.pconn6, &health.ReceiveIPv6, metricRecvDataIPv6)
}

// mkReceiveFunc creates a ReceiveFunc reading from ruc.
// The provided healthItem and metric are updated if non-nil.
func (c *Conn) mkReceiveFunc(ruc *RebindingUDPConn, healthItem *health.ReceiveFuncStats, metric *clientmetric.Metric) conn.ReceiveFunc {
	// epCache caches an IPPort->endpoint for hot flows.
	var epCache ippEndpointCache

	return func(buffs [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		if healthItem != nil {
			healthItem.Enter()
			defer healthItem.Exit()
		}
		if ruc == nil {
			panic("nil RebindingUDPConn")
		}

		batch := c.getReceiveBatchForBuffs(buffs)
		defer c.putReceiveBatch(batch)
		for {
			numMsgs, err := ruc.ReadBatch(batch.msgs[:len(buffs)], 0)
			if err != nil {
				if neterror.PacketWasTruncated(err) {
					continue
				}
				return 0, err
			}

			reportToCaller := false
			for i, msg := range batch.msgs[:numMsgs] {
				if msg.N == 0 {
					sizes[i] = 0
					continue
				}
				ipp := msg.Addr.(*net.UDPAddr).AddrPort()
				if ep, ok := c.receiveIP(msg.Buffers[0][:msg.N], ipp, &epCache); ok {
					if metric != nil {
						metric.Add(1)
					}
					eps[i] = ep
					sizes[i] = msg.N
					reportToCaller = true
				} else {
					sizes[i] = 0
				}
			}
			if reportToCaller {
				return numMsgs, nil
			}
		}
	}
}

// receiveIP is the shared bits of ReceiveIPv4 and ReceiveIPv6.
//
// ok is whether this read should be reported up to wireguard-go (our
// caller).
func (c *Conn) receiveIP(b []byte, ipp netip.AddrPort, cache *ippEndpointCache) (ep *endpoint, ok bool) {
	if stun.Is(b) {
		c.stunReceiveFunc.Load()(b, ipp)
		return nil, false
	}
	if c.handleDiscoMessage(b, ipp, key.NodePublic{}, discoRXPathUDP) {
		return nil, false
	}
	if !c.havePrivateKey.Load() {
		// If we have no private key, we're logged out or
		// stopped. Don't try to pass these wireguard packets
		// up to wireguard-go; it'll just complain (issue 1167).
		return nil, false
	}
	if cache.ipp == ipp && cache.de != nil && cache.gen == cache.de.numStopAndReset() {
		ep = cache.de
	} else {
		c.mu.Lock()
		de, ok := c.peerMap.endpointForIPPort(ipp)
		c.mu.Unlock()
		if !ok {
			return nil, false
		}
		cache.ipp = ipp
		cache.de = de
		cache.gen = de.numStopAndReset()
		ep = de
	}
	ep.noteRecvActivity()
	if stats := c.stats.Load(); stats != nil {
		stats.UpdateRxPhysical(ep.nodeAddr, ipp, len(b))
	}
	return ep, true
}

func (c *connBind) receiveDERP(buffs [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
	health.ReceiveDERP.Enter()
	defer health.ReceiveDERP.Exit()

	for dm := range c.derpRecvCh {
		if c.isClosed() {
			break
		}
		n, ep := c.processDERPReadResult(dm, buffs[0])
		if n == 0 {
			// No data read occurred. Wait for another packet.
			continue
		}
		metricRecvDataDERP.Add(1)
		sizes[0] = n
		eps[0] = ep
		return 1, nil
	}
	return 0, net.ErrClosed
}

func (c *Conn) processDERPReadResult(dm derpReadResult, b []byte) (n int, ep *endpoint) {
	if dm.copyBuf == nil {
		return 0, nil
	}
	var regionID int
	n, regionID = dm.n, dm.regionID
	ncopy := dm.copyBuf(b)
	if ncopy != n {
		err := fmt.Errorf("received DERP packet of length %d that's too big for WireGuard buf size %d", n, ncopy)
		c.logf("magicsock: %v", err)
		return 0, nil
	}

	ipp := netip.AddrPortFrom(derpMagicIPAddr, uint16(regionID))
	if c.handleDiscoMessage(b[:n], ipp, dm.src, discoRXPathDERP) {
		return 0, nil
	}

	var ok bool
	c.mu.Lock()
	ep, ok = c.peerMap.endpointForNodeKey(dm.src)
	c.mu.Unlock()
	if !ok {
		// We don't know anything about this node key, nothing to
		// record or process.
		return 0, nil
	}

	ep.noteRecvActivity()
	if stats := c.stats.Load(); stats != nil {
		stats.UpdateRxPhysical(ep.nodeAddr, ipp, dm.n)
	}
	return n, ep
}

// discoLogLevel controls the verbosity of discovery log messages.
type discoLogLevel int

const (
	// discoLog means that a message should be logged.
	discoLog discoLogLevel = iota

	// discoVerboseLog means that a message should only be logged
	// in TS_DEBUG_DISCO mode.
	discoVerboseLog
)

// TS_DISCO_PONG_IPV4_DELAY, if set, is a time.Duration string that is how much
// fake latency to add before replying to disco pings. This can be used to bias
// peers towards using IPv6 when both IPv4 and IPv6 are available at similar
// speeds.
var debugIPv4DiscoPingPenalty = envknob.RegisterDuration("TS_DISCO_PONG_IPV4_DELAY")

// sendDiscoMessage sends discovery message m to dstDisco at dst.
//
// If dst is a DERP IP:port, then dstKey must be non-zero.
//
// The dstKey should only be non-zero if the dstDisco key
// unambiguously maps to exactly one peer.
func (c *Conn) sendDiscoMessage(dst netip.AddrPort, dstKey key.NodePublic, dstDisco key.DiscoPublic, m disco.Message, logLevel discoLogLevel) (sent bool, err error) {
	isDERP := dst.Addr() == derpMagicIPAddr
	if _, isPong := m.(*disco.Pong); isPong && !isDERP && dst.Addr().Is4() {
		time.Sleep(debugIPv4DiscoPingPenalty())
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false, errConnClosed
	}
	var nonce [disco.NonceLen]byte
	if _, err := crand.Read(nonce[:]); err != nil {
		panic(err) // worth dying for
	}
	pkt := make([]byte, 0, 512) // TODO: size it correctly? pool? if it matters.
	pkt = append(pkt, disco.Magic...)
	pkt = c.discoPublic.AppendTo(pkt)
	di := c.discoInfoLocked(dstDisco)
	c.mu.Unlock()

	if isDERP {
		metricSendDiscoDERP.Add(1)
	} else {
		metricSendDiscoUDP.Add(1)
	}

	box := di.sharedKey.Seal(m.AppendMarshal(nil))
	pkt = append(pkt, box...)
	sent, err = c.sendAddr(dst, dstKey, pkt)
	if sent {
		if logLevel == discoLog || (logLevel == discoVerboseLog && debugDisco()) {
			node := "?"
			if !dstKey.IsZero() {
				node = dstKey.ShortString()
			}
			c.dlogf("[v1] magicsock: disco: %v->%v (%v, %v) sent %v", c.discoShort, dstDisco.ShortString(), node, derpStr(dst.String()), disco.MessageSummary(m))
		}
		if isDERP {
			metricSentDiscoDERP.Add(1)
		} else {
			metricSentDiscoUDP.Add(1)
		}
		switch m.(type) {
		case *disco.Ping:
			metricSentDiscoPing.Add(1)
		case *disco.Pong:
			metricSentDiscoPong.Add(1)
		case *disco.CallMeMaybe:
			metricSentDiscoCallMeMaybe.Add(1)
		}
	} else if err == nil {
		// Can't send. (e.g. no IPv6 locally)
	} else {
		if !c.networkDown() {
			c.logf("magicsock: disco: failed to send %T to %v: %v", m, dst, err)
		}
	}
	return sent, err
}

// discoPcapFrame marshals the bytes for a pcap record that describe a
// disco frame.
//
// Warning: Alloc garbage. Acceptable while capturing.
func discoPcapFrame(src netip.AddrPort, derpNodeSrc key.NodePublic, payload []byte) []byte {
	var (
		b    bytes.Buffer
		flag uint8
	)
	b.Grow(128) // Most disco frames will probably be smaller than this.

	if src.Addr() == derpMagicIPAddr {
		flag |= 0x01
	}
	b.WriteByte(flag) // 1b: flag

	derpSrc := derpNodeSrc.Raw32()
	b.Write(derpSrc[:])                                       // 32b: derp public key
	binary.Write(&b, binary.LittleEndian, uint16(src.Port())) // 2b: port
	addr, _ := src.Addr().MarshalBinary()
	binary.Write(&b, binary.LittleEndian, uint16(len(addr)))    // 2b: len(addr)
	b.Write(addr)                                               // Xb: addr
	binary.Write(&b, binary.LittleEndian, uint16(len(payload))) // 2b: len(payload)
	b.Write(payload)                                            // Xb: payload

	return b.Bytes()
}

type discoRXPath string

const (
	discoRXPathUDP       discoRXPath = "UDP socket"
	discoRXPathDERP      discoRXPath = "DERP"
	discoRXPathRawSocket discoRXPath = "raw socket"
)

// handleDiscoMessage handles a discovery message and reports whether
// msg was a Tailscale inter-node discovery message.
//
// A discovery message has the form:
//
//   - magic             [6]byte
//   - senderDiscoPubKey [32]byte
//   - nonce             [24]byte
//   - naclbox of payload (see tailscale.com/disco package for inner payload format)
//
// For messages received over DERP, the src.Addr() will be derpMagicIP (with
// src.Port() being the region ID) and the derpNodeSrc will be the node key
// it was received from at the DERP layer. derpNodeSrc is zero when received
// over UDP.
func (c *Conn) handleDiscoMessage(msg []byte, src netip.AddrPort, derpNodeSrc key.NodePublic, via discoRXPath) (isDiscoMsg bool) {
	const headerLen = len(disco.Magic) + key.DiscoPublicRawLen
	if len(msg) < headerLen || string(msg[:len(disco.Magic)]) != disco.Magic {
		return false
	}

	// If the first four parts are the prefix of disco.Magic
	// (0x5453f09f) then it's definitely not a valid WireGuard
	// packet (which starts with little-endian uint32 1, 2, 3, 4).
	// Use naked returns for all following paths.
	isDiscoMsg = true

	sender := key.DiscoPublicFromRaw32(mem.B(msg[len(disco.Magic):headerLen]))

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return
	}
	if debugDisco() {
		c.logf("magicsock: disco: got disco-looking frame from %v via %s", sender.ShortString(), via)
	}
	if c.privateKey.IsZero() {
		// Ignore disco messages when we're stopped.
		// Still return true, to not pass it down to wireguard.
		return
	}

	if !c.peerMap.anyEndpointForDiscoKey(sender) {
		metricRecvDiscoBadPeer.Add(1)
		if debugDisco() {
			c.logf("magicsock: disco: ignoring disco-looking frame, don't know endpoint for %v", sender.ShortString())
		}
		return
	}

	// We're now reasonably sure we're expecting communication from
	// this peer, do the heavy crypto lifting to see what they want.
	//
	// From here on, peerNode and de are non-nil.

	di := c.discoInfoLocked(sender)

	sealedBox := msg[headerLen:]
	payload, ok := di.sharedKey.Open(sealedBox)
	if !ok {
		// This might be have been intended for a previous
		// disco key.  When we restart we get a new disco key
		// and old packets might've still been in flight (or
		// scheduled). This is particularly the case for LANs
		// or non-NATed endpoints. UDP offloading on Linux
		// can also cause this when a disco message is
		// received via raw socket at the head of a coalesced
		// group of messages. Don't log in normal case.
		// Callers may choose to pass on to wireguard, in case
		// it's actually a wireguard packet (super unlikely, but).
		if debugDisco() {
			c.logf("magicsock: disco: failed to open naclbox from %v (wrong rcpt?) via %s", sender, via)
		}
		metricRecvDiscoBadKey.Add(1)
		return
	}

	// Emit information about the disco frame into the pcap stream
	// if a capture hook is installed.
	if cb := c.captureHook.Load(); cb != nil {
		cb(capture.PathDisco, time.Now(), discoPcapFrame(src, derpNodeSrc, payload), packet.CaptureMeta{})
	}

	dm, err := disco.Parse(payload)
	if debugDisco() {
		c.logf("magicsock: disco: disco.Parse = %T, %v", dm, err)
	}
	if err != nil {
		// Couldn't parse it, but it was inside a correctly
		// signed box, so just ignore it, assuming it's from a
		// newer version of Tailscale that we don't
		// understand. Not even worth logging about, lest it
		// be too spammy for old clients.
		metricRecvDiscoBadParse.Add(1)
		return
	}

	isDERP := src.Addr() == derpMagicIPAddr
	if isDERP {
		metricRecvDiscoDERP.Add(1)
	} else {
		metricRecvDiscoUDP.Add(1)
	}

	switch dm := dm.(type) {
	case *disco.Ping:
		metricRecvDiscoPing.Add(1)
		c.handlePingLocked(dm, src, di, derpNodeSrc)
	case *disco.Pong:
		metricRecvDiscoPong.Add(1)
		// There might be multiple nodes for the sender's DiscoKey.
		// Ask each to handle it, stopping once one reports that
		// the Pong's TxID was theirs.
		c.peerMap.forEachEndpointWithDiscoKey(sender, func(ep *endpoint) (keepGoing bool) {
			if ep.handlePongConnLocked(dm, di, src) {
				return false
			}
			return true
		})
	case *disco.CallMeMaybe:
		metricRecvDiscoCallMeMaybe.Add(1)
		if !isDERP || derpNodeSrc.IsZero() {
			// CallMeMaybe messages should only come via DERP.
			c.logf("[unexpected] CallMeMaybe packets should only come via DERP")
			return
		}
		nodeKey := derpNodeSrc
		ep, ok := c.peerMap.endpointForNodeKey(nodeKey)
		if !ok {
			metricRecvDiscoCallMeMaybeBadNode.Add(1)
			c.logf("magicsock: disco: ignoring CallMeMaybe from %v; %v is unknown", sender.ShortString(), derpNodeSrc.ShortString())
			return
		}
		epDisco := ep.disco.Load()
		if epDisco == nil {
			return
		}
		if epDisco.key != di.discoKey {
			metricRecvDiscoCallMeMaybeBadDisco.Add(1)
			c.logf("[unexpected] CallMeMaybe from peer via DERP whose netmap discokey != disco source")
			return
		}
		c.dlogf("[v1] magicsock: disco: %v<-%v (%v, %v)  got call-me-maybe, %d endpoints",
			c.discoShort, epDisco.short,
			ep.publicKey.ShortString(), derpStr(src.String()),
			len(dm.MyNumber))
		go ep.handleCallMeMaybe(dm)
	}
	return
}

// unambiguousNodeKeyOfPingLocked attempts to look up an unambiguous mapping
// from a DiscoKey dk (which sent ping dm) to a NodeKey. ok is true
// if there's the NodeKey is known unambiguously.
//
// derpNodeSrc is non-zero if the disco ping arrived via DERP.
//
// c.mu must be held.
func (c *Conn) unambiguousNodeKeyOfPingLocked(dm *disco.Ping, dk key.DiscoPublic, derpNodeSrc key.NodePublic) (nk key.NodePublic, ok bool) {
	if !derpNodeSrc.IsZero() {
		if ep, ok := c.peerMap.endpointForNodeKey(derpNodeSrc); ok {
			epDisco := ep.disco.Load()
			if epDisco != nil && epDisco.key == dk {
				return derpNodeSrc, true
			}
		}
	}

	// Pings after 1.16.0 contains its node source. See if it maps back.
	if !dm.NodeKey.IsZero() {
		if ep, ok := c.peerMap.endpointForNodeKey(dm.NodeKey); ok {
			epDisco := ep.disco.Load()
			if epDisco != nil && epDisco.key == dk {
				return dm.NodeKey, true
			}
		}
	}

	// If there's exactly 1 node in our netmap with DiscoKey dk,
	// then it's not ambiguous which node key dm was from.
	if set := c.peerMap.nodesOfDisco[dk]; len(set) == 1 {
		for nk = range set {
			return nk, true
		}
	}

	return nk, false
}

// di is the discoInfo of the source of the ping.
// derpNodeSrc is non-zero if the ping arrived via DERP.
func (c *Conn) handlePingLocked(dm *disco.Ping, src netip.AddrPort, di *discoInfo, derpNodeSrc key.NodePublic) {
	likelyHeartBeat := src == di.lastPingFrom && time.Since(di.lastPingTime) < 5*time.Second
	di.lastPingFrom = src
	di.lastPingTime = time.Now()
	isDerp := src.Addr() == derpMagicIPAddr

	// If we can figure out with certainty which node key this disco
	// message is for, eagerly update our IP<>node and disco<>node
	// mappings to make p2p path discovery faster in simple
	// cases. Without this, disco would still work, but would be
	// reliant on DERP call-me-maybe to establish the disco<>node
	// mapping, and on subsequent disco handlePongLocked to establish
	// the IP<>disco mapping.
	if nk, ok := c.unambiguousNodeKeyOfPingLocked(dm, di.discoKey, derpNodeSrc); ok {
		if !isDerp {
			c.peerMap.setNodeKeyForIPPort(src, nk)
		}
	}

	// If we got a ping over DERP, then derpNodeSrc is non-zero and we reply
	// over DERP (in which case ipDst is also a DERP address).
	// But if the ping was over UDP (ipDst is not a DERP address), then dstKey
	// will be zero here, but that's fine: sendDiscoMessage only requires
	// a dstKey if the dst ip:port is DERP.
	dstKey := derpNodeSrc

	// Remember this route if not present.
	var numNodes int
	var dup bool
	if isDerp {
		if ep, ok := c.peerMap.endpointForNodeKey(derpNodeSrc); ok {
			if ep.addCandidateEndpoint(src, dm.TxID) {
				return
			}
			numNodes = 1
		}
	} else {
		c.peerMap.forEachEndpointWithDiscoKey(di.discoKey, func(ep *endpoint) (keepGoing bool) {
			if ep.addCandidateEndpoint(src, dm.TxID) {
				dup = true
				return false
			}
			numNodes++
			if numNodes == 1 && dstKey.IsZero() {
				dstKey = ep.publicKey
			}
			return true
		})
		if dup {
			return
		}
		if numNodes > 1 {
			// Zero it out if it's ambiguous, so sendDiscoMessage logging
			// isn't confusing.
			dstKey = key.NodePublic{}
		}
	}

	if numNodes == 0 {
		c.logf("[unexpected] got disco ping from %v/%v for node not in peers", src, derpNodeSrc)
		return
	}

	if !likelyHeartBeat || debugDisco() {
		pingNodeSrcStr := dstKey.ShortString()
		if numNodes > 1 {
			pingNodeSrcStr = "[one-of-multi]"
		}
		c.dlogf("[v1] magicsock: disco: %v<-%v (%v, %v)  got ping tx=%x", c.discoShort, di.discoShort, pingNodeSrcStr, src, dm.TxID[:6])
	}

	ipDst := src
	discoDest := di.discoKey
	go c.sendDiscoMessage(ipDst, dstKey, discoDest, &disco.Pong{
		TxID: dm.TxID,
		Src:  src,
	}, discoVerboseLog)
}

// enqueueCallMeMaybe schedules a send of disco.CallMeMaybe to de via derpAddr
// once we know that our STUN endpoint is fresh.
//
// derpAddr is de.derpAddr at the time of send. It's assumed the peer won't be
// flipping primary DERPs in the 0-30ms it takes to confirm our STUN endpoint.
// If they do, traffic will just go over DERP for a bit longer until the next
// discovery round.
func (c *Conn) enqueueCallMeMaybe(derpAddr netip.AddrPort, de *endpoint) {
	c.mu.Lock()
	defer c.mu.Unlock()

	epDisco := de.disco.Load()
	if epDisco == nil {
		return
	}

	if !c.lastEndpointsTime.After(time.Now().Add(-endpointsFreshEnoughDuration)) {
		c.dlogf("[v1] magicsock: want call-me-maybe but endpoints stale; restunning")

		mak.Set(&c.onEndpointRefreshed, de, func() {
			c.dlogf("[v1] magicsock: STUN done; sending call-me-maybe to %v %v", epDisco.short, de.publicKey.ShortString())
			c.enqueueCallMeMaybe(derpAddr, de)
		})
		// TODO(bradfitz): make a new 'reSTUNQuickly' method
		// that passes down a do-a-lite-netcheck flag down to
		// netcheck that does 1 (or 2 max) STUN queries
		// (UDP-only, not HTTPs) to find our port mapping to
		// our home DERP and maybe one other. For now we do a
		// "full" ReSTUN which may or may not be a full one
		// (depending on age) and may do HTTPS timing queries
		// (if UDP is blocked). Good enough for now.
		go c.ReSTUN("refresh-for-peering")
		return
	}

	eps := make([]netip.AddrPort, 0, len(c.lastEndpoints))
	for _, ep := range c.lastEndpoints {
		eps = append(eps, ep.Addr)
	}
	go de.c.sendDiscoMessage(derpAddr, de.publicKey, epDisco.key, &disco.CallMeMaybe{MyNumber: eps}, discoLog)
	if debugSendCallMeUnknownPeer() {
		// Send a callMeMaybe packet to a non-existent peer
		unknownKey := key.NewNode().Public()
		c.logf("magicsock: sending CallMeMaybe to unknown peer per TS_DEBUG_SEND_CALLME_UNKNOWN_PEER")
		go de.c.sendDiscoMessage(derpAddr, unknownKey, epDisco.key, &disco.CallMeMaybe{MyNumber: eps}, discoLog)
	}
}

// discoInfoLocked returns the previous or new discoInfo for k.
//
// c.mu must be held.
func (c *Conn) discoInfoLocked(k key.DiscoPublic) *discoInfo {
	di, ok := c.discoInfo[k]
	if !ok {
		di = &discoInfo{
			discoKey:   k,
			discoShort: k.ShortString(),
			sharedKey:  c.discoPrivate.Shared(k),
		}
		c.discoInfo[k] = di
	}
	return di
}

func (c *Conn) SetNetworkUp(up bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.networkUp.Load() == up {
		return
	}

	c.logf("magicsock: SetNetworkUp(%v)", up)
	c.networkUp.Store(up)

	if up {
		c.startDerpHomeConnectLocked()
	} else {
		c.portMapper.NoteNetworkDown()
		c.closeAllDerpLocked("network-down")
	}
}

// SetPreferredPort sets the connection's preferred local port.
func (c *Conn) SetPreferredPort(port uint16) {
	if uint16(c.port.Load()) == port {
		return
	}
	c.port.Store(uint32(port))

	if err := c.rebind(dropCurrentPort); err != nil {
		c.logf("%w", err)
		return
	}
	c.resetEndpointStates()
}

// SetPrivateKey sets the connection's private key.
//
// This is only used to be able prove our identity when connecting to
// DERP servers.
//
// If the private key changes, any DERP connections are torn down &
// recreated when needed.
func (c *Conn) SetPrivateKey(privateKey key.NodePrivate) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	oldKey, newKey := c.privateKey, privateKey
	if newKey.Equal(oldKey) {
		return nil
	}
	c.privateKey = newKey
	c.havePrivateKey.Store(!newKey.IsZero())

	if newKey.IsZero() {
		c.publicKeyAtomic.Store(key.NodePublic{})
	} else {
		c.publicKeyAtomic.Store(newKey.Public())
	}

	if oldKey.IsZero() {
		c.everHadKey = true
		c.logf("magicsock: SetPrivateKey called (init)")
		go c.ReSTUN("set-private-key")
	} else if newKey.IsZero() {
		c.logf("magicsock: SetPrivateKey called (zeroed)")
		c.closeAllDerpLocked("zero-private-key")
		c.stopPeriodicReSTUNTimerLocked()
		c.onEndpointRefreshed = nil
	} else {
		c.logf("magicsock: SetPrivateKey called (changed)")
		c.closeAllDerpLocked("new-private-key")
	}

	// Key changed. Close existing DERP connections and reconnect to home.
	if c.myDerp != 0 && !newKey.IsZero() {
		c.logf("magicsock: private key changed, reconnecting to home derp-%d", c.myDerp)
		c.startDerpHomeConnectLocked()
	}

	if newKey.IsZero() {
		c.peerMap.forEachEndpoint(func(ep *endpoint) {
			ep.stopAndReset()
		})
	}

	return nil
}

// UpdatePeers is called when the set of WireGuard peers changes. It
// then removes any state for old peers.
//
// The caller passes ownership of newPeers map to UpdatePeers.
func (c *Conn) UpdatePeers(newPeers map[key.NodePublic]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	oldPeers := c.peerSet
	c.peerSet = newPeers

	// Clean up any key.NodePublic-keyed maps for peers that no longer
	// exist.
	for peer := range oldPeers {
		if _, ok := newPeers[peer]; !ok {
			delete(c.derpRoute, peer)
			delete(c.peerLastDerp, peer)
		}
	}

	if len(oldPeers) == 0 && len(newPeers) > 0 {
		go c.ReSTUN("non-zero-peers")
	}
}

// SetDERPMap controls which (if any) DERP servers are used.
// A nil value means to disable DERP; it's disabled by default.
func (c *Conn) SetDERPMap(dm *tailcfg.DERPMap) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var derpAddr = debugUseDERPAddr()
	if derpAddr != "" {
		derpPort := 443
		if debugUseDERPHTTP() {
			// Match the port for -dev in derper.go
			derpPort = 3340
		}
		dm = &tailcfg.DERPMap{
			OmitDefaultRegions: true,
			Regions: map[int]*tailcfg.DERPRegion{
				999: {
					RegionID: 999,
					Nodes: []*tailcfg.DERPNode{{
						Name:     "999dev",
						RegionID: 999,
						HostName: derpAddr,
						DERPPort: derpPort,
					}},
				},
			},
		}
	}

	if reflect.DeepEqual(dm, c.derpMap) {
		return
	}

	c.derpMapAtomic.Store(dm)
	old := c.derpMap
	c.derpMap = dm
	if dm == nil {
		c.closeAllDerpLocked("derp-disabled")
		return
	}

	// Reconnect any DERP region that changed definitions.
	if old != nil {
		changes := false
		for rid, oldDef := range old.Regions {
			if reflect.DeepEqual(oldDef, dm.Regions[rid]) {
				continue
			}
			changes = true
			if rid == c.myDerp {
				c.myDerp = 0
			}
			c.closeDerpLocked(rid, "derp-region-redefined")
		}
		if changes {
			c.logActiveDerpLocked()
		}
	}

	go c.ReSTUN("derp-map-update")
}

func nodesEqual(x, y []*tailcfg.Node) bool {
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if !x[i].Equal(y[i]) {
			return false
		}
	}
	return true
}

var debugRingBufferMaxSizeBytes = envknob.RegisterInt("TS_DEBUG_MAGICSOCK_RING_BUFFER_MAX_SIZE_BYTES")

// SetNetworkMap is called when the control client gets a new network
// map from the control server. It must always be non-nil.
//
// It should not use the DERPMap field of NetworkMap; that's
// conditionally sent to SetDERPMap instead.
func (c *Conn) SetNetworkMap(nm *netmap.NetworkMap) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return
	}

	priorNetmap := c.netMap
	var priorDebug *tailcfg.Debug
	if priorNetmap != nil {
		priorDebug = priorNetmap.Debug
	}
	debugChanged := !reflect.DeepEqual(priorDebug, nm.Debug)
	metricNumPeers.Set(int64(len(nm.Peers)))

	// Update c.netMap regardless, before the following early return.
	c.netMap = nm

	if priorNetmap != nil && nodesEqual(priorNetmap.Peers, nm.Peers) && !debugChanged {
		// The rest of this function is all adjusting state for peers that have
		// changed. But if the set of peers is equal and the debug flags (for
		// silent disco) haven't changed, no need to do anything else.
		return
	}

	c.logf("[v1] magicsock: got updated network map; %d peers", len(nm.Peers))
	heartbeatDisabled := debugEnableSilentDisco() || (c.netMap != nil && c.netMap.Debug != nil && c.netMap.Debug.EnableSilentDisco)

	// Set a maximum size for our set of endpoint ring buffers by assuming
	// that a single large update is ~500 bytes, and that we want to not
	// use more than 1MiB of memory on phones / 4MiB on other devices.
	// Calculate the per-endpoint ring buffer size by dividing that out,
	// but always storing at least two entries.
	var entriesPerBuffer int = 2
	if len(nm.Peers) > 0 {
		var maxRingBufferSize int
		if runtime.GOOS == "ios" || runtime.GOOS == "android" {
			maxRingBufferSize = 1 * 1024 * 1024
		} else {
			maxRingBufferSize = 4 * 1024 * 1024
		}
		if v := debugRingBufferMaxSizeBytes(); v > 0 {
			maxRingBufferSize = v
		}

		const averageRingBufferElemSize = 512
		entriesPerBuffer = maxRingBufferSize / (averageRingBufferElemSize * len(nm.Peers))
		if entriesPerBuffer < 2 {
			entriesPerBuffer = 2
		}
	}

	// Try a pass of just upserting nodes and creating missing
	// endpoints. If the set of nodes is the same, this is an
	// efficient alloc-free update. If the set of nodes is different,
	// we'll fall through to the next pass, which allocates but can
	// handle full set updates.
	for _, n := range nm.Peers {
		if ep, ok := c.peerMap.endpointForNodeKey(n.Key); ok {
			if n.DiscoKey.IsZero() && !n.IsWireGuardOnly {
				// Discokey transitioned from non-zero to zero? This should not
				// happen in the wild, however it could mean:
				// 1. A node was downgraded from post 0.100 to pre 0.100.
				// 2. A Tailscale node key was extracted and used on a
				//    non-Tailscale node (should not enter here due to the
				//    IsWireGuardOnly check)
				// 3. The server is misbehaving.
				c.peerMap.deleteEndpoint(ep)
				continue
			}
			var oldDiscoKey key.DiscoPublic
			if epDisco := ep.disco.Load(); epDisco != nil {
				oldDiscoKey = epDisco.key
			}
			ep.updateFromNode(n, heartbeatDisabled)
			c.peerMap.upsertEndpoint(ep, oldDiscoKey) // maybe update discokey mappings in peerMap
			continue
		}
		if n.DiscoKey.IsZero() && !n.IsWireGuardOnly {
			// Ancient pre-0.100 node, which does not have a disco key, and will only be reachable via DERP.
			continue
		}

		ep := &endpoint{
			c:                 c,
			debugUpdates:      ringbuffer.New[EndpointChange](entriesPerBuffer),
			publicKey:         n.Key,
			publicKeyHex:      n.Key.UntypedHexString(),
			sentPing:          map[stun.TxID]sentPing{},
			endpointState:     map[netip.AddrPort]*endpointState{},
			heartbeatDisabled: heartbeatDisabled,
			isWireguardOnly:   n.IsWireGuardOnly,
		}
		if len(n.Addresses) > 0 {
			ep.nodeAddr = n.Addresses[0].Addr()
		}
		ep.initFakeUDPAddr()
		if n.DiscoKey.IsZero() {
			ep.disco.Store(nil)
		} else {
			ep.disco.Store(&endpointDisco{
				key:   n.DiscoKey,
				short: n.DiscoKey.ShortString(),
			})

			if debugDisco() { // rather than making a new knob
				c.logf("magicsock: created endpoint key=%s: disco=%s; %v", n.Key.ShortString(), n.DiscoKey.ShortString(), logger.ArgWriter(func(w *bufio.Writer) {
					const derpPrefix = "127.3.3.40:"
					if strings.HasPrefix(n.DERP, derpPrefix) {
						ipp, _ := netip.ParseAddrPort(n.DERP)
						regionID := int(ipp.Port())
						code := c.derpRegionCodeLocked(regionID)
						if code != "" {
							code = "(" + code + ")"
						}
						fmt.Fprintf(w, "derp=%v%s ", regionID, code)
					}

					for _, a := range n.AllowedIPs {
						if a.IsSingleIP() {
							fmt.Fprintf(w, "aip=%v ", a.Addr())
						} else {
							fmt.Fprintf(w, "aip=%v ", a)
						}
					}
					for _, ep := range n.Endpoints {
						fmt.Fprintf(w, "ep=%v ", ep)
					}
				}))
			}
		}
		ep.updateFromNode(n, heartbeatDisabled)
		c.peerMap.upsertEndpoint(ep, key.DiscoPublic{})
	}

	// If the set of nodes changed since the last SetNetworkMap, the
	// upsert loop just above made c.peerMap contain the union of the
	// old and new peers - which will be larger than the set from the
	// current netmap. If that happens, go through the allocful
	// deletion path to clean up moribund nodes.
	if c.peerMap.nodeCount() != len(nm.Peers) {
		keep := make(map[key.NodePublic]bool, len(nm.Peers))
		for _, n := range nm.Peers {
			keep[n.Key] = true
		}
		c.peerMap.forEachEndpoint(func(ep *endpoint) {
			if !keep[ep.publicKey] {
				c.peerMap.deleteEndpoint(ep)
			}
		})
	}

	// discokeys might have changed in the above. Discard unused info.
	for dk := range c.discoInfo {
		if !c.peerMap.anyEndpointForDiscoKey(dk) {
			delete(c.discoInfo, dk)
		}
	}
}

func (c *Conn) wantDerpLocked() bool { return c.derpMap != nil }

// c.mu must be held.
func (c *Conn) closeAllDerpLocked(why string) {
	if len(c.activeDerp) == 0 {
		return // without the useless log statement
	}
	for i := range c.activeDerp {
		c.closeDerpLocked(i, why)
	}
	c.logActiveDerpLocked()
}

// maybeCloseDERPsOnRebind, in response to a rebind, closes all
// DERP connections that don't have a local address in okayLocalIPs
// and pings all those that do.
func (c *Conn) maybeCloseDERPsOnRebind(okayLocalIPs []netip.Prefix) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for regionID, ad := range c.activeDerp {
		la, err := ad.c.LocalAddr()
		if err != nil {
			c.closeOrReconnectDERPLocked(regionID, "rebind-no-localaddr")
			continue
		}
		if !tsaddr.PrefixesContainsIP(okayLocalIPs, la.Addr()) {
			c.closeOrReconnectDERPLocked(regionID, "rebind-default-route-change")
			continue
		}
		regionID := regionID
		dc := ad.c
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := dc.Ping(ctx); err != nil {
				c.mu.Lock()
				defer c.mu.Unlock()
				c.closeOrReconnectDERPLocked(regionID, "rebind-ping-fail")
				return
			}
			c.logf("post-rebind ping of DERP region %d okay", regionID)
		}()
	}
	c.logActiveDerpLocked()
}

// closeOrReconnectDERPLocked closes the DERP connection to the
// provided regionID and starts reconnecting it if it's our current
// home DERP.
//
// why is a reason for logging.
//
// c.mu must be held.
func (c *Conn) closeOrReconnectDERPLocked(regionID int, why string) {
	c.closeDerpLocked(regionID, why)
	if !c.privateKey.IsZero() && c.myDerp == regionID {
		c.startDerpHomeConnectLocked()
	}
}

// c.mu must be held.
// It is the responsibility of the caller to call logActiveDerpLocked after any set of closes.
func (c *Conn) closeDerpLocked(regionID int, why string) {
	if ad, ok := c.activeDerp[regionID]; ok {
		c.logf("magicsock: closing connection to derp-%v (%v), age %v", regionID, why, time.Since(ad.createTime).Round(time.Second))
		go ad.c.Close()
		ad.cancel()
		delete(c.activeDerp, regionID)
		metricNumDERPConns.Set(int64(len(c.activeDerp)))
	}
}

// c.mu must be held.
func (c *Conn) logActiveDerpLocked() {
	now := time.Now()
	c.logf("magicsock: %v active derp conns%s", len(c.activeDerp), logger.ArgWriter(func(buf *bufio.Writer) {
		if len(c.activeDerp) == 0 {
			return
		}
		buf.WriteString(":")
		c.foreachActiveDerpSortedLocked(func(node int, ad activeDerp) {
			fmt.Fprintf(buf, " derp-%d=cr%v,wr%v", node, simpleDur(now.Sub(ad.createTime)), simpleDur(now.Sub(*ad.lastWrite)))
		})
	}))
}

// EndpointChange is a structure containing information about changes made to a
// particular endpoint. This is not a stable interface and could change at any
// time.
type EndpointChange struct {
	When time.Time // when the change occurred
	What string    // what this change is
	From any       `json:",omitempty"` // information about the previous state
	To   any       `json:",omitempty"` // information about the new state
}

func (c *Conn) logEndpointChange(endpoints []tailcfg.Endpoint) {
	c.logf("magicsock: endpoints changed: %s", logger.ArgWriter(func(buf *bufio.Writer) {
		for i, ep := range endpoints {
			if i > 0 {
				buf.WriteString(", ")
			}
			fmt.Fprintf(buf, "%s (%s)", ep.Addr, ep.Type)
		}
	}))
}

// c.mu must be held.
func (c *Conn) foreachActiveDerpSortedLocked(fn func(regionID int, ad activeDerp)) {
	if len(c.activeDerp) < 2 {
		for id, ad := range c.activeDerp {
			fn(id, ad)
		}
		return
	}
	ids := make([]int, 0, len(c.activeDerp))
	for id := range c.activeDerp {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		fn(id, c.activeDerp[id])
	}
}

func (c *Conn) cleanStaleDerp() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.derpCleanupTimerArmed = false

	tooOld := time.Now().Add(-derpInactiveCleanupTime)
	dirty := false
	someNonHomeOpen := false
	for i, ad := range c.activeDerp {
		if i == c.myDerp {
			continue
		}
		if ad.lastWrite.Before(tooOld) {
			c.closeDerpLocked(i, "idle")
			dirty = true
		} else {
			someNonHomeOpen = true
		}
	}
	if dirty {
		c.logActiveDerpLocked()
	}
	if someNonHomeOpen {
		c.scheduleCleanStaleDerpLocked()
	}
}

func (c *Conn) scheduleCleanStaleDerpLocked() {
	if c.derpCleanupTimerArmed {
		// Already going to fire soon. Let the existing one
		// fire lest it get infinitely delayed by repeated
		// calls to scheduleCleanStaleDerpLocked.
		return
	}
	c.derpCleanupTimerArmed = true
	if c.derpCleanupTimer != nil {
		c.derpCleanupTimer.Reset(derpCleanStaleInterval)
	} else {
		c.derpCleanupTimer = time.AfterFunc(derpCleanStaleInterval, c.cleanStaleDerp)
	}
}

// DERPs reports the number of active DERP connections.
func (c *Conn) DERPs() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.activeDerp)
}

// Bind returns the wireguard-go conn.Bind for c.
//
// See https://pkg.go.dev/golang.zx2c4.com/wireguard/conn#Bind
func (c *Conn) Bind() conn.Bind {
	return c.bind
}

// connBind is a wireguard-go conn.Bind for a Conn.
// It bridges the behavior of wireguard-go and a Conn.
// wireguard-go calls Close then Open on device.Up.
// That won't work well for a Conn, which is only closed on shutdown.
// The subsequent Close is a real close.
type connBind struct {
	*Conn
	mu     sync.Mutex
	closed bool
}

var _ conn.Bind = (*connBind)(nil)

// BatchSize returns the number of buffers expected to be passed to
// the ReceiveFuncs, and the maximum expected to be passed to SendBatch.
//
// See https://pkg.go.dev/golang.zx2c4.com/wireguard/conn#Bind.BatchSize
func (c *connBind) BatchSize() int {
	// TODO(raggi): determine by properties rather than hardcoding platform behavior
	switch runtime.GOOS {
	case "linux":
		return conn.IdealBatchSize
	default:
		return 1
	}
}

// Open is called by WireGuard to create a UDP binding.
// The ignoredPort comes from wireguard-go, via the wgcfg config.
// We ignore that port value here, since we have the local port available easily.
//
// See https://pkg.go.dev/golang.zx2c4.com/wireguard/conn#Bind.Open
func (c *connBind) Open(ignoredPort uint16) ([]conn.ReceiveFunc, uint16, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		return nil, 0, errors.New("magicsock: connBind already open")
	}
	c.closed = false
	fns := []conn.ReceiveFunc{c.receiveIPv4(), c.receiveIPv6(), c.receiveDERP}
	if runtime.GOOS == "js" {
		fns = []conn.ReceiveFunc{c.receiveDERP}
	}
	// TODO: Combine receiveIPv4 and receiveIPv6 and receiveIP into a single
	// closure that closes over a *RebindingUDPConn?
	return fns, c.LocalPort(), nil
}

// SetMark is used by wireguard-go to set a mark bit for packets to avoid routing loops.
// We handle that ourselves elsewhere.
//
// See https://pkg.go.dev/golang.zx2c4.com/wireguard/conn#Bind.SetMark
func (c *connBind) SetMark(value uint32) error {
	return nil
}

// Close closes the connBind, unless it is already closed.
//
// See https://pkg.go.dev/golang.zx2c4.com/wireguard/conn#Bind.Close
func (c *connBind) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	// Unblock all outstanding receives.
	c.pconn4.Close()
	c.pconn6.Close()
	if c.closeDisco4 != nil {
		c.closeDisco4.Close()
	}
	if c.closeDisco6 != nil {
		c.closeDisco6.Close()
	}
	// Send an empty read result to unblock receiveDERP,
	// which will then check connBind.Closed.
	// connBind.Closed takes c.mu, but c.derpRecvCh is buffered.
	c.derpRecvCh <- derpReadResult{}
	return nil
}

// isClosed reports whether c is closed.
func (c *connBind) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Close closes the connection.
//
// Only the first close does anything. Any later closes return nil.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closing.Store(true)
	if c.derpCleanupTimerArmed {
		c.derpCleanupTimer.Stop()
	}
	c.stopPeriodicReSTUNTimerLocked()
	c.portMapper.Close()

	c.peerMap.forEachEndpoint(func(ep *endpoint) {
		ep.stopAndReset()
	})

	c.closed = true
	c.connCtxCancel()
	c.closeAllDerpLocked("conn-close")
	// Ignore errors from c.pconnN.Close.
	// They will frequently have been closed already by a call to connBind.Close.
	c.pconn6.Close()
	c.pconn4.Close()

	// Wait on goroutines updating right at the end, once everything is
	// already closed. We want everything else in the Conn to be
	// consistently in the closed state before we release mu to wait
	// on the endpoint updater & derphttp.Connect.
	for c.goroutinesRunningLocked() {
		c.muCond.Wait()
	}

	if pinger := c.getPinger(); pinger != nil {
		pinger.Close()
	}

	return nil
}

func (c *Conn) goroutinesRunningLocked() bool {
	if c.endpointsUpdateActive {
		return true
	}
	// The goroutine running dc.Connect in derpWriteChanOfAddr may linger
	// and appear to leak, as observed in https://github.com/tailscale/tailscale/issues/554.
	// This is despite the underlying context being cancelled by connCtxCancel above.
	// To avoid this condition, we must wait on derpStarted here
	// to ensure that this goroutine has exited by the time Close returns.
	// We only do this if derpWriteChanOfAddr has executed at least once:
	// on the first run, it sets firstDerp := true and spawns the aforementioned goroutine.
	// To detect this, we check activeDerp, which is initialized to non-nil on the first run.
	if c.activeDerp != nil {
		select {
		case <-c.derpStarted:
		default:
			return true
		}
	}
	return false
}

func maxIdleBeforeSTUNShutdown() time.Duration {
	if debugReSTUNStopOnIdle() {
		return 45 * time.Second
	}
	return sessionActiveTimeout
}

func (c *Conn) shouldDoPeriodicReSTUNLocked() bool {
	if c.networkDown() {
		return false
	}
	if len(c.peerSet) == 0 || c.privateKey.IsZero() {
		// If no peers, not worth doing.
		// Also don't if there's no key (not running).
		return false
	}
	if f := c.idleFunc; f != nil {
		idleFor := f()
		if debugReSTUNStopOnIdle() {
			c.logf("magicsock: periodicReSTUN: idle for %v", idleFor.Round(time.Second))
		}
		if idleFor > maxIdleBeforeSTUNShutdown() {
			if c.netMap != nil && c.netMap.Debug != nil && c.netMap.Debug.ForceBackgroundSTUN {
				// Overridden by control.
				return true
			}
			return false
		}
	}
	return true
}

func (c *Conn) onPortMapChanged() { c.ReSTUN("portmap-changed") }

// ReSTUN triggers an address discovery.
// The provided why string is for debug logging only.
func (c *Conn) ReSTUN(why string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		// raced with a shutdown.
		return
	}
	metricReSTUNCalls.Add(1)

	// If the user stopped the app, stop doing work. (When the
	// user stops Tailscale via the GUI apps, ipn/local.go
	// reconfigures the engine with a zero private key.)
	//
	// This used to just check c.privateKey.IsZero, but that broke
	// some end-to-end tests that didn't ever set a private
	// key somehow. So for now, only stop doing work if we ever
	// had a key, which helps real users, but appeases tests for
	// now. TODO: rewrite those tests to be less brittle or more
	// realistic.
	if c.privateKey.IsZero() && c.everHadKey {
		c.logf("magicsock: ReSTUN(%q) ignored; stopped, no private key", why)
		return
	}

	if c.endpointsUpdateActive {
		if c.wantEndpointsUpdate != why {
			c.dlogf("[v1] magicsock: ReSTUN: endpoint update active, need another later (%q)", why)
			c.wantEndpointsUpdate = why
		}
	} else {
		c.endpointsUpdateActive = true
		go c.updateEndpoints(why)
	}
}

// listenPacket opens a packet listener.
// The network must be "udp4" or "udp6".
func (c *Conn) listenPacket(network string, port uint16) (nettype.PacketConn, error) {
	ctx := context.Background() // unused without DNS name to resolve
	if network == "udp4" {
		ctx = sockstats.WithSockStats(ctx, sockstats.LabelMagicsockConnUDP4, c.logf)
	} else {
		ctx = sockstats.WithSockStats(ctx, sockstats.LabelMagicsockConnUDP6, c.logf)
	}
	addr := net.JoinHostPort("", fmt.Sprint(port))
	if c.testOnlyPacketListener != nil {
		return nettype.MakePacketListenerWithNetIP(c.testOnlyPacketListener).ListenPacket(ctx, network, addr)
	}
	return nettype.MakePacketListenerWithNetIP(netns.Listener(c.logf, c.netMon)).ListenPacket(ctx, network, addr)
}

var debugBindSocket = envknob.RegisterBool("TS_DEBUG_MAGICSOCK_BIND_SOCKET")

// bindSocket initializes rucPtr if necessary and binds a UDP socket to it.
// Network indicates the UDP socket type; it must be "udp4" or "udp6".
// If rucPtr had an existing UDP socket bound, it closes that socket.
// The caller is responsible for informing the portMapper of any changes.
// If curPortFate is set to dropCurrentPort, no attempt is made to reuse
// the current port.
func (c *Conn) bindSocket(ruc *RebindingUDPConn, network string, curPortFate currentPortFate) error {
	if debugBindSocket() {
		c.logf("magicsock: bindSocket: network=%q curPortFate=%v", network, curPortFate)
	}

	// Hold the ruc lock the entire time, so that the close+bind is atomic
	// from the perspective of ruc receive functions.
	ruc.mu.Lock()
	defer ruc.mu.Unlock()

	if runtime.GOOS == "js" {
		ruc.setConnLocked(newBlockForeverConn(), "", c.bind.BatchSize())
		return nil
	}

	if debugAlwaysDERP() {
		c.logf("disabled %v per TS_DEBUG_ALWAYS_USE_DERP", network)
		ruc.setConnLocked(newBlockForeverConn(), "", c.bind.BatchSize())
		return nil
	}

	// Build a list of preferred ports.
	// Best is the port that the user requested.
	// Second best is the port that is currently in use.
	// If those fail, fall back to 0.
	var ports []uint16
	if port := uint16(c.port.Load()); port != 0 {
		ports = append(ports, port)
	}
	if ruc.pconn != nil && curPortFate == keepCurrentPort {
		curPort := uint16(ruc.localAddrLocked().Port)
		ports = append(ports, curPort)
	}
	ports = append(ports, 0)
	// Remove duplicates. (All duplicates are consecutive.)
	uniq.ModifySlice(&ports)

	if debugBindSocket() {
		c.logf("magicsock: bindSocket: candidate ports: %+v", ports)
	}

	var pconn nettype.PacketConn
	for _, port := range ports {
		// Close the existing conn, in case it is sitting on the port we want.
		err := ruc.closeLocked()
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, errNilPConn) {
			c.logf("magicsock: bindSocket %v close failed: %v", network, err)
		}
		// Open a new one with the desired port.
		pconn, err = c.listenPacket(network, port)
		if err != nil {
			c.logf("magicsock: unable to bind %v port %d: %v", network, port, err)
			continue
		}
		trySetSocketBuffer(pconn, c.logf)
		// Success.
		if debugBindSocket() {
			c.logf("magicsock: bindSocket: successfully listened %v port %d", network, port)
		}
		ruc.setConnLocked(pconn, network, c.bind.BatchSize())
		if network == "udp4" {
			health.SetUDP4Unbound(false)
		}
		return nil
	}

	// Failed to bind, including on port 0 (!).
	// Set pconn to a dummy conn whose reads block until closed.
	// This keeps the receive funcs alive for a future in which
	// we get a link change and we can try binding again.
	ruc.setConnLocked(newBlockForeverConn(), "", c.bind.BatchSize())
	if network == "udp4" {
		health.SetUDP4Unbound(true)
	}
	return fmt.Errorf("failed to bind any ports (tried %v)", ports)
}

type currentPortFate uint8

const (
	keepCurrentPort = currentPortFate(0)
	dropCurrentPort = currentPortFate(1)
)

// rebind closes and re-binds the UDP sockets.
// We consider it successful if we manage to bind the IPv4 socket.
func (c *Conn) rebind(curPortFate currentPortFate) error {
	if err := c.bindSocket(&c.pconn6, "udp6", curPortFate); err != nil {
		c.logf("magicsock: Rebind ignoring IPv6 bind failure: %v", err)
	}
	if err := c.bindSocket(&c.pconn4, "udp4", curPortFate); err != nil {
		return fmt.Errorf("magicsock: Rebind IPv4 failed: %w", err)
	}
	c.portMapper.SetLocalPort(c.LocalPort())
	return nil
}

// Rebind closes and re-binds the UDP sockets and resets the DERP connection.
// It should be followed by a call to ReSTUN.
func (c *Conn) Rebind() {
	metricRebindCalls.Add(1)
	if err := c.rebind(keepCurrentPort); err != nil {
		c.logf("%w", err)
		return
	}

	var ifIPs []netip.Prefix
	if c.netMon != nil {
		st := c.netMon.InterfaceState()
		defIf := st.DefaultRouteInterface
		ifIPs = st.InterfaceIPs[defIf]
		c.logf("Rebind; defIf=%q, ips=%v", defIf, ifIPs)
	}

	c.maybeCloseDERPsOnRebind(ifIPs)
	c.resetEndpointStates()
}

// resetEndpointStates resets the preferred address for all peers.
// This is called when connectivity changes enough that we no longer
// trust the old routes.
func (c *Conn) resetEndpointStates() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.peerMap.forEachEndpoint(func(ep *endpoint) {
		ep.noteConnectivityChange()
	})
}

// packIPPort packs an IPPort into the form wanted by WireGuard.
func packIPPort(ua netip.AddrPort) []byte {
	ip := ua.Addr().Unmap()
	a := ip.As16()
	ipb := a[:]
	if ip.Is4() {
		ipb = ipb[12:]
	}
	b := make([]byte, 0, len(ipb)+2)
	b = append(b, ipb...)
	b = append(b, byte(ua.Port()))
	b = append(b, byte(ua.Port()>>8))
	return b
}

// ParseEndpoint implements conn.Bind; it's called by WireGuard to connect to an endpoint.
//
// See https://pkg.go.dev/golang.zx2c4.com/wireguard/conn#Bind.ParseEndpoint
func (c *Conn) ParseEndpoint(nodeKeyStr string) (conn.Endpoint, error) {
	k, err := key.ParseNodePublicUntyped(mem.S(nodeKeyStr))
	if err != nil {
		return nil, fmt.Errorf("magicsock: ParseEndpoint: parse failed on %q: %w", nodeKeyStr, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errConnClosed
	}
	ep, ok := c.peerMap.endpointForNodeKey(k)
	if !ok {
		// We should never be telling WireGuard about a new peer
		// before magicsock knows about it.
		c.logf("[unexpected] magicsock: ParseEndpoint: unknown node key=%s", k.ShortString())
		return nil, fmt.Errorf("magicsock: ParseEndpoint: unknown peer %q", k.ShortString())
	}

	return ep, nil
}

// xnetBatchReaderWriter defines the batching i/o methods of
// golang.org/x/net/ipv4.PacketConn (and ipv6.PacketConn).
// TODO(jwhited): This should eventually be replaced with the standard library
// implementation of https://github.com/golang/go/issues/45886
type xnetBatchReaderWriter interface {
	xnetBatchReader
	xnetBatchWriter
}

type xnetBatchReader interface {
	ReadBatch([]ipv6.Message, int) (int, error)
}

type xnetBatchWriter interface {
	WriteBatch([]ipv6.Message, int) (int, error)
}

// batchingUDPConn is a UDP socket that provides batched i/o.
type batchingUDPConn struct {
	pc                    nettype.PacketConn
	xpc                   xnetBatchReaderWriter
	rxOffload             bool                                  // supports UDP GRO or similar
	txOffload             atomic.Bool                           // supports UDP GSO or similar
	setGSOSizeInControl   func(control *[]byte, gsoSize uint16) // typically setGSOSizeInControl(); swappable for testing
	getGSOSizeFromControl func(control []byte) (int, error)     // typically getGSOSizeFromControl(); swappable for testing
	sendBatchPool         sync.Pool
}

func (c *batchingUDPConn) ReadFromUDPAddrPort(p []byte) (n int, addr netip.AddrPort, err error) {
	if c.rxOffload {
		// UDP_GRO is opt-in on Linux via setsockopt(). Once enabled you may
		// receive a "monster datagram" from any read call. The ReadFrom() API
		// does not support passing the GSO size and is unsafe to use in such a
		// case. Other platforms may vary in behavior, but we go with the most
		// conservative approach to prevent this from becoming a footgun in the
		// future.
		return 0, netip.AddrPort{}, errors.New("rx UDP offload is enabled on this socket, single packet reads are unavailable")
	}
	return c.pc.ReadFromUDPAddrPort(p)
}

func (c *batchingUDPConn) SetDeadline(t time.Time) error {
	return c.pc.SetDeadline(t)
}

func (c *batchingUDPConn) SetReadDeadline(t time.Time) error {
	return c.pc.SetReadDeadline(t)
}

func (c *batchingUDPConn) SetWriteDeadline(t time.Time) error {
	return c.pc.SetWriteDeadline(t)
}

const (
	// This was initially established for Linux, but may split out to
	// GOOS-specific values later. It originates as UDP_MAX_SEGMENTS in the
	// kernel's TX path, and UDP_GRO_CNT_MAX for RX.
	udpSegmentMaxDatagrams = 64
)

const (
	// Exceeding these values results in EMSGSIZE.
	maxIPv4PayloadLen = 1<<16 - 1 - 20 - 8
	maxIPv6PayloadLen = 1<<16 - 1 - 8
)

// coalesceMessages iterates msgs, coalescing them where possible while
// maintaining datagram order. All msgs have their Addr field set to addr.
func (c *batchingUDPConn) coalesceMessages(addr *net.UDPAddr, buffs [][]byte, msgs []ipv6.Message) int {
	var (
		base     = -1 // index of msg we are currently coalescing into
		gsoSize  int  // segmentation size of msgs[base]
		dgramCnt int  // number of dgrams coalesced into msgs[base]
		endBatch bool // tracking flag to start a new batch on next iteration of buffs
	)
	maxPayloadLen := maxIPv4PayloadLen
	if addr.IP.To4() == nil {
		maxPayloadLen = maxIPv6PayloadLen
	}
	for i, buff := range buffs {
		if i > 0 {
			msgLen := len(buff)
			baseLenBefore := len(msgs[base].Buffers[0])
			freeBaseCap := cap(msgs[base].Buffers[0]) - baseLenBefore
			if msgLen+baseLenBefore <= maxPayloadLen &&
				msgLen <= gsoSize &&
				msgLen <= freeBaseCap &&
				dgramCnt < udpSegmentMaxDatagrams &&
				!endBatch {
				msgs[base].Buffers[0] = append(msgs[base].Buffers[0], make([]byte, msgLen)...)
				copy(msgs[base].Buffers[0][baseLenBefore:], buff)
				if i == len(buffs)-1 {
					c.setGSOSizeInControl(&msgs[base].OOB, uint16(gsoSize))
				}
				dgramCnt++
				if msgLen < gsoSize {
					// A smaller than gsoSize packet on the tail is legal, but
					// it must end the batch.
					endBatch = true
				}
				continue
			}
		}
		if dgramCnt > 1 {
			c.setGSOSizeInControl(&msgs[base].OOB, uint16(gsoSize))
		}
		// Reset prior to incrementing base since we are preparing to start a
		// new potential batch.
		endBatch = false
		base++
		gsoSize = len(buff)
		msgs[base].OOB = msgs[base].OOB[:0]
		msgs[base].Buffers[0] = buff
		msgs[base].Addr = addr
		dgramCnt = 1
	}
	return base + 1
}

type sendBatch struct {
	msgs []ipv6.Message
	ua   *net.UDPAddr
}

func (c *batchingUDPConn) getSendBatch() *sendBatch {
	batch := c.sendBatchPool.Get().(*sendBatch)
	return batch
}

func (c *batchingUDPConn) putSendBatch(batch *sendBatch) {
	for i := range batch.msgs {
		batch.msgs[i] = ipv6.Message{Buffers: batch.msgs[i].Buffers, OOB: batch.msgs[i].OOB}
	}
	c.sendBatchPool.Put(batch)
}

func (c *batchingUDPConn) WriteBatchTo(buffs [][]byte, addr netip.AddrPort) error {
	batch := c.getSendBatch()
	defer c.putSendBatch(batch)
	if addr.Addr().Is6() {
		as16 := addr.Addr().As16()
		copy(batch.ua.IP, as16[:])
		batch.ua.IP = batch.ua.IP[:16]
	} else {
		as4 := addr.Addr().As4()
		copy(batch.ua.IP, as4[:])
		batch.ua.IP = batch.ua.IP[:4]
	}
	batch.ua.Port = int(addr.Port())
	var (
		n       int
		retried bool
	)
retry:
	if c.txOffload.Load() {
		n = c.coalesceMessages(batch.ua, buffs, batch.msgs)
	} else {
		for i := range buffs {
			batch.msgs[i].Buffers[0] = buffs[i]
			batch.msgs[i].Addr = batch.ua
			batch.msgs[i].OOB = batch.msgs[i].OOB[:0]
		}
		n = len(buffs)
	}

	err := c.writeBatch(batch.msgs[:n])
	if err != nil && c.txOffload.Load() && neterror.ShouldDisableUDPGSO(err) {
		c.txOffload.Store(false)
		retried = true
		goto retry
	}
	if retried {
		return neterror.ErrUDPGSODisabled{OnLaddr: c.pc.LocalAddr().String(), RetryErr: err}
	}
	return err
}

func (c *batchingUDPConn) writeBatch(msgs []ipv6.Message) error {
	var head int
	for {
		n, err := c.xpc.WriteBatch(msgs[head:], 0)
		if err != nil || n == len(msgs[head:]) {
			// Returning the number of packets written would require
			// unraveling individual msg len and gso size during a coalesced
			// write. The top of the call stack disregards partial success,
			// so keep this simple for now.
			return err
		}
		head += n
	}
}

// splitCoalescedMessages splits coalesced messages from the tail of dst
// beginning at index 'firstMsgAt' into the head of the same slice. It reports
// the number of elements to evaluate in msgs for nonzero len (msgs[i].N). An
// error is returned if a socket control message cannot be parsed or a split
// operation would overflow msgs.
func (c *batchingUDPConn) splitCoalescedMessages(msgs []ipv6.Message, firstMsgAt int) (n int, err error) {
	for i := firstMsgAt; i < len(msgs); i++ {
		msg := &msgs[i]
		if msg.N == 0 {
			return n, err
		}
		var (
			gsoSize    int
			start      int
			end        = msg.N
			numToSplit = 1
		)
		gsoSize, err = c.getGSOSizeFromControl(msg.OOB[:msg.NN])
		if err != nil {
			return n, err
		}
		if gsoSize > 0 {
			numToSplit = (msg.N + gsoSize - 1) / gsoSize
			end = gsoSize
		}
		for j := 0; j < numToSplit; j++ {
			if n > i {
				return n, errors.New("splitting coalesced packet resulted in overflow")
			}
			copied := copy(msgs[n].Buffers[0], msg.Buffers[0][start:end])
			msgs[n].N = copied
			msgs[n].Addr = msg.Addr
			start = end
			end += gsoSize
			if end > msg.N {
				end = msg.N
			}
			n++
		}
		if i != n-1 {
			// It is legal for bytes to move within msg.Buffers[0] as a result
			// of splitting, so we only zero the source msg len when it is not
			// the destination of the last split operation above.
			msg.N = 0
		}
	}
	return n, nil
}

func (c *batchingUDPConn) ReadBatch(msgs []ipv6.Message, flags int) (n int, err error) {
	if !c.rxOffload || len(msgs) < 2 {
		return c.xpc.ReadBatch(msgs, flags)
	}
	// Read into the tail of msgs, split into the head.
	readAt := len(msgs) - 2
	numRead, err := c.xpc.ReadBatch(msgs[readAt:], 0)
	if err != nil || numRead == 0 {
		return 0, err
	}
	return c.splitCoalescedMessages(msgs, readAt)
}

func (c *batchingUDPConn) LocalAddr() net.Addr {
	return c.pc.LocalAddr().(*net.UDPAddr)
}

func (c *batchingUDPConn) WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error) {
	return c.pc.WriteToUDPAddrPort(b, addr)
}

func (c *batchingUDPConn) Close() error {
	return c.pc.Close()
}

// tryUpgradeToBatchingUDPConn probes the capabilities of the OS and pconn, and
// upgrades pconn to a *batchingUDPConn if appropriate.
func tryUpgradeToBatchingUDPConn(pconn nettype.PacketConn, network string, batchSize int) nettype.PacketConn {
	if network != "udp4" && network != "udp6" {
		return pconn
	}
	if runtime.GOOS != "linux" {
		return pconn
	}
	if strings.HasPrefix(hostinfo.GetOSVersion(), "2.") {
		// recvmmsg/sendmmsg were added in 2.6.33, but we support down to
		// 2.6.32 for old NAS devices. See https://github.com/tailscale/tailscale/issues/6807.
		// As a cheap heuristic: if the Linux kernel starts with "2", just
		// consider it too old for mmsg. Nobody who cares about performance runs
		// such ancient kernels. UDP offload was added much later, so no
		// upgrades are available.
		return pconn
	}
	uc, ok := pconn.(*net.UDPConn)
	if !ok {
		return pconn
	}
	b := &batchingUDPConn{
		pc:                    pconn,
		getGSOSizeFromControl: getGSOSizeFromControl,
		setGSOSizeInControl:   setGSOSizeInControl,
		sendBatchPool: sync.Pool{
			New: func() any {
				ua := &net.UDPAddr{
					IP: make([]byte, 16),
				}
				msgs := make([]ipv6.Message, batchSize)
				for i := range msgs {
					msgs[i].Buffers = make([][]byte, 1)
					msgs[i].Addr = ua
					msgs[i].OOB = make([]byte, controlMessageSize)
				}
				return &sendBatch{
					ua:   ua,
					msgs: msgs,
				}
			},
		},
	}
	switch network {
	case "udp4":
		b.xpc = ipv4.NewPacketConn(uc)
	case "udp6":
		b.xpc = ipv6.NewPacketConn(uc)
	default:
		panic("bogus network")
	}
	var txOffload bool
	txOffload, b.rxOffload = tryEnableUDPOffload(uc)
	b.txOffload.Store(txOffload)
	return b
}

// RebindingUDPConn is a UDP socket that can be re-bound.
// Unix has no notion of re-binding a socket, so we swap it out for a new one.
type RebindingUDPConn struct {
	// pconnAtomic is a pointer to the value stored in pconn, but doesn't
	// require acquiring mu. It's used for reads/writes and only upon failure
	// do the reads/writes then check pconn (after acquiring mu) to see if
	// there's been a rebind meanwhile.
	// pconn isn't really needed, but makes some of the code simpler
	// to keep it distinct.
	// Neither is expected to be nil, sockets are bound on creation.
	pconnAtomic atomic.Pointer[nettype.PacketConn]

	mu    sync.Mutex // held while changing pconn (and pconnAtomic)
	pconn nettype.PacketConn
	port  uint16
}

// setConnLocked sets the provided nettype.PacketConn. It should be called only
// after acquiring RebindingUDPConn.mu. It upgrades the provided
// nettype.PacketConn to a *batchingUDPConn when appropriate. This upgrade
// is intentionally pushed closest to where read/write ops occur in order to
// avoid disrupting surrounding code that assumes nettype.PacketConn is a
// *net.UDPConn.
func (c *RebindingUDPConn) setConnLocked(p nettype.PacketConn, network string, batchSize int) {
	upc := tryUpgradeToBatchingUDPConn(p, network, batchSize)
	c.pconn = upc
	c.pconnAtomic.Store(&upc)
	c.port = uint16(c.localAddrLocked().Port)
}

// currentConn returns c's current pconn, acquiring c.mu in the process.
func (c *RebindingUDPConn) currentConn() nettype.PacketConn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pconn
}

func (c *RebindingUDPConn) readFromWithInitPconn(pconn nettype.PacketConn, b []byte) (int, netip.AddrPort, error) {
	for {
		n, addr, err := pconn.ReadFromUDPAddrPort(b)
		if err != nil && pconn != c.currentConn() {
			pconn = *c.pconnAtomic.Load()
			continue
		}
		return n, addr, err
	}
}

// ReadFromUDPAddrPort reads a packet from c into b.
// It returns the number of bytes copied and the source address.
func (c *RebindingUDPConn) ReadFromUDPAddrPort(b []byte) (int, netip.AddrPort, error) {
	return c.readFromWithInitPconn(*c.pconnAtomic.Load(), b)
}

// WriteBatchTo writes buffs to addr.
func (c *RebindingUDPConn) WriteBatchTo(buffs [][]byte, addr netip.AddrPort) error {
	for {
		pconn := *c.pconnAtomic.Load()
		b, ok := pconn.(*batchingUDPConn)
		if !ok {
			for _, buf := range buffs {
				_, err := c.writeToUDPAddrPortWithInitPconn(pconn, buf, addr)
				if err != nil {
					return err
				}
			}
			return nil
		}
		err := b.WriteBatchTo(buffs, addr)
		if err != nil {
			if pconn != c.currentConn() {
				continue
			}
			return err
		}
		return err
	}
}

// ReadBatch reads messages from c into msgs. It returns the number of messages
// the caller should evaluate for nonzero len, as a zero len message may fall
// on either side of a nonzero.
func (c *RebindingUDPConn) ReadBatch(msgs []ipv6.Message, flags int) (int, error) {
	for {
		pconn := *c.pconnAtomic.Load()
		b, ok := pconn.(*batchingUDPConn)
		if !ok {
			n, ap, err := c.readFromWithInitPconn(pconn, msgs[0].Buffers[0])
			if err == nil {
				msgs[0].N = n
				msgs[0].Addr = net.UDPAddrFromAddrPort(netaddr.Unmap(ap))
				return 1, nil
			}
			return 0, err
		}
		n, err := b.ReadBatch(msgs, flags)
		if err != nil && pconn != c.currentConn() {
			continue
		}
		return n, err
	}
}

func (c *RebindingUDPConn) Port() uint16 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.port
}

func (c *RebindingUDPConn) LocalAddr() *net.UDPAddr {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.localAddrLocked()
}

func (c *RebindingUDPConn) localAddrLocked() *net.UDPAddr {
	return c.pconn.LocalAddr().(*net.UDPAddr)
}

// errNilPConn is returned by RebindingUDPConn.Close when there is no current pconn.
// It is for internal use only and should not be returned to users.
var errNilPConn = errors.New("nil pconn")

func (c *RebindingUDPConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeLocked()
}

func (c *RebindingUDPConn) closeLocked() error {
	if c.pconn == nil {
		return errNilPConn
	}
	c.port = 0
	return c.pconn.Close()
}

func (c *RebindingUDPConn) writeToUDPAddrPortWithInitPconn(pconn nettype.PacketConn, b []byte, addr netip.AddrPort) (int, error) {
	for {
		n, err := pconn.WriteToUDPAddrPort(b, addr)
		if err != nil && pconn != c.currentConn() {
			pconn = *c.pconnAtomic.Load()
			continue
		}
		return n, err
	}
}

func (c *RebindingUDPConn) WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error) {
	return c.writeToUDPAddrPortWithInitPconn(*c.pconnAtomic.Load(), b, addr)
}

func newBlockForeverConn() *blockForeverConn {
	c := new(blockForeverConn)
	c.cond = sync.NewCond(&c.mu)
	return c
}

// blockForeverConn is a net.PacketConn whose reads block until it is closed.
type blockForeverConn struct {
	mu     sync.Mutex
	cond   *sync.Cond
	closed bool
}

func (c *blockForeverConn) ReadFromUDPAddrPort(p []byte) (n int, addr netip.AddrPort, err error) {
	c.mu.Lock()
	for !c.closed {
		c.cond.Wait()
	}
	c.mu.Unlock()
	return 0, netip.AddrPort{}, net.ErrClosed
}

func (c *blockForeverConn) WriteToUDPAddrPort(p []byte, addr netip.AddrPort) (int, error) {
	// Silently drop writes.
	return len(p), nil
}

func (c *blockForeverConn) LocalAddr() net.Addr {
	// Return a *net.UDPAddr because lots of code assumes that it will.
	return new(net.UDPAddr)
}

func (c *blockForeverConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	c.closed = true
	c.cond.Broadcast()
	return nil
}

func (c *blockForeverConn) SetDeadline(t time.Time) error      { return errors.New("unimplemented") }
func (c *blockForeverConn) SetReadDeadline(t time.Time) error  { return errors.New("unimplemented") }
func (c *blockForeverConn) SetWriteDeadline(t time.Time) error { return errors.New("unimplemented") }

// simpleDur rounds d such that it stringifies to something short.
func simpleDur(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(time.Millisecond)
	}
	if d < time.Minute {
		return d.Round(time.Second)
	}
	return d.Round(time.Minute)
}

func sbPrintAddr(sb *strings.Builder, a netip.AddrPort) {
	is6 := a.Addr().Is6()
	if is6 {
		sb.WriteByte('[')
	}
	fmt.Fprintf(sb, "%s", a.Addr())
	if is6 {
		sb.WriteByte(']')
	}
	fmt.Fprintf(sb, ":%d", a.Port())
}

func (c *Conn) derpRegionCodeOfAddrLocked(ipPort string) string {
	_, portStr, err := net.SplitHostPort(ipPort)
	if err != nil {
		return ""
	}
	regionID, err := strconv.Atoi(portStr)
	if err != nil {
		return ""
	}
	return c.derpRegionCodeOfIDLocked(regionID)
}

func (c *Conn) derpRegionCodeOfIDLocked(regionID int) string {
	if c.derpMap == nil {
		return ""
	}
	if r, ok := c.derpMap.Regions[regionID]; ok {
		return r.RegionCode
	}
	return ""
}

func (c *Conn) UpdateStatus(sb *ipnstate.StatusBuilder) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var tailscaleIPs []netip.Addr
	if c.netMap != nil {
		tailscaleIPs = make([]netip.Addr, 0, len(c.netMap.Addresses))
		for _, addr := range c.netMap.Addresses {
			if !addr.IsSingleIP() {
				continue
			}
			sb.AddTailscaleIP(addr.Addr())
			tailscaleIPs = append(tailscaleIPs, addr.Addr())
		}
	}

	sb.MutateSelfStatus(func(ss *ipnstate.PeerStatus) {
		if !c.privateKey.IsZero() {
			ss.PublicKey = c.privateKey.Public()
		} else {
			ss.PublicKey = key.NodePublic{}
		}
		ss.Addrs = make([]string, 0, len(c.lastEndpoints))
		for _, ep := range c.lastEndpoints {
			ss.Addrs = append(ss.Addrs, ep.Addr.String())
		}
		ss.OS = version.OS()
		if c.derpMap != nil {
			derpRegion, ok := c.derpMap.Regions[c.myDerp]
			if ok {
				ss.Relay = derpRegion.RegionCode
			}
		}
		ss.TailscaleIPs = tailscaleIPs
	})

	if sb.WantPeers {
		c.peerMap.forEachEndpoint(func(ep *endpoint) {
			ps := &ipnstate.PeerStatus{InMagicSock: true}
			//ps.Addrs = append(ps.Addrs, n.Endpoints...)
			ep.populatePeerStatus(ps)
			sb.AddPeer(ep.publicKey, ps)
		})
	}

	c.foreachActiveDerpSortedLocked(func(node int, ad activeDerp) {
		// TODO(bradfitz): add to ipnstate.StatusBuilder
		//f("<li><b>derp-%v</b>: cr%v,wr%v</li>", node, simpleDur(now.Sub(ad.createTime)), simpleDur(now.Sub(*ad.lastWrite)))
	})
}

// SetStatistics specifies a per-connection statistics aggregator.
// Nil may be specified to disable statistics gathering.
func (c *Conn) SetStatistics(stats *connstats.Statistics) {
	c.stats.Store(stats)
}

func ippDebugString(ua netip.AddrPort) string {
	if ua.Addr() == derpMagicIPAddr {
		return fmt.Sprintf("derp-%d", ua.Port())
	}
	return ua.String()
}

// endpointSendFunc is a func that writes encrypted Wireguard payloads from
// WireGuard to a peer. It might write via UDP, DERP, both, or neither.
//
// What these funcs should NOT do is too much work. Minimize use of mutexes, map
// lookups, etc. The idea is that selecting the path to use is done infrequently
// and mostly async from sending packets. When conditions change (including the
// passing of time and loss of confidence in certain routes), then a new send
// func gets set on an sendpoint.
//
// A nil value means the current fast path has expired and needs to be
// recalculated.
type endpointSendFunc func([][]byte) error

// endpointDisco is the current disco key and short string for an endpoint. This
// structure is immutable.
type endpointDisco struct {
	key   key.DiscoPublic // for discovery messages.
	short string          // ShortString of discoKey.
}

// endpoint is a wireguard/conn.Endpoint. In wireguard-go and kernel WireGuard
// there is only one endpoint for a peer, but in Tailscale we distribute a
// number of possible endpoints for a peer which would include the all the
// likely addresses at which a peer may be reachable. This endpoint type holds
// the information required that when WiregGuard-Go wants to send to a
// particular peer (essentally represented by this endpoint type), the send
// function can use the currnetly best known Tailscale endpoint to send packets
// to the peer.
type endpoint struct {
	// atomically accessed; declared first for alignment reasons
	lastRecv              mono.Time
	numStopAndResetAtomic int64
	sendFunc              syncs.AtomicValue[endpointSendFunc] // nil or unset means unused
	debugUpdates          *ringbuffer.RingBuffer[EndpointChange]

	// These fields are initialized once and never modified.
	c            *Conn
	publicKey    key.NodePublic // peer public key (for WireGuard + DERP)
	publicKeyHex string         // cached output of publicKey.UntypedHexString
	fakeWGAddr   netip.AddrPort // the UDP address we tell wireguard-go we're using
	nodeAddr     netip.Addr     // the node's first tailscale address; used for logging & wireguard rate-limiting (Issue 6686)

	disco atomic.Pointer[endpointDisco] // if the peer supports disco, the key and short string

	// mu protects all following fields.
	mu sync.Mutex // Lock ordering: Conn.mu, then endpoint.mu

	heartBeatTimer *time.Timer    // nil when idle
	lastSend       mono.Time      // last time there was outgoing packets sent to this peer (from wireguard-go)
	lastFullPing   mono.Time      // last time we pinged all disco endpoints
	derpAddr       netip.AddrPort // fallback/bootstrap path, if non-zero (non-zero for well-behaved clients)

	bestAddr           addrLatency // best non-DERP path; zero if none
	bestAddrAt         mono.Time   // time best address re-confirmed
	trustBestAddrUntil mono.Time   // time when bestAddr expires
	sentPing           map[stun.TxID]sentPing
	endpointState      map[netip.AddrPort]*endpointState
	isCallMeMaybeEP    map[netip.AddrPort]bool

	pendingCLIPings []pendingCLIPing // any outstanding "tailscale ping" commands running

	// The following fields are related to the new "silent disco"
	// implementation that's a WIP as of 2022-10-20.
	// See #540 for background.
	heartbeatDisabled bool
	pathFinderRunning bool

	expired         bool // whether the node has expired
	isWireguardOnly bool // whether the endpoint is WireGuard only
}

type pendingCLIPing struct {
	res *ipnstate.PingResult
	cb  func(*ipnstate.PingResult)
}

const (
	// sessionActiveTimeout is how long since the last activity we
	// try to keep an established endpoint peering alive.
	// It's also the idle time at which we stop doing STUN queries to
	// keep NAT mappings alive.
	sessionActiveTimeout = 45 * time.Second

	// upgradeInterval is how often we try to upgrade to a better path
	// even if we have some non-DERP route that works.
	upgradeInterval = 1 * time.Minute

	// heartbeatInterval is how often pings to the best UDP address
	// are sent.
	heartbeatInterval = 3 * time.Second

	// trustUDPAddrDuration is how long we trust a UDP address as the exclusive
	// path (without using DERP) without having heard a Pong reply.
	trustUDPAddrDuration = 6500 * time.Millisecond

	// goodEnoughLatency is the latency at or under which we don't
	// try to upgrade to a better path.
	goodEnoughLatency = 5 * time.Millisecond

	// derpInactiveCleanupTime is how long a non-home DERP connection
	// needs to be idle (last written to) before we close it.
	derpInactiveCleanupTime = 60 * time.Second

	// derpCleanStaleInterval is how often cleanStaleDerp runs when there
	// are potentially-stale DERP connections to close.
	derpCleanStaleInterval = 15 * time.Second

	// endpointsFreshEnoughDuration is how long we consider a
	// STUN-derived endpoint valid for. UDP NAT mappings typically
	// expire at 30 seconds, so this is a few seconds shy of that.
	endpointsFreshEnoughDuration = 27 * time.Second

	// endpointTrackerLifetime is how long we continue advertising an
	// endpoint after we last see it. This is intentionally chosen to be
	// slightly longer than a full netcheck period.
	endpointTrackerLifetime = 5*time.Minute + 10*time.Second
)

// Constants that are variable for testing.
var (
	// pingTimeoutDuration is how long we wait for a pong reply before
	// assuming it's never coming.
	pingTimeoutDuration = 5 * time.Second

	// discoPingInterval is the minimum time between pings
	// to an endpoint. (Except in the case of CallMeMaybe frames
	// resetting the counter, as the first pings likely didn't through
	// the firewall)
	discoPingInterval = 5 * time.Second
)

// endpointState is some state and history for a specific endpoint of
// a endpoint. (The subject is the endpoint.endpointState
// map key)
type endpointState struct {
	// all fields guarded by endpoint.mu

	// lastPing is the last (outgoing) ping time.
	lastPing mono.Time

	// lastGotPing, if non-zero, means that this was an endpoint
	// that we learned about at runtime (from an incoming ping)
	// and that is not in the network map. If so, we keep the time
	// updated and use it to discard old candidates.
	lastGotPing time.Time

	// lastGotPingTxID contains the TxID for the last incoming ping. This is
	// used to de-dup incoming pings that we may see on both the raw disco
	// socket on Linux, and UDP socket. We cannot rely solely on the raw socket
	// disco handling due to https://github.com/tailscale/tailscale/issues/7078.
	lastGotPingTxID stun.TxID

	// callMeMaybeTime, if non-zero, is the time this endpoint
	// was advertised last via a call-me-maybe disco message.
	callMeMaybeTime time.Time

	recentPongs []pongReply // ring buffer up to pongHistoryCount entries
	recentPong  uint16      // index into recentPongs of most recent; older before, wrapped

	index int16 // index in nodecfg.Node.Endpoints; meaningless if lastGotPing non-zero
}

// indexSentinelDeleted is the temporary value that endpointState.index takes while
// a endpoint's endpoints are being updated from a new network map.
const indexSentinelDeleted = -1

// shouldDeleteLocked reports whether we should delete this endpoint.
func (st *endpointState) shouldDeleteLocked() bool {
	switch {
	case !st.callMeMaybeTime.IsZero():
		return false
	case st.lastGotPing.IsZero():
		// This was an endpoint from the network map. Is it still in the network map?
		return st.index == indexSentinelDeleted
	default:
		// This was an endpoint discovered at runtime.
		return time.Since(st.lastGotPing) > sessionActiveTimeout
	}
}

// latencyLocked returns the most recent latency measurement, if any.
// endpoint.mu must be held.
func (st *endpointState) latencyLocked() (lat time.Duration, ok bool) {
	if len(st.recentPongs) == 0 {
		return 0, false
	}
	return st.recentPongs[st.recentPong].latency, true
}

func (de *endpoint) deleteEndpointLocked(why string, ep netip.AddrPort) {
	de.debugUpdates.Add(EndpointChange{
		When: time.Now(),
		What: "deleteEndpointLocked-" + why,
		From: ep,
	})
	delete(de.endpointState, ep)
	if de.bestAddr.AddrPort == ep {
		de.debugUpdates.Add(EndpointChange{
			When: time.Now(),
			What: "deleteEndpointLocked-bestAddr-" + why,
			From: de.bestAddr,
		})
		de.bestAddr = addrLatency{}
	}
}

// pongHistoryCount is how many pongReply values we keep per endpointState
const pongHistoryCount = 64

type pongReply struct {
	latency time.Duration
	pongAt  mono.Time      // when we received the pong
	from    netip.AddrPort // the pong's src (usually same as endpoint map key)
	pongSrc netip.AddrPort // what they reported they heard
}

type sentPing struct {
	to      netip.AddrPort
	at      mono.Time
	timer   *time.Timer // timeout timer
	purpose discoPingPurpose
}

// initFakeUDPAddr populates fakeWGAddr with a globally unique fake UDPAddr.
// The current implementation just uses the pointer value of de jammed into an IPv6
// address, but it could also be, say, a counter.
func (de *endpoint) initFakeUDPAddr() {
	var addr [16]byte
	addr[0] = 0xfd
	addr[1] = 0x00
	binary.BigEndian.PutUint64(addr[2:], uint64(reflect.ValueOf(de).Pointer()))
	de.fakeWGAddr = netip.AddrPortFrom(netip.AddrFrom16(addr).Unmap(), 12345)
}

// noteRecvActivity records receive activity on de, and invokes
// Conn.noteRecvActivity no more than once every 10s.
func (de *endpoint) noteRecvActivity() {
	if de.c.noteRecvActivity == nil {
		return
	}
	now := mono.Now()
	elapsed := now.Sub(de.lastRecv.LoadAtomic())
	if elapsed > 10*time.Second {
		de.lastRecv.StoreAtomic(now)
		de.c.noteRecvActivity(de.publicKey)
	}
}

func (de *endpoint) discoShort() string {
	var short string
	if d := de.disco.Load(); d != nil {
		short = d.short
	}
	return short
}

// String exists purely so wireguard-go internals can log.Printf("%v")
// its internal conn.Endpoints and we don't end up with data races
// from fmt (via log) reading mutex fields and such.
func (de *endpoint) String() string {
	return fmt.Sprintf("magicsock.endpoint{%v, %v}", de.publicKey.ShortString(), de.discoShort())
}

func (de *endpoint) ClearSrc()           {}
func (de *endpoint) SrcToString() string { panic("unused") } // unused by wireguard-go
func (de *endpoint) SrcIP() netip.Addr   { panic("unused") } // unused by wireguard-go
func (de *endpoint) DstToString() string { return de.publicKeyHex }
func (de *endpoint) DstIP() netip.Addr   { return de.nodeAddr } // see tailscale/tailscale#6686
func (de *endpoint) DstToBytes() []byte  { return packIPPort(de.fakeWGAddr) }

// addrForSendLocked returns the address(es) that should be used for
// sending the next packet. Zero, one, or both of UDP address and DERP
// addr may be non-zero. If the endpoint is WireGuard only and does not have
// latency information, a bool is returned to indiciate that the
// WireGuard latency discovery pings should be sent.
//
// de.mu must be held.
func (de *endpoint) addrForSendLocked(now mono.Time) (udpAddr, derpAddr netip.AddrPort, sendWGPing bool) {
	udpAddr = de.bestAddr.AddrPort

	if udpAddr.IsValid() && !now.After(de.trustBestAddrUntil) {
		return udpAddr, netip.AddrPort{}, false
	}

	if de.isWireguardOnly {
		// If the endpoint is wireguard-only, we don't have a DERP
		// address to send to, so we have to send to the UDP address.
		udpAddr, shouldPing := de.addrForWireGuardSendLocked(now)
		return udpAddr, netip.AddrPort{}, shouldPing
	}

	// We had a bestAddr but it expired so send both to it
	// and DERP.
	return udpAddr, de.derpAddr, false
}

// addrForWireGuardSendLocked returns the address that should be used for
// sending the next packet. If a packet has never or not recently been sent to
// the endpoint, then a randomly selected address for the endpoint is returned,
// as well as a bool indiciating that WireGuard discovery pings should be started.
// If the addresses have latency information available, then the address with the
// best latency is used.
//
// de.mu must be held.
func (de *endpoint) addrForWireGuardSendLocked(now mono.Time) (udpAddr netip.AddrPort, shouldPing bool) {
	// lowestLatency is a high duration initially, so we
	// can be sure we're going to have a duration lower than this
	// for the first latency retrieved.
	lowestLatency := time.Hour
	for ipp, state := range de.endpointState {
		if latency, ok := state.latencyLocked(); ok {
			if latency < lowestLatency || latency == lowestLatency && ipp.Addr().Is6() {
				// If we have the same latency,IPv6 is prioritized.
				// TODO(catzkorn): Consider a small increase in latency to use
				// IPv6 in comparison to IPv4, when possible.
				lowestLatency = latency
				udpAddr = ipp
			}
		}
	}

	if udpAddr.IsValid() {
		// Set trustBestAddrUntil to an hour, so we will
		// continue to use this address for a long period of time.
		de.bestAddr.AddrPort = udpAddr
		de.trustBestAddrUntil = now.Add(1 * time.Hour)
		return udpAddr, false
	}

	candidates := make([]netip.AddrPort, 0, len(de.endpointState))
	for ipp := range de.endpointState {
		if ipp.Addr().Is4() && de.c.noV4.Load() {
			continue
		}
		if ipp.Addr().Is6() && de.c.noV6.Load() {
			continue
		}
		candidates = append(candidates, ipp)
	}
	// Randomly select an address to use until we retrieve latency information
	// and give it a short trustBestAddrUntil time so we avoid flapping between
	// addresses while waiting on latency information to be populated.
	udpAddr = candidates[rand.Intn(len(candidates))]
	de.bestAddr.AddrPort = udpAddr
	if len(candidates) == 1 {
		// if we only have one address that we can send data too,
		// we should trust it for a longer period of time.
		de.trustBestAddrUntil = now.Add(1 * time.Hour)
	} else {
		de.trustBestAddrUntil = now.Add(15 * time.Second)
	}

	return udpAddr, len(candidates) > 1
}

// heartbeat is called every heartbeatInterval to keep the best UDP path alive,
// or kick off discovery of other paths.
func (de *endpoint) heartbeat() {
	de.mu.Lock()
	defer de.mu.Unlock()

	de.heartBeatTimer = nil

	if de.heartbeatDisabled {
		// If control override to disable heartBeatTimer set, return early.
		return
	}

	if de.lastSend.IsZero() {
		// Shouldn't happen.
		return
	}

	if mono.Since(de.lastSend) > sessionActiveTimeout {
		// Session's idle. Stop heartbeating.
		de.c.dlogf("[v1] magicsock: disco: ending heartbeats for idle session to %v (%v)", de.publicKey.ShortString(), de.discoShort())
		return
	}

	now := mono.Now()
	udpAddr, _, _ := de.addrForSendLocked(now)
	if udpAddr.IsValid() {
		// We have a preferred path. Ping that every 2 seconds.
		de.startDiscoPingLocked(udpAddr, now, pingHeartbeat)
	}

	if de.wantFullPingLocked(now) {
		de.sendDiscoPingsLocked(now, true)
	}

	de.heartBeatTimer = time.AfterFunc(heartbeatInterval, de.heartbeat)
}

// wantFullPingLocked reports whether we should ping to all our peers looking for
// a better path.
//
// de.mu must be held.
func (de *endpoint) wantFullPingLocked(now mono.Time) bool {
	if runtime.GOOS == "js" {
		return false
	}
	if !de.bestAddr.IsValid() || de.lastFullPing.IsZero() {
		return true
	}
	if now.After(de.trustBestAddrUntil) {
		return true
	}
	if de.bestAddr.latency <= goodEnoughLatency {
		return false
	}
	if now.Sub(de.lastFullPing) >= upgradeInterval {
		return true
	}
	return false
}

func (de *endpoint) noteActiveLocked() {
	de.lastSend = mono.Now()
	if de.heartBeatTimer == nil && !de.heartbeatDisabled {
		de.heartBeatTimer = time.AfterFunc(heartbeatInterval, de.heartbeat)
	}
}

// cliPing starts a ping for the "tailscale ping" command. res is value to call cb with,
// already partially filled.
func (de *endpoint) cliPing(res *ipnstate.PingResult, cb func(*ipnstate.PingResult)) {
	de.mu.Lock()
	defer de.mu.Unlock()

	if de.expired {
		res.Err = errExpired.Error()
		cb(res)
		return
	}

	de.pendingCLIPings = append(de.pendingCLIPings, pendingCLIPing{res, cb})

	now := mono.Now()
	udpAddr, derpAddr, _ := de.addrForSendLocked(now)
	if derpAddr.IsValid() {
		de.startDiscoPingLocked(derpAddr, now, pingCLI)
	}
	if udpAddr.IsValid() && now.Before(de.trustBestAddrUntil) {
		// Already have an active session, so just ping the address we're using.
		// Otherwise "tailscale ping" results to a node on the local network
		// can look like they're bouncing between, say 10.0.0.0/9 and the peer's
		// IPv6 address, both 1ms away, and it's random who replies first.
		de.startDiscoPingLocked(udpAddr, now, pingCLI)
	} else {
		for ep := range de.endpointState {
			de.startDiscoPingLocked(ep, now, pingCLI)
		}
	}
	de.noteActiveLocked()
}

var (
	errExpired     = errors.New("peer's node key has expired")
	errNoUDPOrDERP = errors.New("no UDP or DERP addr")
)

func (de *endpoint) send(buffs [][]byte) error {
	if fn := de.sendFunc.Load(); fn != nil {
		return fn(buffs)
	}

	de.mu.Lock()
	if de.expired {
		de.mu.Unlock()
		return errExpired
	}

	// if heartbeat disabled, kick off pathfinder
	if de.heartbeatDisabled {
		if !de.pathFinderRunning {
			de.startPathFinder()
		}
	}

	now := mono.Now()
	udpAddr, derpAddr, startWGPing := de.addrForSendLocked(now)

	if de.isWireguardOnly {
		if startWGPing {
			de.sendWireGuardOnlyPingsLocked(now)
		}
	} else if !udpAddr.IsValid() || now.After(de.trustBestAddrUntil) {
		de.sendDiscoPingsLocked(now, true)
	}
	de.noteActiveLocked()
	de.mu.Unlock()

	if !udpAddr.IsValid() && !derpAddr.IsValid() {
		return errNoUDPOrDERP
	}
	var err error
	if udpAddr.IsValid() {
		_, err = de.c.sendUDPBatch(udpAddr, buffs)
		// TODO(raggi): needs updating for accuracy, as in error conditions we may have partial sends.
		if stats := de.c.stats.Load(); err == nil && stats != nil {
			var txBytes int
			for _, b := range buffs {
				txBytes += len(b)
			}
			stats.UpdateTxPhysical(de.nodeAddr, udpAddr, txBytes)
		}
	}
	if derpAddr.IsValid() {
		allOk := true
		for _, buff := range buffs {
			ok, _ := de.c.sendAddr(derpAddr, de.publicKey, buff)
			if stats := de.c.stats.Load(); stats != nil {
				stats.UpdateTxPhysical(de.nodeAddr, derpAddr, len(buff))
			}
			if !ok {
				allOk = false
			}
		}
		if allOk {
			return nil
		}
	}
	return err
}

func (de *endpoint) discoPingTimeout(txid stun.TxID) {
	de.mu.Lock()
	defer de.mu.Unlock()
	sp, ok := de.sentPing[txid]
	if !ok {
		return
	}
	if debugDisco() || !de.bestAddr.IsValid() || mono.Now().After(de.trustBestAddrUntil) {
		de.c.dlogf("[v1] magicsock: disco: timeout waiting for pong %x from %v (%v, %v)", txid[:6], sp.to, de.publicKey.ShortString(), de.discoShort())
	}
	de.removeSentDiscoPingLocked(txid, sp)
}

// forgetDiscoPing is called by a timer when a ping either fails to send or
// has taken too long to get a pong reply.
func (de *endpoint) forgetDiscoPing(txid stun.TxID) {
	de.mu.Lock()
	defer de.mu.Unlock()
	if sp, ok := de.sentPing[txid]; ok {
		de.removeSentDiscoPingLocked(txid, sp)
	}
}

func (de *endpoint) removeSentDiscoPingLocked(txid stun.TxID, sp sentPing) {
	// Stop the timer for the case where sendPing failed to write to UDP.
	// In the case of a timer already having fired, this is a no-op:
	sp.timer.Stop()
	delete(de.sentPing, txid)
}

// sendDiscoPing sends a ping with the provided txid to ep using de's discoKey.
//
// The caller (startPingLocked) should've already recorded the ping in
// sentPing and set up the timer.
//
// The caller should use de.discoKey as the discoKey argument.
// It is passed in so that sendDiscoPing doesn't need to lock de.mu.
func (de *endpoint) sendDiscoPing(ep netip.AddrPort, discoKey key.DiscoPublic, txid stun.TxID, logLevel discoLogLevel) {
	sent, _ := de.c.sendDiscoMessage(ep, de.publicKey, discoKey, &disco.Ping{
		TxID:    [12]byte(txid),
		NodeKey: de.c.publicKeyAtomic.Load(),
	}, logLevel)
	if !sent {
		de.forgetDiscoPing(txid)
	}
}

// discoPingPurpose is the reason why a discovery ping message was sent.
type discoPingPurpose int

//go:generate go run tailscale.com/cmd/addlicense -file discopingpurpose_string.go go run golang.org/x/tools/cmd/stringer -type=discoPingPurpose -trimprefix=ping
const (
	// pingDiscovery means that purpose of a ping was to see if a
	// path was valid.
	pingDiscovery discoPingPurpose = iota

	// pingHeartbeat means that purpose of a ping was whether a
	// peer was still there.
	pingHeartbeat

	// pingCLI means that the user is running "tailscale ping"
	// from the CLI. These types of pings can go over DERP.
	pingCLI
)

func (de *endpoint) startDiscoPingLocked(ep netip.AddrPort, now mono.Time, purpose discoPingPurpose) {
	if runtime.GOOS == "js" {
		return
	}
	epDisco := de.disco.Load()
	if epDisco == nil {
		return
	}
	if purpose != pingCLI {
		st, ok := de.endpointState[ep]
		if !ok {
			// Shouldn't happen. But don't ping an endpoint that's
			// not active for us.
			de.c.logf("magicsock: disco: [unexpected] attempt to ping no longer live endpoint %v", ep)
			return
		}
		st.lastPing = now
	}

	txid := stun.NewTxID()
	de.sentPing[txid] = sentPing{
		to:      ep,
		at:      now,
		timer:   time.AfterFunc(pingTimeoutDuration, func() { de.discoPingTimeout(txid) }),
		purpose: purpose,
	}
	logLevel := discoLog
	if purpose == pingHeartbeat {
		logLevel = discoVerboseLog
	}
	go de.sendDiscoPing(ep, epDisco.key, txid, logLevel)
}

func (de *endpoint) sendDiscoPingsLocked(now mono.Time, sendCallMeMaybe bool) {
	de.lastFullPing = now
	var sentAny bool
	for ep, st := range de.endpointState {
		if st.shouldDeleteLocked() {
			de.deleteEndpointLocked("sendPingsLocked", ep)
			continue
		}
		if runtime.GOOS == "js" {
			continue
		}
		if !st.lastPing.IsZero() && now.Sub(st.lastPing) < discoPingInterval {
			continue
		}

		firstPing := !sentAny
		sentAny = true

		if firstPing && sendCallMeMaybe {
			de.c.dlogf("[v1] magicsock: disco: send, starting discovery for %v (%v)", de.publicKey.ShortString(), de.discoShort())
		}

		de.startDiscoPingLocked(ep, now, pingDiscovery)
	}
	derpAddr := de.derpAddr
	if sentAny && sendCallMeMaybe && derpAddr.IsValid() {
		// Have our magicsock.Conn figure out its STUN endpoint (if
		// it doesn't know already) and then send a CallMeMaybe
		// message to our peer via DERP informing them that we've
		// sent so our firewall ports are probably open and now
		// would be a good time for them to connect.
		go de.c.enqueueCallMeMaybe(derpAddr, de)
	}
}

// sendWireGuardOnlyPingsLocked evaluates all available addresses for
// a WireGuard only endpoint and initates an ICMP ping for useable
// addresses.
func (de *endpoint) sendWireGuardOnlyPingsLocked(now mono.Time) {
	if runtime.GOOS == "js" {
		return
	}

	// Normally the we only send pings at a low rate as the decision to start
	// sending a ping sets bestAddrAtUntil with a reasonable time to keep trying
	// that address, however, if that code changed we may want to be sure that
	// we don't ever send excessive pings to avoid impact to the client/user.
	if !now.After(de.lastFullPing.Add(10 * time.Second)) {
		return
	}
	de.lastFullPing = now

	for ipp := range de.endpointState {
		if ipp.Addr().Is4() && de.c.noV4.Load() {
			continue
		}
		if ipp.Addr().Is6() && de.c.noV6.Load() {
			continue
		}

		go de.sendWireGuardOnlyPing(ipp, now)
	}
}

// getPinger lazily instantiates a pinger and returns it, if it was
// already instantiated it returns the existing one.
func (c *Conn) getPinger() *ping.Pinger {
	return c.wgPinger.Get(func() *ping.Pinger {
		return ping.New(c.connCtx, c.dlogf, netns.Listener(c.logf, c.netMon))
	})
}

// sendWireGuardOnlyPing sends a ICMP ping to a WireGuard only address to
// discover the latency.
func (de *endpoint) sendWireGuardOnlyPing(ipp netip.AddrPort, now mono.Time) {
	ctx, cancel := context.WithTimeout(de.c.connCtx, 5*time.Second)
	defer cancel()

	de.setLastPing(ipp, now)

	addr := &net.IPAddr{
		IP:   net.IP(ipp.Addr().AsSlice()),
		Zone: ipp.Addr().Zone(),
	}

	p := de.c.getPinger()
	if p == nil {
		de.c.logf("[v2] magicsock: sendWireGuardOnlyPingLocked: pinger is nil")
		return
	}

	latency, err := p.Send(ctx, addr, nil)
	if err != nil {
		de.c.logf("[v2] magicsock: sendWireGuardOnlyPingLocked: %s", err)
		return
	}

	de.mu.Lock()
	defer de.mu.Unlock()

	state, ok := de.endpointState[ipp]
	if !ok {
		return
	}
	state.addPongReplyLocked(pongReply{
		latency: latency,
		pongAt:  now,
		from:    ipp,
		pongSrc: netip.AddrPort{}, // We don't know this.
	})
}

// setLastPing sets lastPing on the endpointState to now.
func (de *endpoint) setLastPing(ipp netip.AddrPort, now mono.Time) {
	de.mu.Lock()
	defer de.mu.Unlock()
	state, ok := de.endpointState[ipp]
	if !ok {
		return
	}
	state.lastPing = now
}

// updateFromNode updates the endpoint based on a tailcfg.Node from a NetMap
// update.
func (de *endpoint) updateFromNode(n *tailcfg.Node, heartbeatDisabled bool) {
	if n == nil {
		panic("nil node when updating endpoint")
	}
	de.mu.Lock()
	defer de.mu.Unlock()

	de.heartbeatDisabled = heartbeatDisabled
	de.expired = n.Expired

	epDisco := de.disco.Load()
	var discoKey key.DiscoPublic
	if epDisco != nil {
		discoKey = epDisco.key
	}

	if discoKey != n.DiscoKey {
		de.c.logf("[v1] magicsock: disco: node %s changed from %s to %s", de.publicKey.ShortString(), discoKey, n.DiscoKey)
		de.disco.Store(&endpointDisco{
			key:   n.DiscoKey,
			short: n.DiscoKey.ShortString(),
		})
		de.debugUpdates.Add(EndpointChange{
			When: time.Now(),
			What: "updateFromNode-resetLocked",
		})
		de.resetLocked()
	}
	if n.DERP == "" {
		if de.derpAddr.IsValid() {
			de.debugUpdates.Add(EndpointChange{
				When: time.Now(),
				What: "updateFromNode-remove-DERP",
				From: de.derpAddr,
			})
		}
		de.derpAddr = netip.AddrPort{}
	} else {
		newDerp, _ := netip.ParseAddrPort(n.DERP)
		if de.derpAddr != newDerp {
			de.debugUpdates.Add(EndpointChange{
				When: time.Now(),
				What: "updateFromNode-DERP",
				From: de.derpAddr,
				To:   newDerp,
			})
		}
		de.derpAddr = newDerp
	}

	for _, st := range de.endpointState {
		st.index = indexSentinelDeleted // assume deleted until updated in next loop
	}

	var newIpps []netip.AddrPort
	for i, epStr := range n.Endpoints {
		if i > math.MaxInt16 {
			// Seems unlikely.
			continue
		}
		ipp, err := netip.ParseAddrPort(epStr)
		if err != nil {
			de.c.logf("magicsock: bogus netmap endpoint %q", epStr)
			continue
		}
		if st, ok := de.endpointState[ipp]; ok {
			st.index = int16(i)
		} else {
			de.endpointState[ipp] = &endpointState{index: int16(i)}
			newIpps = append(newIpps, ipp)
		}
	}
	if len(newIpps) > 0 {
		de.debugUpdates.Add(EndpointChange{
			When: time.Now(),
			What: "updateFromNode-new-Endpoints",
			To:   newIpps,
		})
	}

	// Now delete anything unless it's still in the network map or
	// was a recently discovered endpoint.
	for ep, st := range de.endpointState {
		if st.shouldDeleteLocked() {
			de.deleteEndpointLocked("updateFromNode", ep)
		}
	}

	// Node changed. Invalidate its sending fast path, if any.
	de.sendFunc.Store(nil)
}

// addCandidateEndpoint adds ep as an endpoint to which we should send
// future pings. If there is an existing endpointState for ep, and forRxPingTxID
// matches the last received ping TxID, this function reports true, otherwise
// false.
//
// This is called once we've already verified that we got a valid
// discovery message from de via ep.
func (de *endpoint) addCandidateEndpoint(ep netip.AddrPort, forRxPingTxID stun.TxID) (duplicatePing bool) {
	de.mu.Lock()
	defer de.mu.Unlock()

	if st, ok := de.endpointState[ep]; ok {
		duplicatePing = forRxPingTxID == st.lastGotPingTxID
		if !duplicatePing {
			st.lastGotPingTxID = forRxPingTxID
		}
		if st.lastGotPing.IsZero() {
			// Already-known endpoint from the network map.
			return duplicatePing
		}
		st.lastGotPing = time.Now()
		return duplicatePing
	}

	// Newly discovered endpoint. Exciting!
	de.c.dlogf("[v1] magicsock: disco: adding %v as candidate endpoint for %v (%s)", ep, de.discoShort(), de.publicKey.ShortString())
	de.endpointState[ep] = &endpointState{
		lastGotPing:     time.Now(),
		lastGotPingTxID: forRxPingTxID,
	}

	// If for some reason this gets very large, do some cleanup.
	if size := len(de.endpointState); size > 100 {
		for ep, st := range de.endpointState {
			if st.shouldDeleteLocked() {
				de.deleteEndpointLocked("addCandidateEndpoint", ep)
			}
		}
		size2 := len(de.endpointState)
		de.c.dlogf("[v1] magicsock: disco: addCandidateEndpoint pruned %v candidate set from %v to %v entries", size, size2)
	}
	return false
}

// noteConnectivityChange is called when connectivity changes enough
// that we should question our earlier assumptions about which paths
// work.
func (de *endpoint) noteConnectivityChange() {
	de.mu.Lock()
	defer de.mu.Unlock()

	de.trustBestAddrUntil = 0
}

// handlePongConnLocked handles a Pong message (a reply to an earlier ping).
// It should be called with the Conn.mu held.
//
// It reports whether m.TxID corresponds to a ping that this endpoint sent.
func (de *endpoint) handlePongConnLocked(m *disco.Pong, di *discoInfo, src netip.AddrPort) (knownTxID bool) {
	de.mu.Lock()
	defer de.mu.Unlock()

	isDerp := src.Addr() == derpMagicIPAddr

	sp, ok := de.sentPing[m.TxID]
	if !ok {
		// This is not a pong for a ping we sent.
		return false
	}
	knownTxID = true // for naked returns below
	de.removeSentDiscoPingLocked(m.TxID, sp)

	now := mono.Now()
	latency := now.Sub(sp.at)

	if !isDerp {
		st, ok := de.endpointState[sp.to]
		if !ok {
			// This is no longer an endpoint we care about.
			return
		}

		de.c.peerMap.setNodeKeyForIPPort(src, de.publicKey)

		st.addPongReplyLocked(pongReply{
			latency: latency,
			pongAt:  now,
			from:    src,
			pongSrc: m.Src,
		})
	}

	if sp.purpose != pingHeartbeat {
		de.c.dlogf("[v1] magicsock: disco: %v<-%v (%v, %v)  got pong tx=%x latency=%v pong.src=%v%v", de.c.discoShort, de.discoShort(), de.publicKey.ShortString(), src, m.TxID[:6], latency.Round(time.Millisecond), m.Src, logger.ArgWriter(func(bw *bufio.Writer) {
			if sp.to != src {
				fmt.Fprintf(bw, " ping.to=%v", sp.to)
			}
		}))
	}

	for _, pp := range de.pendingCLIPings {
		de.c.populateCLIPingResponseLocked(pp.res, latency, sp.to)
		go pp.cb(pp.res)
	}
	de.pendingCLIPings = nil

	// Promote this pong response to our current best address if it's lower latency.
	// TODO(bradfitz): decide how latency vs. preference order affects decision
	if !isDerp {
		thisPong := addrLatency{sp.to, latency}
		if betterAddr(thisPong, de.bestAddr) {
			de.c.logf("magicsock: disco: node %v %v now using %v", de.publicKey.ShortString(), de.discoShort(), sp.to)
			de.debugUpdates.Add(EndpointChange{
				When: time.Now(),
				What: "handlePingLocked-bestAddr-update",
				From: de.bestAddr,
				To:   thisPong,
			})
			de.bestAddr = thisPong
		}
		if de.bestAddr.AddrPort == thisPong.AddrPort {
			de.debugUpdates.Add(EndpointChange{
				When: time.Now(),
				What: "handlePingLocked-bestAddr-latency",
				From: de.bestAddr,
				To:   thisPong,
			})
			de.bestAddr.latency = latency
			de.bestAddrAt = now
			de.trustBestAddrUntil = now.Add(trustUDPAddrDuration)
		}
	}
	return
}

// portableTrySetSocketBuffer sets SO_SNDBUF and SO_RECVBUF on pconn to socketBufferSize,
// logging an error if it occurs.
func portableTrySetSocketBuffer(pconn nettype.PacketConn, logf logger.Logf) {
	if c, ok := pconn.(*net.UDPConn); ok {
		// Attempt to increase the buffer size, and allow failures.
		if err := c.SetReadBuffer(socketBufferSize); err != nil {
			logf("magicsock: failed to set UDP read buffer size to %d: %v", socketBufferSize, err)
		}
		if err := c.SetWriteBuffer(socketBufferSize); err != nil {
			logf("magicsock: failed to set UDP write buffer size to %d: %v", socketBufferSize, err)
		}
	}
}

// addrLatency is an IPPort with an associated latency.
type addrLatency struct {
	netip.AddrPort
	latency time.Duration
}

func (a addrLatency) String() string {
	return a.AddrPort.String() + "@" + a.latency.String()
}

// betterAddr reports whether a is a better addr to use than b.
func betterAddr(a, b addrLatency) bool {
	if a.AddrPort == b.AddrPort {
		return false
	}
	if !b.IsValid() {
		return true
	}
	if !a.IsValid() {
		return false
	}
	if a.Addr().Is6() && b.Addr().Is4() {
		// Prefer IPv6 for being a bit more robust, as long as
		// the latencies are roughly equivalent.
		if a.latency/10*9 < b.latency {
			return true
		}
	} else if a.Addr().Is4() && b.Addr().Is6() {
		if betterAddr(b, a) {
			return false
		}
	}

	// If we get here, then both addresses are the same IP type (i.e. both
	// IPv4 or both IPv6). All decisions below are made solely on latency.
	//
	// Determine how much the latencies differ; we ensure the larger
	// latency is the denominator, so this fraction will always be <= 1.0.
	var latencyFraction float64
	if a.latency >= b.latency {
		latencyFraction = float64(b.latency) / float64(a.latency)
	} else {
		latencyFraction = float64(a.latency) / float64(b.latency)
	}

	// Don't change anything if the latency improvement is less than 1%; we
	// want a bit of "stickiness" (a.k.a. hysteresis) to avoid flapping if
	// there's two roughly-equivalent endpoints.
	if latencyFraction >= 0.99 {
		return false
	}

	// The total difference is >1%, so a is better than b if it's
	// lower-latency.
	return a.latency < b.latency
}

// endpoint.mu must be held.
func (st *endpointState) addPongReplyLocked(r pongReply) {
	if n := len(st.recentPongs); n < pongHistoryCount {
		st.recentPong = uint16(n)
		st.recentPongs = append(st.recentPongs, r)
		return
	}
	i := st.recentPong + 1
	if i == pongHistoryCount {
		i = 0
	}
	st.recentPongs[i] = r
	st.recentPong = i
}

// handleCallMeMaybe handles a CallMeMaybe discovery message via
// DERP. The contract for use of this message is that the peer has
// already sent to us via UDP, so their stateful firewall should be
// open. Now we can Ping back and make it through.
func (de *endpoint) handleCallMeMaybe(m *disco.CallMeMaybe) {
	if runtime.GOOS == "js" {
		// Nothing to do on js/wasm if we can't send UDP packets anyway.
		return
	}
	de.mu.Lock()
	defer de.mu.Unlock()

	now := time.Now()
	for ep := range de.isCallMeMaybeEP {
		de.isCallMeMaybeEP[ep] = false // mark for deletion
	}
	var newEPs []netip.AddrPort
	for _, ep := range m.MyNumber {
		if ep.Addr().Is6() && ep.Addr().IsLinkLocalUnicast() {
			// We send these out, but ignore them for now.
			// TODO: teach the ping code to ping on all interfaces
			// for these.
			continue
		}
		mak.Set(&de.isCallMeMaybeEP, ep, true)
		if es, ok := de.endpointState[ep]; ok {
			es.callMeMaybeTime = now
		} else {
			de.endpointState[ep] = &endpointState{callMeMaybeTime: now}
			newEPs = append(newEPs, ep)
		}
	}
	if len(newEPs) > 0 {
		de.debugUpdates.Add(EndpointChange{
			When: time.Now(),
			What: "handleCallMeMaybe-new-endpoints",
			To:   newEPs,
		})

		de.c.dlogf("[v1] magicsock: disco: call-me-maybe from %v %v added new endpoints: %v",
			de.publicKey.ShortString(), de.discoShort(),
			logger.ArgWriter(func(w *bufio.Writer) {
				for i, ep := range newEPs {
					if i > 0 {
						w.WriteString(", ")
					}
					w.WriteString(ep.String())
				}
			}))
	}

	// Delete any prior CallMeMaybe endpoints that weren't included
	// in this message.
	for ep, want := range de.isCallMeMaybeEP {
		if !want {
			delete(de.isCallMeMaybeEP, ep)
			de.deleteEndpointLocked("handleCallMeMaybe", ep)
		}
	}

	// Zero out all the lastPing times to force sendPingsLocked to send new ones,
	// even if it's been less than 5 seconds ago.
	for _, st := range de.endpointState {
		st.lastPing = 0
	}
	de.sendDiscoPingsLocked(mono.Now(), false)
}

func (de *endpoint) populatePeerStatus(ps *ipnstate.PeerStatus) {
	de.mu.Lock()
	defer de.mu.Unlock()

	ps.Relay = de.c.derpRegionCodeOfIDLocked(int(de.derpAddr.Port()))

	if de.lastSend.IsZero() {
		return
	}

	now := mono.Now()
	ps.LastWrite = de.lastSend.WallTime()
	ps.Active = now.Sub(de.lastSend) < sessionActiveTimeout

	if udpAddr, derpAddr, _ := de.addrForSendLocked(now); udpAddr.IsValid() && !derpAddr.IsValid() {
		ps.CurAddr = udpAddr.String()
	}
}

// stopAndReset stops timers associated with de and resets its state back to zero.
// It's called when a discovery endpoint is no longer present in the
// NetworkMap, or when magicsock is transitioning from running to
// stopped state (via SetPrivateKey(zero))
func (de *endpoint) stopAndReset() {
	atomic.AddInt64(&de.numStopAndResetAtomic, 1)
	de.mu.Lock()
	defer de.mu.Unlock()

	if closing := de.c.closing.Load(); !closing {
		de.c.logf("[v1] magicsock: doing cleanup for discovery key %s", de.discoShort())
	}

	de.debugUpdates.Add(EndpointChange{
		When: time.Now(),
		What: "stopAndReset-resetLocked",
	})
	de.resetLocked()
	if de.heartBeatTimer != nil {
		de.heartBeatTimer.Stop()
		de.heartBeatTimer = nil
	}
	de.pendingCLIPings = nil
}

// resetLocked clears all the endpoint's p2p state, reverting it to a
// DERP-only endpoint. It does not stop the endpoint's heartbeat
// timer, if one is running.
func (de *endpoint) resetLocked() {
	de.lastSend = 0
	de.lastFullPing = 0
	de.bestAddr = addrLatency{}
	de.bestAddrAt = 0
	de.trustBestAddrUntil = 0
	for _, es := range de.endpointState {
		es.lastPing = 0
	}
	for txid, sp := range de.sentPing {
		de.removeSentDiscoPingLocked(txid, sp)
	}
}

func (de *endpoint) numStopAndReset() int64 {
	return atomic.LoadInt64(&de.numStopAndResetAtomic)
}

// derpStr replaces DERP IPs in s with "derp-".
func derpStr(s string) string { return strings.ReplaceAll(s, "127.3.3.40:", "derp-") }

// ippEndpointCache is a mutex-free single-element cache, mapping from
// a single netip.AddrPort to a single endpoint.
type ippEndpointCache struct {
	ipp netip.AddrPort
	gen int64
	de  *endpoint
}

// discoInfo is the info and state for the DiscoKey
// in the Conn.discoInfo map key.
//
// Note that a DiscoKey does not necessarily map to exactly one
// node. In the case of shared nodes and users switching accounts, two
// nodes in the NetMap may legitimately have the same DiscoKey.  As
// such, no fields in here should be considered node-specific.
type discoInfo struct {
	// discoKey is the same as the Conn.discoInfo map key,
	// just so you can pass around a *discoInfo alone.
	// Not modified once initialized.
	discoKey key.DiscoPublic

	// discoShort is discoKey.ShortString().
	// Not modified once initialized;
	discoShort string

	// sharedKey is the precomputed key for communication with the
	// peer that has the DiscoKey used to look up this *discoInfo in
	// Conn.discoInfo.
	// Not modified once initialized.
	sharedKey key.DiscoShared

	// Mutable fields follow, owned by Conn.mu:

	// lastPingFrom is the src of a ping for discoKey.
	lastPingFrom netip.AddrPort

	// lastPingTime is the last time of a ping for discoKey.
	lastPingTime time.Time
}

// derpAddrFamSelector is the derphttp.AddressFamilySelector we pass
// to derphttp.Client.SetAddressFamilySelector.
//
// It provides the hint as to whether in an IPv4-vs-IPv6 race that
// IPv4 should be held back a bit to give IPv6 a better-than-50/50
// chance of winning. We only return true when we believe IPv6 will
// work anyway, so we don't artificially delay the connection speed.
type derpAddrFamSelector struct{ c *Conn }

func (s derpAddrFamSelector) PreferIPv6() bool {
	if r := s.c.lastNetCheckReport.Load(); r != nil {
		return r.IPv6
	}
	return false
}

type endpointTrackerEntry struct {
	endpoint tailcfg.Endpoint
	until    time.Time
}

type endpointTracker struct {
	mu    sync.Mutex
	cache map[netip.AddrPort]endpointTrackerEntry
}

func (et *endpointTracker) update(now time.Time, eps []tailcfg.Endpoint) (epsPlusCached []tailcfg.Endpoint) {
	epsPlusCached = eps

	var inputEps set.Slice[netip.AddrPort]
	for _, ep := range eps {
		inputEps.Add(ep.Addr)
	}

	et.mu.Lock()
	defer et.mu.Unlock()

	// Add entries to the return array that aren't already there.
	for k, ep := range et.cache {
		// If the endpoint was in the input list, or has expired, skip it.
		if inputEps.Contains(k) {
			continue
		} else if now.After(ep.until) {
			continue
		}

		// We haven't seen this endpoint; add to the return array
		epsPlusCached = append(epsPlusCached, ep.endpoint)
	}

	// Add entries from the original input array into the cache, and/or
	// extend the lifetime of entries that are already in the cache.
	until := now.Add(endpointTrackerLifetime)
	for _, ep := range eps {
		et.addLocked(now, ep, until)
	}

	// Remove everything that has now expired.
	et.removeExpiredLocked(now)
	return epsPlusCached
}

// add will store the provided endpoint(s) in the cache for a fixed period of
// time, and remove any entries in the cache that have expired.
//
// et.mu must be held.
func (et *endpointTracker) addLocked(now time.Time, ep tailcfg.Endpoint, until time.Time) {
	// If we already have an entry for this endpoint, update the timeout on
	// it; otherwise, add it.
	entry, found := et.cache[ep.Addr]
	if found {
		entry.until = until
	} else {
		entry = endpointTrackerEntry{ep, until}
	}
	mak.Set(&et.cache, ep.Addr, entry)
}

// removeExpired will remove all expired entries from the cache
//
// et.mu must be held
func (et *endpointTracker) removeExpiredLocked(now time.Time) {
	for k, ep := range et.cache {
		if now.After(ep.until) {
			delete(et.cache, k)
		}
	}
}

var (
	metricNumPeers     = clientmetric.NewGauge("magicsock_netmap_num_peers")
	metricNumDERPConns = clientmetric.NewGauge("magicsock_num_derp_conns")

	metricRebindCalls     = clientmetric.NewCounter("magicsock_rebind_calls")
	metricReSTUNCalls     = clientmetric.NewCounter("magicsock_restun_calls")
	metricUpdateEndpoints = clientmetric.NewCounter("magicsock_update_endpoints")

	// Sends (data or disco)
	metricSendDERPQueued      = clientmetric.NewCounter("magicsock_send_derp_queued")
	metricSendDERPErrorChan   = clientmetric.NewCounter("magicsock_send_derp_error_chan")
	metricSendDERPErrorClosed = clientmetric.NewCounter("magicsock_send_derp_error_closed")
	metricSendDERPErrorQueue  = clientmetric.NewCounter("magicsock_send_derp_error_queue")
	metricSendUDP             = clientmetric.NewCounter("magicsock_send_udp")
	metricSendUDPError        = clientmetric.NewCounter("magicsock_send_udp_error")
	metricSendDERP            = clientmetric.NewCounter("magicsock_send_derp")
	metricSendDERPError       = clientmetric.NewCounter("magicsock_send_derp_error")

	// Data packets (non-disco)
	metricSendData            = clientmetric.NewCounter("magicsock_send_data")
	metricSendDataNetworkDown = clientmetric.NewCounter("magicsock_send_data_network_down")
	metricRecvDataDERP        = clientmetric.NewCounter("magicsock_recv_data_derp")
	metricRecvDataIPv4        = clientmetric.NewCounter("magicsock_recv_data_ipv4")
	metricRecvDataIPv6        = clientmetric.NewCounter("magicsock_recv_data_ipv6")

	// Disco packets
	metricSendDiscoUDP         = clientmetric.NewCounter("magicsock_disco_send_udp")
	metricSendDiscoDERP        = clientmetric.NewCounter("magicsock_disco_send_derp")
	metricSentDiscoUDP         = clientmetric.NewCounter("magicsock_disco_sent_udp")
	metricSentDiscoDERP        = clientmetric.NewCounter("magicsock_disco_sent_derp")
	metricSentDiscoPing        = clientmetric.NewCounter("magicsock_disco_sent_ping")
	metricSentDiscoPong        = clientmetric.NewCounter("magicsock_disco_sent_pong")
	metricSentDiscoCallMeMaybe = clientmetric.NewCounter("magicsock_disco_sent_callmemaybe")
	metricRecvDiscoBadPeer     = clientmetric.NewCounter("magicsock_disco_recv_bad_peer")
	metricRecvDiscoBadKey      = clientmetric.NewCounter("magicsock_disco_recv_bad_key")
	metricRecvDiscoBadParse    = clientmetric.NewCounter("magicsock_disco_recv_bad_parse")

	metricRecvDiscoUDP                 = clientmetric.NewCounter("magicsock_disco_recv_udp")
	metricRecvDiscoDERP                = clientmetric.NewCounter("magicsock_disco_recv_derp")
	metricRecvDiscoPing                = clientmetric.NewCounter("magicsock_disco_recv_ping")
	metricRecvDiscoPong                = clientmetric.NewCounter("magicsock_disco_recv_pong")
	metricRecvDiscoCallMeMaybe         = clientmetric.NewCounter("magicsock_disco_recv_callmemaybe")
	metricRecvDiscoCallMeMaybeBadNode  = clientmetric.NewCounter("magicsock_disco_recv_callmemaybe_bad_node")
	metricRecvDiscoCallMeMaybeBadDisco = clientmetric.NewCounter("magicsock_disco_recv_callmemaybe_bad_disco")
	metricRecvDiscoDERPPeerNotHere     = clientmetric.NewCounter("magicsock_disco_recv_derp_peer_not_here")
	metricRecvDiscoDERPPeerGoneUnknown = clientmetric.NewCounter("magicsock_disco_recv_derp_peer_gone_unknown")
	// metricDERPHomeChange is how many times our DERP home region DI has
	// changed from non-zero to a different non-zero.
	metricDERPHomeChange = clientmetric.NewCounter("derp_home_change")

	// Disco packets received bpf read path
	metricRecvDiscoPacketIPv4 = clientmetric.NewCounter("magicsock_disco_recv_bpf_ipv4")
	metricRecvDiscoPacketIPv6 = clientmetric.NewCounter("magicsock_disco_recv_bpf_ipv6")
)
