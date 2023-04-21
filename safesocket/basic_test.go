// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package safesocket

import (
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
)

// downgradeSDDL is a no-op test helper on non-Windows systems.
var downgradeSDDL = func() func() { return func() {} }

func TestBasics(t *testing.T) {
	// Make the socket in a temp dir rather than the cwd
	// so that the test can be run from a mounted filesystem (#2367).
	dir := t.TempDir()
	var sock string
	if runtime.GOOS != "windows" {
		sock = filepath.Join(dir, "test")
	} else {
		sock = fmt.Sprintf(`\\.\pipe\tailscale-test`)
		t.Cleanup(downgradeSDDL())
	}

	l, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}

	errs := make(chan error, 2)

	go func() {
		s, err := l.Accept()
		if err != nil {
			errs <- err
			return
		}
		l.Close()
		s.Write([]byte("hello"))

		b := make([]byte, 1024)
		n, err := s.Read(b)
		if err != nil {
			errs <- err
			return
		}
		t.Logf("server read %d bytes.", n)
		if string(b[:n]) != "world" {
			errs <- fmt.Errorf("got %#v, expected %#v\n", string(b[:n]), "world")
			return
		}
		s.Close()
		errs <- nil
	}()

	go func() {
		s := DefaultConnectionStrategy(sock)
		c, err := Connect(s)
		if err != nil {
			errs <- err
			return
		}
		c.Write([]byte("world"))
		b := make([]byte, 1024)
		n, err := c.Read(b)
		if err != nil {
			errs <- err
			return
		}
		if string(b[:n]) != "hello" {
			errs <- fmt.Errorf("got %#v, expected %#v\n", string(b[:n]), "hello")
		}
		c.Close()
		errs <- nil
	}()

	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
