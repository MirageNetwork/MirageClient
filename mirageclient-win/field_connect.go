package main

import (
	"github.com/tailscale/walk"
)

type connectField struct {
	loginAction      *walk.Action // 登录按钮
	connectAction    *walk.Action // 连接按钮
	disconnectAction *walk.Action // 断开按钮
}

func (m *MiraMenu) newConnectField() (cf *connectField, err error) {
	cf = &connectField{}
	cf.loginAction = walk.NewAction()
	cf.loginAction.SetText("登录…")
	cf.connectAction = walk.NewAction()
	cf.connectAction.SetVisible(false)
	cf.disconnectAction = walk.NewAction()
	cf.disconnectAction.SetText("断开")
	cf.disconnectAction.SetVisible(false)

	// 待登录态连接区样式

	if err := m.tray.ContextMenu().Actions().Add(cf.loginAction); err != nil {
		return nil, err
	}
	if err := m.tray.ContextMenu().Actions().Add(cf.connectAction); err != nil {
		return nil, err
	}
	if err := m.tray.ContextMenu().Actions().Add(cf.disconnectAction); err != nil {
		return nil, err
	}
	if err := m.tray.ContextMenu().Actions().Add(walk.NewSeparatorAction()); err != nil {
		return nil, err
	}
	return cf, nil
}
