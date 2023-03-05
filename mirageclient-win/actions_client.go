package main

import (
	"net/netip"

	"tailscale.com/ipn"
	"tailscale.com/types/preftype"
)

func (m *MiraMenu) kickLogin() {
	prefs := m.createPref()
	if err := m.lc.CheckPrefs(m.ctx, prefs); err != nil {
		go m.SendNotify("Pref出错", err.Error(), NL_Error)
	}
	if err := m.lc.Start(m.ctx, ipn.Options{
		AuthKey:     m.data.AuthKey,
		UpdatePrefs: prefs,
	}); err != nil {
		go m.SendNotify("无法开始", err.Error(), NL_Error)
	}
}

func (m *MiraMenu) createPref() *ipn.Prefs {
	routes := make([]netip.Prefix, 0, 0)
	var tags []string
	prefs := ipn.NewPrefs()
	prefs.ControlURL = m.control_url
	prefs.WantRunning = true
	prefs.RouteAll = m.prefField.prefUsingSubnetAction.Checked()
	prefs.ExitNodeAllowLANAccess = m.exitField.exitAllowLocalAction.Checked()
	prefs.CorpDNS = m.prefField.prefUsingDNSAction.Checked()
	prefs.AllowSingleHosts = true
	prefs.ShieldsUp = m.prefField.prefAllowIncomeAction.Checked()
	prefs.RunSSH = false

	prefs.AdvertiseRoutes = routes
	prefs.AdvertiseTags = tags
	prefs.Hostname = ""
	prefs.ForceDaemon = m.prefField.prefUnattendAction.Checked()
	prefs.LoggedOut = false
	prefs.OperatorUser = ""
	prefs.NetfilterMode = preftype.NetfilterOn

	return prefs
}

func (m *MiraMenu) updatePref(desc string, newPrefs *ipn.MaskedPrefs) {
	curPrefs, err := m.lc.GetPrefs(m.ctx)
	if err != nil {
		go m.SendNotify(desc, "更新Pref出错:"+err.Error(), NL_Error)
		return
	}

	checkPrefs := curPrefs.Clone()
	checkPrefs.ApplyEdits(newPrefs)
	if err := m.lc.CheckPrefs(m.ctx, checkPrefs); err != nil {
		go m.SendNotify(desc, "Pref检查出错:"+err.Error(), NL_Error)
		return
	}

	if _, err := m.lc.EditPrefs(m.ctx, newPrefs); err != nil {
		go m.SendNotify(desc, "设置Pref出错:"+err.Error(), NL_Error)
		return
	}
}
