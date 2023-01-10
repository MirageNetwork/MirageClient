// Copyright (c) 2022 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux || darwin || freebsd || openbsd

package main

// Force registration of tailssh with LocalBackend.
import _ "tailscale.com/ssh/tailssh"
