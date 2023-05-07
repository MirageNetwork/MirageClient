// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	shellquote "github.com/kballard/go-shellquote"
	"github.com/peterbourgon/ff/v3/ffcli"
	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/oauth2/clientcredentials"
	"tailscale.com/client/tailscale"
	"tailscale.com/envknob"
	"tailscale.com/health/healthmsg"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/net/tsaddr"
	"tailscale.com/safesocket"
	"tailscale.com/tailcfg"
	"tailscale.com/types/logger"
	"tailscale.com/types/preftype"
	"tailscale.com/util/dnsname"
	"tailscale.com/version"
	"tailscale.com/version/distro"
)

var upCmd = &ffcli.Command{
	Name:       "up",
	ShortUsage: "up [flags]",
	ShortHelp:  "Connect to Mirage, logging in if needed",

	LongHelp: strings.TrimSpace(`
"mirage up" connects this machine to your Mirage network,
triggering authentication if necessary.

With no flags, "mirage up" brings the network online without
changing any settings. (That is, it's the opposite of "mirage
down").

If flags are specified, the flags must be the complete set of desired
settings. An error is returned if any setting would be changed as a
result of an unspecified flag's default value, unless the --reset flag
is also used. (The flags --auth-key, --force-reauth, and --qr are not
considered settings that need to be re-specified when modifying
settings.)
`),
	FlagSet: upFlagSet,
	Exec: func(ctx context.Context, args []string) error {
		return runUp(ctx, "up", args, upArgsGlobal)
	},
}

func effectiveGOOS() string {
	if v := os.Getenv("TS_DEBUG_UP_FLAG_GOOS"); v != "" {
		return v
	}
	return runtime.GOOS
}

// acceptRouteDefault returns the CLI's default value of --accept-routes as
// a function of the platform it's running on.
func acceptRouteDefault(goos string) bool {
	switch goos {
	case "windows":
		return true
	case "darwin":
		return version.IsSandboxedMacOS()
	default:
		return false
	}
}

var upFlagSet = newUpFlagSet(effectiveGOOS(), &upArgsGlobal, "up")

func inTest() bool { return flag.Lookup("test.v") != nil }

// newUpFlagSet returns a new flag set for the "up" and "login" commands.
func newUpFlagSet(goos string, upArgs *upArgsT, cmd string) *flag.FlagSet {
	if cmd != "up" && cmd != "login" {
		panic("cmd must be up or login")
	}
	upf := newFlagSet(cmd)

	upf.BoolVar(&upArgs.qr, "qr", false, "show QR code for login URLs")
	upf.StringVar(&upArgs.authKeyOrFile, "auth-key", "", `node authorization key; if it begins with "file:", then it's a path to a file containing the authkey`)

	upf.StringVar(&upArgs.server, "login-server", ipn.DefaultControlURL, "base URL of control server")
	upf.BoolVar(&upArgs.acceptRoutes, "accept-routes", acceptRouteDefault(goos), "accept routes advertised by other Mirage nodes")
	upf.BoolVar(&upArgs.acceptDNS, "accept-dns", true, "accept DNS configuration from the admin panel")
	upf.BoolVar(&upArgs.singleRoutes, "host-routes", true, "HIDDEN: install host routes to other Mirage nodes")
	upf.StringVar(&upArgs.exitNodeIP, "exit-node", "", "Mirage exit node (IP or base name) for internet traffic, or empty string to not use an exit node")
	upf.BoolVar(&upArgs.exitNodeAllowLANAccess, "exit-node-allow-lan-access", false, "Allow direct access to the local network when routing traffic via an exit node")
	upf.BoolVar(&upArgs.shieldsUp, "shields-up", false, "don't allow incoming connections")
	upf.BoolVar(&upArgs.runSSH, "ssh", false, "run an SSH server, permitting access per miragenet admin's declared policy")
	upf.StringVar(&upArgs.advertiseTags, "advertise-tags", "", "comma-separated ACL tags to request; each must start with \"tag:\" (e.g. \"tag:eng,tag:montreal,tag:ssh\")")
	upf.StringVar(&upArgs.hostname, "hostname", "", "hostname to use instead of the one provided by the OS")
	upf.StringVar(&upArgs.advertiseRoutes, "advertise-routes", "", "routes to advertise to other nodes (comma-separated, e.g. \"10.0.0.0/8,192.168.0.0/24\") or empty string to not advertise routes")
	upf.BoolVar(&upArgs.advertiseDefaultRoute, "advertise-exit-node", false, "offer to be an exit node for internet traffic for the miragenet")
	if safesocket.GOOSUsesPeerCreds(goos) {
		upf.StringVar(&upArgs.opUser, "operator", "", "Unix username to allow to operate on miraged without sudo")
	}
	switch goos {
	case "linux":
		upf.BoolVar(&upArgs.snat, "snat-subnet-routes", true, "source NAT traffic to local routes advertised with --advertise-routes")
		upf.StringVar(&upArgs.netfilterMode, "netfilter-mode", defaultNetfilterMode(), "netfilter mode (one of on, nodivert, off)")
	case "windows":
		upf.BoolVar(&upArgs.forceDaemon, "unattended", false, "run in \"Unattended Mode\" where Mirage keeps running even after the current GUI user logs out (Windows-only)")
	}
	upf.DurationVar(&upArgs.timeout, "timeout", 0, "maximum amount of time to wait for miraged to enter a Running state; default (0s) blocks forever")

	if cmd == "login" {
		upf.StringVar(&upArgs.profileName, "nickname", "", "short name for the account")
	}

	if cmd == "up" {
		// Some flags are only for "up", not "login".
		upf.BoolVar(&upArgs.json, "json", false, "output in JSON format (WARNING: format subject to change)")
		upf.BoolVar(&upArgs.reset, "reset", false, "reset unspecified settings to their default values")
		upf.BoolVar(&upArgs.forceReauth, "force-reauth", false, "force reauthentication")
		registerAcceptRiskFlag(upf, &upArgs.acceptedRisks)
	}

	return upf
}

