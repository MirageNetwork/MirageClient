package main

import (
	"github.com/tailscale/walk"
	"tailscale.com/ipn"
)

// 节点菜单区
type nodeField struct {
	nodeAction *walk.Action // 本节点按钮
	nodesMenu  *walk.Action // 网络设备菜单
}

func NewNodeField(al *walk.ActionList, rs *runState, bd *backendData) (nf *nodeField, err error) {
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

	rs.Changed().Attach(func(data interface{}) {
		state := data.(int)
		switch ipn.State(state) {
		case ipn.Stopped, ipn.Starting:
			nf.nodeAction.SetText("本设备")
			nf.nodeAction.SetEnabled(false)
			nf.nodeAction.SetVisible(true)
			nf.nodesMenu.SetEnabled(false)
			nf.nodesMenu.SetVisible(true)
		case ipn.Running:
			nf.nodeAction.SetEnabled(true)
			nf.nodeAction.SetVisible(true)
			nf.nodesMenu.SetEnabled(true)
			nf.nodesMenu.SetVisible(true)
		default:
			nf.nodeAction.SetVisible(false)
			nf.nodesMenu.SetVisible(false)
		}
	})

	if err := al.Add(nf.nodeAction); err != nil {
		return nil, err
	}
	if err := al.Add(nf.nodesMenu); err != nil {
		return nil, err
	}
	if err := al.Add(walk.NewSeparatorAction()); err != nil {
		return nil, err
	}
	return nf, nil
}
