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
	"sync"
	"syscall"
	"time"

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

var mu sync.Mutex
var notifyCh chan Notify
var stopDaemonCh chan bool
var releaseTrayCh chan bool

var netMapChn chan bool

type DevMenuPool struct {
	Item systray.MenuItem
	Peer ipnstate.PeerStatus
}

var myDevPool map[netip.Addr]DevMenuPool

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

	systray.SetTemplateIcon(resource.LogoIcon, resource.LogoIcon)
	systray.SetTitle("蜃境")
	systray.SetTooltip("简单安全的组网工具")

	loginMenu := systray.AddMenuItem("登录…", "点击进行登录")
	connectMenu := systray.AddMenuItem("连接", "点击接入蜃境")
	disconnMenu := systray.AddMenuItem("断开", "临时切断蜃境连接")
	systray.AddSeparator()
	userMenu := systray.AddMenuItem("", "")
	userLogoutMenu := userMenu.AddSubMenuItem("登出", "")
	systray.AddSeparator()
	nodeMenu := systray.AddMenuItem("本设备", "单击复制本节点IP")
	devicesMenu := systray.AddMenuItem("网内设备", "显示你可见的全部设备")
	myDevMenu := devicesMenu.AddSubMenuItem("我的设备", "隶属于我的设备")
	myDevNetMenu := myDevMenu.AddSubMenuItem("", "")
	myDevNetMenu.Disable()
	myDevSepar := myDevMenu.AddSubMenuItem("——————", "")
	myDevSepar.Disable()
	myDevPool = make(map[netip.Addr]DevMenuPool)
	devicesMenu.Hide()
	systray.AddSeparator()
	versionMenu := systray.AddMenuItem(backVersion, "点击查看详细信息")
	mQuit := systray.AddMenuItem("退出", "退出蜃境")

	connectMenu.Hide()
	disconnMenu.Hide()
	userMenu.Hide()
	nodeMenu.Hide()
	versionMenu.Hide()
	loginMenu.Hide()

	justLogin := false
	go func() {
		ctxD = context.Background()
		go func(stopLogoSpin chan bool) {
			for {
				select {
				case <-stopLogoSpin:
					return
				default:
					systray.SetTemplateIcon(resource.Mlogo1, resource.Mlogo1)
					<-time.After(300 * time.Millisecond)
					systray.SetTemplateIcon(resource.Mlogo2, resource.Mlogo2)
					<-time.After(300 * time.Millisecond)
				}
			}
		}(releaseTrayCh)
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
			versionMenu.SetTitle(backVersion)
			if st != nil && !justLogin {
				switch st.BackendState {
				case "NeedsLogin":
					systray.SetTemplateIcon(resource.LogoIcon, resource.LogoIcon)
					userMenu.SetTitle("请先登录")
					userMenu.Disable()
					connectMenu.Hide()
					disconnMenu.Hide()
					loginMenu.Enable()
					loginMenu.SetTitle("登录")
					loginMenu.Show()
					nodeMenu.Hide()
					devicesMenu.Hide()
				case "Stopped":
					systray.SetTemplateIcon(resource.Logom, resource.Logom)
					loginMenu.Hide()
					userMenu.Enable()
					userMenu.SetTitle(st.User[st.Self.UserID].DisplayName)
					userMenu.Show()
					connectMenu.Show()
					disconnMenu.Hide()
					nodeMenu.SetTitle("本设备")
					nodeMenu.Disable()
					nodeMenu.Show()
					devicesMenu.Hide()
				case "Running":
					systray.SetTemplateIcon(resource.Mlogo, resource.Mlogo)
					loginMenu.Hide()
					userMenu.Enable()
					userMenu.SetTitle(st.User[st.Self.UserID].DisplayName)
					userMenu.Show()
					connectMenu.Hide()
					disconnMenu.Show()
					if len(st.TailscaleIPs) > 0 {
						nodeMenu.SetTitle("本设备：" + st.Self.HostName + " (" + st.TailscaleIPs[0].String() + ")")
					}
					nodeMenu.Enable()
					nodeMenu.Show()
					for _, myDevItem := range myDevPool {
						myDevItem.Item.Hide()
					}
					for _, peer := range st.Peer {
						if peer.UserID == st.Self.UserID && peer.Online {
							needCreateNewMenuItem := true
							tmpIPAddr := peer.TailscaleIPs[0]
							if tmpIPAddr.Is6() {
								tmpIPAddr = peer.TailscaleIPs[1]
							}

							for ip, myDevMenuItem := range myDevPool {
								if ip.Compare(tmpIPAddr) == 0 {
									myDevMenuItem.Item.Show()
									myDevMenuItem.Item.SetTitle(peer.HostName)
									myDevMenuItem.Peer = *peer
									needCreateNewMenuItem = false
									break
								}
							}
							if needCreateNewMenuItem {
								tmpMyDevSubMenu := myDevMenu.AddSubMenuItem(peer.HostName, tmpIPAddr.String())
								myDevPool[tmpIPAddr] = DevMenuPool{
									Item: *tmpMyDevSubMenu,
									Peer: *peer,
								}
								go func(menuItem DevMenuPool) {
									for {
										select {
										case <-menuItem.Item.ClickedCh:
											if menuItem.Peer.TailscaleIPs[0].Is4() {
												clipboard.WriteAll(menuItem.Peer.TailscaleIPs[0].String())
											} else {
												clipboard.WriteAll(menuItem.Peer.TailscaleIPs[1].String())
											}
											logNotify("设备"+menuItem.Peer.HostName+"的IP已复制", errors.New(""))
										}
									}
								}(myDevPool[tmpIPAddr])
							}
						}
					}
					for uid, user := range st.User {
						if uid == st.Self.UserID {
							myDevNetMenu.SetTitle(user.LoginName)
							break
						}
					}
					devicesMenu.Show()
				}
			}
			select {
			case <-mQuit.ClickedCh:
				systray.Quit()
				fmt.Println("退出...")
				continue
			case <-versionMenu.ClickedCh:
				fmt.Println("you clicked version")
				continue

			case <-loginMenu.ClickedCh:
				go func(stopLogoSpin chan bool) {
					for {
						select {
						case <-stopLogoSpin:
							return
						default:
							loginMenu.Disable()
							loginMenu.SetTitle("登录中…")
							systray.SetTemplateIcon(resource.Mlogo1, resource.Mlogo1)
							<-time.After(300 * time.Millisecond)
							loginMenu.Disable()
							loginMenu.SetTitle("登录中…")
							systray.SetTemplateIcon(resource.Mlogo2, resource.Mlogo2)
							<-time.After(300 * time.Millisecond)
						}
					}
				}(releaseTrayCh)
				kickOffLogin(notifyCh)
				justLogin = true
				continue
			case <-userLogoutMenu.ClickedCh:
				LC.Logout(ctx)
				continue
			case <-connectMenu.ClickedCh:
				doConn()
				continue
			case <-disconnMenu.ClickedCh:
				doDisconn()
				continue
			case <-nodeMenu.ClickedCh:
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
