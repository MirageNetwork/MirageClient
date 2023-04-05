// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// The derper binary is a simple DERP server.
package main // import "tailscale.com/cmd/derper"

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"expvar"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go4.org/mem"
	"golang.org/x/time/rate"
	"tailscale.com/atomicfile"
	"tailscale.com/derp"
	"tailscale.com/derp/derphttp"
	"tailscale.com/metrics"
	"tailscale.com/net/stun"
	"tailscale.com/tsweb"
	"tailscale.com/types/key"
)

var (
	dev        = flag.Bool("dev", false, "run in localhost development mode (overrides -a)")
	addr       = flag.String("a", ":443", "server HTTP/HTTPS listen address, in form \":port\", \"ip:port\", or for IPv6 \"[ip]:port\". If the IP is omitted, it defaults to all interfaces. Serves HTTPS if the port is 443 and/or -certmode is manual, otherwise HTTP.")
	httpPort   = flag.Int("http-port", 80, "The port on which to serve HTTP. Set to -1 to disable. The listener is bound to the same IP (if any) as specified in the -a flag.")
	stunPort   = flag.Int("stun-port", 3478, "The UDP port on which to serve STUN. The listener is bound to the same IP (if any) as specified in the -a flag.")
	configPath = flag.String("c", "", "config file path")
	certMode   = flag.String("certmode", "letsencrypt", "mode for getting a cert. possible options: manual, letsencrypt")
	certDir    = flag.String("certdir", tsweb.DefaultCertDir("derper-certs"), "directory to store LetsEncrypt certs, if addr's port is :443")
	hostname   = flag.String("hostname", "derp.tailscale.com", "LetsEncrypt host name, if addr's port is :443")
	runSTUN    = flag.Bool("stun", true, "whether to run a STUN server. It will bind to the same IP (if any) as the --addr flag value.")
	runDERP    = flag.Bool("derp", true, "whether to run a DERP server. The only reason to set this false is if you're decommissioning a server but want to keep its bootstrap DNS functionality still running.")

	meshPSKFile    = flag.String("mesh-psk-file", defaultMeshPSKFile(), "if non-empty, path to file containing the mesh pre-shared key file. It should contain some hex string; whitespace is trimmed.")
	meshWith       = flag.String("mesh-with", "", "optional comma-separated list of hostnames to mesh with; the server's own hostname can be in the list")
	bootstrapDNS   = flag.String("bootstrap-dns-names", "", "optional comma-separated list of hostnames to make available at /bootstrap-dns")
	unpublishedDNS = flag.String("unpublished-bootstrap-dns-names", "", "optional comma-separated list of hostnames to make available at /bootstrap-dns and not publish in the list")
	verifyClients  = flag.Bool("verify-clients", false, "verify clients to this DERP server through a local tailscaled instance.")

	acceptConnLimit = flag.Float64("accept-connection-limit", math.Inf(+1), "rate limit for accepting new connection")
	acceptConnBurst = flag.Int("accept-connection-burst", math.MaxInt, "burst limit for accepting new connection")
)

var (
	stats             = new(metrics.Set)
	stunDisposition   = &metrics.LabelMap{Label: "disposition"}
	stunAddrFamily    = &metrics.LabelMap{Label: "family"}
	tlsRequestVersion = &metrics.LabelMap{Label: "version"}
	tlsActiveVersion  = &metrics.LabelMap{Label: "version"}

	stunReadError  = stunDisposition.Get("read_error")
	stunNotSTUN    = stunDisposition.Get("not_stun")
	stunWriteError = stunDisposition.Get("write_error")
	stunSuccess    = stunDisposition.Get("success")

	stunIPv4 = stunAddrFamily.Get("ipv4")
	stunIPv6 = stunAddrFamily.Get("ipv6")
)

func init() {
	stats.Set("counter_requests", stunDisposition)
	stats.Set("counter_addrfamily", stunAddrFamily)
	expvar.Publish("stun", stats)
	expvar.Publish("derper_tls_request_version", tlsRequestVersion)
	expvar.Publish("gauge_derper_tls_active_version", tlsActiveVersion)
}

type config struct {
	PrivateKey key.NodePrivate
}

