// SPDX-License-Identifier: FSL-1.1-ALv2

package service

import (
	"strings"
	"testing"
)

func TestDefinitions(t *testing.T) {
	unit := SystemdUnit("/usr/local/bin/ownsand-agent", []string{"OWNSAND_AGENT_RUNTIME=firecracker"})
	for _, want := range []string{"ExecStart=/usr/local/bin/ownsand-agent run", "KillMode=control-group", "RestartPreventExitStatus=3", `Environment="OWNSAND_AGENT_RUNTIME=firecracker"`} {
		if !strings.Contains(unit, want) {
			t.Errorf("systemd unit lacks %q", want)
		}
	}
	plist := LaunchdDaemon("/usr/local/bin/ownsand-agent", []string{"A=<b>"})
	if !strings.Contains(plist, "io.delino.ownsand-agent") || !strings.Contains(plist, "&lt;b&gt;") {
		t.Error("launchd plist")
	}
	if !strings.Contains(WindowsCommand(`C:\ownsand-agent.exe`), "sc.exe create ownsand-agent") {
		t.Error("windows command")
	}
}
