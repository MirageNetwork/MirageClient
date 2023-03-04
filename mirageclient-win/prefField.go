package main

import (
	"github.com/tailscale/walk"
	"tailscale.com/ipn"
)

// 配置菜单区
type prefField struct {
	prefMenu *walk.Action // 配置菜单

	prefAllowIncomeAction *walk.Action // 配置项 -- 允许入流量
	prefUsingDNSAction    *walk.Action // 配置项 -- 使用DNS配置
	prefUsingSubnetAction *walk.Action // 配置项 -- 使用子网路由
	prefUnattendAction    *walk.Action // 配置项 -- 无人值守模式

	prefToDefaultAction *walk.Action // 恢复默认设置
	autoStartUpAction   *walk.Action // 开机自启动

	aboutAction *walk.Action // 关于菜单
}

func (m *MiraMenu) newPrefField() (pf *prefField, err error) {

	pf = &prefField{}
	prefContain, err := walk.NewMenu()
	if err != nil {
		return nil, err
	}
	pf.prefMenu = walk.NewMenuAction(prefContain)
	pf.prefMenu.SetText("配置项")

	pf.prefAllowIncomeAction = walk.NewAction()
	pf.prefAllowIncomeAction.SetText("允许入流量连接")
	pf.prefAllowIncomeAction.SetCheckable(true)
	pf.prefAllowIncomeAction.SetChecked(true)

	pf.prefUsingDNSAction = walk.NewAction()
	pf.prefUsingDNSAction.SetText("使用DNS设置")
	pf.prefUsingDNSAction.SetCheckable(true)
	pf.prefUsingDNSAction.SetChecked(true)

	pf.prefUsingSubnetAction = walk.NewAction()
	pf.prefUsingSubnetAction.SetText("使用子网转发")
	pf.prefUsingSubnetAction.SetCheckable(true)
	pf.prefUsingSubnetAction.SetChecked(true)

	pf.prefUnattendAction = walk.NewAction()
	pf.prefUnattendAction.SetText("无人值守运行")
	pf.prefUnattendAction.SetCheckable(true)
	pf.prefUnattendAction.SetChecked(false)

	pf.prefToDefaultAction = walk.NewAction()
	pf.prefToDefaultAction.SetText("恢复默认设置")

	pf.autoStartUpAction = walk.NewAction()
	pf.autoStartUpAction.SetText("开机自启动")
	pf.autoStartUpAction.SetCheckable(true)
	pf.autoStartUpAction.SetChecked(false)

	pf.aboutAction = walk.NewAction()
	pf.aboutAction.SetText("关于…")

	pf.prefMenu.Menu().Actions().Add(pf.prefAllowIncomeAction)
	pf.prefMenu.Menu().Actions().Add(pf.prefUsingDNSAction)
	pf.prefMenu.Menu().Actions().Add(pf.prefUsingSubnetAction)
	pf.prefMenu.Menu().Actions().Add(pf.prefUnattendAction)
	pf.prefMenu.Menu().Actions().Add(walk.NewSeparatorAction())
	pf.prefMenu.Menu().Actions().Add(pf.prefToDefaultAction)
	pf.prefMenu.Menu().Actions().Add(pf.autoStartUpAction)

	if err := m.tray.ContextMenu().Actions().Add(pf.prefMenu); err != nil {
		return nil, err
	}
	if err := m.tray.ContextMenu().Actions().Add(pf.aboutAction); err != nil {
		return nil, err
	}
	if err := m.tray.ContextMenu().Actions().Add(walk.NewSeparatorAction()); err != nil {
		return nil, err
	}
	return pf, nil
}

func (m *MiraMenu) bindBackendDataChange() {
	m.backendData.PrefsChanged().Attach(func(data interface{}) {
		newPref := data.(*ipn.Prefs)
		m.prefField.prefAllowIncomeAction.SetChecked(!newPref.ShieldsUp)
		m.prefField.prefUsingDNSAction.SetChecked(newPref.CorpDNS)
		m.prefField.prefUsingSubnetAction.SetChecked(newPref.RouteAll)
		m.prefField.prefUnattendAction.SetChecked(newPref.ForceDaemon)
	})
}
