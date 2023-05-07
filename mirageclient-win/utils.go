//go:build windows

package main

import (
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"
	"tailscale.com/ipn"
)

const serviceName = "Mirage"
const defaultServerCode = "sdp.nopkt.com"
const socketPath = `\\.\pipe\ProtectedPrefix\Administrators\Mirage\miraged`
const enginePort = 0    //0 -动态端口机制
const debugPort = 54321 // 调试信息页面端口

var programPath string = filepath.Join(os.Getenv("ProgramData"), serviceName)

type BackendVersion string
type WatcherUpEvent struct{}

// 根据运行状态设置图标
func (m *MiraMenu) ChangeIconDueRunState() {
	switch ipn.State(m.data.State) {
	case ipn.NeedsLogin:
		m.setIcon(Logo)
	case ipn.NoState:
		m.setIcon(HasIssue)
	case ipn.Stopped:
		m.setIcon(Disconn)
	case ipn.Running:
		switch true {
		case m.data.Prefs.AdvertisesExitNode():
			m.setIcon(AsExit)
		case !m.data.Prefs.ExitNodeID.IsZero() || m.data.Prefs.ExitNodeIP.IsValid():
			m.setIcon(Exit)
		default:
			m.setIcon(Conn)
		}
	case ipn.Starting:
		stopSpinner := make(chan struct{})
		m.data.StateChanged().Once(func(data interface{}) {
			stopSpinner <- struct{}{}
		})
		go func(stateChanged <-chan struct{}) {
			iconPtr := true
			ticker := time.NewTicker(300 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
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
func OpenURLInBrowser(url string) {
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

// PopConfirmDlg 弹出确认对话框
func PopConfirmDlg(title, msg string, w, h int) (confirm bool) {
	dlg, err := walk.NewDialogWithFixedSize(nil)
	if err != nil {
		log.Printf("[工具人] 创建对话框出错: %v", err)
	}
	dlg.SetName(title)
	dlg.SetTitle(title)
	// 设置对话框的图标
	dlg.SetIcon(Icons[Logo])
	dlg.SetMinMaxSize(walk.Size{Width: w, Height: h}, walk.Size{Width: w, Height: h})
	dlg.SetX(int(win.GetSystemMetrics(win.SM_CXSCREEN)/2 - int32(w)/2))
	dlg.SetY(int(win.GetSystemMetrics(win.SM_CYSCREEN)/2 - int32(h)/2))
	vboxLayout := walk.NewVBoxLayout()
	vboxLayout.SetMargins(walk.Margins{HNear: 10, VNear: 10, HFar: 10, VFar: 10})

	brusher, err := walk.NewSolidColorBrush(walk.RGB(250, 250, 250))
	if err != nil {
		log.Printf("[工具人] 创建画刷出错: %v", err)
	}
	dlg.SetBackground(brusher)
	dlg.SetLayout(vboxLayout)

	label, err := walk.NewTextLabel(dlg)
	if err != nil {
		log.Printf("[工具人] 创建标签出错: %v", err)
	}
	label.SetText(msg)
	label.SetAlignment(walk.AlignHCenterVCenter)
	label.SetMinMaxSize(walk.Size{Width: w - 20, Height: h - 50}, walk.Size{Width: w - 20, Height: h - 50})
	font, err := walk.NewFont("微软雅黑", 9, 0)
	if err != nil {
		log.Printf("[工具人] 创建字体出错: %v", err)
	}
	label.SetFont(font)

	// 创建按钮
	btns, err := walk.NewComposite(dlg)
	if err != nil {
		log.Printf("[工具人] 创建按钮组合框出错: %v", err)
	}
	btns.SetLayout(walk.NewHBoxLayout())

	// 创建确认按钮
	confirmBtn, err := walk.NewPushButton(btns)
	if err != nil {
		log.Printf("[工具人] 创建确认按钮出错: %v", err)
	}
	confirmBtn.SetText("确认")

	// 创建取消按钮
	cancelBtn, err := walk.NewPushButton(btns)
	if err != nil {
		log.Printf("[工具人] 创建取消按钮出错: %v", err)
	}
	cancelBtn.SetText("取消")

	// 确认按钮点击事件
	confirmBtn.Clicked().Attach(func() {
		confirm = true
		dlg.Accept()
	})

	// 取消按钮点击事件
	cancelBtn.Clicked().Attach(func() {
		dlg.Cancel()
	})

	// 显示对话框
	dlg.Run()
	return
}

// popTextInputDlg 弹出文本输入框
// title: 标题
// label: 标签
// confirm: 用户是否确认
// value: 用户输入的值
func PopTextInputDlg(title, inputtip string) (confirm bool, value string) {
	dlg, err := walk.NewDialogWithFixedSize(nil)
	if err != nil {
		log.Printf("[工具人] 创建对话框出错: %v", err)
	}
	dlg.SetName(title)
	dlg.SetTitle(title)
	// 设置对话框的图标
	dlg.SetIcon(Icons[Logo])
	dlg.SetMinMaxSize(walk.Size{Width: 300, Height: 100}, walk.Size{Width: 300, Height: 100})
	dlg.SetX(int(win.GetSystemMetrics(win.SM_CXSCREEN)/2 - 150))
	dlg.SetY(int(win.GetSystemMetrics(win.SM_CYSCREEN)/2 - 50))
	dlg.SetLayout(walk.NewVBoxLayout())

	label, err := walk.NewTextLabel(dlg)
	if err != nil {
		log.Printf("[工具人] 创建标签出错: %v", err)
	}
	label.SetText(inputtip)
	urlInput, err := walk.NewLineEdit(dlg)
	if err != nil {
		log.Printf("[工具人] 创建输入框出错: %v", err)
	}

	composite, err := walk.NewComposite(dlg)
	if err != nil {
		log.Printf("[工具人] 创建复合控件出错: %v", err)
	}
	composite.SetLayout(walk.NewHBoxLayout())

	okBtn, err := walk.NewPushButton(composite)
	if err != nil {
		log.Printf("创建按钮出错: %v", err)
	}
	okBtn.SetText("确定")
	okBtn.Clicked().Attach(func() {
		value = urlInput.Text()
		dlg.Accept()
	})
	cancelBtn, err := walk.NewPushButton(composite)
	if err != nil {
		log.Printf("[工具人] 创建按钮出错: %v", err)
	}
	cancelBtn.SetText("取消")
	cancelBtn.Clicked().Attach(func() {
		dlg.Cancel()
	})

	// 显示对话框
	dlgrt := dlg.Run()
	if dlgrt == walk.DlgCmdOK {
		confirm = true
	}
	return
}
