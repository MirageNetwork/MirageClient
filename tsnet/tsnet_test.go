// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package tsnet

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/proxy"
	"tailscale.com/ipn"
	"tailscale.com/ipn/store/mem"
	"tailscale.com/net/netns"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest"
	"tailscale.com/tstest/integration"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/logger"
	"tailscale.com/util/must"
)

// TestListener_Server ensures that the listener type always keeps the Server
// method, which is used by some external applications to identify a tsnet.Listener
// from other net.Listeners, as well as access the underlying Server.
func TestListener_Server(t *testing.T) {
	s := &Server{}
	ln := listener{s: s}
	if ln.Server() != s {
		t.Errorf("listener.Server() returned %v, want %v", ln.Server(), s)
	}
}

func TestListenerPort(t *testing.T) {
	errNone := errors.New("sentinel start error")

	tests := []struct {
		network string
		addr    string
		wantErr bool
	}{
		{"tcp", ":80", false},
		{"foo", ":80", true},
		{"tcp", ":http", false},  // built-in name to Go; doesn't require cgo, /etc/services
		{"tcp", ":https", false}, // built-in name to Go; doesn't require cgo, /etc/services
		{"tcp", ":gibberishsdlkfj", true},
		{"tcp", ":%!d(string=80)", true}, // issue 6201
		{"udp", ":80", false},
		{"udp", "100.102.104.108:80", false},
		{"udp", "not-an-ip:80", true},
		{"udp4", ":80", false},
		{"udp4", "100.102.104.108:80", false},
		{"udp4", "not-an-ip:80", true},

		// Verify network type matches IP
		{"tcp4", "1.2.3.4:80", false},
		{"tcp6", "1.2.3.4:80", true},
		{"tcp4", "[12::34]:80", true},
		{"tcp6", "[12::34]:80", false},
	}
	for _, tt := range tests {
		s := &Server{}
		s.initOnce.Do(func() { s.initErr = errNone })
		_, err := s.Listen(tt.network, tt.addr)
		gotErr := err != nil && err != errNone
		if gotErr != tt.wantErr {
			t.Errorf("Listen(%q, %q) error = %v, want %v", tt.network, tt.addr, gotErr, tt.wantErr)
		}
	}
}

var verboseDERP = flag.Bool("verbose-derp", false, "if set, print DERP and STUN logs")
var verboseNodes = flag.Bool("verbose-nodes", false, "if set, print tsnet.Server logs")

func startControl(t *testing.T) (controlURL string) {
	// Corp#4520: don't use netns for tests.
	netns.SetEnabled(false)
	t.Cleanup(func() {
		netns.SetEnabled(true)
	})

	derpLogf := logger.Discard
	if *verboseDERP {
		derpLogf = t.Logf
	}
	derpMap := integration.RunDERPAndSTUN(t, derpLogf, "127.0.0.1")
	control := &testcontrol.Server{
		DERPMap: derpMap,
		DNSConfig: &tailcfg.DNSConfig{
			Proxied: true,
		},
		MagicDNSDomain: "tail-scale.ts.net",
	}
	control.HTTPTestServer = httptest.NewUnstartedServer(control)
	control.HTTPTestServer.Start()
	t.Cleanup(control.HTTPTestServer.Close)
	controlURL = control.HTTPTestServer.URL
	t.Logf("testcontrol listening on %s", controlURL)
	return controlURL
}

type testCertIssuer struct {
	mu    sync.Mutex
	certs map[string]*tls.Certificate

	root    *x509.Certificate
	rootKey *ecdsa.PrivateKey
}

func newCertIssuer() *testCertIssuer {
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	t := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "root",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, t, t, &rootKey.PublicKey, rootKey)
	if err != nil {
		panic(err)
	}
	rootCA, err := x509.ParseCertificate(rootDER)
	if err != nil {
		panic(err)
	}
	return &testCertIssuer{
		certs:   make(map[string]*tls.Certificate),
		root:    rootCA,
		rootKey: rootKey,
	}
}

func (tci *testCertIssuer) getCert(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
	tci.mu.Lock()
	defer tci.mu.Unlock()
	cert, ok := tci.certs[chi.ServerName]
	if ok {
		return cert, nil
	}

	certPrivKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	certTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{chi.ServerName},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, certTmpl, tci.root, &certPrivKey.PublicKey, tci.rootKey)
	if err != nil {
		return nil, err
	}
	cert = &tls.Certificate{
		Certificate: [][]byte{certDER, tci.root.Raw},
		PrivateKey:  certPrivKey,
	}
	tci.certs[chi.ServerName] = cert
	return cert, nil
}