func loadConfig() config {
	if *dev {
		return config{PrivateKey: key.NewNode()}
	}
	if *configPath == "" {
		if os.Getuid() == 0 {
			*configPath = "/var/lib/derper/derper.key"
		} else {
			log.Fatalf("derper: -c <config path> not specified")
		}
		log.Printf("no config path specified; using %s", *configPath)
	}
	b, err := os.ReadFile(*configPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return writeNewConfig()
	case err != nil:
		log.Fatal(err)
		panic("unreachable")
	default:
		var cfg config
		if err := json.Unmarshal(b, &cfg); err != nil {
			log.Fatalf("derper: config: %v", err)
		}
		return cfg
	}
}

func writeNewConfig() config {
	k := key.NewNode()
	if err := os.MkdirAll(filepath.Dir(*configPath), 0777); err != nil {
		log.Fatal(err)
	}
	cfg := config{
		PrivateKey: k,
	}
	b, err := json.MarshalIndent(cfg, "", "\t")
	if err != nil {
		log.Fatal(err)
	}
	if err := atomicfile.WriteFile(*configPath, b, 0600); err != nil {
		log.Fatal(err)
	}
	return cfg
}

func main() {
	flag.Parse()

	if *dev {
		*addr = ":3340" // above the keys DERP
		log.Printf("Running in dev mode.")
		tsweb.DevMode = true
	}

	listenHost, _, err := net.SplitHostPort(*addr)
	if err != nil {
		log.Fatalf("invalid server address: %v", err)
	}

	cfg := loadConfig()

	serveTLS := tsweb.IsProd443(*addr) || *certMode == "manual"

	s := derp.NewServer(cfg.PrivateKey, log.Printf)
	s.SetVerifyClient(*verifyClients)

	if *meshPSKFile != "" {
		b, err := os.ReadFile(*meshPSKFile)
		if err != nil {
			log.Fatal(err)
		}
		key := strings.TrimSpace(string(b))
		if matched, _ := regexp.MatchString(`(?i)^[0-9a-f]{64,}$`, key); !matched {
			log.Fatalf("key in %s must contain 64+ hex digits", *meshPSKFile)
		}
		s.SetMeshKey(key)
		log.Printf("DERP mesh key configured")
	}
	if err := startMesh(s); err != nil {
		log.Fatalf("startMesh: %v", err)
	}
	expvar.Publish("derp", s.ExpVar())

	mux := http.NewServeMux()
	if *runDERP {
		derpHandler := derphttp.Handler(s)
		derpHandler = addWebSocketSupport(s, derpHandler)
		mux.Handle("/derp", derpHandler)
	} else {
		mux.Handle("/derp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "derp server disabled", http.StatusNotFound)
		}))
	}
	mux.HandleFunc("/derp/probe", probeHandler)
	go refreshBootstrapDNSLoop()
	mux.HandleFunc("/bootstrap-dns", handleBootstrapDNS)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(200)
		io.WriteString(w, `<html><body>
<h1>DERP</h1>
<p>
  This is a
  <a href="https://tailscale.com/">Tailscale</a>
  <a href="https://pkg.go.dev/tailscale.com/derp">DERP</a>
  server.
</p>
`)
		if !*runDERP {
			io.WriteString(w, `<p>Status: <b>disabled</b></p>`)
		}
		if tsweb.AllowDebugAccess(r) {
			io.WriteString(w, "<p>Debug info at <a href='/debug/'>/debug/</a>.</p>\n")
		}
	}))
	mux.Handle("/robots.txt", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "User-agent: *\nDisallow: /\n")
	}))
	mux.Handle("/generate_204", http.HandlerFunc(serveNoContent))
	debug := tsweb.Debugger(mux)
	debug.KV("TLS hostname", *hostname)
	debug.KV("Mesh key", s.HasMeshKey())
	debug.Handle("check", "Consistency check", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := s.ConsistencyCheck()
		if err != nil {
			http.Error(w, err.Error(), 500)
		} else {
			io.WriteString(w, "derp.Server ConsistencyCheck okay")
		}
	}))
	debug.Handle("traffic", "Traffic check", http.HandlerFunc(s.ServeDebugTraffic))

	if *runSTUN {
		go serveSTUN(listenHost, *stunPort)
	}

	quietLogger := log.New(logFilter{}, "", 0)
	httpsrv := &http.Server{
		Addr:     *addr,
		Handler:  mux,
		ErrorLog: quietLogger,

		// Set read/write timeout. For derper, this basically
		// only affects TLS setup, as read/write deadlines are
		// cleared on Hijack, which the DERP server does. But
		// without this, we slowly accumulate stuck TLS
		// handshake goroutines forever. This also affects
		// /debug/ traffic, but 30 seconds is plenty for
		// Prometheus/etc scraping.
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	if serveTLS {
		log.Printf("derper: serving on %s with TLS", *addr)
		var certManager certProvider
		certManager, err = certProviderByCertMode(*certMode, *certDir, *hostname)
		if err != nil {
			log.Fatalf("derper: can not start cert provider: %v", err)
		}
		httpsrv.TLSConfig = certManager.TLSConfig()
		getCert := httpsrv.TLSConfig.GetCertificate
		httpsrv.TLSConfig.GetCertificate = func(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			cert, err := getCert(hi)
			if err != nil {
				return nil, err
			}
			cert.Certificate = append(cert.Certificate, s.MetaCert())
			return cert, nil
		}
		// Disable TLS 1.0 and 1.1, which are obsolete and have security issues.
		httpsrv.TLSConfig.MinVersion = tls.VersionTLS12
		httpsrv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS != nil {
				label := "unknown"
				switch r.TLS.Version {
				case tls.VersionTLS10:
					label = "1.0"
				case tls.VersionTLS11:
					label = "1.1"
				case tls.VersionTLS12:
					label = "1.2"
				case tls.VersionTLS13:
					label = "1.3"
				}
				tlsRequestVersion.Add(label, 1)
				tlsActiveVersion.Add(label, 1)
				defer tlsActiveVersion.Add(label, -1)
			}

			// Set HTTP headers to appease automated security scanners.
			//
			// Security automation gets cranky when HTTPS sites don't
			// set HSTS, and when they don't specify a content
			// security policy for XSS mitigation.
			//
			// DERP's HTTP interface is only ever used for debug
			// access (for which trivial safe policies work just
			// fine), and by DERP clients which don't obey any of
			// these browser-centric headers anyway.
			w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; form-action 'none'; base-uri 'self'; block-all-mixed-content; plugin-types 'none'")
			mux.ServeHTTP(w, r)
		})
		if *httpPort > -1 {
			go func() {
				port80mux := http.NewServeMux()
				port80mux.HandleFunc("/generate_204", serveNoContent)
				port80mux.Handle("/", certManager.HTTPHandler(tsweb.Port80Handler{Main: mux}))
				port80srv := &http.Server{
					Addr:        net.JoinHostPort(listenHost, fmt.Sprintf("%d", *httpPort)),
					Handler:     port80mux,
					ErrorLog:    quietLogger,
					ReadTimeout: 30 * time.Second,
					// Crank up WriteTimeout a bit more than usually
					// necessary just so we can do long CPU profiles
					// and not hit net/http/pprof's "profile
					// duration exceeds server's WriteTimeout".
					WriteTimeout: 5 * time.Minute,
				}
				err := port80srv.ListenAndServe()
				if err != nil {
					if err != http.ErrServerClosed {
						log.Fatal(err)
					}
				}
			}()
		}
		err = rateLimitedListenAndServeTLS(httpsrv)
	} else {
		log.Printf("derper: serving on %s", *addr)
		err = httpsrv.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		log.Fatalf("derper: %v", err)
	}
}