func defaultNetfilterMode() string {
	if distro.Get() == distro.Synology {
		return "off"
	}
	return "on"
}

type upArgsT struct {
	qr                     bool
	reset                  bool
	server                 string
	acceptRoutes           bool
	acceptDNS              bool
	singleRoutes           bool
	exitNodeIP             string
	exitNodeAllowLANAccess bool
	shieldsUp              bool
	runSSH                 bool
	forceReauth            bool
	forceDaemon            bool
	advertiseRoutes        string
	advertiseDefaultRoute  bool
	advertiseTags          string
	snat                   bool
	netfilterMode          string
	authKeyOrFile          string // "secret" or "file:/path/to/secret"
	hostname               string
	opUser                 string
	json                   bool
	timeout                time.Duration
	acceptedRisks          string
	profileName            string
}

func (a upArgsT) getAuthKey() (string, error) {
	v := a.authKeyOrFile
	if file, ok := strings.CutPrefix(v, "file:"); ok {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	return v, nil
}

var upArgsGlobal upArgsT

// Fields output when `tailscale up --json` is used. Two JSON blocks will be output.
//
// When "tailscale up" is run it first outputs a block with AuthURL and QR populated,
// providing the link for where to authenticate this client. BackendState would be
// valid but boring, as it will almost certainly be "NeedsLogin". Error would be
// populated if something goes badly wrong.
//
// When the client is authenticated by having someone visit the AuthURL, a second
// JSON block will be output. The AuthURL and QR fields will not be present, the
// BackendState and Error fields will give the result of the authentication.
// Ex:
//
//	{
//	   "AuthURL": "https://login.tailscale.com/a/0123456789abcdef",
//	   "QR": "data:image/png;base64,0123...cdef"
//	   "BackendState": "NeedsLogin"
//	}
//
//	{
//	   "BackendState": "Running"
//	}
type upOutputJSON struct {
	AuthURL      string `json:",omitempty"` // Authentication URL of the form https://login.tailscale.com/a/0123456789
	QR           string `json:",omitempty"` // a DataURL (base64) PNG of a QR code AuthURL
	BackendState string `json:",omitempty"` // name of state like Running or NeedsMachineAuth
	Error        string `json:",omitempty"` // description of an error
}

func warnf(format string, args ...any) {
	printf("Warning: "+format+"\n", args...)
}

var (
	ipv4default = netip.MustParsePrefix("0.0.0.0/0")
	ipv6default = netip.MustParsePrefix("::/0")
)

func validateViaPrefix(ipp netip.Prefix) error {
	if !tsaddr.IsViaPrefix(ipp) {
		return fmt.Errorf("%v is not a 4-in-6 prefix", ipp)
	}
	if ipp.Bits() < (128 - 32) {
		return fmt.Errorf("%v 4-in-6 prefix must be at least a /%v", ipp, 128-32)
	}
	a := ipp.Addr().As16()
	// The first 64 bits of a are the via prefix.
	// The next 32 bits are the "site ID".
	// The last 32 bits are the IPv4.
	// For now, we reserve the top 3 bytes of the site ID,
	// and only allow users to use site IDs 0-255.
	siteID := binary.BigEndian.Uint32(a[8:12])
	if siteID > 0xFF {
		return fmt.Errorf("route %v contains invalid site ID %08x; must be 0xff or less", ipp, siteID)
	}
	return nil
}

func calcAdvertiseRoutes(advertiseRoutes string, advertiseDefaultRoute bool) ([]netip.Prefix, error) {
	routeMap := map[netip.Prefix]bool{}
	if advertiseRoutes != "" {
		var default4, default6 bool
		advroutes := strings.Split(advertiseRoutes, ",")
		for _, s := range advroutes {
			ipp, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, fmt.Errorf("%q is not a valid IP address or CIDR prefix", s)
			}
			if ipp != ipp.Masked() {
				return nil, fmt.Errorf("%s has non-address bits set; expected %s", ipp, ipp.Masked())
			}
			if tsaddr.IsViaPrefix(ipp) {
				if err := validateViaPrefix(ipp); err != nil {
					return nil, err
				}
			}
			if ipp == ipv4default {
				default4 = true
			} else if ipp == ipv6default {
				default6 = true
			}
			routeMap[ipp] = true
		}
		if default4 && !default6 {
			return nil, fmt.Errorf("%s advertised without its IPv6 counterpart, please also advertise %s", ipv4default, ipv6default)
		} else if default6 && !default4 {
			return nil, fmt.Errorf("%s advertised without its IPv4 counterpart, please also advertise %s", ipv6default, ipv4default)
		}
	}
	if advertiseDefaultRoute {
		routeMap[netip.MustParsePrefix("0.0.0.0/0")] = true
		routeMap[netip.MustParsePrefix("::/0")] = true
	}
	if len(routeMap) == 0 {
		return nil, nil
	}
	routes := make([]netip.Prefix, 0, len(routeMap))
	for r := range routeMap {
		routes = append(routes, r)
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Bits() != routes[j].Bits() {
			return routes[i].Bits() < routes[j].Bits()
		}
		return routes[i].Addr().Less(routes[j].Addr())
	})
	return routes, nil
}

