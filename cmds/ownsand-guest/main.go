// Command ownsand-guest is the OwnSand guest helper. It runs inside every
// sandbox guest, listens on the hypervisor socket and serves exec and file
// requests from the device agent. It never listens on a guest network
// interface and never reaches the control plane.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/delinoio/mono/cmds/ownsand-guest/guest"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("ownsand-guest", flag.ContinueOnError)
	port := fs.Uint("port", guest.DefaultVsockPort, "hypervisor socket port to listen on")
	stateDir := fs.String("state-dir", "", "directory for read snapshots (default: platform state directory)")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println(guest.Version)
		return 0
	}
	// The guest helper writes JSON logs to standard error, which the guest
	// init system collects.
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := listenHypervisorSocket(uint32(*port))
	if err != nil {
		log.Error("listen on hypervisor socket failed", "port", *port, "error", err)
		return 1
	}
	log.Info("guest helper listening", "port", *port, "version", guest.Version)
	srv := guest.NewServer(guest.Config{StateDir: *stateDir, Logger: log})
	if err := srv.Serve(ctx, ln); err != nil {
		log.Error("guest helper stopped", "error", err)
		return 1
	}
	return 0
}