const (
	noContentChallengeHeader = "X-Tailscale-Challenge"
	noContentResponseHeader  = "X-Tailscale-Response"
)

// For captive portal detection
func serveNoContent(w http.ResponseWriter, r *http.Request) {
	if challenge := r.Header.Get(noContentChallengeHeader); challenge != "" {
		badChar := strings.IndexFunc(challenge, func(r rune) bool {
			return !isChallengeChar(r)
		}) != -1
		if len(challenge) <= 64 && !badChar {
			w.Header().Set(noContentResponseHeader, "response "+challenge)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func isChallengeChar(c rune) bool {
	// Semi-randomly chosen as a limited set of valid characters
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') ||
		('0' <= c && c <= '9') ||
		c == '.' || c == '-' || c == '_'
}

// probeHandler is the endpoint that js/wasm clients hit to measure
// DERP latency, since they can't do UDP STUN queries.
func probeHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "HEAD", "GET":
		w.Header().Set("Access-Control-Allow-Origin", "*")
	default:
		http.Error(w, "bogus probe method", http.StatusMethodNotAllowed)
	}
}

func serveSTUN(host string, port int) {
	pc, err := net.ListenPacket("udp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		log.Fatalf("failed to open STUN listener: %v", err)
	}
	log.Printf("running STUN server on %v", pc.LocalAddr())
	serverSTUNListener(context.Background(), pc.(*net.UDPConn))
}

func serverSTUNListener(ctx context.Context, pc *net.UDPConn) {
	var buf [64 << 10]byte
	var (
		n   int
		ua  *net.UDPAddr
		err error
	)
	for {
		n, ua, err = pc.ReadFromUDP(buf[:])
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("STUN ReadFrom: %v", err)
			time.Sleep(time.Second)
			stunReadError.Add(1)
			continue
		}
		pkt := buf[:n]
		if !stun.Is(pkt) {
			stunNotSTUN.Add(1)
			continue
		}
		txid, err := stun.ParseBindingRequest(pkt)
		if err != nil {
			stunNotSTUN.Add(1)
			continue
		}
		if ua.IP.To4() != nil {
			stunIPv4.Add(1)
		} else {
			stunIPv6.Add(1)
		}
		addr, _ := netip.AddrFromSlice(ua.IP)
		res := stun.Response(txid, netip.AddrPortFrom(addr, uint16(ua.Port)))
		_, err = pc.WriteTo(res, ua)
		if err != nil {
			stunWriteError.Add(1)
		} else {
			stunSuccess.Add(1)
		}
	}
}

