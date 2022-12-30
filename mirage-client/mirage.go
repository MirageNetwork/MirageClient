package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/mirage-client/resource"

	"github.com/getlantern/systray"

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

type Notify struct {
	NType NotifyType
	NMsg  string
}

var notifyCh chan Notify
var stopDaemonCh chan bool
var releaseTrayCh chan bool

var netMapChn chan bool

type DevMenuPool struct {
	Item systray.MenuItem
	Peer ipnstate.PeerStatus
}

var myDevPool map[netip.Addr]DevMenuPool

var gui MirageMenu

func main() {

	LC = tailscale.LocalClient{
		Socket:        socket_path,
		UseSocketOnly: false}
	ctx = context.Background()
	notifyCh = make(chan Notify, 1)
	stopDaemonCh = make(chan bool)
	releaseTrayCh = make(chan bool)

	netMapChn = make(chan bool)

	onExit := func() {
	}

	go WatchDaemon(ctx, netMapChn)

	systray.Run(onReady, onExit)
}

func WatchDaemon(ctx context.Context, netMapCh chan bool) {
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	watcher, err := LC.WatchIPNBus(watchCtx, 0)
	if err != nil {
		logNotify("守护进程监听管道建立失败", err)
		return
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
	for {
		n, err := watcher.Next()
		if err != nil {
			fmt.Println("[ERROR] " + err.Error())
			continue
		}
		if nm := n.NetMap; nm != nil {
			netMapChn <- true
		}
	}
}

func onReady() {
	gui.init()

	justLogin := false
	go func() {
		ctxD = context.Background()
		go logoSpin(releaseTrayCh, 300)
		go StartDaemon(ctxD, false, stopDaemonCh)

		for {
			st, err := LC.Status(ctx)
			if err != nil {
				log.Error().
					Msg(`Get Status ERROR!`)

			} else if st != nil && st.BackendState != "NoState" && st.BackendState != "Starting" {
				if st.BackendState == "Stopped" && st.User[st.Self.UserID].DisplayName == "" {
					continue
				}
				if st.BackendState != "NeedsLogin" {
					for {
						st, err = LC.Status(ctx)
						if err == nil && (st.BackendState == "Stopped" && st.User[st.Self.UserID].DisplayName != "" || st.BackendState == "Running") {
							break
						}
					}
				}
				break
			}
		}
		releaseTrayCh <- true
		systray.SetTemplateIcon(resource.LogoIcon, resource.LogoIcon)

		for {
			st, err := LC.Status(ctx)
			if err != nil || st == nil {
				log.Error().
					Msg(`Get Status ERROR!`)

			} else {
				log.Info().Msg("Daemon: " + st.Version)
				backVersion = strings.Split(st.Version, "-")[0]
			}
			if st != nil && !justLogin {
				switch st.BackendState {
				case "NeedsLogin":
					gui.setNotLogin(backVersion)
				case "Stopped":
					gui.setStopped(st.User[st.Self.UserID].DisplayName, backVersion)
				case "Running":
					if st.TailscaleIPs[0].Is4() {
						gui.setRunning(st.User[st.Self.UserID].DisplayName, st.Self.HostName, st.TailscaleIPs[0].String(), backVersion)
					} else {
						gui.setRunning(st.User[st.Self.UserID].DisplayName, st.Self.HostName, st.TailscaleIPs[1].String(), backVersion)
					}
					log.Info().Msg("Update the GUI nodelist")
					gui.nodeListMenu.update(st)
				}
			}
			select {
			case <-gui.quitMenu.ClickedCh:
				systray.Quit()
				fmt.Println("退出...")
				continue
			case <-gui.versionMenu.ClickedCh:
				fmt.Println("you clicked version")
				continue

			case <-gui.loginMenu.ClickedCh:
				go logoSpin(releaseTrayCh, 300)
				kickOffLogin(notifyCh)
				justLogin = true
				continue
			case <-gui.userLogoutMenu.ClickedCh:
				LC.Logout(ctx)
				continue
			case <-gui.connectMenu.ClickedCh:
				doConn()
				continue
			case <-gui.disconnMenu.ClickedCh:
				doDisconn()
				continue
			case <-gui.nodeMenu.ClickedCh:
				if len(st.TailscaleIPs) > 0 {
					clipboard.WriteAll(st.TailscaleIPs[0].String())
					logNotify("您的本设备IP已复制", errors.New(""))
				}
				continue
			case <-netMapChn:
				fmt.Println("Refresh menu due to netmap rcvd")
				continue
			case msg := <-notifyCh:
				switch msg.NType {
				case IntoRunning:
					st, err := LC.Status(ctx)
					if err != nil {
						log.Error().
							Msg(`Get Status ERROR!`)
						justLogin = false
						continue
					} else if len(st.TailscaleIPs) < 1 {
						stopDaemonCh <- true
						fmt.Println("首次接入同步状态，请稍后…")
						select {
						case v := <-stopDaemonCh:
							fmt.Println(v)
						}

						socket_path = socket_path + "_"
						LC = tailscale.LocalClient{
							Socket:        socket_path,
							UseSocketOnly: false}

						newctxD := context.Background()
						fmt.Println("开始重启Daemon")
						go StartDaemon(newctxD, false, stopDaemonCh)
						for {
							st, err := LC.Status(ctx)
							if err != nil {
								log.Error().
									Msg(`Get Status ERROR!`)
							} else if st != nil && st.BackendState == "Running" {
								break
							}
						}
					}
					justLogin = false
					releaseTrayCh <- true
					systray.SetTemplateIcon(resource.Mlogo, resource.Mlogo)
					logNotify("已连接", errors.New(""))
				}
				continue
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

func doUpdatePrefs(st *ipnstate.Status, prefs, curPrefs *ipn.Prefs) (simpleUp bool, justEditMP *ipn.MaskedPrefs, err error) {
	if prefs.OperatorUser == "" && curPrefs.OperatorUser == os.Getenv("USER") {
		prefs.OperatorUser = curPrefs.OperatorUser
	}
	tagsChanged := !reflect.DeepEqual(curPrefs.AdvertiseTags, prefs.AdvertiseTags)
	simpleUp = curPrefs.Persist != nil &&
		curPrefs.Persist.LoginName != "" &&
		st.BackendState != ipn.NeedsLogin.String()
	justEdit := st.BackendState == ipn.Running.String() && !tagsChanged

	if justEdit {
		justEditMP = new(ipn.MaskedPrefs)
		justEditMP.WantRunningSet = true
		justEditMP.Prefs = *prefs
		justEditMP.ControlURLSet = true
	}

	return simpleUp, justEditMP, nil
}
