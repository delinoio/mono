// SPDX-License-Identifier: Apache-2.0

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

// serviceEnv carries the agent settings of the current environment into
// the service definition.
func serviceEnv() []string {
	var env []string
	for _, name := range config.EnvNames {
		if v, ok := os.LookupEnv(name); ok && v != "" {
			env = append(env, name+"="+v)
		}
	}
	return env
}

func serviceDefinition(exe string) string {
	if runtime.GOOS == "darwin" {
		return service.LaunchdDaemon(exe, serviceEnv())
	}
	return service.SystemdUnit(exe, serviceEnv())
}

func serviceInstall(stdout, stderr io.Writer) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, "ownsand-agent:", err)
		return exitError
	}
	path, next := service.SystemdUnitPath, "systemctl daemon-reload && systemctl enable --now ownsand-agent"
	if runtime.GOOS == "darwin" {
		path, next = service.LaunchdPlist, "launchctl bootstrap system "+service.LaunchdPlist
	}
	if err := store.WriteFileAtomic(path, []byte(serviceDefinition(exe)), 0o644); err != nil {
		fmt.Fprintln(stderr, "ownsand-agent: write service definition (run as root):", err)
		return exitError
	}
	fmt.Fprintf(stdout, "Wrote %s. Start the service with:\n  %s\n", path, next)
	return exitOK
}