func (tci *testCertIssuer) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(tci.root)
	return p
}

var testCertRoot = newCertIssuer()

func startServer(t *testing.T, ctx context.Context, controlURL, hostname string) (*Server, netip.Addr) {
	t.Helper()

	tmp := filepath.Join(t.TempDir(), hostname)
	os.MkdirAll(tmp, 0755)
	s := &Server{
		Dir:               tmp,
		ControlURL:        controlURL,
		Hostname:          hostname,
		Store:             new(mem.Store),
		Ephemeral:         true,
		getCertForTesting: testCertRoot.getCert,
	}
	if !*verboseNodes {
		s.Logf = logger.Discard
	}
	t.Cleanup(func() { s.Close() })

	status, err := s.Up(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return s, status.TailscaleIPs[0]
}

func TestConn(t *testing.T) {
	tstest.ResourceCheck(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	controlURL := startControl(t)
	s1, s1ip := startServer(t, ctx, controlURL, "s1")
	s2, _ := startServer(t, ctx, controlURL, "s2")

	lc2, err := s2.LocalClient()
	if err != nil {
		t.Fatal(err)
	}

	// ping to make sure the connection is up.
	res, err := lc2.Ping(ctx, s1ip, tailcfg.PingICMP)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ping success: %#+v", res)

	// pass some data through TCP.
	ln, err := s1.Listen("tcp", ":8081")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	w, err := s2.Dial(ctx, "tcp", fmt.Sprintf("%s:8081", s1ip))
	if err != nil {
		t.Fatal(err)
	}

	r, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}

	want := "hello"
	if _, err := io.WriteString(w, want); err != nil {
		t.Fatal(err)
	}

	got := make([]byte, len(want))
	if _, err := io.ReadAtLeast(r, got, len(got)); err != nil {
		t.Fatal(err)
	}
	t.Logf("got: %q", got)
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}

	_, err = s2.Dial(ctx, "tcp", fmt.Sprintf("%s:8082", s1ip)) // some random port
	if err == nil {
		t.Fatalf("unexpected success; should have seen a connection refused error")
	}
}

func TestLoopbackLocalAPI(t *testing.T) {
	tstest.ResourceCheck(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	controlURL := startControl(t)
	s1, _ := startServer(t, ctx, controlURL, "s1")

	addr, proxyCred, localAPICred, err := s1.Loopback()
	if err != nil {
		t.Fatal(err)
	}
	if proxyCred == localAPICred {
		t.Fatal("proxy password matches local API password, they should be different")
	}

	url := "http://" + addr + "/localapi/v0/status"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Errorf("GET %s returned %d, want 403 without Sec- header", url, res.StatusCode)
	}

	req, err = http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Sec-Tailscale", "localapi")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Errorf("GET %s returned %d, want 401 without basic auth", url, res.StatusCode)
	}

	req, err = http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("", localAPICred)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Errorf("GET %s returned %d, want 403 without Sec- header", url, res.StatusCode)
	}

	req, err = http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Sec-Tailscale", "localapi")
	req.SetBasicAuth("", localAPICred)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("GET /status returned %d, want 200", res.StatusCode)
	}
}

