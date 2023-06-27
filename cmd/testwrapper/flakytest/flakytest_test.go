// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package flakytest

import (
	"os"
	"testing"
)

func TestIssueFormat(t *testing.T) {
	testCases := []struct {
		issue string
		want  bool
	}{
		{"https://github.com/tailscale/cOrp/issues/1234", true},
		{"https://github.com/otherproject/corp/issues/1234", false},
		{"https://github.com/tailscale/corp/issues/", false},
	}
	for _, testCase := range testCases {
		if issueRegexp.MatchString(testCase.issue) != testCase.want {
			ss := ""
			if !testCase.want {
				ss = " not"
			}
			t.Errorf("expected issueRegexp to%s match %q", ss, testCase.issue)
		}
	}
}

func TestFlakeRun(t *testing.T) {
	Mark(t, "https://github.com/tailscale/tailscale/issues/0") // random issue
	e := os.Getenv(FlakeAttemptEnv)
	if e == "" {
		t.Skip("not running in testwrapper")
	}
	if e == "1" {
		t.Fatal("failing on purpose")
	}
}
