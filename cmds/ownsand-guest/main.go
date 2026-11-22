// SPDX-License-Identifier: FSL-1.1-ALv2

// Command ownsand-guest is the OwnSand guest helper. It runs inside every
// sandbox guest, listens on the hypervisor socket and serves the
// GuestService of the device agent. See
// docs/cmds-ownsand-agent-foundation.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
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
	envGrowDisk = "OWNSAND_GUEST_GROW_DISK"
)

func envOr(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return fallback
}

// platformMain runs the entry points of a guest OS before the plain
// command line: on Windows guests, the service that the Service Control
// Manager starts and the helper subcommands (service_windows.go). It
// returns false when the plain command line applies.
var platformMain func(args []string) (code int, handled bool)

func main() {
	if isInit() {
		os.Exit(runInit(os.Args[1:]))
	}
	if platformMain != nil {
		if code, ok := platformMain(os.Args[1:]); ok {
			os.Exit(code)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], nil)
	stop()
	os.Exit(code)
}

// run serves the GuestService until ctx ends. openLog returns the log
// output for the state directory; nil logs to standard error.
func run(ctx context.Context, args []string, openLog func(stateDir string) (io.Writer, error)) int {
	fs := flag.NewFlagSet("ownsand-guest", flag.ContinueOnError)
	listen := fs.String("listen", envOr(envListen, guestlisten.DefaultSpec), "listener spec: vsock://:<port>, hvsock://<service GUID>, unix://<path> or tcp://<host:port> (env "+envListen+")")
	stateDir := fs.String("state-dir", envOr(envStateDir, defaultStateDir()), "directory for read snapshots and write records on the guest disk (env "+envStateDir+")")
	clock := fs.String("clock", envOr(envClock, "set"), "set: SetClock sets the system clock; ignore: SetClock changes nothing (env "+envClock+")")
	logLevel := fs.String("log-level", envOr(envLogLevel, "info"), "debug, info, warn or error (env "+envLogLevel+")")
	growDisk := fs.Bool("grow-disk", envOr(envGrowDisk, "") == "1", "grow the guest's system disk to the size of its disk at start; set only by guest images, never on a host (env "+envGrowDisk+"=1)")
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
	var logOut io.Writer = os.Stderr
	if openLog != nil {
		w, err := openLog(*stateDir)
		if err != nil {
			return 1
		}
		logOut = w
	}
	logger := slog.New(slog.NewJSONHandler(logOut, &slog.HandlerOptions{Level: level})).With("component", "ownsand-guest")
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
	// Take the listener before the state directory: a helper that cannot
	// own its address must not touch the state of the one that does. The
	// state directory lock in guest.New then keeps a second helper on
	// another listener from cleaning the snapshots of a running one.
	l, err := guestlisten.Listen(*listen)
	if err != nil {
		logger.Error("listen failed", "error", err)
		return 1
	}
	srv, err := guest.New(guest.Config{StateDir: *stateDir, Version: version, Clock: mode, Logger: logger})
	if err != nil {
		_ = l.Close()
		logger.Error("start failed", "error", err)
		return 1
	}
	// Release the state directory lock when the helper ends, so that a
	// replacement in the same process (tests) can start.
	defer srv.Close()
	// The network tunnel of the Hyper-V runtime lives in the helper that
	// the init process of a Linux guest restarts, so a failed relay comes
	// back with the helper: a terminal tunnel error ends the helper with
	// a failure status (see serve). A tunnel setup failure only logs.
	var tunnelFatal <-chan error
	if isInitChild() {
		fatal, err := startTunnel(logger)
		if err != nil {
			logger.Error("network tunnel failed", "error", err)
		}
		tunnelFatal = fatal
	}
	if *growDisk {
		go growSystemDisk(logger)
	}
	return serve(ctx, logger, tunnelFatal, func(ctx context.Context) error { return srv.Run(ctx, l) })
}

// serve runs the guest service until it ends and returns the exit code of
// the helper. A terminal tunnel error (a receive from tunnelFatal; a nil
// channel never fires) makes it stop the service and return 1 at once, so
// the init process restarts the helper and the host reconnects its tunnel.
func serve(ctx context.Context, logger *slog.Logger, tunnelFatal <-chan error, run func(context.Context) error) int {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(runCtx) }()
	select {
	case err := <-done:
		if err != nil {
			logger.Error("serve failed", "error", err)
			return 1
		}
		return 0
	case err := <-tunnelFatal:
		// Do not wait for the service to drain: the helper is about to
		// be replaced, and in-flight requests end with the process.
		logger.Error("network tunnel failed; exiting so that init restarts the helper", "error", err)
		return 1
	}
}

func defaultStateDir() string {
	if os.PathSeparator == '\\' {
		return `C:\ProgramData\ownsand-guest`
	}
	return "/var/lib/ownsand-guest"
}
