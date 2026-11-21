// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"os"
	"strings"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
)

// serviceEnv carries the agent settings of the current environment into
// the service definition on every host OS: unset and empty variables are
// skipped.
func serviceEnv() []string {
	var env []string
	for _, name := range config.EnvNames {
		if v, ok := os.LookupEnv(name); ok && v != "" {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// environmentNames returns the variable names of NAME=value entries, for
// logs that must not carry values.
func environmentNames(env []string) []string {
	names := make([]string, 0, len(env))
	for _, e := range env {
		name, _, _ := strings.Cut(e, "=")
		names = append(names, name)
	}
	return names
}
