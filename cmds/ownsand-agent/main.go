// Command ownsand-agent is the OwnSand device agent. It runs as a system
// service on each enrolled host, keeps an outbound channel to the control
// plane, runs sandboxes in VMs, enforces their network policy and TTLs, and
// updates itself.
package main

import (
	"os"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/cli"
)

// Version is set at build time with -ldflags "-X main.Version=...".
var Version = "0.0.0-dev"

func main() {
	os.Exit(cli.Main(cli.Env{
		Args:       os.Args[1:],
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		Getenv:     os.Getenv,
		Version:    Version,
		IsTerminal: stdinIsTerminal,
	}))
}

func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
