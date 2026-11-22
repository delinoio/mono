// SPDX-License-Identifier: FSL-1.1-ALv2

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
	env := serviceEnv("")
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

// An explicit --state-dir wins over a conflicting OWNSAND_AGENT_STATE_DIR in
// the unit and in the launchd daemon that `service print` and `service
// install` render.
func TestServiceDefinitionUsesTheExplicitStateDir(t *testing.T) {
	t.Setenv(config.EnvStateDir, "/var/lib/from-env")
	def := serviceDefinition("/usr/bin/ownsand-agent", "/var/lib/from-flag")
	if !strings.Contains(def, "/var/lib/from-flag") || strings.Contains(def, "/var/lib/from-env") {
		t.Fatalf("service definition does not use the explicit state directory:\n%s", def)
	}
	if def := serviceDefinition("/usr/bin/ownsand-agent", ""); !strings.Contains(def, "/var/lib/from-env") {
		t.Fatalf("service definition without the flag does not use the environment:\n%s", def)
	}
}
