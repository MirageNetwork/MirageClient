//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/skratchdot/open-golang/open"
	"tailscale.com/ipn"
)

func WatchDaemon(ctx context.Context) {
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	<-watcherUpCh
	watcher, err := LC.WatchIPNBus(watchCtx, 0)
	retryCounter := 3
	for {
		if err == nil {
			log.Error().
				Msg("守护进程监听管道建立完成")
			break
		} else if retryCounter < 0 {
			logNotify("无法建立守护进程监听管道", err)
			break
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
		if st := n.State; st != nil {
			switch *st {
			case ipn.NeedsLogin:
				if wantRun {
					LC.StartLoginInteractive(ctx)
				}
				stNeedLoginCh <- true
			case ipn.Stopped:
				stStopCh <- true
			case ipn.Running:
				stRunCh <- true
			}
		}
		if loginFin := n.LoginFinished; loginFin != nil {
			go gui.logoSpin(300)
		}
		if url := n.BrowseToURL; url != nil {
			if authURL == "" && wantRun {
				open.Run(*url)
			}
			authURL = *url
		}
	}
}
