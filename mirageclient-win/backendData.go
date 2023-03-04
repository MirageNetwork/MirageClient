package main

import (
	"log"

	"tailscale.com/ipn"
	"tailscale.com/tailcfg"
	"tailscale.com/types/netmap"
)

type backendData struct {
	magicCounter  int                    // 用于1秒内左键点击图标五次开启debug菜单项
	AuthKey       string                 // 认证密钥
	Version       string                 // 后端版本
	ErrMessage    *string                // 错误消息
	State         ipn.State              // 运行状态
	Prefs         *ipn.Prefs             // 本地配置
	NetMap        *netmap.NetworkMap     // 网络状态图
	BackendLogID  *string                // 后端日志ID
	ClientVersion *tailcfg.ClientVersion // 客户端最新版本号

	StatePuber  *DataEventPublisher
	PrefsPuber  *DataEventPublisher
	NetmapPuber *DataEventPublisher
}

func NewBackendData() *backendData {
	bd := &backendData{
		magicCounter:  0,
		Version:       "",
		ErrMessage:    nil,
		State:         ipn.NoState,
		Prefs:         &ipn.Prefs{},
		NetMap:        &netmap.NetworkMap{},
		BackendLogID:  nil,
		ClientVersion: &tailcfg.ClientVersion{},

		StatePuber:  &DataEventPublisher{},
		PrefsPuber:  &DataEventPublisher{},
		NetmapPuber: &DataEventPublisher{},
	}

	return bd
}

func (bd *backendData) SetAuthKey(newV string) {
	bd.AuthKey = newV

}
func (bd *backendData) SetVersion(newV string) {
	if bd.Version == newV {
		return
	}
	bd.Version = newV
}
func (bd *backendData) SetState(newV ipn.State) {
	if bd.State == newV {
		return
	}
	bd.State = newV
	bd.StatePuber.Publish(bd.State)
}
func (bd *backendData) SetPrefs(newV *ipn.Prefs) {
	bd.Prefs = newV
	bd.PrefsPuber.Publish(newV)
}
func (bd *backendData) SetNetMap(newV *netmap.NetworkMap) {
	bd.NetMap = newV
	bd.NetmapPuber.Publish(newV)
}

func (bd *backendData) StateChanged() *DataEvent {
	return bd.StatePuber.Event()
}
func (bd *backendData) PrefsChanged() *DataEvent {
	return bd.PrefsPuber.Event()
}
func (bd *backendData) NetmapChanged() *DataEvent {
	return bd.NetmapPuber.Event()
}

func (s *MiraMenu) LoadPrefs() {
	prefs, err := s.lc.GetPrefs(s.ctx)
	if err != nil {
		s.SendNotify("加载配置错误", err.Error(), NL_Error) // 通知栏提示
		log.Printf("加载配置错误：%s", err)
		return
	}
	s.backendData.Prefs = prefs
	log.Printf("加载配置：%v", prefs)
}
