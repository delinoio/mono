// SPDX-License-Identifier: Apache-2.0

// Command sandcrew-bridge is the SandCrew guest bridge. SandCrew copies it
// into the sandbox of each session and runs
//
//	sandcrew-bridge run --state-dir <workspace>/.sandcrew
//
// as one long-running dev session exec. The bridge installs and starts the
// pinned vendor agent, speaks ACP with it over stdio, reads input files from
// `<state>/inbox/` and writes normalized events to stdout and to
// `<state>/events.jsonl`. The owning contract is
// docs/cmds-sandcrew-bridge-foundation.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/bridge"
	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/logfile"
	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/pins"
	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/version"
	sandcrewv1 "github.com/delinoio/mono/protos/delino/sandcrew/v1"
)

const usage = `usage:
  sandcrew-bridge run --state-dir <dir>
  sandcrew-bridge version
`

func main() {
	os.Exit(int(run(os.Args[1:], os.Stdout, os.Stderr)))
}

func run(args []string, stdout, stderr io.Writer) bridge.ExitCode {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return bridge.ExitUsage
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "sandcrew-bridge %s (protocol %d.%d)\n", version.String(), bridge.ProtocolMajor, bridge.ProtocolMinor)
		return bridge.ExitAgentExited
	case "run":
	default:
		fmt.Fprint(stderr, usage)
		return bridge.ExitUsage
	}
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	stateDir := flags.String("state-dir", "", "the bridge state directory, <workspace>/.sandcrew")
	if err := flags.Parse(args[1:]); err != nil || *stateDir == "" || flags.NArg() != 0 {
		fmt.Fprint(stderr, usage)
		return bridge.ExitUsage
	}
	dir, err := filepath.Abs(*stateDir)
	if err != nil {
		fmt.Fprintf(stderr, "sandcrew-bridge: state directory: %v\n", err)
		return bridge.ExitUsage
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		fmt.Fprintf(stderr, "sandcrew-bridge: state directory %s does not exist\n", dir)
		return bridge.ExitUsage
	}

	// bridge.log and stderr carry the same structured JSON records without
	// ANSI color: they are machine-read.
	logWriter := stderr
	if file, err := logfile.Open(filepath.Join(dir, "bridge.log"), logfile.MaxSize); err != nil {
		fmt.Fprintf(stderr, "sandcrew-bridge: bridge.log: %v\n", err)
	} else {
		defer file.Close()
		logWriter = io.MultiWriter(stderr, file)
	}
	logger := slog.New(slog.NewJSONHandler(logWriter, &slog.HandlerOptions{Level: slog.LevelInfo})).With(
		"bridge_version", version.String(),
		"protocol_version", fmt.Sprintf("%d.%d", bridge.ProtocolMajor, bridge.ProtocolMinor),
	)

	target, ok := pins.TargetFor(runtime.GOOS, runtime.GOARCH)
	if !ok {
		logger.Error("unsupported release target", "goos", runtime.GOOS, "goarch", runtime.GOARCH)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code := bridge.Run(ctx, bridge.Config{
		StateDir:      dir,
		Stdout:        stdout,
		Logger:        logger,
		BridgeVersion: version.String(),
		GuestOS:       guestOS(runtime.GOOS),
		Target:        target,
	})
	logger.Info("bridge exiting", "exit_code", int(code))
	return code
}

func guestOS(goos string) sandcrewv1.GuestOs {
	switch goos {
	case "linux":
		return sandcrewv1.GuestOs_GUEST_OS_LINUX
	case "darwin":
		return sandcrewv1.GuestOs_GUEST_OS_MACOS
	case "windows":
		return sandcrewv1.GuestOs_GUEST_OS_WINDOWS
	default:
		return sandcrewv1.GuestOs_GUEST_OS_UNSPECIFIED
	}
}
