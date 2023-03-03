//go:build windows

package main

import (
	"context"
	"flag"
	"log"
	"time"

	"golang.org/x/sys/windows/svc"
	"tailscale.com/envknob"
	"tailscale.com/logpolicy"
	"tailscale.com/logtail"
	"tailscale.com/types/logger"
	"tailscale.com/util/osshare"
	"tailscale.com/util/winutil"
)

var MM *MiraMenu

// TODO： 以下新版本模式全局变量
var logPol *logpolicy.Policy // 日志策略（后台服务logtail使用）

var args struct { // 命令行参数部分
	asServiceInstaller   bool   // 执行服务安装
	asServiceUninstaller bool   // 执行服务卸载
	asFirewallKillswitch bool   // 执行防火墙调整（被wgengine调用）
	tunGUID              string // 执行防火墙调整参数
	asServiceSubProc     bool   // 作为后台服务子进程被调用
	logid                string // 后台服务日志使用的logtail ID参数
} // 启动参数

var watcher *MiraWatcher // 通讯协程实体

func main() {

	flag.BoolVar(&args.asServiceUninstaller, "uninstall", false, "卸载后台服务")
	flag.BoolVar(&args.asServiceInstaller, "install", false, "安装后台服务")
	flag.BoolVar(&args.asFirewallKillswitch, "firewall", false, "管理防火墙")
	flag.StringVar(&args.tunGUID, "tunGUID", "", "管理防火墙使用tun的GUID值")
	flag.BoolVar(&args.asServiceSubProc, "subproc", false, "是否服务的子进程调用")
	flag.StringVar(&args.logid, "logid", "", "服务子进程使用的logtail ID值")
	flag.Parse()

	isService, err := svc.IsWindowsService()
	if args.asServiceInstaller || args.asServiceUninstaller || args.asFirewallKillswitch || args.asServiceSubProc || isService {
		envknob.PanicIfAnyEnvCheckedInInit()
		envknob.ApplyDiskConfig()
		// 开局先屏蔽TS的日志 （但后续保留日志设置，以防后续我们希望使用logtail）
		envknob.SetNoLogsNoSupport()

		pol := logpolicy.New(logtail.CollectionNode)
		pol.SetVerbosityLevel(0) // 日志级别，越往上级别越高
		logPol = pol
		defer func() {
			// Finish uploading logs after closing everything else.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			pol.Shutdown(ctx)
		}()

		// 判断是否是服务安装
		if isServiceInstaller() {
			return //结束安装
		}
		// 判断是否是服务卸载
		if isServiceUninstaller() {
			return //结束安装
		}

		// 判断是否子进程
		if beWindowsSubprocess() {
			return //结束子进程
		}

		// 判断是Win服务调用则执行服务方法，并以子进程重调此程序
		if isWindowsService() {
			log.Printf("Running service...")
			if err := runWindowsService(pol); err != nil {
				log.Printf("runservice: %v", err)
			}
			log.Printf("Stopped file sharing.")
			osshare.SetFileSharingEnabled(false, logger.Discard)
			log.Printf("Service ended.")
			return //结束服务
		}
	}

	// 客户端要保证单一进程
	_, err = winutil.CreateAppMutex("MirageWin")
	if err != nil {
		return
	}

	// 创建与后台服务的通讯员
	watcher = NewWatcher()

	MM = &MiraMenu{}
	MM.Init()

	MM.SetRx(watcher.Tx)
	MM.SetTx(watcher.Rx)
	MM.SetWatchStart(watcher.Start)

	MM.Start()
}
