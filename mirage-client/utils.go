//go:build windows

package main

import (
	"net/netip"
	"os"
	"path/filepath"
	"reflect"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/preftype"
)

var logo_png string = "./resource/Mirage_logo.png"
var app_name string = "蜃境"
var control_url string = "https://sdp.ipv4.uk"
var console_url string = "https://sdp.ipv4.uk/admin"
var socket_path string = `\\.\pipe\ProtectedPrefix\Administrators\Mirage\miraged`
var state_path string = filepath.Join(os.Getenv("ProgramData"), "Mirage", "server-state.conf")
var pref_path string = filepath.Join(os.Getenv("ProgramData"), "Mirage", "pref.conf")
var tun_name string = "Mirage"
var log_id string = "Mirage"
var engine_port uint16 = 41641

var (
	ipv4default = netip.MustParsePrefix("0.0.0.0/0")
	ipv6default = netip.MustParsePrefix("::/0")
)

func CreateDefaultPref() *ipn.Prefs {
	routes := make([]netip.Prefix, 0, 0)
	var tags []string
	prefs := ipn.NewPrefs()
	prefs.ControlURL = control_url
	prefs.WantRunning = true
	prefs.RouteAll = false
	prefs.ExitNodeAllowLANAccess = false
	prefs.CorpDNS = false
	prefs.AllowSingleHosts = true
	prefs.ShieldsUp = false
	prefs.RunSSH = false

	prefs.AdvertiseRoutes = routes
	prefs.AdvertiseTags = tags
	prefs.Hostname = ""
	prefs.ForceDaemon = true
	prefs.LoggedOut = false
	prefs.OperatorUser = ""
	prefs.NetfilterMode = preftype.NetfilterOn

	return prefs
}

func GetAllMaskedPref(ipnPref ipn.Prefs) ipn.MaskedPrefs {
	return ipn.MaskedPrefs{Prefs: ipnPref,
		ControlURLSet:             true,
		RouteAllSet:               true,
		AllowSingleHostsSet:       true,
		ExitNodeIDSet:             true,
		ExitNodeIPSet:             true,
		ExitNodeAllowLANAccessSet: true,
		CorpDNSSet:                true,
		RunSSHSet:                 true,
		WantRunningSet:            true,
		LoggedOutSet:              true,
		ShieldsUpSet:              true,
		AdvertiseTagsSet:          true,
		HostnameSet:               true,
		NotepadURLsSet:            true,
		ForceDaemonSet:            true,
		EggSet:                    true,
		AdvertiseRoutesSet:        true,
		NoSNATSet:                 true,
		NetfilterModeSet:          true,
		OperatorUserSet:           true,
	}
}

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

func kickLogin() {
	prefs := CreateDefaultPref()
	prefs.CorpDNS = gui.optDNSMenu.Checked()
	prefs.RouteAll = gui.optSubnetMenu.Checked()
	if err := LC.CheckPrefs(ctx, prefs); err != nil {
		logNotify("Pref出错", err)
	}
	if err := LC.Start(ctx, ipn.Options{
		AuthKey:     "",
		UpdatePrefs: prefs,
	}); err != nil {
		logNotify("无法开始", err)
	}
}

func refreshPrefs() {
	newPref, err := LC.GetPrefs(ctx)
	if err == nil {
		if newPref.CorpDNS {
			gui.optDNSMenu.Check()
		} else {
			gui.optDNSMenu.Uncheck()
		}
		if newPref.RouteAll {
			gui.optSubnetMenu.Check()
		} else {
			gui.optSubnetMenu.Uncheck()
		}
	}
}

func switchDNSOpt(newV bool) error {
	maskedPrefs := &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			CorpDNS: newV,
		},
		CorpDNSSet: true,
	}
	curPrefs, err := LC.GetPrefs(ctx)
	if err != nil {
		return err
	}

	checkPrefs := curPrefs.Clone()
	checkPrefs.ApplyEdits(maskedPrefs)
	if err := LC.CheckPrefs(ctx, checkPrefs); err != nil {
		return err
	}

	_, err = LC.EditPrefs(ctx, maskedPrefs)
	return err
}

func switchSubnetOpt(newV bool) error {
	maskedPrefs := &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			RouteAll: newV,
		},
		RouteAllSet: true,
	}
	curPrefs, err := LC.GetPrefs(ctx)
	if err != nil {
		return err
	}

	checkPrefs := curPrefs.Clone()
	checkPrefs.ApplyEdits(maskedPrefs)
	if err := LC.CheckPrefs(ctx, checkPrefs); err != nil {
		return err
	}

	_, err = LC.EditPrefs(ctx, maskedPrefs)
	return err
}
