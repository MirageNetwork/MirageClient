//go:build windows

package main

import (
	"log"
	"net/netip"
	"strings"

	"github.com/tailscale/walk"
	"tailscale.com/ipn"
	"tailscale.com/net/tsaddr"
)

// 连接动作
func (m *MiraMenu) DoConn() {
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

// 断开连接动作
func (m *MiraMenu) DoDisconn() {
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

// 登出动作
func (m *MiraMenu) DoLogout() {
	err := m.lc.Logout(m.ctx)
	if err != nil {
		go m.SendNotify("登出出错", err.Error(), NL_Error)
		return
	}
}

// 登录动作
func (m *MiraMenu) DoLogin() {
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
				newServerCode = defaultServerCode
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

// 显示关于
func (m *MiraMenu) ShowAbout() {
	msg := "蜃境客户端版本：" + strings.Split(m.data.Version, "-")[0]
	if len(strings.Split(m.data.Version, "-")) > 1 {
		msg += " (" + strings.TrimPrefix(m.data.Version, strings.Split(m.data.Version, "-")[0]+"-") + ")"
	}
	if m.data.ClientVersion != nil {
		if m.data.ClientVersion.LatestVersion == m.data.Version {
			msg += "\n已是最新版本"
		} else if m.data.ClientVersion.LatestVersion != "" {
			msg += "\n官方最新版本：" + m.data.ClientVersion.LatestVersion + "\n是否下载？"
			msgid := walk.MsgBox(m.mw, "关于蜃境", msg, walk.MsgBoxIconInformation|walk.MsgBoxOKCancel)
			if msgid == 1 {
				OpenURLInBrowser(m.data.ClientVersion.NotifyURL)
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

// SetAllowIncome 设置允许入流量
func (m *MiraMenu) SetAllowIncome() {
	newV := m.prefField.prefAllowIncomeAction.Checked()
	m.prefField.prefAllowIncomeAction.SetChecked(!newV)
	maskedPrefs := &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			ShieldsUp: !newV,
		},
		ShieldsUpSet: true,
	}
	m.updatePref("设置允许入流量", maskedPrefs)
}

// SetDNSOpt 设置使用DNS配置
func (m *MiraMenu) SetDNSOpt() {
	newV := m.prefField.prefUsingDNSAction.Checked()
	m.prefField.prefUsingDNSAction.SetChecked(!newV)
	maskedPrefs := &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			CorpDNS: newV,
		},
		CorpDNSSet: true,
	}
	m.updatePref("设置使用DNS选项", maskedPrefs)
}

// SetSubnetOpt 设置使用子网转发
func (m *MiraMenu) SetSubnetOpt() {
	newV := m.prefField.prefUsingSubnetAction.Checked()
	m.prefField.prefUsingSubnetAction.SetChecked(!newV)
	maskedPrefs := &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			RouteAll: newV,
		},
		RouteAllSet: true,
	}
	m.updatePref("设置使用子网选项", maskedPrefs)
}

// SetUnattendOpt 设置无人值守
func (m *MiraMenu) SetUnattendOpt() {
	newV := m.prefField.prefUnattendAction.Checked()
	m.prefField.prefUnattendAction.SetChecked(!newV)
	maskedPrefs := &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			ForceDaemon: newV,
		},
		ForceDaemonSet: true,
	}
	m.updatePref("设置无人值守", maskedPrefs)
}

// SetPrefsDefault 恢复为默认配置
func (m *MiraMenu) SetPrefsDefault() {
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
	m.updatePref("恢复默认配置", maskedPrefs)
}

// SetAllowLocalNet 设置本地网络不走出口
func (m *MiraMenu) SetAllowLocalNet() {
	newV := m.exitField.exitAllowLocalAction.Checked()
	maskedPrefs := &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			ExitNodeAllowLANAccess: newV,
		},
		ExitNodeAllowLANAccessSet: true,
	}
	m.updatePref("[设置本地网络不走出口]", maskedPrefs)
}

// SetAsExitNode 设置为出口节点
func (m *MiraMenu) SetAsExitNode() {
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
	m.updatePref("[设置为出口节点]", maskedPrefs)
}
