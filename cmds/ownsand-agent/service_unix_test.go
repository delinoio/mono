// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"strings"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/service"
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

func TestServiceDefinitionsRenderEveryAgentVariable(t *testing.T) {
	setAllAgentEnv(t)
	env := serviceEnv()
	unit := service.SystemdUnit("/usr/bin/ownsand-agent", env)
	plist := service.LaunchdDaemon("/usr/bin/ownsand-agent", env)
	for _, name := range config.EnvNames {
		if !strings.Contains(unit, `Environment="`+name+`=v-`+name+`"`) {
			t.Errorf("systemd unit misses %s:\n%s", name, unit)
		}
		if !strings.Contains(plist, "<key>"+name+"</key>\n\t\t<string>v-"+name+"</string>") {
			t.Errorf("launchd daemon misses %s:\n%s", name, plist)
		}
	}
}
