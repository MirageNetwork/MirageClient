package main

import (
	"log"
	"net/netip"
	"strings"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"
	"tailscale.com/ipn"
	"tailscale.com/net/tsaddr"
	"tailscale.com/types/preftype"
)

func (m *MiraMenu) doConn() {
	_, err := m.lc.EditPrefs(m.ctx, &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			WantRunning: true,
		},
		WantRunningSet: true,
	})
	if err != nil {
		log.Printf("Change state to run failed!")
		return
	}
}

func (m *MiraMenu) doDisconn() {
	st, err := m.lc.Status(m.ctx)
	if err != nil {
		log.Printf("Get current status failed!")
		return
	}
	if st.BackendState == "Running" || st.BackendState == "Starting" {
		_, err = m.lc.EditPrefs(m.ctx, &ipn.MaskedPrefs{
			Prefs: ipn.Prefs{
				WantRunning: false,
			},
			WantRunningSet: true,
		})
		if err != nil {
			log.Printf("Disconnect failed!")
		}
	}
}

func (m *MiraMenu) doLogout() {
	m.lc.Logout(m.ctx)
}

func (m *MiraMenu) doLogin() {
	serverCodeData, err := m.lc.GetStore(m.ctx, string(ipn.CurrentServerCodeKey))
	if err != nil && !strings.Contains(err.Error(), ipn.ErrStateNotExist.Error()) {
		go m.SendNotify("读取服务器代码出错", err.Error(), NL_Error)
	} else if err != nil && strings.Contains(err.Error(), ipn.ErrStateNotExist.Error()) || serverCodeData == nil || string(serverCodeData) == "" {
		m.tray.SetVisible(false)
		confirm, newServerCode := PopTextInputDlg("初始化", "请输入您接入的控制器代码（留空默认）:")
		m.tray.SetVisible(true)
		log.Printf("doLogin: %v, %v", confirm, newServerCode)
		if confirm {
			if newServerCode == "" {
				newServerCode = "ipv4.uk"
			}
			err := m.lc.SetStore(m.ctx, string(ipn.CurrentServerCodeKey), newServerCode)
			if err != nil {
				go m.SendNotify("设置服务器代码出错", err.Error(), NL_Error)
				return
			}
			m.control_url = "https://sdp." + newServerCode
			if !strings.Contains(newServerCode, ".") {
				m.control_url = m.control_url + ".com"
			}
			st, err := m.lc.Status(m.ctx)
			if err != nil {
				go m.SendNotify("获取状态出错", err.Error(), NL_Error)
				return
			}
			if st.BackendState != "Running" {
				m.kickLogin()
			}
			m.lc.StartLoginInteractive(m.ctx)
		}
	} else {
		serverCode := string(serverCodeData)
		m.control_url = "https://sdp." + serverCode
		if !strings.Contains(serverCode, ".") {
			m.control_url = m.control_url + ".com"
		}

		st, err := m.lc.Status(m.ctx)
		if err != nil {
			go m.SendNotify("获取状态出错", err.Error(), NL_Error)
			return
		}
		if st.BackendState != "Running" {
			m.kickLogin()
		}
		m.lc.StartLoginInteractive(m.ctx)
	}
}

func (m *MiraMenu) kickLogin() {
	prefs := m.createPref()
	if err := m.lc.CheckPrefs(m.ctx, prefs); err != nil {
		go m.SendNotify("Pref出错", err.Error(), NL_Error)
	}
	if err := m.lc.Start(m.ctx, ipn.Options{
		AuthKey:     m.backendData.AuthKey,
		UpdatePrefs: prefs,
	}); err != nil {
		go m.SendNotify("无法开始", err.Error(), NL_Error)
	}
}

func (m *MiraMenu) createPref() *ipn.Prefs {
	routes := make([]netip.Prefix, 0, 0)
	var tags []string
	prefs := ipn.NewPrefs()
	prefs.ControlURL = m.control_url
	prefs.WantRunning = true
	prefs.RouteAll = m.prefField.prefUsingSubnetAction.Checked()
	prefs.ExitNodeAllowLANAccess = m.exitField.exitAllowLocalAction.Checked()
	prefs.CorpDNS = m.prefField.prefUsingDNSAction.Checked()
	prefs.AllowSingleHosts = true
	prefs.ShieldsUp = m.prefField.prefAllowIncomeAction.Checked()
	prefs.RunSSH = false

	prefs.AdvertiseRoutes = routes
	prefs.AdvertiseTags = tags
	prefs.Hostname = ""
	prefs.ForceDaemon = m.prefField.prefUnattendAction.Checked()
	prefs.LoggedOut = false
	prefs.OperatorUser = ""
	prefs.NetfilterMode = preftype.NetfilterOn

	return prefs
}

