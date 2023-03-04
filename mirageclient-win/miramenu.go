package main

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/tailscale/walk"
	"tailscale.com/client/tailscale"
	"tailscale.com/ipn"
	"tailscale.com/net/tsaddr"
	"tailscale.com/tailcfg"
	"tailscale.com/types/netmap"
)

type MiraMenu struct {
	mw   *walk.MainWindow
	tray *walk.NotifyIcon

	rx         chan interface{}
	tx         chan interface{}
	rcvdRx     *DataEventPublisher
	startWatch func(context.Context, tailscale.LocalClient) error

	ctx         context.Context
	cancel      context.CancelFunc
	lc          tailscale.LocalClient
	control_url string

	backendData *backendData

	connectField *connectField
	userField    *userField
	nodeField    *nodeField
	exitField    *exitField
	prefField    *prefField

	debugAction *walk.Action
	exitAction  *walk.Action
}

func (s *MiraMenu) Init() {
	var err error
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.lc = tailscale.LocalClient{
		Socket:        socket_path,
		UseSocketOnly: false}
	s.rcvdRx = &DataEventPublisher{}

	s.mw, err = walk.NewMainWindow()
	if err != nil {
		log.Fatal(err)
	}
	s.tray, err = walk.NewNotifyIcon(s.mw)
	if err != nil {
		log.Fatal(err)
	}
	if err := s.tray.SetVisible(true); err != nil {
		log.Fatal(err)
	}
	s.tray.MouseUp().Attach(func(x, y int, button walk.MouseButton) {
		if button == walk.LeftButton {
			if s.backendData.magicCounter == 0 {
				go func() {
					<-time.After(1 * time.Second)
					s.backendData.magicCounter = 0
				}()
			}
			s.backendData.magicCounter++
			if s.backendData.magicCounter == 5 {
				s.backendData.magicCounter = 0
				s.debugAction.SetVisible(true)
			}
		}
	})
	s.setTip("蜃境-简单安全的组网工具")

	s.backendData = NewBackendData()

	s.setIcon(Logo)
	s.backendData.StateChanged().Attach(func(data interface{}) {
		s.changeIconDueRunState(data)
	})
	s.backendData.PrefsChanged().Attach(func(data interface{}) {
		s.changeIconDueRunState(data)
		newPrefs := data.(*ipn.Prefs)
		s.updateCurrentExitNode(newPrefs.ExitNodeID)
	})

	s.connectField, err = s.newConnectField()
	if err != nil {
		log.Printf("初始化连接菜单区错误：%s", err)
	}
	s.userField, err = s.newUserField()
	if err != nil {
		log.Printf("初始化用户菜单区错误：%s", err)
	}
	s.nodeField, err = s.newNodeField()
	if err != nil {
		log.Printf("初始化节点菜单区错误：%s", err)
	}
	s.exitField, err = s.newExitField()
	if err != nil {
		log.Printf("初始化出口节点菜单区错误：%s", err)
	}
	s.prefField, err = s.newPrefField()
	if err != nil {
		log.Printf("初始化配置项菜单区错误：%s", err)
	}

	resetAction := walk.NewAction()
	resetAction.SetText("#重置服务器并登出")
	resetAction.Triggered().Attach(func() {
		s.tray.SetVisible(false)
		confirm := PopConfirmDlg("重置服务器并登出", "重置服务器并登出后，下次登录时需重设服务器代码，是否继续？", 300, 150)
		s.tray.SetVisible(true)
		if confirm {
			err := s.lc.SetStore(s.ctx, string(ipn.CurrentServerCodeKey), "")
			if err != nil {
				go s.SendNotify("重设置服务器代码出错", err.Error(), NL_Error)
				return
			}
			err = s.lc.Logout(s.ctx)
			if err != nil {
				go s.SendNotify("登出出错", err.Error(), NL_Error)
				return
			}
		}
	})

	setAuthKeyAction := walk.NewAction()
	setAuthKeyAction.SetText("#设置授权密钥")
	setAuthKeyAction.Triggered().Attach(func() {
		s.tray.SetVisible(false)
		confirm, authKey := PopTextInputDlg("设置授权密钥登录", "请输入您的授权密钥")
		s.tray.SetVisible(true)
		if confirm {
			s.backendData.SetAuthKey(authKey)
			if s.backendData.State > 2 {
				s.lc.Logout(s.ctx)
			}
		}
	})
	cleanAuthKeyAction := walk.NewAction()
	cleanAuthKeyAction.SetText("#清除授权密钥并登出")
	cleanAuthKeyAction.Triggered().Attach(func() {
		s.tray.SetVisible(false)
		confirm := PopConfirmDlg("清除授权密钥", "是否要清除授权密钥并登出？", 200, 100)
		s.tray.SetVisible(true)
		if confirm {
			s.backendData.SetAuthKey("")
			if s.backendData.State > 2 {
				s.lc.Logout(s.ctx)
			}
		}
	})
	uninstallServiceAction := walk.NewAction()
	uninstallServiceAction.SetText("#卸载后台服务并退出")
	uninstallServiceAction.Triggered().Attach(func() {
		s.tray.SetVisible(false)
		confirm := PopConfirmDlg("卸载后台服务", "将要卸载后台服务并关闭应用，是否继续？", 200, 100)
		s.tray.SetVisible(true)
		if confirm {
			err := ElevateToUinstallService()
			if err != nil {
				go s.SendNotify("卸载后台服务出错", err.Error(), NL_Error)
				return
			}
			walk.App().Exit(0)
		}
	})
	debugContain, err := walk.NewMenu()
	if err != nil {
		log.Printf("初始化调试菜单区错误：%s", err)
	}
	debugContain.Actions().Add(setAuthKeyAction)
	debugContain.Actions().Add(cleanAuthKeyAction)
	debugContain.Actions().Add(resetAction)
	debugContain.Actions().Add(uninstallServiceAction)
	s.debugAction = walk.NewMenuAction(debugContain)
	s.debugAction.SetText("调试项")
	s.debugAction.SetVisible(false)
	s.tray.ContextMenu().Actions().Add(s.debugAction)

	s.exitAction = walk.NewAction()
	s.exitAction.SetText("退出")
	s.exitAction.Triggered().Attach(func() {
		walk.App().Exit(0)
	})

	s.tray.ContextMenu().Actions().Add(s.exitAction)

	s.connectField.loginAction.Triggered().Attach(s.doLogin)
	s.userField.logoutAction.Triggered().Attach(s.doLogout)
	s.connectField.connectAction.Triggered().Attach(s.doConn)
	s.connectField.disconnectAction.Triggered().Attach(s.doDisconn)

	s.exitField.exitAllowLocalAction.Triggered().Attach(s.setAllowLocalNet)
	s.exitField.exitRunExitAction.Triggered().Attach(s.setAsExitNode)

	s.prefField.prefAllowIncomeAction.Triggered().Attach(s.setAllowIncome)
	s.prefField.prefUsingDNSAction.Triggered().Attach(s.setDNSOpt)
	s.prefField.prefUsingSubnetAction.Triggered().Attach(s.setSubnetOpt)
	s.prefField.prefUnattendAction.Triggered().Attach(s.setUnattendOpt)
	s.prefField.prefToDefaultAction.Triggered().Attach(s.setPrefsDefault)

	s.prefField.aboutAction.Triggered().Attach(s.showAbout)

	s.nodeField.nodeAction.Triggered().Attach(func() {
		if s.backendData.NetMap != nil {
			selfIPv4 := s.backendData.NetMap.Addresses[0].Addr()
			if !selfIPv4.Is4() {
				if len(s.backendData.NetMap.Addresses) > 1 {
					selfIPv4 = s.backendData.NetMap.Addresses[1].Addr()
				}
			}
			walk.Clipboard().SetText(selfIPv4.String())
			s.SendNotify("我的地址", "已复制IP地址 ("+selfIPv4.String()+") 到剪贴板", NL_Info)
		}
	})
	s.bindBackendDataChange()
	s.backendData.NetmapChanged().Attach(func(data interface{}) {
		// 更新本设备信息
		netmap := data.(*netmap.NetworkMap)
		selfIPv4 := netmap.Addresses[0].Addr()
		if !selfIPv4.Is4() {
			if len(netmap.Addresses) > 1 {
				selfIPv4 = netmap.Addresses[1].Addr()
			}
		}
		selfName := netmap.SelfNode.DisplayName(true)
		s.nodeField.nodeAction.SetText("本设备: " + selfName + " (" + selfIPv4.String() + ")")
		// 清理节点菜单区
		s.nodeField.nodesMenu.Menu().Actions().Clear()
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
		peerMenuList := peerNodeContain.Actions()
		// 清理出口节点菜单区
		for i := 0; i < s.exitField.exitNodeList.Len(); i++ {
			s.exitField.exitNodeMenu.Menu().Actions().Remove(s.exitField.exitNodeList.At(i))
		}
		s.exitField.exitNodeList.Clear()
		for sni := range s.exitField.exitNodeIDMap {
			delete(s.exitField.exitNodeIDMap, sni)
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
				go s.SendNotify(name, "已复制节点IP("+ip.String()+")到剪贴板", NL_Info)
			})

			if tsaddr.ContainsExitRoutes(node.AllowedIPs) { // 是出口节点
				tmpExitNodeAction := walk.NewAction()
				tmpExitNodeAction.SetText(name)
				tmpExitNodeAction.SetCheckable(true)
				tmpExitNodeAction.SetChecked(node.StableID != "" && !s.backendData.Prefs.ExitNodeID.IsZero() && s.backendData.Prefs.ExitNodeID == node.StableID)
				tmpExitNodeAction.Triggered().Attach(func() {
					for i := 0; i < s.exitField.exitNodeList.Len(); i++ {
						s.exitField.exitNodeList.At(i).SetChecked(false)
					}
					s.setUseExitNode(node.StableID)
				})
				s.exitField.exitNodeList.Add(tmpExitNodeAction)
				s.exitField.exitNodeIDMap[node.StableID] = s.exitField.exitNodeList.Len()
				s.exitField.exitNodeMenu.Menu().Actions().Insert(s.exitField.exitNodeList.Len(), tmpExitNodeAction)
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
			s.nodeField.nodesMenu.Menu().Actions().Add(myNodeMenu)
			s.nodeField.nodesMenu.Menu().Actions().Add(walk.NewSeparatorAction())
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
			s.nodeField.nodesMenu.Menu().Actions().Add(peerMenuList.At(i))
		}
		if tagNodeMenu.Menu().Actions().Len() > 0 { // 有标签节点
			tagNodeHeaderAction := walk.NewAction()
			tagNodeHeaderAction.SetText("标签节点")
			tagNodeHeaderAction.SetEnabled(false)
			tagNodeMenu.Menu().Actions().Insert(0, tagNodeHeaderAction)
			tagNodeMenu.Menu().Actions().Insert(1, walk.NewSeparatorAction())
			peerMenuList.Add(tagNodeMenu)
			s.nodeField.nodesMenu.Menu().Actions().Add(tagNodeMenu)
		}
		if s.nodeField.nodesMenu.Menu().Actions().Len() > 0 { // 有节点
			s.nodeField.nodesMenu.SetVisible(true)
		} else { // 无节点
			s.nodeField.nodesMenu.SetVisible(false)
		}

		if s.exitField.exitNodeList.Len() > 0 { // 有出口节点
			noneExitAction := walk.NewAction()
			noneExitAction.SetText("不使用")
			noneExitAction.SetCheckable(true)
			noneExitAction.SetChecked(s.backendData.Prefs.ExitNodeID.IsZero())
			noneExitAction.Triggered().Attach(func() {
				for i := 0; i < s.exitField.exitNodeList.Len(); i++ {
					s.exitField.exitNodeList.At(i).SetChecked(false)
				}
				s.setUseExitNode("")
			})
			s.exitField.exitNodeIDMap[""] = 0
			s.exitField.exitNodeList.Insert(0, noneExitAction)
			s.exitField.exitNodeMenu.Menu().Actions().Insert(1, noneExitAction)
			s.exitField.exitNodeListTitle.SetText("出口节点")
		} else { // 无出口节点
			s.exitField.exitNodeListTitle.SetText("无可用出口节点")
		}

		// 检查密钥过期情况
		lastDays := ""
		if !netmap.SelfNode.KeyExpiry.IsZero() && !netmap.SelfNode.KeyExpiry.After(time.Now().AddDate(0, 0, 7)) {
			lastDays = strings.TrimSuffix((netmap.SelfNode.KeyExpiry.Sub(time.Now()) / time.Duration(time.Hour*24)).String(), "ns")
			go func(lastDays string) {
				confirm := PopConfirmDlg("临期设备延期提醒", "该设备密钥还有"+lastDays+"天过期，是否现在进行登录延期（将轮换新设备密钥）", 300, 150)
				if confirm {
					s.lc.StartLoginInteractive(s.ctx)
				}
			}(lastDays)
		}
	})
}

