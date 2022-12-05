package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"sync"
	"syscall"
	"time"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
)

func updatePrefs(st *ipnstate.Status, prefs, curPrefs *ipn.Prefs) (simpleUp bool, justEditMP *ipn.MaskedPrefs, err error) {
	if prefs.OperatorUser == "" && curPrefs.OperatorUser == os.Getenv("USER") {
		prefs.OperatorUser = curPrefs.OperatorUser
	}
	tagsChanged := !reflect.DeepEqual(curPrefs.AdvertiseTags, prefs.AdvertiseTags)
	simpleUp = curPrefs.Persist != nil &&
		curPrefs.Persist.LoginName != "" &&
		st.BackendState != ipn.NeedsLogin.String()
	justEdit := st.BackendState == ipn.Running.String() && !tagsChanged

	if justEdit {
		justEditMP = new(ipn.MaskedPrefs)
		justEditMP.WantRunningSet = true
		justEditMP.Prefs = *prefs
		justEditMP.ControlURLSet = true
	}

	return simpleUp, justEditMP, nil
}

func kickOffLogin(notifyCh chan Notify) {
	st, err := LC.Status(ctx)
	if err != nil {
		logNotify("获取当前守护程序状态失败！", err)
	}
	origAuthURL := st.AuthURL
	fmt.Println(origAuthURL)

	prefs := CreateDefaultPref()
	curPrefs, err := localClient.GetPrefs(ctx)
	if err != nil {
		logNotify("获取当前守护程序Pref失败！", err)
	}
	prefs.ProfileName = curPrefs.ProfileName

	_, justEditMP, err := updatePrefs(st, prefs, curPrefs)
	if err != nil {
		logNotify("获取Pref更新模式失败！", err)
	}
	if justEditMP != nil {
		justEditMP.EggSet = false
		_, err := localClient.EditPrefs(ctx, justEditMP)
		if err != nil {
			logNotify("守护进程配置信息无法更新", err)
		}
	}

	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	watcher, err := localClient.WatchIPNBus(watchCtx, 0)
	if err != nil {
		logNotify("守护进程监听管道建立失败", err)
		return
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

	running := make(chan bool, 1) // gets value once in state ipn.Running
	pumpErr := make(chan error, 1)

	var loginOnce sync.Once
	startLoginInteractive := func() { loginOnce.Do(func() { localClient.StartLoginInteractive(ctx) }) }

	getURLNotify := make(chan Notify, 1)
	go func(Ch chan Notify) {
		for {
			n, err := watcher.Next()
			if err != nil {
				pumpErr <- err
				return
			}
			if n.ErrMessage != nil {
				msg := *n.ErrMessage
				logNotify("守护进程错误："+msg, errors.New(msg))
				return
			}
			if s := n.State; s != nil {
				switch *s {
				case ipn.NeedsLogin:
					startLoginInteractive()
				case ipn.NeedsMachineAuth:
					logNotify("机器需要认证！", errors.New("机器需要认证"))
				case ipn.Running:
					select {
					case running <- true:
					default:
					}
					cancelWatch()
				}
			}
			if url := n.BrowseToURL; url != nil {
				//logNotify("请访问："+*url, errors.New("机器需访问URL"))
				sendNotify := Notify{
					NType: OpenURL,
					NMsg:  *url,
				}
				Ch <- sendNotify
			}
		}
	}(getURLNotify)

	if err := localClient.Start(ctx, ipn.Options{
		AuthKey:     "",
		UpdatePrefs: prefs,
	}); err != nil {
		logNotify("无法开始", err)
	}

	var timeoutCh <-chan time.Time
	select {
	case <-running:
		return
	case <-watchCtx.Done():
		select {
		case <-running:
			return
		default:
		}
		logNotify("watcher错误", watchCtx.Err())
		return
	case err := <-pumpErr:
		select {
		case <-running:
			return
		default:
		}
		logNotify("pump错误", err)
		return
	case <-timeoutCh:
		logNotify("超时错误", errors.New("TimeOut!"))
		return
	case sender := <-getURLNotify:
		notifyCh <- sender
		fmt.Println("I got the URL, pls open it!")
		return
	}
}