func PopConfirmDlg(title, msg string, w, h int) (confirm bool) {
	dlg, err := walk.NewDialogWithFixedSize(nil)
	if err != nil {
		log.Printf("[工具人] 创建对话框出错: %v", err)
	}
	dlg.SetName(title)
	dlg.SetTitle(title)
	// 设置对话框的图标
	dlg.SetIcon(icons[Logo])
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
	dlg.SetIcon(icons[Logo])
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
func (m *MiraMenu) showAbout() {
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
				m.openURLInBrowser(m.backendData.ClientVersion.NotifyURL)
			}
			return
		} else {
			msg += "\n最新版本未知"
		}
	} else {
		msg += "\n最新版本未知"
	}
	walk.MsgBox(m.mw, "关于蜃境", msg, walk.MsgBoxIconInformation)
}

func (m *MiraMenu) setAllowIncome() {
	newV := m.prefField.prefAllowIncomeAction.Checked()
	m.prefField.prefAllowIncomeAction.SetChecked(!newV)
	maskedPrefs := &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			ShieldsUp: !newV,
		},
		ShieldsUpSet: true,
	}
	curPrefs, err := m.lc.GetPrefs(m.ctx)
	if err != nil {
		go m.SendNotify("设置允许入流量", "获取Pref出错"+err.Error(), NL_Error)
		return
	}

	checkPrefs := curPrefs.Clone()
	checkPrefs.ApplyEdits(maskedPrefs)
	if err := m.lc.CheckPrefs(m.ctx, checkPrefs); err != nil {
		go m.SendNotify("设置允许入流量", "Pref检查出错:"+err.Error(), NL_Error)
		return
	}

	_, err = m.lc.EditPrefs(m.ctx, maskedPrefs)
	if err != nil {
		go m.SendNotify("设置允许入流量", "设置Pref出错:"+err.Error(), NL_Error)
		return
	}
}

func (m *MiraMenu) setDNSOpt() {
	newV := m.prefField.prefUsingDNSAction.Checked()
	m.prefField.prefUsingDNSAction.SetChecked(!newV)
	maskedPrefs := &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			CorpDNS: newV,
		},
		CorpDNSSet: true,
	}
	curPrefs, err := m.lc.GetPrefs(m.ctx)
	if err != nil {
		go m.SendNotify("设置使用DNS选项", "获取Pref出错"+err.Error(), NL_Error)
		return
	}

	checkPrefs := curPrefs.Clone()
	checkPrefs.ApplyEdits(maskedPrefs)
	if err := m.lc.CheckPrefs(m.ctx, checkPrefs); err != nil {
		go m.SendNotify("设置使用DNS选项", "Pref检查出错:"+err.Error(), NL_Error)
		return
	}

	_, err = m.lc.EditPrefs(m.ctx, maskedPrefs)
	if err != nil {
		go m.SendNotify("设置使用DNS选项", "设置Pref出错:"+err.Error(), NL_Error)
		return
	}
}

func (m *MiraMenu) setSubnetOpt() {
	newV := m.prefField.prefUsingSubnetAction.Checked()
	m.prefField.prefUsingSubnetAction.SetChecked(!newV)
	maskedPrefs := &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			RouteAll: newV,
		},
		RouteAllSet: true,
	}
	curPrefs, err := m.lc.GetPrefs(m.ctx)
	if err != nil {
		go m.SendNotify("设置使用子网选项", "获取Pref出错:"+err.Error(), NL_Error)
		return // err
	}

	checkPrefs := curPrefs.Clone()
	checkPrefs.ApplyEdits(maskedPrefs)
	if err := m.lc.CheckPrefs(m.ctx, checkPrefs); err != nil {
		go m.SendNotify("设置使用子网选项", "Pref检查出错:"+err.Error(), NL_Error)
		return // err
	}

	_, err = m.lc.EditPrefs(m.ctx, maskedPrefs)
	if err != nil {
		go m.SendNotify("设置使用子网选项", "设置Pref出错:"+err.Error(), NL_Error)
		return // err
	}
}
func (m *MiraMenu) setUnattendOpt() {
	newV := m.prefField.prefUnattendAction.Checked()
	m.prefField.prefUnattendAction.SetChecked(!newV)
	maskedPrefs := &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			ForceDaemon: newV,
		},
		ForceDaemonSet: true,
	}
	curPrefs, err := m.lc.GetPrefs(m.ctx)
	if err != nil {
		go m.SendNotify("设置无人值守", "获取Pref出错:"+err.Error(), NL_Error)
		return // err
	}

	checkPrefs := curPrefs.Clone()
	checkPrefs.ApplyEdits(maskedPrefs)
	if err := m.lc.CheckPrefs(m.ctx, checkPrefs); err != nil {
		go m.SendNotify("设置无人值守", "Pref检查出错:"+err.Error(), NL_Error)
		return // err
	}

	_, err = m.lc.EditPrefs(m.ctx, maskedPrefs)
	if err != nil {
		go m.SendNotify("设置无人值守", "设置Pref出错:"+err.Error(), NL_Error)
		return // err
	}
}

