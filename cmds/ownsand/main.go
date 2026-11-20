// SPDX-License-Identifier: Apache-2.0

// Command ownsand is the OwnSand command-line interface. See
// docs/cmds-ownsand-cli-foundation.md.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/delinoio/mono/cmds/ownsand/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := cli.Run(ctx, cli.Env{Args: os.Args[1:]})
	stop()
	os.Exit(code)
}
