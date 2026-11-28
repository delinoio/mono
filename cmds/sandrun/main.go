// SPDX-License-Identifier: Apache-2.0

// Command SandRun is the SandRun command-line interface and the sandrun mcp MCP
// server. See docs/cmds-sandrun-cli-foundation.md.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/delinoio/mono/cmds/sandrun/internal/cli"
)

func main() {
	os.Exit(runSandRun(nil))
}

func runSandRun(interruptRestored func()) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	// Restore the default interrupt action after cancellation so a later
	// interrupt can end a command blocked on credential input.
	go func() {
		<-ctx.Done()
		stop()
		if interruptRestored != nil {
			interruptRestored()
		}
	}()
	code := runCLI(ctx, cli.Env{Args: os.Args[1:]})
	stop()
	return code
}
