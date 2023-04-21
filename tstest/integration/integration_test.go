// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package integration

//go:generate go run gen_deps.go

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go4.org/mem"
	"tailscale.com/cmd/testwrapper/flakytest"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnlocal"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/ipn/store"
	"tailscale.com/safesocket"
	"tailscale.com/syncs"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

var (
	verboseTailscaled = flag.Bool("verbose-tailscaled", false, "verbose tailscaled logging")
	verboseTailscale  = flag.Bool("verbose-tailscale", false, "verbose tailscale CLI logging")
)

var mainError syncs.AtomicValue[error]

func TestMain(m *testing.M) {
	// Have to disable UPnP which hits the network, otherwise it fails due to HTTP proxy.
	os.Setenv("TS_DISABLE_UPNP", "true")
	flag.Parse()
	v := m.Run()
	CleanupBinaries()
	if v != 0 {
		os.Exit(v)
	}
	if err := mainError.Load(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestOneNodeUpNoAuth(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	n1 := newTestNode(t, env)

	d1 := n1.StartDaemon()
	n1.AwaitResponding()
	n1.MustUp()

	t.Logf("Got IP: %v", n1.AwaitIP())
	n1.AwaitRunning()

	d1.MustCleanShutdown(t)

	t.Logf("number of HTTP logcatcher requests: %v", env.LogCatcher.numRequests())
}

func TestOneNodeExpiredKey(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	n1 := newTestNode(t, env)

	d1 := n1.StartDaemon()
	n1.AwaitResponding()
	n1.MustUp()
	n1.AwaitRunning()

	nodes := env.Control.AllNodes()
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d nodes", len(nodes))
	}

	nodeKey := nodes[0].Key
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := env.Control.AwaitNodeInMapRequest(ctx, nodeKey); err != nil {
		t.Fatal(err)
	}
	cancel()

	env.Control.SetExpireAllNodes(true)
	n1.AwaitNeedsLogin()
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	if err := env.Control.AwaitNodeInMapRequest(ctx, nodeKey); err != nil {
		t.Fatal(err)
	}
	cancel()

	env.Control.SetExpireAllNodes(false)
	n1.AwaitRunning()

	d1.MustCleanShutdown(t)
}

func TestCollectPanic(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	n := newTestNode(t, env)

	cmd := exec.Command(env.daemon, "--cleanup")
	cmd.Env = append(os.Environ(),
		"TS_PLEASE_PANIC=1",
		"TS_LOG_TARGET="+n.env.LogCatcherServer.URL,
	)
	got, _ := cmd.CombinedOutput() // we expect it to fail, ignore err
	t.Logf("initial run: %s", got)

	// Now we run it again, and on start, it will upload the logs to logcatcher.
	cmd = exec.Command(env.daemon, "--cleanup")
	cmd.Env = append(os.Environ(), "TS_LOG_TARGET="+n.env.LogCatcherServer.URL)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleanup failed: %v: %q", err, out)
	}
	if err := tstest.WaitFor(20*time.Second, func() error {
		const sub = `panic`
		if !n.env.LogCatcher.logsContains(mem.S(sub)) {
			return fmt.Errorf("log catcher didn't see %#q; got %s", sub, n.env.LogCatcher.logsString())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestControlTimeLogLine(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.LogCatcher.StoreRawJSON()
	n := newTestNode(t, env)

	n.StartDaemon()
	n.AwaitResponding()
	n.MustUp()
	n.AwaitRunning()

	if err := tstest.WaitFor(20*time.Second, func() error {
		const sub = `"controltime":"2020-08-03T00:00:00.000000001Z"`
		if !n.env.LogCatcher.logsContains(mem.S(sub)) {
			return fmt.Errorf("log catcher didn't see %#q; got %s", sub, n.env.LogCatcher.logsString())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// test Issue 2321: Start with UpdatePrefs should save prefs to disk
func TestStateSavedOnStart(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	n1 := newTestNode(t, env)

	d1 := n1.StartDaemon()
	n1.AwaitResponding()
	n1.MustUp()

	t.Logf("Got IP: %v", n1.AwaitIP())
	n1.AwaitRunning()

	p1 := n1.diskPrefs()
	t.Logf("Prefs1: %v", p1.Pretty())

	// Bring it down, to prevent an EditPrefs call in the
	// subsequent "up", as we want to test the bug when
	// cmd/tailscale implements "up" via LocalBackend.Start.
	n1.MustDown()

	// And change the hostname to something:
	if err := n1.Tailscale("up", "--login-server="+n1.env.ControlServer.URL, "--hostname=foo").Run(); err != nil {
		t.Fatalf("up: %v", err)
	}

	p2 := n1.diskPrefs()
	if pretty := p1.Pretty(); pretty == p2.Pretty() {
		t.Errorf("Prefs didn't change on disk after 'up', still: %s", pretty)
	}
	if p2.Hostname != "foo" {
		t.Errorf("Prefs.Hostname = %q; want foo", p2.Hostname)
	}

	d1.MustCleanShutdown(t)
}

func TestOneNodeUpAuth(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, configureControl(func(control *testcontrol.Server) {
		control.RequireAuth = true
	}))

	n1 := newTestNode(t, env)
	d1 := n1.StartDaemon()

	n1.AwaitListening()

	st := n1.MustStatus()
	t.Logf("Status: %s", st.BackendState)

	t.Logf("Running up --login-server=%s ...", env.ControlServer.URL)

	cmd := n1.Tailscale("up", "--login-server="+env.ControlServer.URL)
	var authCountAtomic int32
	cmd.Stdout = &authURLParserWriter{fn: func(urlStr string) error {
		if env.Control.CompleteAuth(urlStr) {
			atomic.AddInt32(&authCountAtomic, 1)
			t.Logf("completed auth path %s", urlStr)
			return nil
		}
		err := fmt.Errorf("Failed to complete auth path to %q", urlStr)
		t.Log(err)
		return err
	}}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("up: %v", err)
	}
	t.Logf("Got IP: %v", n1.AwaitIP())

	n1.AwaitRunning()

	if n := atomic.LoadInt32(&authCountAtomic); n != 1 {
		t.Errorf("Auth URLs completed = %d; want 1", n)
	}

	d1.MustCleanShutdown(t)
}

func TestTwoNodes(t *testing.T) {
	flakytest.Mark(t, "https://github.com/tailscale/tailscale/issues/3598")
	t.Parallel()
	env := newTestEnv(t)

	// Create two nodes:
	n1 := newTestNode(t, env)
	n1SocksAddrCh := n1.socks5AddrChan()
	d1 := n1.StartDaemon()

	n2 := newTestNode(t, env)
	n2SocksAddrCh := n2.socks5AddrChan()
	d2 := n2.StartDaemon()

	n1Socks := n1.AwaitSocksAddr(n1SocksAddrCh)
	n2Socks := n1.AwaitSocksAddr(n2SocksAddrCh)
	t.Logf("node1 SOCKS5 addr: %v", n1Socks)
	t.Logf("node2 SOCKS5 addr: %v", n2Socks)

	n1.AwaitListening()
	n2.AwaitListening()
	n1.MustUp()
	n2.MustUp()
	n1.AwaitRunning()
	n2.AwaitRunning()

	if err := tstest.WaitFor(2*time.Second, func() error {
		st := n1.MustStatus()
		if len(st.Peer) == 0 {
			return errors.New("no peers")
		}
		if len(st.Peer) > 1 {
			return fmt.Errorf("got %d peers; want 1", len(st.Peer))
		}
		peer := st.Peer[st.Peers()[0]]
		if peer.ID == st.Self.ID {
			return errors.New("peer is self")
		}
		return nil
	}); err != nil {
		t.Error(err)
	}

	d1.MustCleanShutdown(t)
	d2.MustCleanShutdown(t)
}

func TestNodeAddressIPFields(t *testing.T) {
	flakytest.Mark(t, "https://github.com/tailscale/tailscale/issues/7008")
	t.Parallel()
	env := newTestEnv(t)
	n1 := newTestNode(t, env)
	d1 := n1.StartDaemon()

	n1.AwaitListening()
	n1.MustUp()
	n1.AwaitRunning()

	testNodes := env.Control.AllNodes()

	if len(testNodes) != 1 {
		t.Errorf("Expected %d nodes, got %d", 1, len(testNodes))
	}
	node := testNodes[0]
	if len(node.Addresses) == 0 {
		t.Errorf("Empty Addresses field in node")
	}
	if len(node.AllowedIPs) == 0 {
		t.Errorf("Empty AllowedIPs field in node")
	}

	d1.MustCleanShutdown(t)
}

func TestAddPingRequest(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	n1 := newTestNode(t, env)
	n1.StartDaemon()

	n1.AwaitListening()
	n1.MustUp()
	n1.AwaitRunning()

	gotPing := make(chan bool, 1)
	waitPing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPing <- true
	}))
	defer waitPing.Close()

	nodes := env.Control.AllNodes()
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d nodes", len(nodes))
	}

	nodeKey := nodes[0].Key

	// Check that we get at least one ping reply after 10 tries.
	for try := 1; try <= 10; try++ {
		t.Logf("ping %v ...", try)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := env.Control.AwaitNodeInMapRequest(ctx, nodeKey); err != nil {
			t.Fatal(err)
		}
		cancel()

		pr := &tailcfg.PingRequest{URL: fmt.Sprintf("%s/ping-%d", waitPing.URL, try), Log: true}
		if !env.Control.AddPingRequest(nodeKey, pr) {
			t.Logf("failed to AddPingRequest")
			continue
		}

		// Wait for PingRequest to come back
		pingTimeout := time.NewTimer(2 * time.Second)
		defer pingTimeout.Stop()
		select {
		case <-gotPing:
			t.Logf("got ping; success")
			return
		case <-pingTimeout.C:
			// Try again.
		}
	}
	t.Error("all ping attempts failed")
}

func TestC2NPingRequest(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	n1 := newTestNode(t, env)
	n1.StartDaemon()

	n1.AwaitListening()
	n1.MustUp()
	n1.AwaitRunning()

	gotPing := make(chan bool, 1)
	waitPing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("unexpected ping method %q", r.Method)
		}
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ping body read error: %v", err)
		}
		const want = "HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nabc"
		if string(got) != want {
			t.Errorf("body error\n got: %q\nwant: %q", got, want)
		}
		gotPing <- true
	}))
	defer waitPing.Close()

	nodes := env.Control.AllNodes()
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d nodes", len(nodes))
	}

	nodeKey := nodes[0].Key

	// Check that we get at least one ping reply after 10 tries.
	for try := 1; try <= 10; try++ {
		t.Logf("ping %v ...", try)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := env.Control.AwaitNodeInMapRequest(ctx, nodeKey); err != nil {
			t.Fatal(err)
		}
		cancel()

		pr := &tailcfg.PingRequest{
			URL:     fmt.Sprintf("%s/ping-%d", waitPing.URL, try),
			Log:     true,
			Types:   "c2n",
			Payload: []byte("POST /echo HTTP/1.0\r\nContent-Length: 3\r\n\r\nabc"),
		}
		if !env.Control.AddPingRequest(nodeKey, pr) {
			t.Logf("failed to AddPingRequest")
			continue
		}

		// Wait for PingRequest to come back
		pingTimeout := time.NewTimer(2 * time.Second)
		defer pingTimeout.Stop()
		select {
		case <-gotPing:
			t.Logf("got ping; success")
			return
		case <-pingTimeout.C:
			// Try again.
		}
	}
	t.Error("all ping attempts failed")
}

