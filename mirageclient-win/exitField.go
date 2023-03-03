package main

import (
	"github.com/tailscale/walk"
	"tailscale.com/ipn"
	"tailscale.com/tailcfg"
)

// 出口节点菜单区
type exitField struct {
	exitNodeMenu      *walk.Action                 // 出口节点菜单
	exitNodeListTitle *walk.Action                 // 出口节点列表标题
	exitNodeList      *walk.ActionList             // 出口节点菜单 (有出口节点时，首个永远是‘不使用’)
	exitNodeIDMap     map[tailcfg.StableNodeID]int // 出口节点ID映射表

	exitPrefTitle        *walk.Action // 出口节点配置标题
	exitAllowLocalAction *walk.Action // 出口节点配置项 -- 允许访问本地网络
	exitRunExitAction    *walk.Action // 出口节点配置项 -- 用作出口节点
}

func NewExitField(al *walk.ActionList, rs *runState, bd *backendData) (ef *exitField, err error) {
	ef = &exitField{}
	exitNodeContain, err := walk.NewMenu()
	if err != nil {
		return nil, err
	}
	ef.exitNodeMenu = walk.NewMenuAction(exitNodeContain)
	ef.exitNodeMenu.SetText("出口节点")
	ef.exitNodeMenu.SetVisible(false)

	ef.exitNodeListTitle = walk.NewAction()
	ef.exitNodeListTitle.SetText("无可用出口节点")
	ef.exitNodeListTitle.SetEnabled(false)
	exitNodeListConatin, err := walk.NewMenu()
	ef.exitNodeList = walk.NewMenuAction(exitNodeListConatin).Menu().Actions()
	ef.exitNodeIDMap = make(map[tailcfg.StableNodeID]int)

	ef.exitPrefTitle = walk.NewAction()
	ef.exitPrefTitle.SetText("配置项")
	ef.exitPrefTitle.SetEnabled(false)

	ef.exitAllowLocalAction = walk.NewAction()
	ef.exitAllowLocalAction.SetText("本地网络不走出口")
	ef.exitAllowLocalAction.SetCheckable(true)
	ef.exitAllowLocalAction.SetChecked(false)

	ef.exitRunExitAction = walk.NewAction()
	ef.exitRunExitAction.SetText("用作出口节点…")
	ef.exitRunExitAction.SetCheckable(true)
	ef.exitRunExitAction.SetChecked(false)

	ef.exitNodeMenu.Menu().Actions().Add(ef.exitNodeListTitle)
	ef.exitNodeMenu.Menu().Actions().Add(walk.NewSeparatorAction())
	ef.exitNodeMenu.Menu().Actions().Add(ef.exitPrefTitle)
	ef.exitNodeMenu.Menu().Actions().Add(ef.exitAllowLocalAction)
	ef.exitNodeMenu.Menu().Actions().Add(ef.exitRunExitAction)

	rs.Changed().Attach(func(data interface{}) {
		state := data.(int)
		switch ipn.State(state) {
		case ipn.Stopped, ipn.Starting:
			ef.exitNodeMenu.SetEnabled(false)
			ef.exitNodeMenu.SetVisible(true)
		case ipn.Running:
			ef.exitNodeMenu.SetEnabled(true)
			ef.exitNodeMenu.SetVisible(true)
		default:
			ef.exitNodeMenu.SetVisible(false)
		}
	})
	bd.PrefsChanged().Attach(func(data interface{}) {
		prefs := data.(*ipn.Prefs)
		ef.exitAllowLocalAction.SetChecked(prefs.ExitNodeAllowLANAccess)

		if prefs.AdvertisesExitNode() {
			ef.exitRunExitAction.SetText("正用作出口节点")
			ef.exitRunExitAction.SetChecked(true)
		} else {
			ef.exitRunExitAction.SetText("用作出口节点…")
			ef.exitRunExitAction.SetChecked(false)
		}
	})

	if err := al.Add(ef.exitNodeMenu); err != nil {
		return nil, err
	}
	if err := al.Add(walk.NewSeparatorAction()); err != nil {
		return nil, err
	}
	return ef, nil
}

// 更新出口节点（被动响应）
func (m *MiraMenu) updateCurrentExitNode(stableID tailcfg.StableNodeID) {
	for i := 0; i < m.exitField.exitNodeList.Len(); i++ {
		m.exitField.exitNodeList.At(i).SetChecked(false)
	}
	if index, ok := m.exitField.exitNodeIDMap[stableID]; ok {
		m.exitField.exitNodeList.At(index).SetChecked(true)
	}
	if node, ok := m.backendData.NetMap.PeerWithStableID(m.backendData.Prefs.ExitNodeID); ok {
		m.exitField.exitNodeMenu.SetText("出口节点(" + node.DisplayName(true) + ")")
		return
	}
	m.exitField.exitNodeMenu.SetText("出口节点")
}

// 设置出口节点(点击按钮动作)
func (m *MiraMenu) setUseExitNode(stableID tailcfg.StableNodeID) {
	if m.exitField.exitRunExitAction.Checked() {
		go m.SendNotify("设置出口节点", "当前节点用作出口节点，无法使用其他节点作为出口节点", NL_Warn)
		return
	}
	maskedPrefs := &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			ExitNodeID: stableID,
		},
		ExitNodeIDSet: true,
	}
	curPrefs, err := m.lc.GetPrefs(m.ctx)
	if err != nil {
		go m.SendNotify("设置出口节点", "获取当前配置失败:"+err.Error(), NL_Error)
		return
	}

	checkPrefs := curPrefs.Clone()
	checkPrefs.ApplyEdits(maskedPrefs)
	if err := m.lc.CheckPrefs(m.ctx, checkPrefs); err != nil {
		go m.SendNotify("设置出口节点", "检查配置失败:"+err.Error(), NL_Error)
		return
	}

	_, err = m.lc.EditPrefs(m.ctx, maskedPrefs)
	if err != nil {
		go m.SendNotify("设置出口节点", "更新配置失败:"+err.Error(), NL_Error)
		return
	}
}
