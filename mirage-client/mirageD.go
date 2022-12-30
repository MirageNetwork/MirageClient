package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dblohm7/wingoes/com"
	"github.com/rs/zerolog/log"
	"github.com/tailscale/wireguard-go/tun"
	"golang.org/x/sys/windows"
	"tailscale.com/control/controlclient"
	"tailscale.com/envknob"
	"tailscale.com/ipn/ipnlocal"
	"tailscale.com/ipn/ipnserver"
	"tailscale.com/ipn/store"
	"tailscale.com/logtail/backoff"
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
	"tailscale.com/util/winutil"
	"tailscale.com/wgengine"
	"tailscale.com/wgengine/monitor"
	"tailscale.com/wgengine/netstack"
	"tailscale.com/wgengine/router"
)

func init() {
	// Initialize COM process-wide
	comProcessType := com.GUIApp
	if err := com.StartRuntime(comProcessType); err != nil {
		log.Printf("wingoes.com.StartRuntime(%d) failed: %v", comProcessType, err)
	}
}

func init() {
	tstunNew = tstunNewWithWindowsRetries
}

type serverOptions struct {
	VarRoot string

	LoginFlags controlclient.LoginFlags
}

func StartDaemon(ctx context.Context, cleanup bool, stopSignalCh chan bool) {

	envknob.PanicIfAnyEnvCheckedInInit()
	envknob.ApplyDiskConfig()
	envknob.SetNoLogsNoSupport()

	if err := envknob.ApplyDiskConfigError(); err != nil {
		log.Printf("Error reading environment config: %v", err)
	}

	var logf logger.Logf = log.Printf

	logf = logger.RateLimitedFn(logf, 5*time.Second, 5, 100)

	if cleanup {
		dns.Cleanup(logf, tun_name)
		router.Cleanup(logf, tun_name)
		return
	}

	ln, _, err := safesocket.Listen(socket_path, safesocket.WindowsLocalPort)
	if err != nil {
		logNotify("[守护进程]\n监听创建失败", err)
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		stopSignalCh <- false
	}()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)
	signal.Ignore(syscall.SIGPIPE)
	go func(guiStop chan bool) {
		select {
		case s := <-interrupt:
			logf("tailscaled got signal %v; shutting down", s)
			cancel()
		case <-ctx.Done():
		case v := <-guiStop:
			logf("GUI told daemon to stop %v", v)
			cancel()
		}
	}(stopSignalCh)

	srv := ipnserver.New(logf, log_id)
	var lbErr syncs.AtomicValue[error]

	go func() {
		t0 := time.Now()
		lb, err := getLocalBackend(ctx, logf, log_id)
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

	if err := ns.Start(lb); err != nil {
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
	if dir := filepath.Dir(state_path); strings.EqualFold(filepath.Base(dir), "Mirage") {
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
	netns.SetEnabled(true)

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

	e, err = wgengine.NewUserspaceEngine(logf, conf)
	if err != nil {
		return nil, false, err
	}
	return e, false, nil
}

func newNetstack(logf logger.Logf, dialer *tsdial.Dialer, e wgengine.Engine) (*netstack.Impl, error) {
	tunDev, magicConn, dns, ok := e.(wgengine.InternalsGetter).GetInternals()
	if !ok {
		return nil, fmt.Errorf("%T is not a wgengine.InternalsGetter", e)
	}
	return netstack.Create(logf, tunDev, e, magicConn, dialer, dns)
}

func tstunNewWithWindowsRetries(logf logger.Logf, tunName string) (_ tun.Device, devName string, _ error) {
	bo := backoff.NewBackoff("tstunNew", logf, 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for {
		dev, devName, err := tstun.New(logf, tunName)
		if err == nil {
			return dev, devName, err
		}
		if errors.Is(err, windows.ERROR_DEVICE_NOT_AVAILABLE) || windowsUptime() < 10*time.Minute {
			// Wintun is not installing correctly. Dump the state of NetSetupSvc
			// (which is a user-mode service that must be active for network devices
			// to install) and its dependencies to the log.
			winutil.LogSvcState(logf, "NetSetupSvc")
		}
		bo.BackOff(ctx, err)
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
	}
}

var (
	kernel32           = windows.NewLazySystemDLL("kernel32.dll")
	getTickCount64Proc = kernel32.NewProc("GetTickCount64")
)

func windowsUptime() time.Duration {
	r, _, _ := getTickCount64Proc.Call()
	return time.Duration(int64(r)) * time.Millisecond
}
