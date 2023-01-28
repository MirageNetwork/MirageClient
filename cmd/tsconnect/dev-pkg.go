// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"log"
)

func runDevPkg() {
	buildOptions, err := commonPkgSetup(devMode)
	if err != nil {
		log.Fatalf("Cannot setup: %v", err)
	}
	runEsbuildServe(*buildOptions)
}
