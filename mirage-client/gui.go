//go:build windows

package main

import (
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/gen2brain/beeep"
	"github.com/rs/zerolog/log"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/mirage-client/resource"
	"tailscale.com/mirage-client/systray"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

type NodeMenuItem struct {
	Menu *systray.MenuItem
	Peer ipnstate.PeerStatus
}
type NodeListMenuItem struct {
	Outer  *systray.MenuItem
	Header *systray.MenuItem
	Line   *systray.SeparatorItem
	Nodes  map[netip.Addr]NodeMenuItem
}

func (s *NodeListMenuItem) init(o *systray.MenuItem) {
	s.Outer = o.AddSubMenuItem("", "")
	s.Header = s.Outer.AddSubMenuItem("", "")
	s.Header.Disable()
	s.Line = s.Outer.AddSeparator()
	s.Nodes = make(map[netip.Addr]NodeMenuItem)
}
func (s *NodeListMenuItem) hideMe() {
	for _, node := range s.Nodes {
		node.Menu.Hide()
	}
	s.Outer.Hide()
}
func (s *NodeListMenuItem) showMe() {
	s.Outer.Show()
}

type NodeListMenu struct {
	Outer       *systray.MenuItem
	myNodes     NodeListMenuItem
	Line        *systray.SeparatorItem
	friendNodes map[tailcfg.UserID]*NodeListMenuItem
}

func (s *NodeListMenu) hideAllNodes() {
	s.Outer.Hide()
	s.myNodes.hideMe()
	for _, friNode := range s.friendNodes {
		friNode.hideMe()
	}
}

func (s *NodeListMenu) update(st *ipnstate.Status) {
	openMyNodeList := false
	openFriNodeList := make(map[tailcfg.UserID]bool)

	s.hideAllNodes()

	//遍历st的peer进行更新或新增
	for _, peer := range st.Peer {

		//是否自有节点
		if peer.UserID == st.Self.UserID && peer.Online {
			openMyNodeList = true
			tmpIPAddr := peer.TailscaleIPs[0]
			if tmpIPAddr.Is6() {
				tmpIPAddr = peer.TailscaleIPs[1]
			}
			if node, exist := s.myNodes.Nodes[tmpIPAddr]; exist {
				node.Peer = *peer
				titleName := strings.Split(peer.DNSName, ".")[0]
				if titleName != peer.HostName {
					titleName = titleName + "(" + peer.HostName + ")"
				}
				node.Menu.SetTitle(titleName)
				node.Menu.Show()

			} else {
				titleName := strings.Split(peer.DNSName, ".")[0]
				if titleName != peer.HostName {
					titleName = titleName + "(" + peer.HostName + ")"
				}
				tmpNodeMenu := s.myNodes.Outer.AddSubMenuItem(titleName, tmpIPAddr.String())
				s.myNodes.Nodes[tmpIPAddr] = NodeMenuItem{
					Menu: tmpNodeMenu,
					Peer: *peer,
				}
				go func(menuItem NodeMenuItem) {
					for {
						select {
						case <-menuItem.Menu.ClickedCh:
							if menuItem.Peer.TailscaleIPs[0].Is4() {
								clipboard.WriteAll(menuItem.Peer.TailscaleIPs[0].String())
							} else {
								clipboard.WriteAll(menuItem.Peer.TailscaleIPs[1].String())
							}
							logNotify("设备"+strings.Split(menuItem.Peer.DNSName, ".")[0]+"的IP已复制", errors.New(""))
						}
					}
				}(s.myNodes.Nodes[tmpIPAddr])
			}
		} else if peer.Online { //非自有节点
			openFriNodeList[peer.UserID] = true
			tmpIPAddr := peer.TailscaleIPs[0]
			if tmpIPAddr.Is6() {
				tmpIPAddr = peer.TailscaleIPs[1]
			}

			if friNode, exist := s.friendNodes[peer.UserID]; exist { //是否已存在友节点菜单
				if node, exist := friNode.Nodes[tmpIPAddr]; exist {
					node.Peer = *peer
					node.Menu.SetTitle(peer.DNSName)
					node.Menu.Show()
				} else {
					tmpNodeMenu := friNode.Outer.AddSubMenuItem(peer.DNSName, tmpIPAddr.String())
					friNode.Nodes[tmpIPAddr] = NodeMenuItem{
						Menu: tmpNodeMenu,
						Peer: *peer,
					}
					go func(menuItem NodeMenuItem) {
						for {
							select {
							case <-menuItem.Menu.ClickedCh:
								if menuItem.Peer.TailscaleIPs[0].Is4() {
									clipboard.WriteAll(menuItem.Peer.TailscaleIPs[0].String())
								} else {
									clipboard.WriteAll(menuItem.Peer.TailscaleIPs[1].String())
								}
								logNotify("设备"+menuItem.Peer.DNSName+"的IP已复制", errors.New(""))
							}
						}
					}(friNode.Nodes[tmpIPAddr])
				}
			} else {
				s.friendNodes[peer.UserID] = new(NodeListMenuItem)
				s.friendNodes[peer.UserID].init(s.Outer)
				tmpNodeMenu := s.friendNodes[peer.UserID].Outer.AddSubMenuItem(peer.DNSName, tmpIPAddr.String())
				s.friendNodes[peer.UserID].Nodes[tmpIPAddr] = NodeMenuItem{
					Menu: tmpNodeMenu,
					Peer: *peer,
				}
				go func(menuItem NodeMenuItem) {
					for {
						select {
						case <-menuItem.Menu.ClickedCh:
							if menuItem.Peer.TailscaleIPs[0].Is4() {
								clipboard.WriteAll(menuItem.Peer.TailscaleIPs[0].String())
							} else {
								clipboard.WriteAll(menuItem.Peer.TailscaleIPs[1].String())
							}
							logNotify("设备"+menuItem.Peer.DNSName+"的IP已复制", errors.New(""))
						}
					}
				}(s.friendNodes[peer.UserID].Nodes[tmpIPAddr])
			}
		}
	}
	//更新用户信息显示部分
	showNodeListMenu := false
	showNodeListLine := false
	for uid, user := range st.User {
		if uid == st.Self.UserID && openMyNodeList {
			s.myNodes.Header.SetTitle(user.LoginName)
			s.myNodes.showMe()
			showNodeListMenu = true
		} else if needOpen, exist := openFriNodeList[uid]; exist && needOpen {
			s.friendNodes[uid].Outer.SetTitle(user.DisplayName)
			s.friendNodes[uid].Header.SetTitle(user.LoginName)
			s.friendNodes[uid].showMe()
			showNodeListMenu = true
			if openMyNodeList {
				showNodeListLine = true
			}
		}
	}
	if showNodeListMenu {
		s.Outer.Show()
	} else {
		s.Outer.Hide()
	}
	if showNodeListLine {
		s.Line.Show()
	} else {
		s.Line.Hide()
	}
}

func (s *NodeListMenu) init() {
	s.Outer = systray.AddMenuItem("网内设备", "查看有哪些设备可访问")
	s.myNodes.init(s.Outer)
	s.myNodes.Outer.SetTitle("我的设备")
	s.myNodes.Outer.SetTooltip("隶属于我的设备")
	s.Line = s.Outer.AddSeparator()
	s.friendNodes = make(map[tailcfg.UserID]*NodeListMenuItem)
}
func (s *NodeListMenu) Hide() {
	s.Outer.Hide()
}

type ExitNodeListMenu struct {
	Outer     *systray.MenuItem
	NoneExit  *systray.MenuItem
	ExitNodes map[key.NodePublic]NodeMenuItem

	AllowLocalNetworkAccess *systray.MenuItem
	RunExitNode             *systray.MenuItem
	TailLine                *systray.SeparatorItem
}

func (s *ExitNodeListMenu) update(st *ipnstate.Status) {
	currentExitNodeName := ""
	for key, exitnode := range s.ExitNodes {
		exitnode.Menu.Uncheck()
		if peerst, ok := st.Peer[key]; !ok || !peerst.ExitNodeOption {
			exitnode.Menu.Hide()
		} else {
			if st.ExitNodeStatus != nil && st.ExitNodeStatus.ID == st.Peer[key].ID {
				currentExitNodeName = st.Peer[key].DNSName
				if st.Peer[key].UserID == st.Self.UserID {
					currentExitNodeName = strings.Split(currentExitNodeName, ".")[0]
				}
				s.NoneExit.Uncheck()
				exitnode.Menu.Check()
			}
			exitnode.Peer = *st.Peer[key]
			exitnode.Menu.Show()

		}
	}
	for key, peerst := range st.Peer {
		if peerst.ExitNodeOption {
			if _, ok := s.ExitNodes[key]; !ok {
				checked := false
				if st.ExitNodeStatus != nil && peerst.ID == st.ExitNodeStatus.ID {
					currentExitNodeName = peerst.DNSName
					if peerst.UserID == st.Self.UserID {
						currentExitNodeName = strings.Split(currentExitNodeName, ".")[0]
					}
					checked = true
					s.NoneExit.Uncheck()
				}
				nodename := peerst.DNSName
				if peerst.UserID == st.Self.UserID {
					nodename = strings.Split(nodename, ".")[0]
				}
				tmpExitNodeMenu := s.Outer.AddSubMenuItemCheckbox(nodename, "", checked)

				s.ExitNodes[key] = NodeMenuItem{
					Menu: tmpExitNodeMenu,
					Peer: *peerst,
				}
				go func(menuItem NodeMenuItem) {
					for {
						select {
						case <-menuItem.Menu.ClickedCh:
							if !menuItem.Menu.Checked() {
								switchExitNode(menuItem.Peer.ID)
							}
						}
					}
				}(s.ExitNodes[key])
			}
		}
	}

	if currentExitNodeName != "" {
		s.Outer.SetTitle("出口节点(" + currentExitNodeName + ")")
	} else {
		s.Outer.SetTitle("出口节点")
		s.NoneExit.Check()
	}
}

func (s *ExitNodeListMenu) init() {
	s.Outer = systray.AddMenuItem("出口节点", "出口节点列表")
	s.NoneExit = s.Outer.AddSubMenuItemCheckbox("不使用", "不使用任何出口节点", true)
	s.ExitNodes = make(map[key.NodePublic]NodeMenuItem)
	s.AllowLocalNetworkAccess = systray.AddMenuItemCheckbox("允许本地网络访问", "使用出口节点时对本地网络保持允许访问", false)
	s.RunExitNode = systray.AddMenuItem("用作出口节点…", "将本机用于出口节点")
	s.TailLine = systray.AddSeparator()
}

func (s *ExitNodeListMenu) Hide() {
	s.Outer.Hide()
	s.AllowLocalNetworkAccess.Hide()
	s.RunExitNode.Hide()
	s.TailLine.Hide()
}

func (s *ExitNodeListMenu) ShowDisabled() {
	s.Outer.Disable()
	s.AllowLocalNetworkAccess.Disable()
	s.RunExitNode.Disable()
	s.Outer.Show()
	s.AllowLocalNetworkAccess.Show()
	s.RunExitNode.Show()
	s.TailLine.Show()
}

func (s *ExitNodeListMenu) Show() {
	s.Outer.Enable()
	s.AllowLocalNetworkAccess.Enable()
	s.RunExitNode.Enable()
	s.Outer.Show()
	s.AllowLocalNetworkAccess.Show()
	s.RunExitNode.Show()
	s.TailLine.Show()
}

type MirageMenu struct {
	isLogoSpin     bool
	logoSpinChn    chan bool
	logoSpinFinChn chan bool

	loginMenu   *systray.MenuItem //登录按钮
	connectMenu *systray.MenuItem //连接按钮
	disconnMenu *systray.MenuItem //断开按钮
	//添加一个分割线
	userMenu *systray.MenuItem //用户下拉菜单 - 初步有登出，后续有添加用户、切换用户、访问管理面板
	//设计为头像、displayname、loginname
	userConsoleMenu *systray.MenuItem
	userLogoutMenu  *systray.MenuItem
	//添加一个分割线
	nodeMenu     *systray.MenuItem //本结点按钮：显示本设备、dnsname(Mirage IP)，单击进行复制
	nodeListMenu NodeListMenu      //在网设备菜单：下级为：我的设备菜单、其他各用户设备菜单
	nodePartLine *systray.SeparatorItem
	//添加一个分割线
	///下列是后续待添加项目
	exitNodeMenu  ExitNodeListMenu  //出口节点列表菜单
	optionMenu    *systray.MenuItem //配置项目菜单
	optSubnetMenu *systray.MenuItem //配置-应用子网转发开关
	optDNSMenu    *systray.MenuItem //配置-应用DNS开关
	//待添加部分完
	versionMenu *systray.MenuItem //关于菜单：目前显示版本号
	quitMenu    *systray.MenuItem //退出按钮
}

func (s *MirageMenu) init() {
	s.isLogoSpin = false
	s.logoSpinChn = make(chan bool, 1)
	s.logoSpinFinChn = make(chan bool, 1)

	systray.SetTemplateIcon(resource.LogoIcon, resource.LogoIcon)
	systray.SetTitle("蜃境")
	systray.SetTooltip("简单安全的组网工具")

	s.loginMenu = systray.AddMenuItem("登录…", "点击进行登录")
	s.connectMenu = systray.AddMenuItem("连接", "点击接入蜃境")
	s.disconnMenu = systray.AddMenuItem("断开", "临时切断蜃境连接")
	systray.AddSeparator()
	s.userMenu = systray.AddMenuItem("", "")

	s.userConsoleMenu = s.userMenu.AddSubMenuItem("控制台", "")
	s.userLogoutMenu = s.userMenu.AddSubMenuItem("登出", "")
	systray.AddSeparator()
	s.nodeMenu = systray.AddMenuItem("本设备", "单击复制本节点IP")
	s.nodeListMenu.init()
	s.nodePartLine = systray.AddSeparator()
	s.exitNodeMenu.init()
	s.optionMenu = systray.AddMenuItem("配置项", "配置该设备蜃境网络")
	s.optDNSMenu = s.optionMenu.AddSubMenuItemCheckbox("使用DNS设置", "是否使用蜃境网络的DNS配置", false)
	s.optSubnetMenu = s.optionMenu.AddSubMenuItemCheckbox("使用子网转发", "是否使用蜃境网络的子网转发", false)

	s.versionMenu = systray.AddMenuItem("", "点击查看详细信息")
	s.quitMenu = systray.AddMenuItem("退出", "退出蜃境")
}

func (s *MirageMenu) hideAll() {
	s.loginMenu.Hide()
	s.connectMenu.Hide()
	s.disconnMenu.Hide()

	s.userMenu.Hide()
	s.userLogoutMenu.Hide()
	s.nodeMenu.Hide()
	s.nodeListMenu.Hide()
	s.nodePartLine.Hide()

	s.versionMenu.Hide()
	s.quitMenu.Hide()
}

func (s *MirageMenu) setNotLogin(version string) {

	if s.isLogoSpin {
		s.logoSpinChn <- true
		<-s.logoSpinFinChn
		s.isLogoSpin = false
	}
	systray.SetTemplateIcon(resource.LogoIcon, resource.LogoIcon)

	s.loginMenu.Enable()
	s.loginMenu.SetTitle("登录")
	s.loginMenu.Show()
	s.connectMenu.Hide()
	s.disconnMenu.Hide()

	s.userMenu.SetTitle("请先登录")
	s.userMenu.Disable()
	s.userMenu.Show()
	s.userLogoutMenu.Hide()
	s.nodeMenu.Hide()
	s.nodeListMenu.Hide()
	s.nodePartLine.Hide()

	s.exitNodeMenu.Hide()

	s.versionMenu.SetTitle(version)
	s.versionMenu.Show()
	s.quitMenu.Show()
}

func (s *MirageMenu) setStopped(userDisplayName string, version string) {

	if s.isLogoSpin {
		s.logoSpinChn <- true
		<-s.logoSpinFinChn
		s.isLogoSpin = false
	}
	systray.SetTemplateIcon(resource.Logom, resource.Logom)

	s.loginMenu.Hide()
	s.connectMenu.Show()
	s.disconnMenu.Hide()

	s.userMenu.SetTitle(userDisplayName)
	s.userMenu.Enable()
	s.userMenu.Show()
	s.userLogoutMenu.Show()

	s.nodeMenu.SetTitle("本设备")
	s.nodeMenu.Disable()
	s.nodeMenu.Show()
	s.nodeListMenu.Outer.Disable()
	s.nodeListMenu.Outer.Show()
	s.nodePartLine.Show()

	s.exitNodeMenu.ShowDisabled()

	s.versionMenu.SetTitle(version)
	s.versionMenu.Show()
	s.quitMenu.Show()
}

func (s *MirageMenu) setRunning(userDisplayName string, nodeDNSName string, nodeMIP string, version string) {

	if s.isLogoSpin {
		s.logoSpinChn <- true
		<-s.logoSpinFinChn
		s.isLogoSpin = false
	}
	systray.SetTemplateIcon(resource.Mlogo, resource.Mlogo)

	s.loginMenu.Hide()
	s.connectMenu.Hide()
	s.disconnMenu.Show()

	s.userMenu.SetTitle(userDisplayName)
	s.userMenu.Enable()
	s.userMenu.Show()
	s.userLogoutMenu.Show()

	s.nodeMenu.SetTitle("本设备：" + nodeDNSName + " (" + nodeMIP + ")")
	s.nodeMenu.Enable()
	s.nodeMenu.Show()
	s.nodeListMenu.Outer.Enable()
	s.nodeListMenu.Outer.Show()
	s.nodePartLine.Show()

	s.exitNodeMenu.Show()

	s.versionMenu.SetTitle(version)
	s.versionMenu.Show()
	s.quitMenu.Show()
}

func (s *MirageMenu) updateNodeList(st *ipnstate.Status) {
	s.nodeListMenu.hideAllNodes()

}

func (s *MirageMenu) logoSpin(interval time.Duration) {

	s.isLogoSpin = true
	s.loginMenu.SetTitle("连接中…")
	s.loginMenu.Disable()

	for {
		select {
		case <-s.logoSpinChn:
			s.logoSpinFinChn <- true
			return
		default:
			systray.SetTemplateIcon(resource.Mlogo1, resource.Mlogo1)
			<-time.After(interval * time.Millisecond)
			systray.SetTemplateIcon(resource.Mlogo2, resource.Mlogo2)
			<-time.After(interval * time.Millisecond)
		}
	}
}

func logNotify(msg string, err error) {
	log.Error().Msg(msg + err.Error())
	beeep.Notify(app_name, msg, logo_png)
}
