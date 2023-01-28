// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package netns

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// verifies tailscaleBypassMark is in sync with wgengine.
func TestBypassMarkInSync(t *testing.T) {
	want := fmt.Sprintf("%q", fmt.Sprintf("0x%x", tailscaleBypassMark))
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "../../wgengine/router/router_linux.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, ident := range vs.Names {
				if ident.Name != "tailscaleBypassMark" {
					continue
				}
				valExpr := vs.Values[i]
				lit, ok := valExpr.(*ast.BasicLit)
				if !ok {
					t.Errorf("tailscaleBypassMark = %T, expected *ast.BasicLit", valExpr)
				}
				if lit.Value == want {
					// Pass.
					return
				}
				t.Fatalf("router_linux.go's tailscaleBypassMark = %s; not in sync with netns's %s", lit.Value, want)
			}
		}
	}
	t.Errorf("tailscaleBypassMark not found in router_linux.go")
}

func TestSocketMarkWorks(t *testing.T) {
	_ = socketMarkWorks()
	// we cannot actually assert whether the test runner has SO_MARK available
	// or not, as we don't know. We're just checking that it doesn't panic.
}
