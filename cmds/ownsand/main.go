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
	// The first interrupt cancels ctx. Stop catching the signal at that point
	// so that a second interrupt takes the default action and ends a command
	// that does not return after the cancellation, for example one that is
	// blocked in a call that cannot observe ctx.
	go func() {
		<-ctx.Done()
		stop()
	}()
	code := cli.Run(ctx, cli.Env{Args: os.Args[1:]})
	stop()
	os.Exit(code)
}
