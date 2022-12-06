package main

import (
	"context"
	"fmt"
	"os"
	"reflect"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/mirage-client/resource"

	"github.com/getlantern/systray"
	"github.com/skratchdot/open-golang/open"

	"tailscale.com/client/tailscale"

	"github.com/atotto/clipboard"
	"github.com/gen2brain/beeep"
	"github.com/rs/zerolog/log"
)

var ctx context.Context
var backVersion string

var LC tailscale.LocalClient

type NotifyType int

const (
	OpenURL NotifyType = iota
)

type Notify struct {
	NType NotifyType
	NMsg  string
}

var notifyCh chan Notify

func main() {

	LC = tailscale.LocalClient{
		Socket:        "",
		UseSocketOnly: false}
	ctx = context.Background()
	notifyCh = make(chan Notify, 1)

	onExit := func() {
		doCleanUp()
	}
	ctxD := context.Background()
	go StartDaemon(ctxD, false)

	systray.Run(onReady, onExit)
}

func onReady() {

	systray.SetTemplateIcon(resource.LogoIcon, resource.LogoIcon)
	systray.SetTitle("蜃境")
	systray.SetTooltip("简单安全的组网工具")

	go func() {

		loginMenu := systray.AddMenuItem("登录…", "点击进行登录")
		connectMenu := systray.AddMenuItem("连接", "点击接入蜃境")
		disconnMenu := systray.AddMenuItem("断开", "临时切断蜃境连接")
		systray.AddSeparator()
		userMenu := systray.AddMenuItem("", "")
		userLogoutMenu := userMenu.AddSubMenuItem("登出", "")
		nodeMenu := systray.AddMenuItem("本设备", "单击复制本节点IP")
		systray.AddSeparator()
		versionMenu := systray.AddMenuItem(backVersion, "点击查看详细信息")
		mQuit := systray.AddMenuItem("退出", "退出蜃境")

		for {

			st, err := LC.Status(ctx)
			if err != nil {
				log.Error().
					Msg(`Get Status ERROR!`)

			} /* else {
				log.Info().Msg("Daemon: " + st.Version)
				backVersion = strings.Split(st.Version, "-")[0]
			}
			versionMenu.SetTitle(backVersion)

			switch st.BackendState {
			case "NeedsLogin":
				userMenu.SetTitle("请先登录")
				userMenu.Disable()
				connectMenu.Hide()
				disconnMenu.Hide()
				loginMenu.Show()
				nodeMenu.Hide()
			case "Stopped":
				loginMenu.Hide()
				userMenu.Enable()
				userMenu.SetTitle(st.User[st.Self.UserID].LoginName)
				userMenu.Show()
				connectMenu.Show()
				disconnMenu.Hide()
				nodeMenu.SetTitle("本设备")
				nodeMenu.Disable()
				nodeMenu.Show()
			case "Running":
				loginMenu.Hide()
				userMenu.Enable()
				userMenu.SetTitle(st.User[st.Self.UserID].LoginName)
				userMenu.Show()
				connectMenu.Hide()
				disconnMenu.Show()
				if len(st.Self.TailscaleIPs) > 0 {
					nodeMenu.SetTitle("本设备：" + st.Self.HostName + " (" + st.Self.TailscaleIPs[0].String() + ")")
				}
				nodeMenu.Enable()
				nodeMenu.Show()
			}
			*/
			select {
			case <-mQuit.ClickedCh:
				systray.Quit()
				fmt.Println("退出...")
				continue
			case <-versionMenu.ClickedCh:
				fmt.Println("you clicked version")
				continue
			case <-loginMenu.ClickedCh:
				kickOffLogin()
				//open.Run(st.AuthURL)
				continue
			case <-userLogoutMenu.ClickedCh:
				doSavePref()
				LC.Logout(ctx)
				continue
			case <-connectMenu.ClickedCh:
				doConn()
				continue
			case <-disconnMenu.ClickedCh:
				doDisconn()
				continue
			case <-nodeMenu.ClickedCh:
				if len(st.Self.TailscaleIPs) > 0 {
					clipboard.WriteAll(st.Self.TailscaleIPs[0].String())
					beeep.Notify("蜃境", "您的本设备IP已复制", "Mirage_logo.png")
				}
				continue
			case msg := <-notifyCh:
				if msg.NType == OpenURL {
					open.Run(msg.NMsg)
				} else {
					beeep.Notify("蜃境", msg.NMsg, "Mirage_logo.png")
				}
				continue
			}
		}
	}()

}

func doInit() {
	var ipnPref *ipn.Prefs
	_, err := os.Stat(prefFileName)
	if err == nil {
		ipnPref, err = ipn.LoadPrefs(prefFileName)
		if err != nil {
			log.Error().Msg("Can't read Prefs from the conf file!")
			return
		}
	} else {
		ipnPref = CreateDefaultPref()
	}
	LC.Start(ctx, ipn.Options{
		AuthKey:     "",
		UpdatePrefs: ipnPref,
	})
}

func doCleanUp() {
	doSavePref()
	doDisconn()
}

func doSavePref() {
	ipnPref, err := LC.GetPrefs(ctx)
	if err != nil {
		log.Error().Msg("Load Pref from current failed!")
		return
	}
	ipn.SavePrefs(prefFileName, ipnPref)
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