// Issue 2434: when "down" (WantRunning false), tailscaled shouldn't
// be connected to control.
func TestNoControlConnWhenDown(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	n1 := newTestNode(t, env)

	d1 := n1.StartDaemon()
	n1.AwaitResponding()

	// Come up the first time.
	n1.MustUp()
	ip1 := n1.AwaitIP()
	n1.AwaitRunning()

	// Then bring it down and stop the daemon.
	n1.MustDown()
	d1.MustCleanShutdown(t)

	env.LogCatcher.Reset()
	d2 := n1.StartDaemon()
	n1.AwaitResponding()

	st := n1.MustStatus()
	if got, want := st.BackendState, "Stopped"; got != want {
		t.Fatalf("after restart, state = %q; want %q", got, want)
	}

	ip2 := n1.AwaitIP()
	if ip1 != ip2 {
		t.Errorf("IPs different: %q vs %q", ip1, ip2)
	}

	// The real test: verify our daemon doesn't have an HTTP request open.:
	if n := env.Control.InServeMap(); n != 0 {
		t.Errorf("in serve map = %d; want 0", n)
	}

	d2.MustCleanShutdown(t)
}

// Issue 2137: make sure Windows tailscaled works with the CLI alone,
// without the GUI to kick off a Start.
func TestOneNodeUpWindowsStyle(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	n1 := newTestNode(t, env)
	n1.upFlagGOOS = "windows"

	d1 := n1.StartDaemonAsIPNGOOS("windows")
	n1.AwaitResponding()
	n1.MustUp("--unattended")

	t.Logf("Got IP: %v", n1.AwaitIP())
	n1.AwaitRunning()

	d1.MustCleanShutdown(t)
}

