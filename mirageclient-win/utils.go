package main

import (
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"
	"tailscale.com/ipn"
	"tailscale.com/types/netmap"
)

const logo_png string = "logo.png"
const app_name string = "蜃境"
const serviceName string = "Mirage"
const socket_path string = `\\.\pipe\ProtectedPrefix\Administrators\Mirage\miraged`
const engine_port uint16 = 0 //动态端口机制
var program_path string = filepath.Join(os.Getenv("ProgramData"), serviceName)
var control_url string = "https://sdp.ipv4.uk" //TODO: 改为读取conf文件，首次通过gui设置

var (
	ipv4default = netip.MustParsePrefix("0.0.0.0/0")
	ipv6default = netip.MustParsePrefix("::/0")
)

// 废弃: 设置连接菜单域以及用户菜单域登出处理函数
func (m *MiraMenu) SetConnFieldHandler(loginHnD, logoutHnD, connHnD, disconnHnd func()) {
	m.connectField.loginAction.Triggered().Attach(loginHnD)
	m.userField.logoutAction.Triggered().Attach(logoutHnD)
	m.connectField.connectAction.Triggered().Attach(connHnD)
	m.connectField.disconnectAction.Triggered().Attach(disconnHnd)
}

// 废弃: 设置首选项菜单域处理函数
func (m *MiraMenu) SetPrefFieldHandler(useDNSHnD, useSubnetHnD func(en bool) error) {
	m.prefField.prefUsingDNSAction.Triggered().Attach(func() {
		err := useDNSHnD(m.prefField.prefUsingDNSAction.Checked())
		if err != nil {
			log.Printf("切换DNS使用开关失败%s", err)
		}
	})
	m.prefField.prefUsingSubnetAction.Triggered().Attach(func() {
		err := useSubnetHnD(m.prefField.prefUsingSubnetAction.Checked())
		if err != nil {
			log.Printf("切换子网转发使用开关失败%s", err)
		}
	})

	m.prefField.aboutAction.Triggered().Attach(func() {
		msg := "蜃境客户端版本：" + strings.Split(m.backendData.Version, "-")[0]
		if len(strings.Split(m.backendData.Version, "-")) > 1 {
			msg += " (" + strings.TrimPrefix(m.backendData.Version, strings.Split(m.backendData.Version, "-")[0]+"-") + ")"
		}
		if m.backendData.ClientVersion != nil {
			if m.backendData.ClientVersion.RunningLatest {
				msg += "\n已是最新版本"
			} else if m.backendData.ClientVersion.Notify {
				msg += "\n有更新版本：" + m.backendData.ClientVersion.LatestVersion + "\n是否去更新？"
				msgid := walk.MsgBox(m.mw, "关于蜃境", msg, walk.MsgBoxIconInformation|walk.MsgBoxOKCancel)
				if msgid == 1 {
					openURLInBrowser(m.backendData.ClientVersion.NotifyURL)
				}
				return
			} else {
				msg += "\n最新版本未知"
			}
		} else {
			msg += "\n最新版本未知"
		}
		walk.MsgBox(m.mw, "关于蜃境", msg, walk.MsgBoxIconInformation)
	})

}

func (m *MiraMenu) UpdateRunState(state ipn.State) {
	m.runState.Set(state)
}
func (m *MiraMenu) UpdateVersion(version string) {
	m.backendData.SetVersion(version)
}
func (m *MiraMenu) UpdatePrefs(prefs *ipn.Prefs) {
	m.backendData.SetPrefs(prefs)
}
func (m *MiraMenu) UpdateNetmap(netmap *netmap.NetworkMap) {
	m.backendData.SetNetMap(netmap)
}

func (m *MiraMenu) SetPrefsLoader(prefsLoader func() (*ipn.Prefs, error)) {
	m.prefsLoader = prefsLoader
}

func (m *MiraMenu) changeIconDueRunState(data interface{}) {
	switch ipn.State(m.runState.state) {
	case ipn.NeedsLogin:
		m.setIcon(Logo)
	case ipn.NoState:
		m.setIcon(HasIssue)
	case ipn.Stopped:
		m.setIcon(Disconn)
	case ipn.Running:
		switch true {
		case m.backendData.Prefs.AdvertisesExitNode():
			m.setIcon(AsExit)
		case !m.backendData.Prefs.ExitNodeID.IsZero() || m.backendData.Prefs.ExitNodeIP.IsValid():
			m.setIcon(Exit)
		default:
			m.setIcon(Conn)
		}
	case ipn.Starting:
		// TODO: 设置旋转logo
	}

}

func openURLInBrowser(url string) {
	win.ShellExecute(0, nil, syscall.StringToUTF16Ptr(url), nil, nil, win.SW_SHOWDEFAULT)
}

type NotifyLvL int // 通知等级
const (
	NL_Msg   NotifyLvL = iota // 普通消息
	NL_Info                   // 信息
	NL_Warn                   // 警告
	NL_Error                  // 错误
)

// SendNotify 发送通知到系统弹出消息（会同时记录日志）
func (s *MiraMenu) SendNotify(title string, msg string, level NotifyLvL) {
	var send func(string, string) error
	switch level {
	case NL_Msg:
		send = s.tray.ShowMessage
	case NL_Info:
		send = s.tray.ShowInfo
	case NL_Warn:
		send = s.tray.ShowWarning
	case NL_Error:
		send = s.tray.ShowError
	}

	if msg != "" {
		log.Printf("[小喇叭] 标题: %s; 内容: %s", title, msg)
		err := send(title, msg)
		if err != nil {
			log.Printf("发送通知失败: %s", err)
		}
	} else {
		log.Printf("[小喇叭]: %s; ", title)
		send("", title)
	}
}
