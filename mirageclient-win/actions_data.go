//go:build windows

package main

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tailscale/walk"
	"tailscale.com/ipn"
	"tailscale.com/net/tsaddr"
	"tailscale.com/tailcfg"
	"tailscale.com/types/netmap"
)

// 处理接收到的通讯兵消息
func (s *MiraMenu) handleRx() {
	var newMsg interface{}
	for {
		select {
		case newMsg = <-s.rx:
			// 开启通讯兵
			switch newMsg.(type) {
			case error: // 遇到通讯兵无法恢复严重错误崩溃，导致程序只能由用户选择重启动通讯员或者退出程序
				go func(msg string) {
					confirm := PopConfirmDlg("严重错误", "程序通讯报错:"+msg+" 无法执行，是否重试？", 300, 150)
					if confirm {
						go s.startWatch(s.ctx, s.lc)
						return
					} else {
						os.Exit(-1)
						return
					}
				}(newMsg.(error).Error())
			case ipn.State: // 状态更新
				s.data.SetState(newMsg.(ipn.State).String())
			case BackendVersion:
				s.data.SetVersion(string(newMsg.(BackendVersion)))
			case *ipn.Prefs:
				s.data.SetPrefs(newMsg.(*ipn.Prefs))
			case *netmap.NetworkMap:
				s.data.SetNetMap(newMsg.(*netmap.NetworkMap))
			}
		}
		s.rcvdRx.Publish(newMsg)
	}
}

// 状态更新动作
func (m *MiraMenu) bindStateChange() {
	m.data.StateChanged().Attach(func(data interface{}) {
		m.ChangeIconDueRunState()
		state := data.(ipn.State)
		switch ipn.State(state) {
		case ipn.Stopped:
			m.connectField.connectAction.SetText("连接")
			m.connectField.connectAction.SetEnabled(true)
			m.connectField.connectAction.SetVisible(true)
			m.connectField.disconnectAction.SetVisible(false)
			m.connectField.loginAction.SetVisible(false)

			m.userField.userMenu.SetVisible(true)

			m.nodeField.nodeAction.SetText("本设备")
			m.nodeField.nodeAction.SetEnabled(false)
			m.nodeField.nodeAction.SetVisible(true)
			m.nodeField.nodesMenu.SetEnabled(false)
			m.nodeField.nodesMenu.SetVisible(true)

			m.exitField.exitNodeMenu.SetEnabled(false)
			m.exitField.exitNodeMenu.SetVisible(true)
		case ipn.Starting:
			m.connectField.connectAction.SetEnabled(false)
			m.connectField.connectAction.SetText("正在连接……")
			m.connectField.connectAction.SetVisible(true)
			m.connectField.disconnectAction.SetEnabled(true)
			m.connectField.disconnectAction.SetVisible(true)
			m.connectField.loginAction.SetVisible(false)

			m.userField.userMenu.SetVisible(true)

			m.nodeField.nodeAction.SetText("本设备")
			m.nodeField.nodeAction.SetEnabled(false)
			m.nodeField.nodeAction.SetVisible(true)
			m.nodeField.nodesMenu.SetEnabled(false)
			m.nodeField.nodesMenu.SetVisible(true)

			m.exitField.exitNodeMenu.SetEnabled(false)
			m.exitField.exitNodeMenu.SetVisible(true)
		case ipn.Running:
			m.connectField.connectAction.SetText("已连接")
			m.connectField.connectAction.SetEnabled(false)
			m.connectField.connectAction.SetVisible(true)
			m.connectField.disconnectAction.SetEnabled(true)
			m.connectField.disconnectAction.SetVisible(true)
			m.connectField.loginAction.SetVisible(false)

			m.userField.userMenu.SetVisible(true)

			m.nodeField.nodeAction.SetEnabled(true)
			m.nodeField.nodeAction.SetVisible(true)
			m.nodeField.nodesMenu.SetEnabled(true)
			m.nodeField.nodesMenu.SetVisible(true)

			m.exitField.exitNodeMenu.SetEnabled(true)
			m.exitField.exitNodeMenu.SetVisible(true)
		default:
			m.connectField.connectAction.SetVisible(false)
			m.connectField.disconnectAction.SetVisible(false)
			m.connectField.loginAction.SetVisible(true)

			m.userField.userMenu.SetVisible(false)

			m.nodeField.nodeAction.SetVisible(false)
			m.nodeField.nodesMenu.SetVisible(false)

			m.exitField.exitNodeMenu.SetVisible(false)
		}
	})
}

