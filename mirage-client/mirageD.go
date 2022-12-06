package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"tailscale.com/control/controlclient"
	"tailscale.com/envknob"
	"tailscale.com/ipn/ipnlocal"
	"tailscale.com/ipn/ipnserver"
	"tailscale.com/ipn/store"
	"tailscale.com/net/dns"
	"tailscale.com/net/dnsfallback"
	"tailscale.com/net/netns"
	"tailscale.com/net/tsdial"
	"tailscale.com/net/tstun"
	"tailscale.com/safesocket"
	"tailscale.com/smallzstd"
	"tailscale.com/syncs"
	"tailscale.com/types/logger"
	"tailscale.com/util/multierr"
	"tailscale.com/wgengine"
	"tailscale.com/wgengine/monitor"
	"tailscale.com/wgengine/netstack"
	"tailscale.com/wgengine/router"
)

type serverOptions struct {
	VarRoot string

	LoginFlags controlclient.LoginFlags
}

func StartDaemon(ctx context.Context, cleanup bool) {
	envknob.PanicIfAnyEnvCheckedInInit()
	envknob.ApplyDiskConfig()
	envknob.SetNoLogsNoSupport()

	if err := envknob.ApplyDiskConfigError(); err != nil {
		log.Printf("Error reading environment config: %v", err)
	}

	var logf logger.Logf = log.Printf
	logid := "MirageD"

	logf = logger.RateLimitedFn(logf, 5*time.Second, 5, 100)

	if cleanup {
		if envknob.Bool("TS_PLEASE_PANIC") {
			panic("TS_PLEASE_PANIC asked us to panic")
		}
		dns.Cleanup(logf, tun_name)
		router.Cleanup(logf, tun_name)
		return
	}

	ln, _, err := safesocket.Listen(socket_path, safesocket.WindowsLocalPort)
	if err != nil {
		logNotify("[守护进程]\n监听创建失败", errors.New("[守护进程]\n监听创建失败"))
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// interrupt := make(chan os.Signal, 1)
	// signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)
	// signal.Ignore(syscall.SIGPIPE)
	// go func() {
	// 	select {
	// 	case s := <-interrupt:
	// 		logf("tailscaled got signal %v; shutting down", s)
	// 		cancel()
	// 	case <-ctx.Done():
	// 	}
	// }()

	srv := ipnserver.New(logf, logid)
	var lbErr syncs.AtomicValue[error]

	go func() {
		t0 := time.Now()
		if s, ok := envknob.LookupInt("TS_DEBUG_BACKEND_DELAY_SEC"); ok {
			d := time.Duration(s) * time.Second
			logf("sleeping %v before starting backend...", d)
			select {
			case <-time.After(d):
				logf("slept %v; starting backend...", d)
			case <-ctx.Done():
				return
			}
		}
		lb, err := getLocalBackend(ctx, logf, logid)
		if err == nil {
			logf("got LocalBackend in %v", time.Since(t0).Round(time.Millisecond))
			srv.SetLocalBackend(lb)
			return
		}
		lbErr.Store(err) // before the following cancel
		cancel()         // make srv.Run below complete
	}()

	err = srv.Run(ctx, ln)

	if err != nil && lbErr.Load() != nil {
		logNotify("[守护进程]\ngetLocalBackend error", errors.New(lbErr.Load().Error()))
		return
	}

	// Cancelation is not an error: it is the only way to stop ipnserver.
	if err != nil && !errors.Is(err, context.Canceled) {
		logNotify("[守护进程]\nipnserver.Run error", errors.New(err.Error()))
		return
	}

	return
}

func getLocalBackend(ctx context.Context, logf logger.Logf, logid string) (_ *ipnlocal.LocalBackend, retErr error) {
	linkMon, err := monitor.New(logf)
	if err != nil {
		return nil, fmt.Errorf("monitor.New: %w", err)
	}

	dialer := &tsdial.Dialer{Logf: logf} // mutated below (before used)
	e, onlyNetstack, err := createEngine(logf, linkMon, dialer)
	if err != nil {
		return nil, fmt.Errorf("createEngine: %w", err)
	}
	if _, ok := e.(wgengine.ResolvingEngine).GetResolver(); !ok {
		panic("internal error: exit node resolver not wired up")
	}

	ns, err := newNetstack(logf, dialer, e)
	if err != nil {
		return nil, fmt.Errorf("newNetstack: %w", err)
	}
	ns.ProcessLocalIPs = onlyNetstack
	ns.ProcessSubnets = true

	if onlyNetstack {
		dialer.UseNetstackForIP = func(ip netip.Addr) bool {
			_, ok := e.PeerForIP(ip)
			return ok
		}
		dialer.NetstackDialTCP = func(ctx context.Context, dst netip.AddrPort) (net.Conn, error) {
			return ns.DialContextTCP(ctx, dst)
		}
	}

	e = wgengine.NewWatchdog(e)

	opts := ipnServerOpts()

	store, err := store.New(logf, state_path)
	if err != nil {
		return nil, fmt.Errorf("store.New: %w", err)
	}

	lb, err := ipnlocal.NewLocalBackend(logf, logid, store, "", dialer, e, opts.LoginFlags)
	if err != nil {
		return nil, fmt.Errorf("ipnlocal.NewLocalBackend: %w", err)
	}
	lb.SetVarRoot(opts.VarRoot)
	if root := lb.TailscaleVarRoot(); root != "" {
		dnsfallback.SetCachePath(filepath.Join(root, "derpmap.cached.json"))
	}
	lb.SetDecompressor(func() (controlclient.Decompressor, error) {
		return smallzstd.NewDecoder(nil)
	})

	ns.SetLocalBackend(lb)
	if err := ns.Start(); err != nil {
		logNotify("[守护进程]\nfailed to start netstack", errors.New(err.Error()))
	}
	return lb, nil
}

func createEngine(logf logger.Logf, linkMon *monitor.Mon, dialer *tsdial.Dialer) (e wgengine.Engine, onlyNetstack bool, err error) {
	var errs []error
	for _, name := range strings.Split(tun_name, ",") {
		logf("wgengine.NewUserspaceEngine(tun %q) ...", name)
		e, onlyNetstack, err = tryEngine(logf, linkMon, dialer, name)
		if err == nil {
			return e, onlyNetstack, nil
		}
		logf("wgengine.NewUserspaceEngine(tun %q) error: %v", name, err)
		errs = append(errs, err)
	}
	return nil, false, multierr.New(errs...)
}

func ipnServerOpts() (o serverOptions) {
	if dir := filepath.Dir(state_path); strings.EqualFold(filepath.Base(dir), "tailscale") {
		o.VarRoot = dir
	}

	return o
}

var tstunNew = tstun.New

func tryEngine(logf logger.Logf, linkMon *monitor.Mon, dialer *tsdial.Dialer, name string) (e wgengine.Engine, onlyNetstack bool, err error) {
	conf := wgengine.Config{
		ListenPort:  engine_port,
		LinkMonitor: linkMon,
		Dialer:      dialer,
	}

	onlyNetstack = name == "userspace-networking"
	netns.SetEnabled(!onlyNetstack)

	if onlyNetstack {
	} else {
		dev, devName, err := tstunNew(logf, name)
		if err != nil {
			tstun.Diagnose(logf, name, err)
			return nil, false, fmt.Errorf("tstun.New(%q): %w", name, err)
		}
		conf.Tun = dev
		if strings.HasPrefix(name, "tap:") {
			conf.IsTAP = true
			e, err := wgengine.NewUserspaceEngine(logf, conf)
			return e, false, err
		}

		r, err := router.New(logf, dev, linkMon)
		if err != nil {
			dev.Close()
			return nil, false, fmt.Errorf("creating router: %w", err)
		}
		d, err := dns.NewOSConfigurator(logf, devName)
		if err != nil {
			dev.Close()
			r.Close()
			return nil, false, fmt.Errorf("dns.NewOSConfigurator: %w", err)
		}
		conf.DNS = d
		conf.Router = r
		conf.Router = netstack.NewSubnetRouterWrapper(conf.Router)
	}
	e, err = wgengine.NewUserspaceEngine(logf, conf)
	if err != nil {
		return nil, onlyNetstack, err
	}
	return e, onlyNetstack, nil
}

func newNetstack(logf logger.Logf, dialer *tsdial.Dialer, e wgengine.Engine) (*netstack.Impl, error) {
	tunDev, magicConn, dns, ok := e.(wgengine.InternalsGetter).GetInternals()
	if !ok {
		return nil, fmt.Errorf("%T is not a wgengine.InternalsGetter", e)
	}
	return netstack.Create(logf, tunDev, e, magicConn, dialer, dns)
}