func (s *MiraMenu) setIcon(state IconType) {
	if err := s.tray.SetIcon(icons[state]); err != nil {
		log.Fatal(err)
	}
}
func (s *MiraMenu) setTip(tip string) {
	if err := s.tray.SetToolTip(tip); err != nil {
		log.Fatal(err)
	}
}

func (s *MiraMenu) SetRx(rx chan interface{}) {
	s.rx = rx
}

func (s *MiraMenu) SetTx(tx chan interface{}) {
	s.tx = tx
}

func (s *MiraMenu) SetWatchStart(starter func(context.Context, tailscale.LocalClient) error) {
	s.startWatch = starter
}
func (s *MiraMenu) handleRx() {
	var newMsg interface{}
	for {
		select {
		case newMsg = <-s.rx:
			// 开启通讯员
			switch newMsg.(type) {
			case error: // 遇到通讯员无法恢复严重错误崩溃，导致程序只能由用户选择重启动通讯员或者退出程序
				go func(msg string) {
					confirm := PopConfirmDlg("严重错误", "程序通讯员报错:"+msg+" 无法执行，重试还是退出？", 300, 150)
					if !confirm {
						go s.startWatch(s.ctx, s.lc)
						return
					} else {
						walk.App().Exit(-1)
						return
					}
				}(newMsg.(error).Error())
			case ipn.State: // 状态更新
				s.UpdateRunState(newMsg.(ipn.State).String())
			case BackendVersion:
				s.UpdateVersion(string(newMsg.(BackendVersion)))
			case *ipn.Prefs:
				s.UpdatePrefs(newMsg.(*ipn.Prefs))
			case *netmap.NetworkMap:
				s.UpdateNetmap(newMsg.(*netmap.NetworkMap))
			}
		}
		s.rcvdRx.Publish(newMsg)
	}
}

