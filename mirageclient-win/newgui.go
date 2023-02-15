package main

import "github.com/lxn/walk"

func initGUI() {
	mainWin, err := walk.NewMainWindow()
	if err != nil {
		return
	}
	logoIcon, err := walk.Resources.Icon("./icons/logo.ico")
	if err != nil {
		return
	}
	notifyIcon, err := walk.NewNotifyIcon(mainWin)
	if err != nil {
		return
	}
	defer notifyIcon.Dispose()
	notifyIcon.SetIcon(logoIcon)
	notifyIcon.SetVisible(true)
	mainWin.Run()

}
