// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package flakytest contains test helpers for marking a test as flaky. For
// tests run using cmd/testwrapper, a failed flaky test will cause tests to be
// re-run a few time until they succeed or exceed our iteration limit.
package flakytest

import (
	"fmt"
	"os"
	"regexp"
	"testing"
)

// FlakyTestLogMessage is a sentinel value that is printed to stderr when a
// flaky test is marked. This is used by cmd/testwrapper to detect flaky tests
// and retry them.
const FlakyTestLogMessage = "flakytest: this is a known flaky test"

// FlakeAttemptEnv is an environment variable that is set by cmd/testwrapper
// when a flaky test is retried. It contains the attempt number, starting at 1.
const FlakeAttemptEnv = "TS_TESTWRAPPER_ATTEMPT"

var issueRegexp = regexp.MustCompile(`\Ahttps://github\.com/tailscale/[a-zA-Z0-9_.-]+/issues/\d+\z`)

// Mark sets the current test as a flaky test, such that if it fails, it will
// be retried a few times on failure. issue must be a GitHub issue that tracks
// the status of the flaky test being marked, of the format:
//
//	https://github.com/tailscale/myRepo-H3re/issues/12345
func Mark(t testing.TB, issue string) {
	if !issueRegexp.MatchString(issue) {
		t.Fatalf("bad issue format: %q", issue)
	}

	fmt.Fprintln(os.Stderr, FlakyTestLogMessage) // sentinel value for testwrapper
	t.Logf("flakytest: issue tracking this flaky test: %s", issue)
}
