package main

import (
	"log"

	"github.com/tailscale/walk"
	"tailscale.com/ipn"
	"tailscale.com/tailcfg"
	"tailscale.com/types/netmap"
)

type backendData struct {
	magicCounter  int                    // 用于2秒内左键点击图标五次开启debug菜单项
	Version       string                 // 后端版本
	ErrMessage    *string                // 错误消息
	Prefs         *ipn.Prefs             // 本地配置
	NetMap        *netmap.NetworkMap     // 网络状态图
	BackendLogID  *string                // 后端日志ID
	ClientVersion *tailcfg.ClientVersion // 客户端最新版本号

	loadPuber *DataEventPublisher

	ErrMessagePuber *walk.StringEventPublisher
	PrefsPuber      *DataEventPublisher
	NetmapPuber     *DataEventPublisher
}

func NewBackendData() *backendData {
	bd := &backendData{
		magicCounter:  0,
		Version:       "",
		ErrMessage:    nil,
		Prefs:         &ipn.Prefs{},
		NetMap:        &netmap.NetworkMap{},
		BackendLogID:  nil,
		ClientVersion: &tailcfg.ClientVersion{},

		loadPuber:       &DataEventPublisher{},
		ErrMessagePuber: &walk.StringEventPublisher{},
		PrefsPuber:      &DataEventPublisher{},
		NetmapPuber:     &DataEventPublisher{},
	}

	return bd
}

func (bd *backendData) SetVersion(newV string) {
	if bd.Version == newV {
		return
	}
	bd.Version = newV
}

func (bd *backendData) SetPrefs(newV *ipn.Prefs) {
	bd.Prefs = newV
	bd.PrefsPuber.Publish(newV)
}
func (bd *backendData) SetNetMap(newV *netmap.NetworkMap) {
	bd.NetMap = newV
	bd.NetmapPuber.Publish(newV)
}

func (bd *backendData) PrefsChanged() *DataEvent {
	return bd.PrefsPuber.Event()
}
func (bd *backendData) NetmapChanged() *DataEvent {
	return bd.NetmapPuber.Event()
}

func (bd *backendData) LoadPrefs(loader func() (*ipn.Prefs, error)) {
	newPrefs, err := loader()
	if err != nil {
		log.Printf("加载配置错误：%s", err)
	}
	bd.Prefs = newPrefs
	bd.PrefsPuber.Publish(newPrefs)
}
