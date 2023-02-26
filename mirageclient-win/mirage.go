//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/ncruces/zenity"
	"tailscale.com/envknob"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/logpolicy"
	"tailscale.com/logtail"
	"tailscale.com/mirageclient-win/resource"
	"tailscale.com/mirageclient-win/systray"
	"tailscale.com/types/logger"
	"tailscale.com/util/osshare"
	"tailscale.com/util/winutil"

	"github.com/skratchdot/open-golang/open"

	"tailscale.com/client/tailscale"

	"github.com/atotto/clipboard"
	"github.com/rs/zerolog/log"
)

var backVersion string

var LC tailscale.LocalClient

var magicVersionCounter int

type NotifyType int

const (
	OpenURL NotifyType = iota
	RestartDaemon
	IntoRunning
)

var netMapChn chan bool
var prefChn chan bool

// some state channel
var stNeedLoginCh chan bool
var stStopCh chan bool
var stStartingCh chan bool
var stRunCh chan bool

// TODO： 以下新版本模式全局变量
var logPol *logpolicy.Policy // 日志策略（后台服务logtail使用）

var args struct { // 命令行参数部分
	debugDaemon bool // 仅用于方便调试服务的daemon

	asServiceInstaller   bool   // 执行服务安装
	asFirewallKillswitch bool   // 执行防火墙调整（被wgengine调用）
	tunGUID              string // 执行防火墙调整参数
	asServiceSubProc     bool   // 作为后台服务子进程被调用
	logid                string // 后台服务日志使用的logtail ID参数
} // 启动参数

var watcher *MiraWatcher // 通讯协程实体
var gui MiraMenu         // gui界面实体

var ctx context.Context
var cancel context.CancelFunc

func main() {
	//err1 := uninstallSystemDaemonWindows()
	//uninstallWinTun()
	//fmt.Println(err1)
	// cgao6: 以上仅调试时使用，发布时切勿开启

	envknob.PanicIfAnyEnvCheckedInInit()
	envknob.ApplyDiskConfig()
	// 开局先屏蔽TS的日志 （但后续保留日志设置，以防后续我们希望使用logtail）
	envknob.SetNoLogsNoSupport()

	flag.BoolVar(&args.debugDaemon, "debugD", false, "调试后台服务")

	flag.BoolVar(&args.asServiceInstaller, "install", false, "安装后台服务")
	flag.BoolVar(&args.asFirewallKillswitch, "firewall", false, "管理防火墙")
	flag.StringVar(&args.tunGUID, "tunGUID", "", "管理防火墙使用tun的GUID值")
	flag.BoolVar(&args.asServiceSubProc, "subproc", false, "是否服务的子进程调用")
	flag.StringVar(&args.logid, "logid", "", "服务子进程使用的logtail ID值")
	flag.Parse()

	// 判断是否调试daemon
	if args.debugDaemon {
		beWindowsSubprocess()
		return
	}

	// 判断是否是服务安装
	if beServiceInstaller() {
		return //结束安装
	}

	// 判断是否子进程
	if beWindowsSubprocess() {
		return //结束子进程
	}

	// 判断是Win服务调用则执行服务方法，并以子进程重调此程序
	if isWindowsService() {
		pol := logpolicy.New(logtail.CollectionNode)
		pol.SetVerbosityLevel(0) // 日志级别，越往上级别越高
		logPol = pol
		defer func() {
			// Finish uploading logs after closing everything else.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			pol.Shutdown(ctx)
		}()
		log.Info().Msgf("Running service...")
		if err := runWindowsService(pol); err != nil {
			log.Error().Msgf("runservice: %v", err)
		}
		log.Info().Msgf("Stopped file sharing.")
		osshare.SetFileSharingEnabled(false, logger.Discard)
		log.Info().Msgf("Service ended.")
		return //结束服务
	}

	// 客户端要保证单一进程
	_, err := winutil.CreateAppMutex("MirageWin")
	//_, err := CreateMutex("MirageWin")
	if err != nil {
		return
	}

	// 创建与后台服务的通讯员
	watcher = NewWatcher()

	// 非Win服务则执行后序GUI部分
	LC = tailscale.LocalClient{
		Socket:        socket_path,
		UseSocketOnly: false}
	ctx, cancel = context.WithCancel(context.Background())

	stNeedLoginCh = make(chan bool)
	stStopCh = make(chan bool)
	stStartingCh = make(chan bool)
	stRunCh = make(chan bool)

	netMapChn = make(chan bool)
	prefChn = make(chan bool)

	magicVersionCounter = 0

	onExit := func() {
	}

	systray.Run(onReady, onExit)
}

func onReady() {
	gui.init()

	go func() {
		go gui.logoSpin(300)

		// 开启通讯员
		go watcher.Start()

		getST()
		go gui.setNotLogin(backVersion)

		for {
			select {
			case newMsg := <-watcher.Tx:
				// 开启通讯员
				switch newMsg.Type {
				case Fatal: // 遇到通讯员无法恢复严重错误崩溃，导致程序只能由用户选择重启动通讯员或者退出程序
					go gui.setErr()
					go func(msg string) {
						feedback := zenity.Question("程序通讯员报错"+msg+"无法执行，重试还是退出？",
							zenity.WindowIcon(logo_png),
							zenity.Title("严重错误"),
							zenity.OKLabel("重试"),
							zenity.CancelLabel("退出"))
						if feedback == nil {
							go watcher.Start()
							return
						} else {
							systray.Quit()
							return
						}
					}(newMsg.data.(error).Error())
				case Error:
					logNotify(newMsg.data.(error).Error(), errors.New("收到通讯员错误："+newMsg.data.(error).Error()))
					go gui.setErr()
				}
				go watcher.Start()
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
				if st.Self.KeyExpiry != nil && !st.Self.KeyExpiry.After(time.Now().AddDate(0, 0, 7)) {
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
						LC.SetStore(ctx, string(ipn.CurrentServerCodeKey), newServerCode)
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
				open.Run(control_url + "/admin")
			case <-gui.loginMenu.ClickedCh:
				serverCodeData, err := LC.GetStore(ctx, string(ipn.CurrentServerCodeKey))
				if err != nil && err != ipn.ErrStateNotExist {
					logNotify("读取服务器代码出错", err)
				} else if err == ipn.ErrStateNotExist || serverCodeData == nil || string(serverCodeData) == "" {
					newServerCode, err := zenity.Entry("请输入您接入的控制器代码（留空默认）:",
						zenity.WindowIcon(logo_png),
						zenity.Title("初始化"),
						zenity.OKLabel("确定"),
						zenity.CancelLabel("取消"))
					if err == nil {
						if newServerCode == "" {
							newServerCode = "ipv4.uk"
						}
						err := LC.SetStore(ctx, string(ipn.CurrentServerCodeKey), newServerCode)
						if err != nil {
							logNotify("设置服务器代码出错", err)
							break
						}
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
					serverCode := string(serverCodeData)
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
