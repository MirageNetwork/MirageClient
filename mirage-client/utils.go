package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/gen2brain/beeep"
	"github.com/rs/zerolog/log"
	"tailscale.com/client/tailscale"
	"tailscale.com/hostinfo"
	"tailscale.com/ipn"
	"tailscale.com/types/preftype"
)

var logo_png string = "./resource/Mirage_logo.png"
var app_name string = "蜃境"
var control_url string = "https://sdp.ipv4.uk"
var socket_path string = `\\.\pipe\ProtectedPrefix\Administrators\Mirage\miraged`
var state_path string = filepath.Join(os.Getenv("ProgramData"), "Mirage", "server-state.conf")
var pref_path string = filepath.Join(os.Getenv("ProgramData"), "Mirage", "pref.conf")
var tun_name string = "Mirage"
var log_id string = "Mirage"
var engine_port uint16 = 41641

var (
	ipv4default = netip.MustParsePrefix("0.0.0.0/0")
	ipv6default = netip.MustParsePrefix("::/0")
)

func CreateDefaultPref() *ipn.Prefs {
	routes := make([]netip.Prefix, 0, 0)
	var tags []string
	prefs := ipn.NewPrefs()
	prefs.ControlURL = control_url
	prefs.WantRunning = true
	prefs.RouteAll = true
	prefs.ExitNodeAllowLANAccess = false
	prefs.CorpDNS = false
	prefs.AllowSingleHosts = true
	prefs.ShieldsUp = false
	prefs.RunSSH = false

	prefs.AdvertiseRoutes = routes
	prefs.AdvertiseTags = tags
	prefs.Hostname = ""
	prefs.ForceDaemon = true
	prefs.OperatorUser = ""
	prefs.NetfilterMode = preftype.NetfilterOn

	return prefs
}

func GetAllMaskedPref(ipnPref ipn.Prefs) ipn.MaskedPrefs {
	return ipn.MaskedPrefs{Prefs: ipnPref,
		ControlURLSet:             true,
		RouteAllSet:               true,
		AllowSingleHostsSet:       true,
		ExitNodeIDSet:             true,
		ExitNodeIPSet:             true,
		ExitNodeAllowLANAccessSet: true,
		CorpDNSSet:                true,
		RunSSHSet:                 true,
		WantRunningSet:            true,
		LoggedOutSet:              true,
		ShieldsUpSet:              true,
		AdvertiseTagsSet:          true,
		HostnameSet:               true,
		NotepadURLsSet:            true,
		ForceDaemonSet:            true,
		EggSet:                    true,
		AdvertiseRoutesSet:        true,
		NoSNATSet:                 true,
		NetfilterModeSet:          true,
		OperatorUserSet:           true,
	}
}

func SimpleRun(lc tailscale.LocalClient, ctx context.Context) {
	hi := hostinfo.New()
	fmt.Println(hi.Desktop)
	_, err := lc.EditPrefs(ctx, &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			WantRunning: true,
		},
		WantRunningSet: true,
	})
	if err != nil {
		log.Error().Msg(err.Error())
	}
}
func SimpleStop(lc tailscale.LocalClient, ctx context.Context) {
	_, err := lc.EditPrefs(ctx, &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			WantRunning: false,
		},
		WantRunningSet: true,
	})
	if err != nil {
		log.Error().Msg(err.Error())
	}
}
func SavePref(lc tailscale.LocalClient, ctx context.Context) {
	ipnPref, err := lc.GetPrefs(ctx)
	if err != nil {
		log.Error().Msg("Load Pref from current failed!")
		return
	}
	ipn.SavePrefs(pref_path, ipnPref)
}

func LoadPref(lc tailscale.LocalClient, ctx context.Context) {
	ipnPref, err := ipn.LoadPrefs(pref_path)
	if err != nil {
		log.Error().Msg("Can't read Prefs from the conf file!")
		return
	}
	maskedIPN := GetAllMaskedPref(*ipnPref)
	_, err3 := lc.EditPrefs(ctx, &maskedIPN)

	if err3 != nil {
		log.Error().Msg("Can't update the daemon status to Prefs saved before!")
		return
	}
}

func logNotify(msg string, err error) {
	log.Error().Msg(msg + err.Error())
	beeep.Notify(app_name, msg, logo_png)
}

func StartWatcher(ctx context.Context, localClient tailscale.LocalClient, isRunning chan bool, notLogin chan bool, notAuth chan bool, errHappen chan error, doLogin chan bool, stopWatch chan bool) {
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer func() {
		fmt.Println("utils-log-135")
		cancelWatch()
	}()
	watcher, err := localClient.WatchIPNBus(watchCtx, 0)
	if err != nil {
		logNotify("守护进程通讯无法建立", err)
		return
	}
	defer func() {
		fmt.Println("utils-log-144")
		watcher.Close()
	}()

	go func() {
		interrupt := make(chan os.Signal, 1)
		signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)
		select {
		case <-interrupt:
			fmt.Println("utils-log-146")
			cancelWatch()
		case <-watchCtx.Done():
			fmt.Println("utils-log-149")
		case <-stopWatch:
			fmt.Println("utils-log-151")
			cancelWatch()
		}
	}()

	go func() {
		for {
			n, err := watcher.Next()
			if err != nil {
				errHappen <- err
			}
			if n.ErrMessage != nil {
				msg := *n.ErrMessage
				errHappen <- errors.New(msg)
			}
			if s := n.State; s != nil {
				switch *s {
				case ipn.NeedsLogin:
					localClient.StartLoginInteractive(ctx)
					notLogin <- true
				case ipn.NeedsMachineAuth:
					notAuth <- true
				case ipn.Running:
					select {
					case isRunning <- true:
					default:
					}
					cancelWatch()
				}
			}
			st, _ := localClient.Status(ctx)
			fmt.Println(st.AuthURL)
			if url := n.BrowseToURL; url != nil && <-doLogin {
				var loginOnce sync.Once
				loginOnce.Do(func() { localClient.StartLoginInteractive(ctx) })
			}
		}
	}()

}
