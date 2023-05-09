//go:build windows

package localapi

import (
	"io"
	"log"
	"net/http"
	"os"

	"golang.org/x/sys/windows/registry"
)

const (
	keyName = `SOFTWARE\Microsoft\Windows\CurrentVersion\Run`
	appName = "Mirage"
)

// 检查是否已启用开机自启动
func isStartupEnabled() bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, keyName, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(appName)
	if err != nil {
		return false
	}
	return true
}

// 启用开机自启动
func enableStartup() {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, keyName, registry.SET_VALUE)
	if err != nil {
		log.Printf("WIN1: %v, %s", err, keyName)
		return
	}
	defer k.Close()

	exePath, err := os.Executable()
	if err != nil {
		log.Printf("WIN2: %v", err)
		return
	}

	if err = k.SetStringValue(appName, exePath); err != nil {
		log.Printf("WIN3: %v", err)
		return
	}

	log.Printf("Startup enabled.")
}

// 禁用开机自启动
func disableStartup() {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, keyName, registry.SET_VALUE)
	if err != nil {
		log.Printf("WINA: %v", err)
		return
	}
	defer k.Close()

	if err = k.DeleteValue(appName); err != nil {
		log.Printf("WINB: %v", err)
		return
	}

	log.Printf("Startup disabled.")
}

func (h *Handler) serveGetAutoStart(w http.ResponseWriter, r *http.Request) {
	var value []byte
	if isStartupEnabled() {
		value = []byte("true")
	} else {
		value = []byte("false")
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write(value)
}
func (h *Handler) serveSwitchAutoStart(w http.ResponseWriter, r *http.Request) {
	if isStartupEnabled() {
		disableStartup()
	} else {
		enableStartup()
	}
	w.Header().Set("Content-Type", "text/plain")
	io.WriteString(w, "done\n")
}

func init() {
	winHndGetAutoStart = (*Handler).serveGetAutoStart
	winHndSwitchAutoStart = (*Handler).serveSwitchAutoStart
}
