//go:build windows
package main

import (
	"tailscale.com/ipn"
	"tailscale.com/tailcfg"
	"tailscale.com/types/netmap"
)

type DataPool struct {
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

func NewDataPool() *DataPool {
	pool := &DataPool{
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

	return pool
}

// 以下为数据设置

func (pool *DataPool) SetAuthKey(newV string) {
	pool.AuthKey = newV

}
func (pool *DataPool) SetVersion(newV string) {
	if pool.Version == newV {
		return
	}
	pool.Version = newV
}
func (pool *DataPool) SetState(newV string) {
	state := map[string]ipn.State{
		"NoState":          ipn.NoState,
		"InUseOtherUser":   ipn.InUseOtherUser,
		"NeedsLogin":       ipn.NeedsLogin,
		"NeedsMachineAuth": ipn.NeedsMachineAuth,
		"Stopped":          ipn.Stopped,
		"Starting":         ipn.Starting,
		"Running":          ipn.Running,
	}[newV]
	if pool.State == state {
		return
	}
	pool.State = state
	pool.StatePuber.Publish(pool.State)
}
func (pool *DataPool) SetPrefs(newV *ipn.Prefs) {
	pool.Prefs = newV
	pool.PrefsPuber.Publish(newV)
}
func (pool *DataPool) SetNetMap(newV *netmap.NetworkMap) {
	pool.NetMap = newV
	pool.NetmapPuber.Publish(newV)
}

// 以下为事件订阅

func (pool *DataPool) StateChanged() *DataEvent {
	return pool.StatePuber.Event()
}
func (pool *DataPool) PrefsChanged() *DataEvent {
	return pool.PrefsPuber.Event()
}
func (pool *DataPool) NetmapChanged() *DataEvent {
	return pool.NetmapPuber.Event()
}
