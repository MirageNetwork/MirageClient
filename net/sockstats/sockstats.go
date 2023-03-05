// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package sockstats collects statistics about network sockets used by
// the Tailscale client. The context where sockets are used must be
// instrumented with the WithSockStats() function.
//
// Only available on POSIX platforms when built with Tailscale's fork of Go.
package sockstats

import (
	"context"

	"tailscale.com/net/interfaces"
)

type SockStats struct {
	Stats      map[string]SockStat
	Interfaces []string
}

type SockStat struct {
	TxBytes            int64
	RxBytes            int64
	TxBytesByInterface map[string]int64
	RxBytesByInterface map[string]int64
}

func WithSockStats(ctx context.Context, label string) context.Context {
	return withSockStats(ctx, label)
}

func Get() *SockStats {
	return get()
}

// LinkMonitor is the interface for the parts of wgengine/mointor's Mon that we
// need, to avoid the dependency.
type LinkMonitor interface {
	InterfaceState() *interfaces.State
	RegisterChangeCallback(interfaces.ChangeFunc) (unregister func())
}

func SetLinkMonitor(lm LinkMonitor) {
	setLinkMonitor(lm)
}
