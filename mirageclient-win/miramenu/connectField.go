package miramenu

import (
	"github.com/tailscale/walk"
	"tailscale.com/ipn"
)

type connectField struct {
	loginAction      *walk.Action // 登录按钮
	connectAction    *walk.Action // 连接按钮
	disconnectAction *walk.Action // 断开按钮
}

func NewConnectField(al *walk.ActionList, rs *runState) (cf *connectField, err error) {
	cf = &connectField{}
	cf.loginAction = walk.NewAction()
	cf.loginAction.SetText("登录…")
	cf.connectAction = walk.NewAction()
	cf.connectAction.SetVisible(false)
	cf.disconnectAction = walk.NewAction()
	cf.disconnectAction.SetText("断开")
	cf.disconnectAction.SetVisible(false)

	// 待登录态连接区样式
	rs.Changed().Attach(func(data interface{}) {
		state := data.(int)
		switch ipn.State(state) {
		case ipn.Stopped:
			cf.connectAction.SetText("连接")
			cf.connectAction.SetEnabled(true)
			cf.connectAction.SetVisible(true)
			cf.disconnectAction.SetVisible(false)
			cf.loginAction.SetVisible(false)
		case ipn.Starting:
			cf.connectAction.SetEnabled(false)
			cf.connectAction.SetText("正在连接……")
			cf.connectAction.SetVisible(true)
			cf.disconnectAction.SetEnabled(true)
			cf.disconnectAction.SetVisible(true)
			cf.loginAction.SetVisible(false)
		case ipn.Running:
			cf.connectAction.SetText("已连接")
			cf.connectAction.SetEnabled(false)
			cf.connectAction.SetVisible(true)
			cf.disconnectAction.SetEnabled(true)
			cf.disconnectAction.SetVisible(true)
			cf.loginAction.SetVisible(false)
		default:
			cf.connectAction.SetVisible(false)
			cf.disconnectAction.SetVisible(false)
			cf.loginAction.SetVisible(true)
		}
	})

	if err := al.Add(cf.loginAction); err != nil {
		return nil, err
	}
	if err := al.Add(cf.connectAction); err != nil {
		return nil, err
	}
	if err := al.Add(cf.disconnectAction); err != nil {
		return nil, err
	}
	if err := al.Add(walk.NewSeparatorAction()); err != nil {
		return nil, err
	}
	return cf, nil
}
