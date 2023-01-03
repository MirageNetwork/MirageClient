// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Code generated by tailscale.com/cmd/cloner; DO NOT EDIT.

package wgcfg

import (
	"net/netip"

	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logid"
)

// Clone makes a deep copy of Config.
// The result aliases no memory with the original.
func (src *Config) Clone() *Config {
	if src == nil {
		return nil
	}
	dst := new(Config)
	*dst = *src
	dst.Addresses = append(src.Addresses[:0:0], src.Addresses...)
	dst.DNS = append(src.DNS[:0:0], src.DNS...)
	dst.Peers = make([]Peer, len(src.Peers))
	for i := range dst.Peers {
		dst.Peers[i] = *src.Peers[i].Clone()
	}
	return dst
}

// A compilation failure here means this code must be regenerated, with the command at the top of this file.
var _ConfigCloneNeedsRegeneration = Config(struct {
	Name           string
	NodeID         tailcfg.StableNodeID
	PrivateKey     key.NodePrivate
	Addresses      []netip.Prefix
	MTU            uint16
	DNS            []netip.Addr
	Peers          []Peer
	NetworkLogging struct {
		NodeID   logid.PrivateID
		DomainID logid.PrivateID
	}
}{})

// Clone makes a deep copy of Peer.
// The result aliases no memory with the original.
func (src *Peer) Clone() *Peer {
	if src == nil {
		return nil
	}
	dst := new(Peer)
	*dst = *src
	dst.AllowedIPs = append(src.AllowedIPs[:0:0], src.AllowedIPs...)
	return dst
}

// A compilation failure here means this code must be regenerated, with the command at the top of this file.
var _PeerCloneNeedsRegeneration = Peer(struct {
	PublicKey           key.NodePublic
	DiscoKey            key.DiscoPublic
	AllowedIPs          []netip.Prefix
	PersistentKeepalive uint16
	WGEndpoint          key.NodePublic
}{})
