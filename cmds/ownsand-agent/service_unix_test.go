// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"strings"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/service"
)

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
