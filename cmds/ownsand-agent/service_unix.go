// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package main

import (
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/service"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
)

func runService(cfg config.Config) int {
	return runInteractive(cfg)
}

// runConfigError ends `run` when its configuration does not parse. systemd
// and launchd read the standard error of the agent, so it only prints the
// error and returns the usage exit code.
func runConfigError(cfgErr error, stderr io.Writer) int {
	fmt.Fprintln(stderr, "ownsand-agent:", cfgErr)
	return exitUsage
}

func serviceDefinition(exe, stateDir string) (string, error) {
	if runtime.GOOS == "darwin" {
		return service.LaunchdDaemon(exe, serviceEnv(stateDir))
	}
	return service.SystemdUnit(exe, serviceEnv(stateDir)), nil
}

func serviceInstall(stateDir string, stdout, stderr io.Writer) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, "ownsand-agent:", err)
		return exitError
	}
	path, next := service.SystemdUnitPath, "systemctl daemon-reload && systemctl enable --now ownsand-agent"
	if runtime.GOOS == "darwin" {
		path, next = service.LaunchdPlist, "launchctl bootstrap system "+service.LaunchdPlist
	}
	return installServiceDefinition(path, next, exe, stateDir, serviceDefinition, stdout, stderr)
}

// installServiceDefinition renders the definition and then writes it to path.
// It renders first: a definition that cannot be rendered leaves an existing
// service definition at path untouched.
func installServiceDefinition(path, next, exe, stateDir string, render func(exe, stateDir string) (string, error), stdout, stderr io.Writer) int {
	def, err := render(exe, stateDir)
	if err != nil {
		fmt.Fprintln(stderr, "ownsand-agent: render service definition:", err)
		return exitError
	}
	if err := store.WriteFileAtomic(path, []byte(def), 0o644); err != nil {
		fmt.Fprintln(stderr, "ownsand-agent: write service definition (run as root):", err)
		return exitError
	}
	fmt.Fprintf(stdout, "Wrote %s. Start the service with:\n  %s\n", path, next)
	return exitOK
}
