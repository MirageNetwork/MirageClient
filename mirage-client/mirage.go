package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/mirage-client/resource"

	"github.com/getlantern/systray"
	"github.com/skratchdot/open-golang/open"

	"tailscale.com/client/tailscale"

	"github.com/atotto/clipboard"
	"github.com/rs/zerolog/log"
)

var ctx, ctxD context.Context

var backVersion string

var LC tailscale.LocalClient

type NotifyType int

const (
	OpenURL NotifyType = iota
	RestartDaemon
	IntoRunning
)

var stopDaemonCh chan bool

var netMapChn chan bool

var watcherUpCh chan bool

// some state channel
var stNeedLoginCh chan bool
var stStopCh chan bool
var stRunCh chan bool

var authURL string
var wantRun bool

var gui MirageMenu

func main() {
	LC = tailscale.LocalClient{
		Socket:        socket_path,
		UseSocketOnly: false}
	ctx = context.Background()
	stopDaemonCh = make(chan bool)

	watcherUpCh = make(chan bool)
	stNeedLoginCh = make(chan bool)
	stStopCh = make(chan bool)
	stRunCh = make(chan bool)

	netMapChn = make(chan bool)

	authURL = ""
	wantRun = false

	onExit := func() {
	}

	go WatchDaemon(ctx)

	systray.Run(onReady, onExit)
}

func onReady() {
	gui.init()

	go func() {
		ctxD = context.Background()
		go gui.logoSpin(300)
		go StartDaemon(ctxD, false, stopDaemonCh)

		getST()
		gui.setNotLogin(backVersion)

		for {
			select {
			case <-stNeedLoginCh:
				getST()
				gui.setNotLogin(backVersion)
			case <-stStopCh:
				st := getST()
				gui.setStopped(st.User[st.Self.UserID].DisplayName, backVersion)
			case <-stRunCh:
				st := getST()
				if authURL != "" {
					authURL = ""
					systray.SetTemplateIcon(resource.Mlogo, resource.Mlogo)
					logNotify("已连接", errors.New(""))
				}

				if st.TailscaleIPs[0].Is4() {
					gui.setRunning(st.User[st.Self.UserID].DisplayName, st.Self.HostName, st.TailscaleIPs[0].String(), backVersion)
				} else {
					gui.setRunning(st.User[st.Self.UserID].DisplayName, st.Self.HostName, st.TailscaleIPs[1].String(), backVersion)
				}
				gui.nodeListMenu.update(st)
			case <-gui.quitMenu.ClickedCh:
				systray.Quit()
				fmt.Println("退出...")
			case <-gui.versionMenu.ClickedCh:
				fmt.Println("you clicked version")
			case <-gui.registerMenu.ClickedCh:
				open.Run(adduser_url)
			case <-gui.loginMenu.ClickedCh:
				wantRun = true
				if authURL != "" {
					open.Run(authURL)
				} else {
					kickLogin()
				}
			case <-gui.userLogoutMenu.ClickedCh:
				wantRun = false
				LC.Logout(ctx)
			case <-gui.connectMenu.ClickedCh:
				doConn()
			case <-gui.disconnMenu.ClickedCh:
				doDisconn()
			case <-gui.nodeMenu.ClickedCh:
				st := getST()
				if len(st.TailscaleIPs) > 0 {
					clipboard.WriteAll(st.TailscaleIPs[0].String())
					logNotify("您的本设备IP已复制", errors.New(""))
				}
			case <-netMapChn:
				st := getST()
				gui.nodeListMenu.update(st)
				fmt.Println("Refresh menu due to netmap rcvd")
			}
		}
	}()
}

func doConn() {
	_, err := LC.EditPrefs(ctx, &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			WantRunning: true,
		},
		WantRunningSet: true,
	})
	if err != nil {
		log.Error().Msg("Change state to run failed!")
		return
	}
}

func doDisconn() {
	st, err := LC.Status(ctx)
	if err != nil {
		log.Error().Msg("Get current status failed!")
		return
	}
	if st.BackendState == "Running" {
		_, err = LC.EditPrefs(ctx, &ipn.MaskedPrefs{
			Prefs: ipn.Prefs{
				WantRunning: false,
			},
			WantRunningSet: true,
		})
		if err != nil {
			log.Error().Msg("Disconnect failed!")
		}
	}
}

func getST() *ipnstate.Status {
	st, err := LC.Status(ctx)
	if err != nil || st == nil {
		log.Error().
			Msg(`Get Status ERROR!`)
		return nil
	} else {
		//log.Info().Msg("Daemon: " + st.Version)
		backVersion = "正元蜃境" + strings.Split(st.Version, "-")[0]
		return st
	}
}
