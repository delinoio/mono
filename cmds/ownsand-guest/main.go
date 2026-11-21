// SPDX-License-Identifier: Apache-2.0

// Command ownsand-guest is the OwnSand guest helper. It runs inside every
// sandbox guest, listens on the hypervisor socket and serves the
// GuestService of the device agent. See
// docs/cmds-ownsand-agent-foundation.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/delinoio/mono/cmds/internal/loglevel"
	"github.com/delinoio/mono/cmds/ownsand-guest/guest"
	"github.com/delinoio/mono/cmds/ownsand-guest/guestlisten"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// Environment variables. Each overrides the default of its flag.
const (
	envListen   = "OWNSAND_GUEST_LISTEN"
	envStateDir = "OWNSAND_GUEST_STATE_DIR"
	envClock    = "OWNSAND_GUEST_CLOCK"
	envLogLevel = "OWNSAND_GUEST_LOG_LEVEL"
)

func envOr(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return fallback
}

func main() {
	if isInit() {
		os.Exit(runInit(os.Args[1:]))
	}
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("ownsand-guest", flag.ContinueOnError)
	listen := fs.String("listen", envOr(envListen, guestlisten.DefaultSpec), "listener spec: vsock://:<port>, unix://<path> or tcp://<host:port> (env "+envListen+")")
	stateDir := fs.String("state-dir", envOr(envStateDir, defaultStateDir()), "directory for read snapshots on the guest disk (env "+envStateDir+")")
	clock := fs.String("clock", envOr(envClock, "set"), "set: SetClock sets the system clock; ignore: SetClock changes nothing (env "+envClock+")")
	logLevel := fs.String("log-level", envOr(envLogLevel, "info"), "debug, info, warn or error (env "+envLogLevel+")")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println(version)
		return 0
	}
	level, err := loglevel.Parse(*logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ownsand-guest: invalid log level %q\n", *logLevel)
		return 2
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})).With("component", "ownsand-guest")
	var mode guest.ClockMode
	switch *clock {
	case "set":
		mode = guest.ClockModeSet
	case "ignore":
		mode = guest.ClockModeIgnore
	default:
		fmt.Fprintf(os.Stderr, "ownsand-guest: invalid clock mode %q\n", *clock)
		return 2
	}
	srv, err := guest.New(guest.Config{StateDir: *stateDir, Version: version, Clock: mode, Logger: logger})
	if err != nil {
		logger.Error("start failed", "error", err)
		return 1
	}
	// The network tunnel of the Hyper-V runtime lives in the helper that
	// the init process of a Linux guest restarts, so a failed relay comes
	// back with the helper.
	if isInitChild() {
		if err := startTunnel(logger); err != nil {
			logger.Error("network tunnel failed", "error", err)
		}
	}
	l, err := guestlisten.Listen(*listen)
	if err != nil {
		logger.Error("listen failed", "error", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx, l); err != nil {
		logger.Error("serve failed", "error", err)
		return 1
	}
	return 0
}

func defaultStateDir() string {
	if os.PathSeparator == '\\' {
		return `C:\ProgramData\ownsand-guest`
	}
	return "/var/lib/ownsand-guest"
}
