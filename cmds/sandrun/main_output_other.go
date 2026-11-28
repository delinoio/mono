// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin

package main

import (
	"context"

	"github.com/delinoio/mono/cmds/sandrun/internal/cli"
)

func runCLI(ctx context.Context, env cli.Env) int {
	return cli.Run(ctx, env)
}
