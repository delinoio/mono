// SPDX-License-Identifier: Apache-2.0

// Command tide is the tide command-line interface and the tide mcp MCP
// server. See docs/cmds-tide-cli-foundation.md.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/delinoio/mono/cmds/tide/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	// Restore the default interrupt action after cancellation so a later
	// interrupt can end a command blocked on credential input.
	go func() {
		<-ctx.Done()
		stop()
	}()
	code := cli.Run(ctx, cli.Env{Args: os.Args[1:]})
	stop()
	os.Exit(code)
}
