// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build for_go_mod_tidy_only

package tooldeps

import (
	_ "github.com/golangci/golangci-lint/cmd/golangci-lint"
	_ "github.com/tailscale/depaware/depaware"
	_ "golang.org/x/tools/cmd/goimports"
)