func (s *MiraMenu) Start() {
	defer s.cancel()
	defer s.tray.Dispose()

	go s.handleRx()

	for {
		go s.startWatch(s.ctx, s.lc)

		firstRx := make(chan interface{})
		s.rcvdRx.Event().Once(func(data interface{}) {
			firstRx <- data
		})
		msg := <-firstRx
		switch msg.(type) {
		case error:
			confirm := PopConfirmDlg("严重错误", "程序通讯员报错:"+msg.(error).Error()+" 无法执行，重试还是退出？", 300, 150)
			if confirm {
				s.cancel()
				s.ctx, s.cancel = context.WithCancel(context.Background())
				go s.startWatch(s.ctx, s.lc)
			} else {
				walk.App().Exit(-1)
				return
			}
		case *WatcherUpEvent:
			isAutoStartUp, err := s.lc.GetAutoStartUp(s.ctx)
			if err != nil {
				log.Printf("获取自启动状态失败：%s", err)
			} else {
			}
			s.prefField.autoStartUpAction.SetChecked(isAutoStartUp)
			s.prefField.autoStartUpAction.Triggered().Attach(func() {
				s.lc.SetAutoStartUp(s.ctx)
			})

			prefs, err := s.lc.GetPrefs(s.ctx)
			if err != nil {
				s.SendNotify("加载配置错误", err.Error(), NL_Error) // 通知栏提示
				log.Printf("加载配置错误：%s", err)
				return
			}
			log.Printf("加载配置：%v", prefs)
			st, err := s.lc.Status(s.ctx)
			if err != nil {
				s.SendNotify("加载状态错误", err.Error(), NL_Error) // 通知栏提示
				log.Printf("加载状态错误：%s", err)
				return
			}
			log.Printf("状态：%v", st)

			s.UpdatePrefs(prefs)
			s.UpdateVersion(st.Version)
			s.UpdateRunState(st.BackendState)

			s.mw.Run()
		}
	}
}
