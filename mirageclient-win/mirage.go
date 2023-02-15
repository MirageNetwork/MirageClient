//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ncruces/zenity"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnlocal"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/mirageclient-win/resource"
	"tailscale.com/mirageclient-win/systray"

	"github.com/skratchdot/open-golang/open"

	"tailscale.com/client/tailscale"

	"github.com/atotto/clipboard"
	"github.com/rs/zerolog/log"
)

var ctx, ctxD context.Context

var backVersion string

var LC tailscale.LocalClient
var LBChn chan *ipnlocal.LocalBackend
var LB *ipnlocal.LocalBackend

var magicVersionCounter int

type NotifyType int

const (
	OpenURL NotifyType = iota
	RestartDaemon
	IntoRunning
)

var netMapChn chan bool
var prefChn chan bool

var watcherUpCh chan bool

// some state channel
var stNeedLoginCh chan bool
var stStopCh chan bool
var stStartingCh chan bool
var stRunCh chan bool

var gui MirageMenu

func main() {
	_, err := CreateMutex("MirageWin")
	if err != nil {
		return
	}

	LC = tailscale.LocalClient{
		Socket:        socket_path,
		UseSocketOnly: false}
	ctx = context.Background()

	watcherUpCh = make(chan bool)
	stNeedLoginCh = make(chan bool)
	stStopCh = make(chan bool)
	stStartingCh = make(chan bool)
	stRunCh = make(chan bool)

	netMapChn = make(chan bool)
	prefChn = make(chan bool)

	LBChn = make(chan *ipnlocal.LocalBackend)
	magicVersionCounter = 0

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
		go StartDaemon(ctxD, false, LBChn)

		getST()
		go gui.setNotLogin(backVersion)

		LB := <-LBChn
		for {
			select {
			case <-stNeedLoginCh:
				getST()
				go gui.setNotLogin(backVersion)
			case <-stStopCh:
				refreshPrefs()
				st := getST()
				go gui.setStopped(st.User[st.Self.UserID].DisplayName, backVersion)
			case <-stStartingCh:
				st := getST()
				go gui.setStarting(st.User[st.Self.UserID].DisplayName, backVersion)
			case <-stRunCh:
				st := getST()
				systray.SetIcon(resource.Mlogo)
				logNotify("已连接", errors.New(""))
				lastDays := ""
				if !st.Self.KeyExpiry.After(time.Now().AddDate(0, 0, 7)) {
					lastDays = strings.TrimSuffix((st.Self.KeyExpiry.Sub(time.Now()) / time.Duration(time.Hour*24)).String(), "ns")
					go func(lastDays string) {

						feedback := zenity.Question("该设备还有"+lastDays+"天过期",
							zenity.WindowIcon(logo_png),
							zenity.Title("临期设备提醒"),
							zenity.OKLabel("登录延期"),
							zenity.CancelLabel("暂时不"))
						if feedback == nil {
							LC.StartLoginInteractive(ctx)
							return
						} else {
							return
						}
					}(lastDays)
				}

				if st.TailscaleIPs[0].Is4() {
					go gui.setRunning(st.User[st.Self.UserID].DisplayName, strings.Split(st.Self.DNSName, ".")[0], st.TailscaleIPs[0].String(), backVersion, lastDays)
				} else {
					go gui.setRunning(st.User[st.Self.UserID].DisplayName, strings.Split(st.Self.DNSName, ".")[0], st.TailscaleIPs[1].String(), backVersion, lastDays)
				}
				refreshPrefs()
				gui.nodeListMenu.update(st)
				gui.exitNodeMenu.update(st)
			case <-gui.quitMenu.ClickedCh:
				systray.Quit()
				fmt.Println("退出...")
			case <-gui.versionMenu.ClickedCh:
				if magicVersionCounter == 0 {
					go func() {
						<-time.After(10 * time.Second)
						magicVersionCounter = 0
					}()
				}
				magicVersionCounter++
				if magicVersionCounter == 3 {
					magicVersionCounter = 0
					newServerCode, err := zenity.Entry("新控制器代码（留空默认，下次登录生效）:",
						zenity.WindowIcon(logo_png),
						zenity.Title("重设定"),
						zenity.OKLabel("确定"),
						zenity.CancelLabel("取消"))
					if err == nil {
						if newServerCode == "" {
							newServerCode = "ipv4.uk"
						}
						LB.SetServerCode(newServerCode)
					}
				}
			case <-gui.optDNSMenu.ClickedCh:
				switchDNSOpt(!gui.optDNSMenu.Checked())
			case <-gui.optSubnetMenu.ClickedCh:
				switchSubnetOpt(!gui.optSubnetMenu.Checked())
			case <-gui.exitNodeMenu.AllowLocalNetworkAccess.ClickedCh:
				switchAllowLocalNet(!gui.exitNodeMenu.AllowLocalNetworkAccess.Checked())
			case <-gui.exitNodeMenu.NoneExit.ClickedCh:
				switchExitNode("")
			case <-gui.exitNodeMenu.RunExitNode.ClickedCh:
				if !gui.exitNodeMenu.RunExitNode.Checked() {
					go func() {
						feedback := zenity.Question("将该设备用作出口节点意味着您的蜃境网络中的其他设备可以将它们的网络流量通过您的IP发送",
							zenity.WindowIcon(logo_png),
							zenity.Title("用作出口节点？"),
							zenity.OKLabel("确定"),
							zenity.CancelLabel("取消"))
						if feedback == nil {
							turnonExitNode()
							return
						} else {
							return
						}
					}()
				} else {
					turnoffExitNode()
				}
			case <-gui.userConsoleMenu.ClickedCh:
				open.Run(console_url)
			case <-gui.loginMenu.ClickedCh:
				if LB.GetServerCode() == "" {
					newServerCode, err := zenity.Entry("请输入您接入的控制器代码（留空默认）:",
						zenity.WindowIcon(logo_png),
						zenity.Title("初始化"),
						zenity.OKLabel("确定"),
						zenity.CancelLabel("取消"))
					if err == nil {
						if newServerCode == "" {
							newServerCode = "ipv4.uk"
						}
						LB.SetServerCode(newServerCode)
						control_url = "https://sdp." + newServerCode
						if !strings.Contains(newServerCode, ".") {
							control_url = control_url + ".com"
						}
						st := getST()
						if st.BackendState != "Running" {
							kickLogin()
						}
						LC.StartLoginInteractive(ctx)
					}
				} else {
					serverCode := LB.GetServerCode()
					control_url = "https://sdp." + serverCode
					if !strings.Contains(serverCode, ".") {
						control_url = control_url + ".com"
					}

					st := getST()
					if st.BackendState != "Running" {
						kickLogin()
					}
					LC.StartLoginInteractive(ctx)
				}
			case <-gui.userLogoutMenu.ClickedCh:
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
				if st.BackendState == "Stopped" {
					refreshPrefs()
					go gui.setStopped(st.User[st.Self.UserID].DisplayName, backVersion)
				} else if st.BackendState == "Running" {
					lastDays := ""
					if !st.Self.KeyExpiry.After(time.Now().AddDate(0, 0, 7)) {
						lastDays = strings.TrimSuffix((st.Self.KeyExpiry.Sub(time.Now()) / time.Duration(time.Hour*24)).String(), "ns")
					}
					if st.TailscaleIPs[0].Is4() {
						go gui.setRunning(st.User[st.Self.UserID].DisplayName, strings.Split(st.Self.DNSName, ".")[0], st.TailscaleIPs[0].String(), backVersion, lastDays)
					} else {
						go gui.setRunning(st.User[st.Self.UserID].DisplayName, strings.Split(st.Self.DNSName, ".")[0], st.TailscaleIPs[1].String(), backVersion, lastDays)
					}
					refreshPrefs()
					gui.nodeListMenu.update(st)
					gui.exitNodeMenu.update(st)
				}
				fmt.Println("Refresh menu due to netmap rcvd")
			case <-prefChn:
				refreshPrefs()
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
	if st.BackendState == "Running" || st.BackendState == "Starting" {
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
		backVersion = "蜃境" + strings.Split(st.Version, "-")[0]
		return st
	}
}