// prefsFromUpArgs returns the ipn.Prefs for the provided args.
//
// Note that the parameters upArgs and warnf are named intentionally
// to shadow the globals to prevent accidental misuse of them. This
// function exists for testing and should have no side effects or
// outside interactions (e.g. no making Tailscale LocalAPI calls).
func prefsFromUpArgs(upArgs upArgsT, warnf logger.Logf, st *ipnstate.Status, goos string) (*ipn.Prefs, error) {
	routes, err := calcAdvertiseRoutes(upArgs.advertiseRoutes, upArgs.advertiseDefaultRoute)
	if err != nil {
		return nil, err
	}

	if upArgs.exitNodeIP == "" && upArgs.exitNodeAllowLANAccess {
		return nil, fmt.Errorf("--exit-node-allow-lan-access can only be used with --exit-node")
	}

	var tags []string
	if upArgs.advertiseTags != "" {
		tags = strings.Split(upArgs.advertiseTags, ",")
		for _, tag := range tags {
			err := tailcfg.CheckTag(tag)
			if err != nil {
				return nil, fmt.Errorf("tag: %q: %s", tag, err)
			}
		}
	}

	if err := dnsname.ValidHostname(upArgs.hostname); upArgs.hostname != "" && err != nil {
		return nil, err
	}

	prefs := ipn.NewPrefs()
	prefs.ControlURL = upArgs.server
	prefs.WantRunning = true
	prefs.RouteAll = upArgs.acceptRoutes
	if distro.Get() == distro.Synology {
		// ipn.NewPrefs returns a non-zero Netfilter default. But Synology only
		// supports "off" mode.
		prefs.NetfilterMode = preftype.NetfilterOff
	}
	if upArgs.exitNodeIP != "" {
		if err := prefs.SetExitNodeIP(upArgs.exitNodeIP, st); err != nil {
			var e ipn.ExitNodeLocalIPError
			if errors.As(err, &e) {
				return nil, fmt.Errorf("%w; did you mean --advertise-exit-node?", err)
			}
			return nil, err
		}
	}

	prefs.ExitNodeAllowLANAccess = upArgs.exitNodeAllowLANAccess
	prefs.CorpDNS = upArgs.acceptDNS
	prefs.AllowSingleHosts = upArgs.singleRoutes
	prefs.ShieldsUp = upArgs.shieldsUp
	prefs.RunSSH = upArgs.runSSH
	prefs.AdvertiseRoutes = routes
	prefs.AdvertiseTags = tags
	prefs.Hostname = upArgs.hostname
	prefs.ForceDaemon = upArgs.forceDaemon
	prefs.OperatorUser = upArgs.opUser
	prefs.ProfileName = upArgs.profileName

	if goos == "linux" {
		prefs.NoSNAT = !upArgs.snat

		switch upArgs.netfilterMode {
		case "on":
			prefs.NetfilterMode = preftype.NetfilterOn
		case "nodivert":
			prefs.NetfilterMode = preftype.NetfilterNoDivert
			warnf("netfilter=nodivert; add iptables calls to ts-* chains manually.")
		case "off":
			prefs.NetfilterMode = preftype.NetfilterOff
			if defaultNetfilterMode() != "off" {
				warnf("netfilter=off; configure iptables yourself.")
			}
		default:
			return nil, fmt.Errorf("invalid value --netfilter-mode=%q", upArgs.netfilterMode)
		}
	}
	return prefs, nil
}