// 配置更新动作
func (m *MiraMenu) bindPrefsChange() {
	m.data.PrefsChanged().Attach(func(data interface{}) {
		m.ChangeIconDueRunState()
		prefs := data.(*ipn.Prefs)

		m.userField.consoleAction.SetVisible(prefs.AdminPageURL() != "")

		m.updateCurrentExitNode(prefs.ExitNodeID)
		m.exitField.exitAllowLocalAction.SetChecked(prefs.ExitNodeAllowLANAccess)
		if prefs.AdvertisesExitNode() {
			m.exitField.exitRunExitAction.SetText("正用作出口节点")
			m.exitField.exitRunExitAction.SetChecked(true)
		} else {
			m.exitField.exitRunExitAction.SetText("用作出口节点…")
			m.exitField.exitRunExitAction.SetChecked(false)
		}

		m.prefField.prefAllowIncomeAction.SetChecked(!prefs.ShieldsUp)
		m.prefField.prefUsingDNSAction.SetChecked(prefs.CorpDNS)
		m.prefField.prefUsingSubnetAction.SetChecked(prefs.RouteAll)
		m.prefField.prefUnattendAction.SetChecked(prefs.ForceDaemon)
	})
}

// 网络图更新动作
func (m *MiraMenu) bindNetMapChange() {
	m.data.NetmapChanged().Attach(func(data interface{}) {
		netmap := data.(*netmap.NetworkMap)
		m.userField.userMenu.SetText(netmap.UserProfiles[netmap.SelfNode.User].DisplayName)

		selfIPv4 := netmap.Addresses[0].Addr()
		if !selfIPv4.Is4() {
			if len(netmap.Addresses) > 1 {
				selfIPv4 = netmap.Addresses[1].Addr()
			}
		}
		selfName := netmap.SelfNode.DisplayName(true)
		m.nodeField.nodeAction.SetText("本设备: " + selfName + " (" + selfIPv4.String() + ")")
		// 清理节点菜单区
		m.nodeField.nodesMenu.Menu().Actions().Clear()
		myNodeContain, err := walk.NewMenu()
		if err != nil {
			log.Printf("初始化标签节点菜单区错误：%s", err)
		}
		myNodeMenu := walk.NewMenuAction(myNodeContain)
		myNodeMenu.SetText("我的设备")
		tagNodeContain, err := walk.NewMenu()
		if err != nil {
			log.Printf("初始化标签节点菜单区错误：%s", err)
		}
		tagNodeMenu := walk.NewMenuAction(tagNodeContain)
		tagNodeMenu.SetText("标签节点")
		peerNodeContain, err := walk.NewMenu()
		if err != nil {
			log.Printf("初始化标签节点菜单区错误：%s", err)
		}
		peerMenuList := peerNodeContain.Actions()
		// 清理出口节点菜单区
		for i := 0; i < m.exitField.exitNodeList.Len(); i++ {
			m.exitField.exitNodeMenu.Menu().Actions().Remove(m.exitField.exitNodeList.At(i))
		}
		m.exitField.exitNodeList.Clear()
		for sni := range m.exitField.exitNodeIDMap {
			delete(m.exitField.exitNodeIDMap, sni)
		}

		// 生成节点及出口菜单区
		for _, node := range netmap.Peers {
			name, hostname := node.DisplayNames(true)
			if hostname != "" && hostname != name {
				name += "(" + hostname + ")"
			}
			ip := node.Addresses[0].Addr()
			if !ip.Is4() {
				ip = node.Addresses[1].Addr()
			}

			tmpNodeAction := walk.NewAction()
			tmpNodeAction.SetText(name)
			tmpNodeAction.Triggered().Attach(func() {
				walk.Clipboard().SetText(ip.String())
				go m.SendNotify(name, "已复制节点IP("+ip.String()+")到剪贴板", NL_Info)
			})

			if tsaddr.ContainsExitRoutes(node.AllowedIPs) { // 是出口节点
				tmpExitNodeAction := walk.NewAction()
				tmpExitNodeAction.SetText(name)
				tmpExitNodeAction.SetCheckable(true)
				tmpExitNodeAction.SetChecked(node.StableID != "" && !m.data.Prefs.ExitNodeID.IsZero() && m.data.Prefs.ExitNodeID == node.StableID)
				tmpExitNodeAction.Triggered().Attach(func() {
					for i := 0; i < m.exitField.exitNodeList.Len(); i++ {
						m.exitField.exitNodeList.At(i).SetChecked(false)
					}
					m.setUseExitNode(node.StableID)
				})
				m.exitField.exitNodeList.Add(tmpExitNodeAction)
				m.exitField.exitNodeIDMap[node.StableID] = m.exitField.exitNodeList.Len()
				m.exitField.exitNodeMenu.Menu().Actions().Insert(m.exitField.exitNodeList.Len(), tmpExitNodeAction)
			}

			if node.Tags != nil { // 有标签的节点
				tagNodeMenu.Menu().Actions().Add(tmpNodeAction)
			} else if node.User == netmap.SelfNode.User && node.ID != netmap.SelfNode.ID { // 本用户节点
				myNodeMenu.Menu().Actions().Add(tmpNodeAction)
			} else if node.User != netmap.SelfNode.User { // 其他用户节点
				peerMenu := &walk.Action{}
				peerMenuExist := false
				nodeOwner := strconv.FormatInt(int64(node.User), 10)
				if !node.Sharer.IsZero() && node.Sharer != node.User {
					nodeOwner = strconv.FormatInt(int64(node.Sharer), 10)
				}

				for i := 0; i < peerMenuList.Len(); i++ {
					if peerMenu = peerMenuList.At(i); peerMenu.Text() == nodeOwner {
						peerMenuExist = true
						break
					}
				}
				if !peerMenuExist {
					peerMenuContain, err := walk.NewMenu()
					if err != nil {
						log.Printf("初始化其他用户节点菜单区错误：%s", err)
					}
					peerMenu = walk.NewMenuAction(peerMenuContain)
					peerMenu.SetText(nodeOwner)
					peerMenuList.Add(peerMenu)
				}
				peerMenu.Menu().Actions().Add(tmpNodeAction)
			}
		}
		if myNodeMenu.Menu().Actions().Len() > 0 { // 有本用户节点
			myNodeHeaderAction := walk.NewAction()
			myNodeHeaderAction.SetText(netmap.UserProfiles[netmap.SelfNode.User].LoginName)
			myNodeHeaderAction.SetEnabled(false)
			myNodeMenu.Menu().Actions().Insert(0, myNodeHeaderAction)
			myNodeMenu.Menu().Actions().Insert(1, walk.NewSeparatorAction())
			m.nodeField.nodesMenu.Menu().Actions().Add(myNodeMenu)
			m.nodeField.nodesMenu.Menu().Actions().Add(walk.NewSeparatorAction())
		}
		for i := 0; i < peerMenuList.Len(); i++ {
			peerId, err := strconv.ParseInt(peerMenuList.At(i).Text(), 10, 64)
			if err != nil {
				log.Printf("解析用户ID错误：%s", err)
			}
			peerMenuList.At(i).SetText(netmap.UserProfiles[tailcfg.UserID(peerId)].DisplayName)

			peerNodeHeaderAction := walk.NewAction()
			peerNodeHeaderAction.SetText(netmap.UserProfiles[tailcfg.UserID(peerId)].LoginName)
			peerNodeHeaderAction.SetEnabled(false)
			peerMenuList.At(i).Menu().Actions().Insert(0, peerNodeHeaderAction)
			peerMenuList.At(i).Menu().Actions().Insert(1, walk.NewSeparatorAction())
			m.nodeField.nodesMenu.Menu().Actions().Add(peerMenuList.At(i))
		}
		if tagNodeMenu.Menu().Actions().Len() > 0 { // 有标签节点
			tagNodeHeaderAction := walk.NewAction()
			tagNodeHeaderAction.SetText("标签节点")
			tagNodeHeaderAction.SetEnabled(false)
			tagNodeMenu.Menu().Actions().Insert(0, tagNodeHeaderAction)
			tagNodeMenu.Menu().Actions().Insert(1, walk.NewSeparatorAction())
			peerMenuList.Add(tagNodeMenu)
			m.nodeField.nodesMenu.Menu().Actions().Add(tagNodeMenu)
		}
		if m.nodeField.nodesMenu.Menu().Actions().Len() > 0 { // 有节点
			m.nodeField.nodesMenu.SetVisible(true)
		} else { // 无节点
			m.nodeField.nodesMenu.SetVisible(false)
		}

		if m.exitField.exitNodeList.Len() > 0 { // 有出口节点
			noneExitAction := walk.NewAction()
			noneExitAction.SetText("不使用")
			noneExitAction.SetCheckable(true)
			noneExitAction.SetChecked(m.data.Prefs.ExitNodeID.IsZero())
			noneExitAction.Triggered().Attach(func() {
				for i := 0; i < m.exitField.exitNodeList.Len(); i++ {
					m.exitField.exitNodeList.At(i).SetChecked(false)
				}
				m.setUseExitNode("")
			})
			m.exitField.exitNodeIDMap[""] = 0
			m.exitField.exitNodeList.Insert(0, noneExitAction)
			m.exitField.exitNodeMenu.Menu().Actions().Insert(1, noneExitAction)
			m.exitField.exitNodeListTitle.SetText("出口节点")
		} else { // 无出口节点
			m.exitField.exitNodeListTitle.SetText("无可用出口节点")
		}

		// 检查密钥过期情况
		lastDays := ""
		if !netmap.SelfNode.KeyExpiry.IsZero() && !netmap.SelfNode.KeyExpiry.After(time.Now().AddDate(0, 0, 7)) {
			lastDays = strings.TrimSuffix((netmap.SelfNode.KeyExpiry.Sub(time.Now()) / time.Duration(time.Hour*24)).String(), "ns")
			go func(lastDays string) {
				confirm := PopConfirmDlg("临期设备延期提醒", "该设备密钥还有"+lastDays+"天过期，是否现在进行登录延期（将轮换新设备密钥）", 300, 150)
				if confirm {
					m.lc.StartLoginInteractive(m.ctx)
				}
			}(lastDays)
		}
	})
}

func (m *MiraMenu) bindDataPool() {
	m.bindStateChange()
	m.bindPrefsChange()
	m.bindNetMapChange()
}
