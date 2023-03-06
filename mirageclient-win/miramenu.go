//go:build windows
package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/tailscale/walk"
	"tailscale.com/client/tailscale"
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

	data *DataPool

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
		Socket:        socketPath,
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
			if s.data.magicCounter == 0 {
				go func() {
					<-time.After(1 * time.Second)
					s.data.magicCounter = 0
				}()
			}
			s.data.magicCounter++
			if s.data.magicCounter == 5 {
				s.data.magicCounter = 0
				s.debugAction.SetVisible(true)
				log.Printf("调试模式菜单已显示")
			}
		}
	})
	s.setTip("蜃境-简单安全的组网工具")

	s.data = NewDataPool()

	s.setIcon(Logo)

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

	s.debugAction, err = s.newDebugField()
	if err != nil {
		log.Printf("初始化调试菜单区错误：%s", err)
	}

	s.exitAction = walk.NewAction()
	s.exitAction.SetText("退出")
	s.exitAction.Triggered().Attach(func() {
		os.Exit(0)
	})

	s.tray.ContextMenu().Actions().Add(s.exitAction)

	s.connectField.loginAction.Triggered().Attach(s.DoLogin)
	s.userField.logoutAction.Triggered().Attach(s.DoLogout)
	s.connectField.connectAction.Triggered().Attach(s.DoConn)
	s.connectField.disconnectAction.Triggered().Attach(s.DoDisconn)

	s.exitField.exitAllowLocalAction.Triggered().Attach(s.SetAllowLocalNet)
	s.exitField.exitRunExitAction.Triggered().Attach(s.SetAsExitNode)

	s.prefField.prefAllowIncomeAction.Triggered().Attach(s.SetAllowIncome)
	s.prefField.prefUsingDNSAction.Triggered().Attach(s.SetDNSOpt)
	s.prefField.prefUsingSubnetAction.Triggered().Attach(s.SetSubnetOpt)
	s.prefField.prefUnattendAction.Triggered().Attach(s.SetUnattendOpt)
	s.prefField.prefToDefaultAction.Triggered().Attach(s.SetPrefsDefault)

	s.prefField.aboutAction.Triggered().Attach(s.ShowAbout)

	s.nodeField.nodeAction.Triggered().Attach(func() {
		if s.data.NetMap != nil {
			selfIPv4 := s.data.NetMap.Addresses[0].Addr()
			if !selfIPv4.Is4() {
				if len(s.data.NetMap.Addresses) > 1 {
					selfIPv4 = s.data.NetMap.Addresses[1].Addr()
				}
			}
			walk.Clipboard().SetText(selfIPv4.String())
			s.SendNotify("我的地址", "已复制IP地址 ("+selfIPv4.String()+") 到剪贴板", NL_Info)
		}
	})

	s.bindDataPool()
}

func (s *MiraMenu) setIcon(state IconType) {
	if err := s.tray.SetIcon(Icons[state]); err != nil {
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
				os.Exit(-1)
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

			s.data.SetPrefs(prefs)
			s.data.SetVersion(st.Version)
			s.data.SetState(st.BackendState)

			s.mw.Run()
		}
	}
}