// updatePrefs returns how to edit preferences based on the
// flag-provided 'prefs' and the currently active 'curPrefs'.
//
// It returns a non-nil justEditMP if we're already running and none of
// the flags require a restart, so we can just do an EditPrefs call and
// change the prefs at runtime (e.g. changing hostname, changing
// advertised routes, etc).
//
// It returns simpleUp if we're running a simple "tailscale up" to
// transition to running from a previously-logged-in but down state,
// without changing any settings.
func updatePrefs(prefs, curPrefs *ipn.Prefs, env upCheckEnv) (simpleUp bool, justEditMP *ipn.MaskedPrefs, err error) {
	if !env.upArgs.reset {
		applyImplicitPrefs(prefs, curPrefs, env)

		if err := checkForAccidentalSettingReverts(prefs, curPrefs, env); err != nil {
			return false, nil, err
		}
	}

	controlURLChanged := curPrefs.ControlURL != prefs.ControlURL &&
		!(ipn.IsLoginServerSynonym(curPrefs.ControlURL) && ipn.IsLoginServerSynonym(prefs.ControlURL))
	if controlURLChanged && env.backendState == ipn.Running.String() && !env.upArgs.forceReauth {
		return false, nil, fmt.Errorf("can't change --login-server without --force-reauth")
	}

	// Do this after validations to avoid the 5s delay if we're going to error
	// out anyway.
	wantSSH, haveSSH := env.upArgs.runSSH, curPrefs.RunSSH
	if err := presentSSHToggleRisk(wantSSH, haveSSH, env.upArgs.acceptedRisks); err != nil {
		return false, nil, err
	}

	if env.upArgs.forceReauth && isSSHOverTailscale() {
		if err := presentRiskToUser(riskLoseSSH, `You are connected over Mirage; this action will result in your SSH session disconnecting.`, env.upArgs.acceptedRisks); err != nil {
			return false, nil, err
		}
	}

	tagsChanged := !reflect.DeepEqual(curPrefs.AdvertiseTags, prefs.AdvertiseTags)

	simpleUp = env.flagSet.NFlag() == 0 &&
		curPrefs.Persist != nil &&
		curPrefs.Persist.LoginName != "" &&
		env.backendState != ipn.NeedsLogin.String()

	justEdit := env.backendState == ipn.Running.String() &&
		!env.upArgs.forceReauth &&
		env.upArgs.authKeyOrFile == "" &&
		!controlURLChanged &&
		!tagsChanged

	if justEdit {
		justEditMP = new(ipn.MaskedPrefs)
		justEditMP.WantRunningSet = true
		justEditMP.Prefs = *prefs
		visitFlags := env.flagSet.Visit
		if env.upArgs.reset {
			visitFlags = env.flagSet.VisitAll
		}
		visitFlags(func(f *flag.Flag) {
			updateMaskedPrefsFromUpOrSetFlag(justEditMP, f.Name)
		})
	}

	return simpleUp, justEditMP, nil
}

func presentSSHToggleRisk(wantSSH, haveSSH bool, acceptedRisks string) error {
	if !isSSHOverTailscale() || wantSSH == haveSSH {
		return nil
	}
	if wantSSH {
		return presentRiskToUser(riskLoseSSH, `You are connected over Mirage; this action will reroute SSH traffic to Mirage SSH and will result in your session disconnecting.`, acceptedRisks)
	}
	return presentRiskToUser(riskLoseSSH, `You are connected using Mirage SSH; this action will result in your session disconnecting.`, acceptedRisks)
}