var validProdHostname = regexp.MustCompile(`^derp([^.]*)\.tailscale\.com\.?$`)

func prodAutocertHostPolicy(_ context.Context, host string) error {
	if validProdHostname.MatchString(host) {
		return nil
	}
	return errors.New("invalid hostname")
}

func defaultMeshPSKFile() string {
	try := []string{
		"/home/derp/keys/derp-mesh.key",
		filepath.Join(os.Getenv("HOME"), "keys", "derp-mesh.key"),
	}
	for _, p := range try {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func rateLimitedListenAndServeTLS(srv *http.Server) error {
	addr := srv.Addr
	if addr == "" {
		addr = ":https"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	rln := newRateLimitedListener(ln, rate.Limit(*acceptConnLimit), *acceptConnBurst)
	expvar.Publish("tls_listener", rln.ExpVar())
	defer rln.Close()
	return srv.ServeTLS(rln, "", "")
}

type rateLimitedListener struct {
	// These are at the start of the struct to ensure 64-bit alignment
	// on 32-bit architecture regardless of what other fields may exist
	// in this package.
	numAccepts expvar.Int // does not include number of rejects
	numRejects expvar.Int

	net.Listener

	lim *rate.Limiter
}

func newRateLimitedListener(ln net.Listener, limit rate.Limit, burst int) *rateLimitedListener {
	return &rateLimitedListener{Listener: ln, lim: rate.NewLimiter(limit, burst)}
}

func (l *rateLimitedListener) ExpVar() expvar.Var {
	m := new(metrics.Set)
	m.Set("counter_accepted_connections", &l.numAccepts)
	m.Set("counter_rejected_connections", &l.numRejects)
	return m
}

var errLimitedConn = errors.New("cannot accept connection; rate limited")

func (l *rateLimitedListener) Accept() (net.Conn, error) {
	// Even under a rate limited situation, we accept the connection immediately
	// and close it, rather than being slow at accepting new connections.
	// This provides two benefits: 1) it signals to the client that something
	// is going on on the server, and 2) it prevents new connections from
	// piling up and occupying resources in the OS kernel.
	// The client will retry as needing (with backoffs in place).
	cn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if !l.lim.Allow() {
		l.numRejects.Add(1)
		cn.Close()
		return nil, errLimitedConn
	}
	l.numAccepts.Add(1)
	return cn, nil
}

// logFilter is used to filter out useless error logs that are logged to
// the net/http.Server.ErrorLog logger.
type logFilter struct{}

func (logFilter) Write(p []byte) (int, error) {
	b := mem.B(p)
	if mem.HasSuffix(b, mem.S(": EOF\n")) ||
		mem.HasSuffix(b, mem.S(": i/o timeout\n")) ||
		mem.HasSuffix(b, mem.S(": read: connection reset by peer\n")) ||
		mem.HasSuffix(b, mem.S(": remote error: tls: bad certificate\n")) ||
		mem.HasSuffix(b, mem.S(": tls: first record does not look like a TLS handshake\n")) {
		// Skip this log message, but say that we processed it
		return len(p), nil
	}

	log.Printf("%s", p)
	return len(p), nil
}
