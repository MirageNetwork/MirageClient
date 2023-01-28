// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package apitype contains types for the Tailscale LocalAPI and control plane API.
package apitype

import "tailscale.com/tailcfg"

// LocalAPIHost is the Host header value used by the LocalAPI.
const LocalAPIHost = "local-miraged.sock"

// WhoIsResponse is the JSON type returned by tailscaled debug server's /whois?ip=$IP handler.
type WhoIsResponse struct {
	Node        *tailcfg.Node
	UserProfile *tailcfg.UserProfile

	// Caps are extra capabilities that the remote Node has to this node.
	Caps []string `json:",omitempty"`
}

// FileTarget is a node to which files can be sent, and the PeerAPI
// URL base to do so via.
type FileTarget struct {
	Node *tailcfg.Node

	// PeerAPI is the http://ip:port URL base of the node's PeerAPI,
	// without any path (not even a single slash).
	PeerAPIURL string
}

type WaitingFile struct {
	Name string
	Size int64
}