func runUp(ctx context.Context, cmd string, args []string, upArgs upArgsT) (retErr error) {
	var egg bool
	if len(args) > 0 {
		egg = fmt.Sprint(args) == "[up down down left right left right b a]"
		if !egg {
			fatalf("too many non-flag arguments: %q", args)
		}
	}

	st, err := localClient.Status(ctx)
	if err != nil {
		return fixTailscaledConnectError(err)
	}
	origAuthURL := st.AuthURL

	// printAuthURL reports whether we should print out the
	// provided auth URL from an IPN notify.
	printAuthURL := func(url string) bool {
		if upArgs.authKeyOrFile != "" {
			// Issue 1755: when using an authkey, don't
			// show an authURL that might still be pending
			// from a previous non-completed interactive
			// login.
			return false
		}
		if upArgs.forceReauth && url == origAuthURL {
			return false
		}
		return true
	}

	if distro.Get() == distro.Synology {
		notSupported := "not supported on Synology" //; see https://github.com/tailscale/tailscale/issues/1995"
		if upArgs.acceptRoutes {
			return errors.New("--accept-routes is " + notSupported)
		}
		if upArgs.exitNodeIP != "" {
			return errors.New("--exit-node is " + notSupported)
		}
		if upArgs.netfilterMode != "off" {
			return errors.New("--netfilter-mode values besides \"off\" " + notSupported)
		}
	}

	prefs, err := prefsFromUpArgs(upArgs, warnf, st, effectiveGOOS())
	if err != nil {
		fatalf("%s", err)
	}

	if len(prefs.AdvertiseRoutes) > 0 {
		if err := localClient.CheckIPForwarding(context.Background()); err != nil {
			warnf("%v", err)
		}
	}

	curPrefs, err := localClient.GetPrefs(ctx)
	if err != nil {
		return err
	}
	if cmd == "up" {
		// "tailscale up" should not be able to change the
		// profile name.
		prefs.ProfileName = curPrefs.ProfileName
	}

	env := upCheckEnv{
		goos:          effectiveGOOS(),
		distro:        distro.Get(),
		user:          os.Getenv("USER"),
		flagSet:       upFlagSet,
		upArgs:        upArgs,
		backendState:  st.BackendState,
		curExitNodeIP: exitNodeIP(curPrefs, st),
	}

	defer func() {
		if retErr == nil {
			checkUpWarnings(ctx)
		}
	}()

	simpleUp, justEditMP, err := updatePrefs(prefs, curPrefs, env)
	if err != nil {
		fatalf("%s", err)
	}
	if justEditMP != nil {
		justEditMP.EggSet = egg
		_, err := localClient.EditPrefs(ctx, justEditMP)
		return err
	}

	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	watcher, err := localClient.WatchIPNBus(watchCtx, 0)
	if err != nil {
		return err
	}
	defer watcher.Close()

	go func() {
		interrupt := make(chan os.Signal, 1)
		signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)
		select {
		case <-interrupt:
			cancelWatch()
		case <-watchCtx.Done():
		}
	}()

	running := make(chan bool, 1) // gets value once in state ipn.Running
	pumpErr := make(chan error, 1)

	var printed bool // whether we've yet printed anything to stdout or stderr
	var loginOnce sync.Once
	startLoginInteractive := func() { loginOnce.Do(func() { localClient.StartLoginInteractive(ctx) }) }

	go func() {
		for {
			n, err := watcher.Next()
			if err != nil {
				pumpErr <- err
				return
			}
			if n.ErrMessage != nil {
				msg := *n.ErrMessage
				fatalf("backend error: %v\n", msg)
			}
			if s := n.State; s != nil {
				switch *s {
				case ipn.NeedsLogin:
					startLoginInteractive()
				case ipn.NeedsMachineAuth:
					printed = true
					if env.upArgs.json {
						printUpDoneJSON(ipn.NeedsMachineAuth, "")
					} else {
						fmt.Fprintf(Stderr, "\nTo approve your machine, visit (as admin):\n\n\t%s\n\n", prefs.AdminPageURL())
					}
				case ipn.Running:
					// Done full authentication process
					if env.upArgs.json {
						printUpDoneJSON(ipn.Running, "")
					} else if printed {
						// Only need to print an update if we printed the "please click" message earlier.
						fmt.Fprintf(Stderr, "Success.\n")
					}
					select {
					case running <- true:
					default:
					}
					cancelWatch()
				}
			}
			if url := n.BrowseToURL; url != nil && printAuthURL(*url) {
				printed = true
				if upArgs.json {
					js := &upOutputJSON{AuthURL: *url, BackendState: st.BackendState}

					q, err := qrcode.New(*url, qrcode.Medium)
					if err == nil {
						png, err := q.PNG(128)
						if err == nil {
							js.QR = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
						}
					}

					data, err := json.MarshalIndent(js, "", "\t")
					if err != nil {
						printf("upOutputJSON marshalling error: %v", err)
					} else {
						outln(string(data))
					}
				} else {
					fmt.Fprintf(Stderr, "\nTo authenticate, visit:\n\n\t%s\n\n", *url)
					if upArgs.qr {
						q, err := qrcode.New(*url, qrcode.Medium)
						if err != nil {
							log.Printf("QR code error: %v", err)
						} else {
							fmt.Fprintf(Stderr, "%s\n", q.ToString(false))
						}
					}
				}
			}
		}
	}()

	// Special case: bare "tailscale up" means to just start
	// running, if there's ever been a login.
	if simpleUp {
		_, err := localClient.EditPrefs(ctx, &ipn.MaskedPrefs{
			Prefs: ipn.Prefs{
				WantRunning: true,
			},
			WantRunningSet: true,
		})
		if err != nil {
			return err
		}
	} else {
		if err := localClient.CheckPrefs(ctx, prefs); err != nil {
			return err
		}

		authKey, err := upArgs.getAuthKey()
		if err != nil {
			return err
		}
		authKey, err = resolveAuthKey(ctx, authKey, upArgs.advertiseTags)
		if err != nil {
			return err
		}
		if err := localClient.Start(ctx, ipn.Options{
			AuthKey:     authKey,
			UpdatePrefs: prefs,
		}); err != nil {
			return err
		}
		if upArgs.forceReauth {
			startLoginInteractive()
		}
	}

	// This whole 'up' mechanism is too complicated and results in
	// hairy stuff like this select. We're ultimately waiting for
	// 'running' to be done, but even in the case where
	// it succeeds, other parts may shut down concurrently so we
	// need to prioritize reads from 'running' if it's
	// readable; its send does happen before the pump mechanism
	// shuts down. (Issue 2333)
	var timeoutCh <-chan time.Time
	if upArgs.timeout > 0 {
		timeoutTimer := time.NewTimer(upArgs.timeout)
		defer timeoutTimer.Stop()
		timeoutCh = timeoutTimer.C
	}
	select {
	case <-running:
		return nil
	case <-watchCtx.Done():
		select {
		case <-running:
			return nil
		default:
		}
		return watchCtx.Err()
	case err := <-pumpErr:
		select {
		case <-running:
			return nil
		default:
		}
		return err
	case <-timeoutCh:
		return errors.New(`timeout waiting for Mirage service to enter a Running state; check health with "mirage status"`)
	}
}

// upWorthWarning reports whether the health check message s is worth warning
// about during "tailscale up". Many of the health checks are noisy or confusing
// or very ephemeral and happen especially briefly at startup.
//
// TODO(bradfitz): change the server to send typed warnings with metadata about
// the health check, rather than just a string.
func upWorthyWarning(s string) bool {
	return strings.Contains(s, healthmsg.TailscaleSSHOnBut) ||
		strings.Contains(s, healthmsg.WarnAcceptRoutesOff)
}

