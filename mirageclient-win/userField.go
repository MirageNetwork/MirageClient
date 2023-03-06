//go:build windows
package main

import (
	"github.com/tailscale/walk"
)

// 用户菜单区
type userField struct {
	userMenu *walk.Action // 用户菜单（登录态：显示用户账号，下挂添加用户、控制台、登出）

	userList      *walk.ActionList // TODO：用户列表菜单
	userListTail  *walk.Action     // TODO：用户列表末分隔符
	addUserAction *walk.Action     // TODO：添加用户按钮
	addUserTail   *walk.Action     // TODO：添加用户末分隔符

	consoleAction *walk.Action // 控制台按钮  （仅管理员用户显示）

	logoutAction *walk.Action // 登出按钮
}

func (m *MiraMenu) newUserField() (uf *userField, err error) {
	uf = &userField{}
	userMenuContain, err := walk.NewMenu()
	if err != nil {
		return nil, err
	}
	uf.userMenu = walk.NewMenuAction(userMenuContain)
	uf.userMenu.SetText("用户账号")
	uf.userMenu.SetVisible(false)

	uf.consoleAction = walk.NewAction()
	uf.consoleAction.SetText("控制台")
	uf.logoutAction = walk.NewAction()
	uf.logoutAction.SetText("登出")

	uf.userMenu.Menu().Actions().Add(uf.consoleAction)
	uf.userMenu.Menu().Actions().Add(walk.NewSeparatorAction())
	uf.userMenu.Menu().Actions().Add(uf.logoutAction)

	uf.consoleAction.Triggered().Attach(func() { OpenURLInBrowser(m.data.Prefs.AdminPageURL()) })

	if err := m.tray.ContextMenu().Actions().Add(uf.userMenu); err != nil {
		return nil, err
	}
	if err := m.tray.ContextMenu().Actions().Add(walk.NewSeparatorAction()); err != nil {
		return nil, err
	}

	return uf, nil
}
