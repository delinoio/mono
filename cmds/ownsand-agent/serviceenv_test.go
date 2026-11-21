// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
)

func setAllAgentEnv(t *testing.T) []string {
	t.Helper()
	var want []string
	for _, name := range config.EnvNames {
		t.Setenv(name, "v-"+name)
		want = append(want, name+"=v-"+name)
	}
	return want
}

func TestServiceEnvCarriesEveryAgentVariable(t *testing.T) {
	want := setAllAgentEnv(t)
	got := serviceEnv()
	if len(got) != 10 || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("serviceEnv() = %v, want %v", got, want)
	}
}

func TestServiceEnvSkipsUnsetAndEmptyVariables(t *testing.T) {
	for _, name := range config.EnvNames {
		t.Setenv(name, "")
	}
	t.Setenv(config.EnvStateDir, "/var/lib/x")
	t.Setenv(config.EnvLinuxKernel, "")
	t.Setenv(config.EnvLinuxRootfs, "/rootfs")
	got := serviceEnv()
	want := []string{config.EnvStateDir + "=/var/lib/x", config.EnvLinuxRootfs + "=/rootfs"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("serviceEnv() = %v, want %v", got, want)
	}
}

func TestEnvironmentNamesDropTheValues(t *testing.T) {
	got := environmentNames([]string{"A=1", "B=x=y", "C="})
	if strings.Join(got, ",") != "A,B,C" {
		t.Fatalf("environmentNames() = %v", got)
	}
}