func checkUpWarnings(ctx context.Context) {
	st, err := localClient.StatusWithoutPeers(ctx)
	if err != nil {
		// Ignore. Don't spam more.
		return
	}
	var warn []string
	for _, w := range st.Health {
		if upWorthyWarning(w) {
			warn = append(warn, w)
		}
	}
	if len(warn) == 0 {
		return
	}
	if len(warn) == 1 {
		printf("%s\n", warn[0])
		return
	}
	printf("# Health check warnings:\n")
	for _, m := range warn {
		printf("#     - %s\n", m)
	}
}

func printUpDoneJSON(state ipn.State, errorString string) {
	js := &upOutputJSON{BackendState: state.String(), Error: errorString}
	data, err := json.MarshalIndent(js, "", "  ")
	if err != nil {
		log.Printf("printUpDoneJSON marshalling error: %v", err)
	} else {
		outln(string(data))
	}
}

var (
	prefsOfFlag = map[string][]string{} // "exit-node" => ExitNodeIP, ExitNodeID
)

func init() {
	// Both these have the same ipn.Pref:
	addPrefFlagMapping("advertise-exit-node", "AdvertiseRoutes")
	addPrefFlagMapping("advertise-routes", "AdvertiseRoutes")

	// And this flag has two ipn.Prefs:
	addPrefFlagMapping("exit-node", "ExitNodeIP", "ExitNodeID")

	// The rest are 1:1:
	addPrefFlagMapping("accept-dns", "CorpDNS")
	addPrefFlagMapping("accept-routes", "RouteAll")
	addPrefFlagMapping("advertise-tags", "AdvertiseTags")
	addPrefFlagMapping("host-routes", "AllowSingleHosts")
	addPrefFlagMapping("hostname", "Hostname")
	addPrefFlagMapping("login-server", "ControlURL")
	addPrefFlagMapping("netfilter-mode", "NetfilterMode")
	addPrefFlagMapping("shields-up", "ShieldsUp")
	addPrefFlagMapping("snat-subnet-routes", "NoSNAT")
	addPrefFlagMapping("exit-node-allow-lan-access", "ExitNodeAllowLANAccess")
	addPrefFlagMapping("unattended", "ForceDaemon")
	addPrefFlagMapping("operator", "OperatorUser")
	addPrefFlagMapping("ssh", "RunSSH")
	addPrefFlagMapping("nickname", "ProfileName")
}

func addPrefFlagMapping(flagName string, prefNames ...string) {
	prefsOfFlag[flagName] = prefNames
	prefType := reflect.TypeOf(ipn.Prefs{})
	for _, pref := range prefNames {
		// Crash at runtime if there's a typo in the prefName.
		if _, ok := prefType.FieldByName(pref); !ok {
			panic(fmt.Sprintf("invalid ipn.Prefs field %q", pref))
		}
	}
}

// preflessFlag reports whether flagName is a flag that doesn't
// correspond to an ipn.Pref.
func preflessFlag(flagName string) bool {
	switch flagName {
	case "auth-key", "force-reauth", "reset", "qr", "json", "timeout", "accept-risk":
		return true
	}
	return false
}

func updateMaskedPrefsFromUpOrSetFlag(mp *ipn.MaskedPrefs, flagName string) {
	if preflessFlag(flagName) {
		return
	}
	if prefs, ok := prefsOfFlag[flagName]; ok {
		for _, pref := range prefs {
			reflect.ValueOf(mp).Elem().FieldByName(pref + "Set").SetBool(true)
		}
		return
	}
	panic(fmt.Sprintf("internal error: unhandled flag %q", flagName))
}

const accidentalUpPrefix = "Error: changing settings via 'mirage up' requires mentioning all\n" +
	"non-default flags. To proceed, either re-run your command with --reset or\n" +
	"use the command below to explicitly mention the current value of\n" +
	"all non-default settings:\n\n" +
	"\tmirage up"

// upCheckEnv are extra parameters describing the environment as
// needed by checkForAccidentalSettingReverts and friends.
type upCheckEnv struct {
	goos          string
	user          string
	flagSet       *flag.FlagSet
	upArgs        upArgsT
	backendState  string
	curExitNodeIP netip.Addr
	distro        distro.Distro
}

