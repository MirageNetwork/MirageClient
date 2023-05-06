//go:build windows

package main

import (
	"context"
	"fmt"
	"log"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.zx2c4.com/wintun"
	"tailscale.com/logpolicy"
	"tailscale.com/net/dns"
	"tailscale.com/types/logger"
	"tailscale.com/util/winutil"
)

type ipnService struct {
	Policy *logpolicy.Policy
}

const (
	cmdUninstallWinTun = svc.Cmd(128 + iota)
)

var syslogf logger.Logf = logger.Discard

// 运行Windows服务（实质实现了Execute钩子给Windows Service Manager
func runWindowsService(pol *logpolicy.Policy) error {
	if winutil.GetPolicyInteger("LogSCMInteractions", 0) != 0 {
		syslog, err := eventlog.Open(serviceName)
		if err == nil {
			syslogf = func(format string, args ...any) {
				syslog.Info(0, fmt.Sprintf(format, args...))
			}
			defer syslog.Close()
		}
	}

	syslogf("Service entering svc.Run")
	defer syslogf("Service exiting svc.Run")
	return svc.Run(serviceName, &ipnService{Policy: pol})
}

// 提供给service manager的钩子函数
func (service *ipnService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	defer syslogf("SvcStopped notification imminent")
	changes <- svc.Status{State: svc.StartPending}
	syslogf("Service start pending")

	svcAccepts := svc.AcceptStop
	if winutil.GetPolicyInteger("FlushDNSOnSessionUnlock", 0) != 0 {
		svcAccepts |= svc.AcceptSessionChange
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	doneCh := make(chan struct{})
	go func() { // 实质启动daemon子进程
		defer close(doneCh)
		args := []string{"-subproc", "-logid", service.Policy.PublicID.String()} //传递子进程指示参数和logtail ID参数
		logger := log.New(log.Default().Writer(), "", 0)
		babysitProc(ctx, args, logger.Printf)
	}()

	changes <- svc.Status{State: svc.Running, Accepts: svcAccepts}
	syslogf("Service running")

	for {
		select {
		case <-doneCh:
			return false, windows.NO_ERROR
		case cmd := <-r:
			log.Printf("Got Windows Service event: %v", cmdName(cmd.Cmd))
			switch cmd.Cmd {
			case svc.Stop:
				changes <- svc.Status{State: svc.StopPending}
				syslogf("Service stop pending")
				cancel() // so BabysitProc will kill the child process
			case svc.Interrogate:
				syslogf("Service interrogation")
				changes <- cmd.CurrentStatus
			case svc.SessionChange:
				syslogf("Service session change notification")
				handleSessionChange(cmd)
				changes <- cmd.CurrentStatus
			case cmdUninstallWinTun:
				syslogf("Stopping miraged child process and uninstalling WinTun")
				// At this point, doneCh is the channel which will be closed when the
				// tailscaled subprocess exits. We save that to childDoneCh.
				childDoneCh := doneCh
				// We reset doneCh to a new channel that will keep the event loop
				// running until the uninstallation is done.
				doneCh = make(chan struct{})
				// Trigger subprocess shutdown.
				cancel()
				go func() {
					// When this goroutine completes, tell the service to break out of its
					// event loop.
					defer close(doneCh)
					// Wait for the subprocess to shutdown.
					<-childDoneCh
					// Now uninstall WinTun.
					uninstallWinTun()
				}()
				changes <- svc.Status{State: svc.StopPending}
			}
		}
	}
}

func cmdName(c svc.Cmd) string {
	switch c {
	case svc.Stop:
		return "Stop"
	case svc.Pause:
		return "Pause"
	case svc.Continue:
		return "Continue"
	case svc.Interrogate:
		return "Interrogate"
	case svc.Shutdown:
		return "Shutdown"
	case svc.ParamChange:
		return "ParamChange"
	case svc.NetBindAdd:
		return "NetBindAdd"
	case svc.NetBindRemove:
		return "NetBindRemove"
	case svc.NetBindEnable:
		return "NetBindEnable"
	case svc.NetBindDisable:
		return "NetBindDisable"
	case svc.DeviceEvent:
		return "DeviceEvent"
	case svc.HardwareProfileChange:
		return "HardwareProfileChange"
	case svc.PowerEvent:
		return "PowerEvent"
	case svc.SessionChange:
		return "SessionChange"
	case svc.PreShutdown:
		return "PreShutdown"
	case cmdUninstallWinTun:
		return "(Application Defined) Uninstall WinTun"
	}
	return fmt.Sprintf("Unknown-Service-Cmd-%d", c)
}

func uninstallWinTun() {
	dll := windows.NewLazyDLL("wintun.dll")
	if err := dll.Load(); err != nil {
		log.Printf("Cannot load wintun.dll for uninstall: %v", err)
		return
	}

	log.Printf("Removing wintun driver...")
	err := wintun.Uninstall()
	log.Printf("Uninstall: %v", err)
}

func handleSessionChange(chgRequest svc.ChangeRequest) {
	if chgRequest.Cmd != svc.SessionChange || chgRequest.EventType != windows.WTS_SESSION_UNLOCK {
		return
	}

	log.Printf("Received WTS_SESSION_UNLOCK event, initiating DNS flush.")
	go func() {
		err := dns.Flush()
		if err != nil {
			log.Printf("Error flushing DNS on session unlock: %v", err)
		}
	}()
}

func isWindowsService() bool {
	v, err := svc.IsWindowsService()
	if err != nil {
		log.Fatalf("svc.IsWindowsService failed: %v", err)
	}
	return v
}
