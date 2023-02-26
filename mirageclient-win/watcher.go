//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/skratchdot/open-golang/open"
	"tailscale.com/ipn"
)

type watcherMsgType int

const (
	Fatal watcherMsgType = iota
	Error
)

type watcherMsg struct {
	Type watcherMsgType
	data interface{}
}

type MiraWatcher struct { // 通讯协程实体
	mu        sync.Mutex         // 状态锁
	ctx       context.Context    // 通讯协程上下文
	Stop      context.CancelFunc // 通讯协程退出函数
	isRunning bool               // 通信协程运行状态
	Rx        chan watcherMsg    // 通信携程接收管道
	Tx        chan watcherMsg    // 通信协程发送管道
}

// 创建通讯协程函数
func NewWatcher() *MiraWatcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &MiraWatcher{
		ctx:       ctx,
		Stop:      cancel,
		isRunning: false,
		Rx:        make(chan watcherMsg, 3), // TODO:暂时设置缓存3条
		Tx:        make(chan watcherMsg, 3), // TODO:暂时设置缓存3条
	}
}

func (w *MiraWatcher) GetStatus() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.isRunning
}
func (w *MiraWatcher) SetStatus(v bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.isRunning = v
}

func (w *MiraWatcher) Start() error {

	// 检查服务是否在正常运行
	if !isServiceRunning() { // 未在正常运行以管理员权限调用尝试使其正常运行
		err := elevateToStartService()
		if err != nil {
			w.Tx <- watcherMsg{
				Type: Fatal,
				data: err,
			}
			return err
		}
	}
	// 之后试探状态
	for !isServiceRunning() {
		select {
		case <-time.Tick(time.Second):
		case <-time.After(time.Second * 30):
			break
		}
	}
	if !isServiceRunning() {
		err := errors.New("后台服务未正常运行")
		w.Tx <- watcherMsg{
			Type: Fatal,
			data: err,
		}
		return err
	}

	WatchDaemon(ctx)

	return nil
}

func WatchDaemon(ctx context.Context) {
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()

	watcher, err := LC.WatchIPNBus(watchCtx, 0)
	retryCounter := 3
	for {
		if err == nil {
			log.Error().
				Msg("守护进程监听管道建立完成")
			break
		} else if retryCounter < 0 {
			logNotify("无法建立守护进程监听管道", err)
			return // Todo
		}
		log.Error().
			Msg("守护进程监听管道建立失败,等待1秒重试:" + err.Error())
		<-time.After(time.Second * 1)
		retryCounter--
		watcher, err = LC.WatchIPNBus(watchCtx, 0)
	}
	defer watcher.Close()

	go func() {
		interrupt := make(chan os.Signal, 1)
		signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)
		select {
		case <-interrupt:
			cancelWatch()
		case <-watchCtx.Done():
		}
	}()

	for {
		n, err := watcher.Next()
		if err != nil {
			fmt.Println("[ERROR] " + err.Error())
			continue
		}

		if nm := n.NetMap; nm != nil {
			netMapChn <- true
		}

		if pref := n.Prefs; pref != nil {
			prefChn <- true
		}
		if st := n.State; st != nil {
			switch *st {
			case ipn.NeedsLogin:
				stNeedLoginCh <- true
			case ipn.Stopped:
				stStopCh <- true
			case ipn.Starting:
				stStartingCh <- true
			case ipn.Running:
				stRunCh <- true
			}
		}
		if url := n.BrowseToURL; url != nil {
			prefs, err := LC.GetPrefs(ctx)
			if err != nil {
				break
			}
			if prefs.WantRunning {
				open.Run(*url)
				fmt.Println("I opened this url: " + *url)
			}
		}
	}
}