func (m *MiraMenu) setPrefsDefault() {
	maskedPrefs := &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			ShieldsUp:   false,
			CorpDNS:     true,
			RouteAll:    true,
			ForceDaemon: false,
		},
		ShieldsUpSet:   true,
		CorpDNSSet:     true,
		RouteAllSet:    true,
		ForceDaemonSet: true,
	}
	curPrefs, err := m.lc.GetPrefs(m.ctx)
	if err != nil {
		go m.SendNotify("恢复默认配置", "获取Pref出错:"+err.Error(), NL_Error)
		return // err
	}

	checkPrefs := curPrefs.Clone()
	checkPrefs.ApplyEdits(maskedPrefs)
	if err := m.lc.CheckPrefs(m.ctx, checkPrefs); err != nil {
		go m.SendNotify("恢复默认配置", "Pref检查出错:"+err.Error(), NL_Error)
		return // err
	}

	_, err = m.lc.EditPrefs(m.ctx, maskedPrefs)
	if err != nil {
		go m.SendNotify("恢复默认配置", "设置Pref出错:"+err.Error(), NL_Error)
		return // err
	}
}

func (m *MiraMenu) setAllowLocalNet() {
	newV := m.exitField.exitAllowLocalAction.Checked()
	maskedPrefs := &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			ExitNodeAllowLANAccess: newV,
		},
		ExitNodeAllowLANAccessSet: true,
	}
	curPrefs, err := m.lc.GetPrefs(m.ctx)
	if err != nil {
		go m.SendNotify("[设置本地网络不走出口] 获取Pref出错", err.Error(), NL_Error)
		return // err
	}

	checkPrefs := curPrefs.Clone()
	checkPrefs.ApplyEdits(maskedPrefs)
	if err := m.lc.CheckPrefs(m.ctx, checkPrefs); err != nil {
		go m.SendNotify("[设置本地网络不走出口] Pref检查出错", err.Error(), NL_Error)
		return // err
	}

	_, err = m.lc.EditPrefs(m.ctx, maskedPrefs)
	if err != nil {
		go m.SendNotify("[设置本地网络不走出口] 设置Pref出错", err.Error(), NL_Error)
		return // err
	}
	return
}

func (m *MiraMenu) setAsExitNode() {
	newV := m.exitField.exitRunExitAction.Checked()
	routes := make([]netip.Prefix, 0)

	if newV { // 设置为出口节点
		confirm := PopConfirmDlg("用作出口节点？", "你是否确定想要将此设备用作出口节点？ \n\n将该设备用作出口节点意味着您的蜃境网络中的其他设备可以将它们的网络流量通过您的IP发送\n\n注意：在Windows设备上运行出口节点并不稳定", 300, 150)
		if !confirm {
			m.exitField.exitRunExitAction.SetChecked(false)
			return
		}
		st, err := m.lc.Status(m.ctx)
		if err != nil {
			go m.SendNotify("[设置为出口节点] 获取状态出错", err.Error(), NL_Error)
			return // err
		}
		if st.ExitNodeStatus != nil {
			go m.SendNotify("[设置为出口节点]", "正在使用其他出口节点，不能用作出口节点", NL_Warn)
			return // err
		}
		routes = append(routes, tsaddr.AllIPv4(), tsaddr.AllIPv6())
	}

	maskedPrefs := &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			AdvertiseRoutes: routes,
		},
		AdvertiseRoutesSet: true,
	}
	curPrefs, err := m.lc.GetPrefs(m.ctx)
	if err != nil {
		go m.SendNotify("[设置为出口节点] 获取Pref出错", err.Error(), NL_Error)
		return // err
	}
	checkPrefs := curPrefs.Clone()
	checkPrefs.ApplyEdits(maskedPrefs)
	if err := m.lc.CheckPrefs(m.ctx, checkPrefs); err != nil {
		go m.SendNotify("[设置为出口节点] Pref检查出错", err.Error(), NL_Error)
		return // err
	}
	_, err = m.lc.EditPrefs(m.ctx, maskedPrefs)
	if err != nil {
		go m.SendNotify("[设置为出口节点] 设置Pref出错", err.Error(), NL_Error)
		return // err
	}
}
