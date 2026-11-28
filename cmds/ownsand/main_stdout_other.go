// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin

package main

import (
	"context"

	"github.com/delinoio/mono/cmds/ownsand/internal/cli"
)

func runCLI(ctx context.Context, args []string) int {
	return cli.Run(ctx, cli.Env{Args: args})
}