// checkForAccidentalSettingReverts (the "up checker") checks for
// people running "tailscale up" with a subset of the flags they
// originally ran it with.
//
// For example, in Tailscale 1.6 and prior, a user might've advertised
// a tag, but later tried to change just one other setting and forgot
// to mention the tag later and silently wiped it out. We now
// require --reset to change preferences to flag default values when
// the flag is not mentioned on the command line.
//
// curPrefs is what's currently active on the server.
//
// mp is the mask of settings actually set, where mp.Prefs is the new
// preferences to set, including any values set from implicit flags.
func checkForAccidentalSettingReverts(newPrefs, curPrefs *ipn.Prefs, env upCheckEnv) error {
	if curPrefs.ControlURL == "" {
		// Don't validate things on initial "up" before a control URL has been set.
		return nil
	}

	flagIsSet := map[string]bool{}
	env.flagSet.Visit(func(f *flag.Flag) {
		flagIsSet[f.Name] = true
	})

	if len(flagIsSet) == 0 {
		// A bare "tailscale up" is a special case to just
		// mean bringing the network up without any changes.
		return nil
	}

	// flagsCur is what flags we'd need to use to keep the exact
	// settings as-is.
	flagsCur := prefsToFlags(env, curPrefs)
	flagsNew := prefsToFlags(env, newPrefs)

	var missing []string
	for flagName := range flagsCur {
		valCur, valNew := flagsCur[flagName], flagsNew[flagName]
		if flagIsSet[flagName] {
			continue
		}
		if reflect.DeepEqual(valCur, valNew) {
			continue
		}
		if flagName == "login-server" && ipn.IsLoginServerSynonym(valCur) && ipn.IsLoginServerSynonym(valNew) {
			continue
		}
		if flagName == "accept-routes" && valNew == false && env.goos == "linux" && env.distro == distro.Synology {
			// Issue 3176. Old prefs had 'RouteAll: true' on disk, so ignore that.
			continue
		}
		if flagName == "netfilter-mode" && valNew == preftype.NetfilterOn && env.goos == "linux" && env.distro == distro.Synology {
			// Issue 6811. Ignore on Synology.
			continue
		}
		missing = append(missing, fmtFlagValueArg(flagName, valCur))
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)

	// Compute the stringification of the explicitly provided args in flagSet
	// to prepend to the command to run.
	var explicit []string
	env.flagSet.Visit(func(f *flag.Flag) {
		type isBool interface {
			IsBoolFlag() bool
		}
		if ib, ok := f.Value.(isBool); ok && ib.IsBoolFlag() {
			if f.Value.String() == "false" {
				explicit = append(explicit, "--"+f.Name+"=false")
			} else {
				explicit = append(explicit, "--"+f.Name)
			}
		} else {
			explicit = append(explicit, fmtFlagValueArg(f.Name, f.Value.String()))
		}
	})

	var sb strings.Builder
	sb.WriteString(accidentalUpPrefix)

	for _, a := range append(explicit, missing...) {
		fmt.Fprintf(&sb, " %s", a)
	}
	sb.WriteString("\n\n")
	return errors.New(sb.String())
}

// applyImplicitPrefs mutates prefs to add implicit preferences for the user operator.
// If the operator flag is passed no action is taken, otherwise this only needs to be set if it doesn't
// match the current user.
//
// curUser is os.Getenv("USER"). It's pulled out for testability.
func applyImplicitPrefs(prefs, oldPrefs *ipn.Prefs, env upCheckEnv) {
	explicitOperator := false
	env.flagSet.Visit(func(f *flag.Flag) {
		if f.Name == "operator" {
			explicitOperator = true
		}
	})

	if prefs.OperatorUser == "" && oldPrefs.OperatorUser == env.user && !explicitOperator {
		prefs.OperatorUser = oldPrefs.OperatorUser
	}
}

func flagAppliesToOS(flag, goos string) bool {
	switch flag {
	case "netfilter-mode", "snat-subnet-routes":
		return goos == "linux"
	case "unattended":
		return goos == "windows"
	}
	return true
}

func prefsToFlags(env upCheckEnv, prefs *ipn.Prefs) (flagVal map[string]any) {
	ret := make(map[string]any)

	exitNodeIPStr := func() string {
		if prefs.ExitNodeIP.IsValid() {
			return prefs.ExitNodeIP.String()
		}
		if prefs.ExitNodeID.IsZero() || !env.curExitNodeIP.IsValid() {
			return ""
		}
		return env.curExitNodeIP.String()
	}

	fs := newUpFlagSet(env.goos, new(upArgsT) /* dummy */, "up")
	fs.VisitAll(func(f *flag.Flag) {
		if preflessFlag(f.Name) {
			return
		}
		set := func(v any) {
			if flagAppliesToOS(f.Name, env.goos) {
				ret[f.Name] = v
			} else {
				ret[f.Name] = nil
			}
		}
		switch f.Name {
		default:
			panic(fmt.Sprintf("unhandled flag %q", f.Name))
		case "ssh":
			set(prefs.RunSSH)
		case "login-server":
			set(prefs.ControlURL)
		case "accept-routes":
			set(prefs.RouteAll)
		case "host-routes":
			set(prefs.AllowSingleHosts)
		case "accept-dns":
			set(prefs.CorpDNS)
		case "shields-up":
			set(prefs.ShieldsUp)
		case "exit-node":
			set(exitNodeIPStr())
		case "exit-node-allow-lan-access":
			set(prefs.ExitNodeAllowLANAccess)
		case "advertise-tags":
			set(strings.Join(prefs.AdvertiseTags, ","))
		case "hostname":
			set(prefs.Hostname)
		case "operator":
			set(prefs.OperatorUser)
		case "advertise-routes":
			var sb strings.Builder
			for i, r := range withoutExitNodes(prefs.AdvertiseRoutes) {
				if i > 0 {
					sb.WriteByte(',')
				}
				sb.WriteString(r.String())
			}
			set(sb.String())
		case "advertise-exit-node":
			set(hasExitNodeRoutes(prefs.AdvertiseRoutes))
		case "snat-subnet-routes":
			set(!prefs.NoSNAT)
		case "netfilter-mode":
			set(prefs.NetfilterMode.String())
		case "unattended":
			set(prefs.ForceDaemon)
		}
	})
	return ret
}

