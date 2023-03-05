package main

import (
	"log"
	"os"

	"github.com/tailscale/walk"
	"tailscale.com/ipn"
)

func (m *MiraMenu) newDebugField() (df *walk.Action, err error) {
	setAuthKeyAction := walk.NewAction()
	setAuthKeyAction.SetText("#设置授权密钥")
	setAuthKeyAction.Triggered().Attach(func() {
		m.tray.SetVisible(false)
		confirm, authKey := PopTextInputDlg("设置授权密钥登录", "请输入您的授权密钥")
		m.tray.SetVisible(true)
		if confirm {
			m.data.SetAuthKey(authKey)
			if m.data.State > 2 {
				m.DoLogout()
			}
		}
	})
	cleanAuthKeyAction := walk.NewAction()
	cleanAuthKeyAction.SetText("#清除授权密钥并登出")
	cleanAuthKeyAction.Triggered().Attach(func() {
		m.tray.SetVisible(false)
		confirm := PopConfirmDlg("清除授权密钥", "是否要清除授权密钥并登出？", 200, 100)
		m.tray.SetVisible(true)
		if confirm {
			m.data.SetAuthKey("")
			if m.data.State > 2 {
				m.DoLogout()
			}
		}
	})
	resetAction := walk.NewAction()
	resetAction.SetText("#重置服务器并登出")
	resetAction.Triggered().Attach(func() {
		m.tray.SetVisible(false)
		confirm := PopConfirmDlg("重置服务器并登出", "重置服务器并登出后，下次登录时需重设服务器代码，是否继续？", 300, 150)
		m.tray.SetVisible(true)
		if confirm {
			err := m.lc.SetStore(m.ctx, string(ipn.CurrentServerCodeKey), "")
			if err != nil {
				go m.SendNotify("重设置服务器代码出错", err.Error(), NL_Error)
				return
			}
			m.DoLogout()
		}
	})
	uninstallServiceAction := walk.NewAction()
	uninstallServiceAction.SetText("#卸载后台服务并退出")
	uninstallServiceAction.Triggered().Attach(func() {
		m.tray.SetVisible(false)
		confirm := PopConfirmDlg("卸载后台服务", "将要卸载后台服务并关闭应用，是否继续？", 200, 100)
		m.tray.SetVisible(true)
		if confirm {
			err := ElevateToUinstallService()
			if err != nil {
				go m.SendNotify("卸载后台服务出错", err.Error(), NL_Error)
				return
			}
			os.Exit(0)
		}
	})
	debugContain, err := walk.NewMenu()
	if err != nil {
		log.Printf("初始化调试菜单区错误：%s", err)
		return nil, err
	}
	debugContain.Actions().Add(setAuthKeyAction)
	debugContain.Actions().Add(cleanAuthKeyAction)
	debugContain.Actions().Add(resetAction)
	debugContain.Actions().Add(uninstallServiceAction)
	m.debugAction = walk.NewMenuAction(debugContain)
	m.debugAction.SetText("调试项")
	m.debugAction.SetVisible(false)
	err = m.tray.ContextMenu().Actions().Add(m.debugAction)
	return m.debugAction, err
}