func TestLoopbackSOCKS5(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("TODO(#7876): test regressed on windows while CI was broken")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	controlURL := startControl(t)
	s1, s1ip := startServer(t, ctx, controlURL, "s1")
	s2, _ := startServer(t, ctx, controlURL, "s2")

	addr, proxyCred, _, err := s2.Loopback()
	if err != nil {
		t.Fatal(err)
	}

	ln, err := s1.Listen("tcp", ":8081")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	auth := &proxy.Auth{User: "tsnet", Password: proxyCred}
	socksDialer, err := proxy.SOCKS5("tcp", addr, auth, proxy.Direct)
	if err != nil {
		t.Fatal(err)
	}

	w, err := socksDialer.Dial("tcp", fmt.Sprintf("%s:8081", s1ip))
	if err != nil {
		t.Fatal(err)
	}

	r, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}

	want := "hello"
	if _, err := io.WriteString(w, want); err != nil {
		t.Fatal(err)
	}

	got := make([]byte, len(want))
	if _, err := io.ReadAtLeast(r, got, len(got)); err != nil {
		t.Fatal(err)
	}
	t.Logf("got: %q", got)
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTailscaleIPs(t *testing.T) {
	controlURL := startControl(t)

	tmp := t.TempDir()
	tmps1 := filepath.Join(tmp, "s1")
	os.MkdirAll(tmps1, 0755)
	s1 := &Server{
		Dir:        tmps1,
		ControlURL: controlURL,
		Hostname:   "s1",
		Store:      new(mem.Store),
		Ephemeral:  true,
	}
	defer s1.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s1status, err := s1.Up(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var upIp4, upIp6 netip.Addr
	for _, ip := range s1status.TailscaleIPs {
		if ip.Is6() {
			upIp6 = ip
		}
		if ip.Is4() {
			upIp4 = ip
		}
	}

	sIp4, sIp6 := s1.TailscaleIPs()
	if !(upIp4 == sIp4 && upIp6 == sIp6) {
		t.Errorf("s1.TailscaleIPs returned a different result than S1.Up, (%s, %s) != (%s, %s)",
			sIp4, upIp4, sIp6, upIp6)
	}
}

// TestListenerCleanup is a regression test to verify that s.Close doesn't
// deadlock if a listener is still open.
func TestListenerCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	controlURL := startControl(t)
	s1, _ := startServer(t, ctx, controlURL, "s1")

	ln, err := s1.Listen("tcp", ":8081")
	if err != nil {
		t.Fatal(err)
	}

	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	if err := ln.Close(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("second ln.Close error: %v, want net.ErrClosed", err)
	}
}

func TestFunnel(t *testing.T) {
	ctx, dialCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dialCancel()

	controlURL := startControl(t)
	s1, _ := startServer(t, ctx, controlURL, "s1")
	s2, _ := startServer(t, ctx, controlURL, "s2")

	ln := must.Get(s1.ListenFunnel("tcp", ":443"))
	defer ln.Close()
	wantSrcAddrPort := netip.MustParseAddrPort("127.0.0.1:1234")
	wantTarget := ipn.HostPort("s1.tail-scale.ts.net:443")
	srv := &http.Server{
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			tc, ok := c.(*tls.Conn)
			if !ok {
				t.Errorf("ConnContext called with non-TLS conn: %T", c)
			}
			if fc, ok := tc.NetConn().(*ipn.FunnelConn); !ok {
				t.Errorf("ConnContext called with non-FunnelConn: %T", c)
			} else if fc.Src != wantSrcAddrPort {
				t.Errorf("ConnContext called with wrong SrcAddrPort; got %v, want %v", fc.Src, wantSrcAddrPort)
			} else if fc.Target != wantTarget {
				t.Errorf("ConnContext called with wrong Target; got %q, want %q", fc.Target, wantTarget)
			}
			return ctx
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "hello")
		}),
	}
	go srv.Serve(ln)

	c := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialIngressConn(s2, s1, addr)
			},
			TLSClientConfig: &tls.Config{
				RootCAs: testCertRoot.Pool(),
			},
		},
	}
	resp, err := c.Get("https://s1.tail-scale.ts.net:443")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("unexpected status code: %v", resp.StatusCode)
		return
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello" {
		t.Errorf("unexpected body: %q", body)
	}
}

func dialIngressConn(from, to *Server, target string) (net.Conn, error) {
	toLC := must.Get(to.LocalClient())
	toStatus := must.Get(toLC.StatusWithoutPeers(context.Background()))
	peer6 := toStatus.Self.PeerAPIURL[1] // IPv6
	toPeerAPI, ok := strings.CutPrefix(peer6, "http://")
	if !ok {
		return nil, fmt.Errorf("unexpected PeerAPIURL %q", peer6)
	}

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 30*time.Second)
	outConn, err := from.Dial(dialCtx, "tcp", toPeerAPI)
	dialCancel()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", "/v0/ingress", nil)
	if err != nil {
		return nil, err
	}
	req.Host = toPeerAPI
	req.Header.Set("Tailscale-Ingress-Src", "127.0.0.1:1234")
	req.Header.Set("Tailscale-Ingress-Target", target)
	if err := req.Write(outConn); err != nil {
		return nil, err
	}

	br := bufio.NewReader(outConn)
	res, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close() // just to appease vet
	if res.StatusCode != 101 {
		return nil, fmt.Errorf("unexpected status code: %v", res.StatusCode)
	}
	return &bufferedConn{outConn, br}, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) {
	return c.reader.Read(b)
}