func fmtFlagValueArg(flagName string, val any) string {
	if val == true {
		return "--" + flagName
	}
	if val == "" {
		return "--" + flagName + "="
	}
	return fmt.Sprintf("--%s=%v", flagName, shellquote.Join(fmt.Sprint(val)))
}

func hasExitNodeRoutes(rr []netip.Prefix) bool {
	var v4, v6 bool
	for _, r := range rr {
		if r.Bits() == 0 {
			if r.Addr().Is4() {
				v4 = true
			} else if r.Addr().Is6() {
				v6 = true
			}
		}
	}
	return v4 && v6
}

// withoutExitNodes returns rr unchanged if it has only 1 or 0 /0
// routes. If it has both IPv4 and IPv6 /0 routes, then it returns
// a copy with all /0 routes removed.
func withoutExitNodes(rr []netip.Prefix) []netip.Prefix {
	if !hasExitNodeRoutes(rr) {
		return rr
	}
	var out []netip.Prefix
	for _, r := range rr {
		if r.Bits() > 0 {
			out = append(out, r)
		}
	}
	return out
}

// exitNodeIP returns the exit node IP from p, using st to map
// it from its ID form to an IP address if needed.
func exitNodeIP(p *ipn.Prefs, st *ipnstate.Status) (ip netip.Addr) {
	if p == nil {
		return
	}
	if p.ExitNodeIP.IsValid() {
		return p.ExitNodeIP
	}
	id := p.ExitNodeID
	if id.IsZero() {
		return
	}
	for _, p := range st.Peer {
		if p.ID == id {
			if len(p.TailscaleIPs) > 0 {
				return p.TailscaleIPs[0]
			}
			break
		}
	}
	return
}

func anyPeerAdvertisingRoutes(st *ipnstate.Status) bool {
	for _, ps := range st.Peer {
		if ps.PrimaryRoutes == nil {
			continue
		}
		if ps.PrimaryRoutes.Len() > 0 {
			return true
		}
	}
	return false
}

func init() {
	// Required to use our client API. We're fine with the instability since the
	// client lives in the same repo as this code.
	tailscale.I_Acknowledge_This_API_Is_Unstable = true
}

// resolveAuthKey either returns v unchanged (in the common case) or, if it
// starts with "tskey-client-" (as Tailscale OAuth secrets do) parses it like
//
//	tskey-client-xxxx[?ephemeral=false&bar&preauthorized=BOOL&baseURL=...]
//
// and does the OAuth2 dance to get and return an authkey. The "ephemeral"
// property defaults to true if unspecified. The "preauthorized" defaults to
// false. The "baseURL" defaults to https://api.tailscale.com.
// The passed in tags are required, and must be non-empty. These will be
// set on the authkey generated by the OAuth2 dance.
func resolveAuthKey(ctx context.Context, v, tags string) (string, error) {
	if !strings.HasPrefix(v, "tskey-client-") {
		return v, nil
	}
	if !envknob.Bool("TS_EXPERIMENT_OAUTH_AUTHKEY") {
		return "", errors.New("oauth authkeys are in experimental status")
	}
	if tags == "" {
		return "", errors.New("oauth authkeys require --advertise-tags")
	}

	clientSecret, named, _ := strings.Cut(v, "?")
	attrs, err := url.ParseQuery(named)
	if err != nil {
		return "", err
	}
	for k := range attrs {
		switch k {
		case "ephemeral", "preauthorized", "baseURL":
		default:
			return "", fmt.Errorf("unknown attribute %q", k)
		}
	}
	getBool := func(name string, def bool) (bool, error) {
		v := attrs.Get(name)
		if v == "" {
			return def, nil
		}
		ret, err := strconv.ParseBool(v)
		if err != nil {
			return false, fmt.Errorf("invalid attribute boolean attribute %s value %q", name, v)
		}
		return ret, nil
	}
	ephemeral, err := getBool("ephemeral", true)
	if err != nil {
		return "", err
	}
	preauth, err := getBool("preauthorized", false)
	if err != nil {
		return "", err
	}

	baseURL := "https://api.tailscale.com"
	if v := attrs.Get("baseURL"); v != "" {
		baseURL = v
	}

	credentials := clientcredentials.Config{
		ClientID:     "some-client-id", // ignored
		ClientSecret: clientSecret,
		TokenURL:     baseURL + "/api/v2/oauth/token",
		Scopes:       []string{"device"},
	}

	tsClient := tailscale.NewClient("-", nil)
	tsClient.HTTPClient = credentials.Client(ctx)
	tsClient.BaseURL = baseURL

	caps := tailscale.KeyCapabilities{
		Devices: tailscale.KeyDeviceCapabilities{
			Create: tailscale.KeyDeviceCreateCapabilities{
				Reusable:      false,
				Ephemeral:     ephemeral,
				Preauthorized: preauth,
				Tags:          strings.Split(tags, ","),
			},
		},
	}

	authkey, _, err := tsClient.CreateKey(ctx, caps)
	if err != nil {
		return "", err
	}
	return authkey, nil
}
