// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package tailscaleroot

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestDockerfileVersion(t *testing.T) {
	goMod, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^go (\d\.\d+)\r?$`).FindStringSubmatch(string(goMod))
	if m == nil {
		t.Fatalf("didn't find go version in go.mod")
	}
	goVersion := m[1]

	dockerFile, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	wantSub := fmt.Sprintf("FROM golang:%s-alpine AS build-env", goVersion)
	if !strings.Contains(string(dockerFile), wantSub) {
		t.Errorf("didn't find %q in Dockerfile", wantSub)
	}
}
