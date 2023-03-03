package main

import (
	"github.com/tailscale/walk"
	"tailscale.com/ipn"
)

type runState struct {
	state     ipn.State
	publisher *DataEventPublisher
	finPuber  *walk.EventPublisher
}

func NewRunState() *runState {
	rs := &runState{
		state:     ipn.NoState,
		publisher: &DataEventPublisher{},
		finPuber:  &walk.EventPublisher{},
	}

	return rs
}

// 设置当前运行态
func (rs *runState) Set(newState ipn.State) {
	if rs.state == newState {
		return
	}
	rs.state = newState
	rs.publisher.Publish(int(rs.state))
}

// 返回当前状态
func (rs *runState) Get() ipn.State {
	return rs.state
}

// 当前登陆运行态是否变化事件
func (rs *runState) Changed() *DataEvent {
	return rs.publisher.Event()
}

// 登录完成事件
func (rs *runState) LoginFinish() *walk.Event {
	return rs.finPuber.Event()
}

// 通知完成登录
func (rs *runState) noticeLoginFinish() {
	rs.finPuber.Publish()
}
