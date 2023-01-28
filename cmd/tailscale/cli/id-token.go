// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"context"
	"errors"

	"github.com/peterbourgon/ff/v3/ffcli"
)

var idTokenCmd = &ffcli.Command{
	Name:       "id-token",
	ShortUsage: "id-token <aud>",
	ShortHelp:  "fetch an OIDC id-token for the Tailscale machine",
	Exec:       runIDToken,
}

func runIDToken(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: id-token <aud>")
	}

	tr, err := localClient.IDToken(ctx, args[0])
	if err != nil {
		return err
	}

	outln(tr.IDToken)
	return nil
}
