package main

import (
	"github.com/tailscale/walk"
)

// 节点菜单区
type nodeField struct {
	nodeAction *walk.Action // 本节点按钮
	nodesMenu  *walk.Action // 网络设备菜单
}

func (m *MiraMenu) newNodeField() (nf *nodeField, err error) {
	nf = &nodeField{}
	nf.nodeAction = walk.NewAction()
	nf.nodeAction.SetText("本设备")
	nf.nodeAction.SetVisible(false)

	nodeContain, err := walk.NewMenu()
	if err != nil {
		return nil, err
	}
	nf.nodesMenu = walk.NewMenuAction(nodeContain)
	nf.nodesMenu.SetText("网内设备")
	nf.nodesMenu.SetVisible(false)

	if err := m.tray.ContextMenu().Actions().Add(nf.nodeAction); err != nil {
		return nil, err
	}
	if err := m.tray.ContextMenu().Actions().Add(nf.nodesMenu); err != nil {
		return nil, err
	}
	if err := m.tray.ContextMenu().Actions().Add(walk.NewSeparatorAction()); err != nil {
		return nil, err
	}
	return nf, nil
}
