// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Code generated by the following command; DO NOT EDIT.
//   tailscale.com/cmd/cloner -type Persist

package persist

import (
	"tailscale.com/types/key"
	"tailscale.com/types/structs"
	"tailscale.com/types/wgkey"
)

// Clone makes a deep copy of Persist.
// The result aliases no memory with the original.
func (src *Persist) Clone() *Persist {
	if src == nil {
		return nil
	}
	dst := new(Persist)
	*dst = *src
	return dst
}

// A compilation failure here means this code must be regenerated, with the command at the top of this file.
var _PersistNeedsRegeneration = Persist(struct {
	_                               structs.Incomparable
	LegacyFrontendPrivateMachineKey key.MachinePrivate
	PrivateNodeKey                  wgkey.Private
	OldPrivateNodeKey               wgkey.Private
	Provider                        string
	LoginName                       string
}{})
