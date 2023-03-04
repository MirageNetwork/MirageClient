package main

import (
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"syscall"
	"time"

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

type BackendVersion string
type WatcherUpEvent struct{}

func (m *MiraMenu) UpdateRunState(stateStr string) {
	state := map[string]ipn.State{
		"NoState":          ipn.NoState,
		"InUseOtherUser":   ipn.InUseOtherUser,
		"NeedsLogin":       ipn.NeedsLogin,
		"NeedsMachineAuth": ipn.NeedsMachineAuth,
		"Stopped":          ipn.Stopped,
		"Starting":         ipn.Starting,
		"Running":          ipn.Running,
	}[stateStr]
	m.backendData.SetState(state)
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

func (m *MiraMenu) changeIconDueRunState(data interface{}) {
	switch ipn.State(m.backendData.State) {
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
		stopSpinner := make(chan struct{})
		m.backendData.StateChanged().Once(func(data interface{}) {
			stopSpinner <- struct{}{}
		})
		go func(stateChanged <-chan struct{}) {
			iconPtr := true
			for {
				select {
				case <-time.Tick(300 * time.Millisecond):
					if iconPtr {
						m.setIcon(Ing1)
					} else {
						m.setIcon(Ing2)
					}
					iconPtr = !iconPtr
				case <-stateChanged:
					return
				}
			}
		}(stopSpinner)
	}

}

// openURLInBrowser 在浏览器中打开指定的url
func (s *MiraMenu) openURLInBrowser(url string) {
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