// TestNATPing creates two nodes, n1 and n2, sets up masquerades for both and
// tries to do bi-directional pings between them.
func TestNATPing(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	registerNode := func() (*testNode, key.NodePublic) {
		n := newTestNode(t, env)
		n.StartDaemon()
		n.AwaitListening()
		n.MustUp()
		n.AwaitRunning()
		k := n.MustStatus().Self.PublicKey
		return n, k
	}
	n1, k1 := registerNode()
	n2, k2 := registerNode()

	n1IP := n1.AwaitIP()
	n2IP := n2.AwaitIP()

	n1ExternalIP := netip.MustParseAddr("100.64.1.1")
	n2ExternalIP := netip.MustParseAddr("100.64.2.1")

	tests := []struct {
		name       string
		pairs      []testcontrol.MasqueradePair
		n1SeesN2IP netip.Addr
		n2SeesN1IP netip.Addr
	}{
		{
			name:       "no_nat",
			n1SeesN2IP: n2IP,
			n2SeesN1IP: n1IP,
		},
		{
			name: "n1_has_external_ip",
			pairs: []testcontrol.MasqueradePair{
				{
					Node:              k1,
					Peer:              k2,
					NodeMasqueradesAs: n1ExternalIP,
				},
			},
			n1SeesN2IP: n2IP,
			n2SeesN1IP: n1ExternalIP,
		},
		{
			name: "n2_has_external_ip",
			pairs: []testcontrol.MasqueradePair{
				{
					Node:              k2,
					Peer:              k1,
					NodeMasqueradesAs: n2ExternalIP,
				},
			},
			n1SeesN2IP: n2ExternalIP,
			n2SeesN1IP: n1IP,
		},
		{
			name: "both_have_external_ips",
			pairs: []testcontrol.MasqueradePair{
				{
					Node:              k1,
					Peer:              k2,
					NodeMasqueradesAs: n1ExternalIP,
				},
				{
					Node:              k2,
					Peer:              k1,
					NodeMasqueradesAs: n2ExternalIP,
				},
			},
			n1SeesN2IP: n2ExternalIP,
			n2SeesN1IP: n1ExternalIP,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env.Control.SetMasqueradeAddresses(tc.pairs)

			s1 := n1.MustStatus()
			n2AsN1Peer := s1.Peer[k2]
			if got := n2AsN1Peer.TailscaleIPs[0]; got != tc.n1SeesN2IP {
				t.Fatalf("n1 sees n2 as %v; want %v", got, tc.n1SeesN2IP)
			}

			s2 := n2.MustStatus()
			n1AsN2Peer := s2.Peer[k1]
			if got := n1AsN2Peer.TailscaleIPs[0]; got != tc.n2SeesN1IP {
				t.Fatalf("n2 sees n1 as %v; want %v", got, tc.n2SeesN1IP)
			}

			if err := n1.Tailscale("ping", tc.n1SeesN2IP.String()).Run(); err != nil {
				t.Fatal(err)
			}

			if err := n1.Tailscale("ping", "-peerapi", tc.n1SeesN2IP.String()).Run(); err != nil {
				t.Fatal(err)
			}

			if err := n2.Tailscale("ping", tc.n2SeesN1IP.String()).Run(); err != nil {
				t.Fatal(err)
			}

			if err := n2.Tailscale("ping", "-peerapi", tc.n2SeesN1IP.String()).Run(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLogoutRemovesAllPeers(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	// Spin up some nodes.
	nodes := make([]*testNode, 2)
	for i := range nodes {
		nodes[i] = newTestNode(t, env)
		nodes[i].StartDaemon()
		nodes[i].AwaitResponding()
		nodes[i].MustUp()
		nodes[i].AwaitIP()
		nodes[i].AwaitRunning()
	}
	expectedPeers := len(nodes) - 1

	// Make every node ping every other node.
	// This makes sure magicsock is fully populated.
	for i := range nodes {
		for j := range nodes {
			if i <= j {
				continue
			}
			if err := tstest.WaitFor(20*time.Second, func() error {
				return nodes[i].Ping(nodes[j])
			}); err != nil {
				t.Fatalf("ping %v -> %v: %v", nodes[i].AwaitIP(), nodes[j].AwaitIP(), err)
			}
		}
	}

	// wantNode0PeerCount waits until node[0] status includes exactly want peers.
	wantNode0PeerCount := func(want int) {
		if err := tstest.WaitFor(20*time.Second, func() error {
			s := nodes[0].MustStatus()
			if peers := s.Peers(); len(peers) != want {
				return fmt.Errorf("want %d peer(s) in status, got %v", want, peers)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	wantNode0PeerCount(expectedPeers) // all other nodes are peers
	nodes[0].MustLogOut()
	wantNode0PeerCount(0) // node[0] is logged out, so it should not have any peers

	nodes[0].MustUp() // This will create a new node
	expectedPeers++

	nodes[0].AwaitIP()
	wantNode0PeerCount(expectedPeers) // all existing peers and the new node
}

// testEnv contains the test environment (set of servers) used by one
// or more nodes.
type testEnv struct {
	t      testing.TB
	cli    string
	daemon string

	LogCatcher       *LogCatcher
	LogCatcherServer *httptest.Server

	Control       *testcontrol.Server
	ControlServer *httptest.Server

	TrafficTrap       *trafficTrap
	TrafficTrapServer *httptest.Server
}

type testEnvOpt interface {
	modifyTestEnv(*testEnv)
}

type configureControl func(*testcontrol.Server)

func (f configureControl) modifyTestEnv(te *testEnv) {
	f(te.Control)
}

// newTestEnv starts a bunch of services and returns a new test environment.
// newTestEnv arranges for the environment's resources to be cleaned up on exit.
func newTestEnv(t testing.TB, opts ...testEnvOpt) *testEnv {
	if runtime.GOOS == "windows" {
		t.Skip("not tested/working on Windows yet")
	}
	flakytest.Mark(t, "https://github.com/tailscale/tailscale/issues/7036")
	derpMap := RunDERPAndSTUN(t, logger.Discard, "127.0.0.1")
	logc := new(LogCatcher)
	control := &testcontrol.Server{
		DERPMap: derpMap,
	}
	control.HTTPTestServer = httptest.NewUnstartedServer(control)
	trafficTrap := new(trafficTrap)
	e := &testEnv{
		t:                 t,
		cli:               TailscaleBinary(t),
		daemon:            TailscaledBinary(t),
		LogCatcher:        logc,
		LogCatcherServer:  httptest.NewServer(logc),
		Control:           control,
		ControlServer:     control.HTTPTestServer,
		TrafficTrap:       trafficTrap,
		TrafficTrapServer: httptest.NewServer(trafficTrap),
	}
	for _, o := range opts {
		o.modifyTestEnv(e)
	}
	control.HTTPTestServer.Start()
	t.Cleanup(func() {
		// Shut down e.
		if err := e.TrafficTrap.Err(); err != nil {
			e.t.Errorf("traffic trap: %v", err)
			e.t.Logf("logs: %s", e.LogCatcher.logsString())
		}
		e.LogCatcherServer.Close()
		e.TrafficTrapServer.Close()
		e.ControlServer.Close()
	})
	return e
}

// testNode is a machine with a tailscale & tailscaled.
// Currently, the test is simplistic and user==node==machine.
// That may grow complexity later to test more.
type testNode struct {
	env *testEnv

	dir        string // temp dir for sock & state
	sockFile   string
	stateFile  string
	upFlagGOOS string // if non-empty, sets TS_DEBUG_UP_FLAG_GOOS for cmd/tailscale CLI

	mu        sync.Mutex
	onLogLine []func([]byte)
}

// newTestNode allocates a temp directory for a new test node.
// The node is not started automatically.
func newTestNode(t *testing.T, env *testEnv) *testNode {
	dir := t.TempDir()
	sockFile := filepath.Join(dir, "tailscale.sock")
	if len(sockFile) >= 104 {
		t.Fatalf("sockFile path %q (len %v) is too long, must be < 104", sockFile, len(sockFile))
	}
	return &testNode{
		env:       env,
		dir:       dir,
		sockFile:  sockFile,
		stateFile: filepath.Join(dir, "tailscale.state"),
	}
}

func (n *testNode) diskPrefs() *ipn.Prefs {
	t := n.env.t
	t.Helper()
	if _, err := os.ReadFile(n.stateFile); err != nil {
		t.Fatalf("reading prefs: %v", err)
	}
	fs, err := store.NewFileStore(nil, n.stateFile)
	if err != nil {
		t.Fatalf("reading prefs, NewFileStore: %v", err)
	}
	p, err := ipnlocal.ReadStartupPrefsForTest(t.Logf, fs)
	if err != nil {
		t.Fatalf("reading prefs, ReadDiskPrefsForTest: %v", err)
	}
	return p.AsStruct()
}

// AwaitResponding waits for n's tailscaled to be up enough to be
// responding, but doesn't wait for any particular state.
func (n *testNode) AwaitResponding() {
	t := n.env.t
	t.Helper()
	n.AwaitListening()

	st := n.MustStatus()
	t.Logf("Status: %s", st.BackendState)

	if err := tstest.WaitFor(20*time.Second, func() error {
		const sub = `Program starting: `
		if !n.env.LogCatcher.logsContains(mem.S(sub)) {
			return fmt.Errorf("log catcher didn't see %#q; got %s", sub, n.env.LogCatcher.logsString())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// addLogLineHook registers a hook f to be called on each tailscaled
// log line output.
func (n *testNode) addLogLineHook(f func([]byte)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.onLogLine = append(n.onLogLine, f)
}

// socks5AddrChan returns a channel that receives the address (e.g. "localhost:23874")
// of the node's SOCKS5 listener, once started.
func (n *testNode) socks5AddrChan() <-chan string {
	ch := make(chan string, 1)
	n.addLogLineHook(func(line []byte) {
		const sub = "SOCKS5 listening on "
		i := mem.Index(mem.B(line), mem.S(sub))
		if i == -1 {
			return
		}
		addr := string(line)[i+len(sub):]
		select {
		case ch <- addr:
		default:
		}
	})
	return ch
}

func (n *testNode) AwaitSocksAddr(ch <-chan string) string {
	t := n.env.t
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case v := <-ch:
		return v
	case <-timer.C:
		t.Fatal("timeout waiting for node to log its SOCK5 listening address")
		panic("unreachable")
	}
}

// nodeOutputParser parses stderr of tailscaled processes, calling the
// per-line callbacks previously registered via
// testNode.addLogLineHook.
type nodeOutputParser struct {
	buf bytes.Buffer
	n   *testNode
}

func (op *nodeOutputParser) Write(p []byte) (n int, err error) {
	n, err = op.buf.Write(p)
	op.parseLines()
	return
}

func (op *nodeOutputParser) parseLines() {
	n := op.n
	buf := op.buf.Bytes()
	for len(buf) > 0 {
		nl := bytes.IndexByte(buf, '\n')
		if nl == -1 {
			break
		}
		line := buf[:nl+1]
		buf = buf[nl+1:]
		lineTrim := bytes.TrimSpace(line)

		n.mu.Lock()
		for _, f := range n.onLogLine {
			f(lineTrim)
		}
		n.mu.Unlock()
	}
	if len(buf) == 0 {
		op.buf.Reset()
	} else {
		io.CopyN(io.Discard, &op.buf, int64(op.buf.Len()-len(buf)))
	}
}

type Daemon struct {
	Process *os.Process
}

func (d *Daemon) MustCleanShutdown(t testing.TB) {
	d.Process.Signal(os.Interrupt)
	ps, err := d.Process.Wait()
	if err != nil {
		t.Fatalf("tailscaled Wait: %v", err)
	}
	if ps.ExitCode() != 0 {
		t.Errorf("tailscaled ExitCode = %d; want 0", ps.ExitCode())
	}
}

// StartDaemon starts the node's tailscaled, failing if it fails to start.
// StartDaemon ensures that the process will exit when the test completes.
func (n *testNode) StartDaemon() *Daemon {
	return n.StartDaemonAsIPNGOOS(runtime.GOOS)
}

func (n *testNode) StartDaemonAsIPNGOOS(ipnGOOS string) *Daemon {
	t := n.env.t
	cmd := exec.Command(n.env.daemon,
		"--tun=userspace-networking",
		"--state="+n.stateFile,
		"--socket="+n.sockFile,
		"--socks5-server=localhost:0",
	)
	if *verboseTailscaled {
		cmd.Args = append(cmd.Args, "-verbose=2")
	}
	cmd.Env = append(os.Environ(),
		"TS_DEBUG_PERMIT_HTTP_C2N=1",
		"TS_LOG_TARGET="+n.env.LogCatcherServer.URL,
		"HTTP_PROXY="+n.env.TrafficTrapServer.URL,
		"HTTPS_PROXY="+n.env.TrafficTrapServer.URL,
		"TS_DEBUG_FAKE_GOOS="+ipnGOOS,
		"TS_LOGS_DIR="+t.TempDir(),
		"TS_NETCHECK_GENERATE_204_URL="+n.env.ControlServer.URL+"/generate_204",
	)
	cmd.Stderr = &nodeOutputParser{n: n}
	if *verboseTailscaled {
		cmd.Stdout = os.Stdout
		cmd.Stderr = io.MultiWriter(cmd.Stderr, os.Stderr)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting tailscaled: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	return &Daemon{
		Process: cmd.Process,
	}
}

func (n *testNode) MustUp(extraArgs ...string) {
	t := n.env.t
	args := []string{
		"up",
		"--login-server=" + n.env.ControlServer.URL,
	}
	args = append(args, extraArgs...)
	cmd := n.Tailscale(args...)
	t.Logf("Running %v ...", cmd)
	cmd.Stdout = nil // in case --verbose-tailscale was set
	cmd.Stderr = nil // in case --verbose-tailscale was set
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("up: %v, %v", string(b), err)
	}
}

func (n *testNode) MustDown() {
	t := n.env.t
	t.Logf("Running down ...")
	if err := n.Tailscale("down", "--accept-risk=all").Run(); err != nil {
		t.Fatalf("down: %v", err)
	}
}

func (n *testNode) MustLogOut() {
	t := n.env.t
	t.Logf("Running logout ...")
	if err := n.Tailscale("logout").Run(); err != nil {
		t.Fatalf("logout: %v", err)
	}
}

func (n *testNode) Ping(otherNode *testNode) error {
	t := n.env.t
	ip := otherNode.AwaitIP().String()
	t.Logf("Running ping %v (from %v)...", ip, n.AwaitIP())
	return n.Tailscale("ping", ip).Run()
}

// AwaitListening waits for the tailscaled to be serving local clients
// over its localhost IPC mechanism. (Unix socket, etc)
func (n *testNode) AwaitListening() {
	t := n.env.t
	s := safesocket.DefaultConnectionStrategy(n.sockFile)
	if err := tstest.WaitFor(20*time.Second, func() (err error) {
		c, err := safesocket.Connect(s)
		if err != nil {
			return err
		}
		c.Close()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (n *testNode) AwaitIPs() []netip.Addr {
	t := n.env.t
	t.Helper()
	var addrs []netip.Addr
	if err := tstest.WaitFor(20*time.Second, func() error {
		cmd := n.Tailscale("ip")
		cmd.Stdout = nil // in case --verbose-tailscale was set
		cmd.Stderr = nil // in case --verbose-tailscale was set
		out, err := cmd.Output()
		if err != nil {
			return err
		}
		ips := string(out)
		ipslice := strings.Fields(ips)
		addrs = make([]netip.Addr, len(ipslice))

		for i, ip := range ipslice {
			netIP, err := netip.ParseAddr(ip)
			if err != nil {
				t.Fatal(err)
			}
			addrs[i] = netIP
		}
		return nil
	}); err != nil {
		t.Fatalf("awaiting an IP address: %v", err)
	}
	if len(addrs) == 0 {
		t.Fatalf("returned IP address was blank")
	}
	return addrs
}

// AwaitIP returns the IP address of n.
func (n *testNode) AwaitIP() netip.Addr {
	t := n.env.t
	t.Helper()
	ips := n.AwaitIPs()
	return ips[0]
}

// AwaitRunning waits for n to reach the IPN state "Running".
func (n *testNode) AwaitRunning() {
	t := n.env.t
	t.Helper()
	if err := tstest.WaitFor(20*time.Second, func() error {
		st, err := n.Status()
		if err != nil {
			return err
		}
		if st.BackendState != "Running" {
			return fmt.Errorf("in state %q", st.BackendState)
		}
		return nil
	}); err != nil {
		t.Fatalf("failure/timeout waiting for transition to Running status: %v", err)
	}
}

// AwaitNeedsLogin waits for n to reach the IPN state "NeedsLogin".
func (n *testNode) AwaitNeedsLogin() {
	t := n.env.t
	t.Helper()
	if err := tstest.WaitFor(20*time.Second, func() error {
		st, err := n.Status()
		if err != nil {
			return err
		}
		if st.BackendState != "NeedsLogin" {
			return fmt.Errorf("in state %q", st.BackendState)
		}
		return nil
	}); err != nil {
		t.Fatalf("failure/timeout waiting for transition to NeedsLogin status: %v", err)
	}
}

// Tailscale returns a command that runs the tailscale CLI with the provided arguments.
// It does not start the process.
func (n *testNode) Tailscale(arg ...string) *exec.Cmd {
	cmd := exec.Command(n.env.cli, "--socket="+n.sockFile)
	cmd.Args = append(cmd.Args, arg...)
	cmd.Dir = n.dir
	cmd.Env = append(os.Environ(),
		"TS_DEBUG_UP_FLAG_GOOS="+n.upFlagGOOS,
		"TS_LOGS_DIR="+n.env.t.TempDir(),
	)
	if *verboseTailscale {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	return cmd
}

func (n *testNode) Status() (*ipnstate.Status, error) {
	cmd := n.Tailscale("status", "--json")
	cmd.Stdout = nil // in case --verbose-tailscale was set
	cmd.Stderr = nil // in case --verbose-tailscale was set
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("running tailscale status: %v, %s", err, out)
	}
	st := new(ipnstate.Status)
	if err := json.Unmarshal(out, st); err != nil {
		return nil, fmt.Errorf("decoding tailscale status JSON: %w", err)
	}
	return st, nil
}

func (n *testNode) MustStatus() *ipnstate.Status {
	tb := n.env.t
	tb.Helper()
	st, err := n.Status()
	if err != nil {
		tb.Fatal(err)
	}
	return st
}

// trafficTrap is an HTTP proxy handler to note whether any
// HTTP traffic tries to leave localhost from tailscaled. We don't
// expect any, so any request triggers a failure.
type trafficTrap struct {
	atomicErr syncs.AtomicValue[error]
}

func (tt *trafficTrap) Err() error {
	return tt.atomicErr.Load()
}

func (tt *trafficTrap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var got bytes.Buffer
	r.Write(&got)
	err := fmt.Errorf("unexpected HTTP proxy via proxy: %s", got.Bytes())
	mainError.Store(err)
	if tt.Err() == nil {
		// Best effort at remembering the first request.
		tt.atomicErr.Store(err)
	}
	log.Printf("Error: %v", err)
	w.WriteHeader(403)
}

type authURLParserWriter struct {
	buf bytes.Buffer
	fn  func(urlStr string) error
}

var authURLRx = regexp.MustCompile(`(https?://\S+/auth/\S+)`)

func (w *authURLParserWriter) Write(p []byte) (n int, err error) {
	n, err = w.buf.Write(p)
	m := authURLRx.FindSubmatch(w.buf.Bytes())
	if m != nil {
		urlStr := string(m[1])
		w.buf.Reset() // so it's not matched again
		if err := w.fn(urlStr); err != nil {
			return 0, err
		}
	}
	return n, err
}
