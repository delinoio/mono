// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/service"
)

func TestServiceDefinitionsRenderEveryAgentVariable(t *testing.T) {
	setAllAgentEnv(t)
	env := serviceEnv("")
	unit := service.SystemdUnit("/usr/bin/ownsand-agent", env)
	plist, err := service.LaunchdDaemon("/usr/bin/ownsand-agent", env)
	if err != nil {
		t.Fatal(err)
	}
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
	def, err := serviceDefinition("/usr/bin/ownsand-agent", "/var/lib/from-flag")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def, "/var/lib/from-flag") || strings.Contains(def, "/var/lib/from-env") {
		t.Fatalf("service definition does not use the explicit state directory:\n%s", def)
	}
	if def, err := serviceDefinition("/usr/bin/ownsand-agent", ""); err != nil || !strings.Contains(def, "/var/lib/from-env") {
		t.Fatalf("service definition without the flag does not use the environment:\n%s", def)
	}
}

// A definition that cannot be rendered ends `service install` before any
// write: an existing definition keeps its content.
func TestInstallServiceDefinitionKeepsTheExistingFileWhenRenderFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "definition")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	render := func(exe, stateDir string) (string, error) { return "", errors.New("cannot render") }
	if code := installServiceDefinition(path, "next", "exe", "", render, &stdout, &stderr); code != exitError {
		t.Fatalf("exit code = %d, want %d", code, exitError)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "old" {
		t.Fatalf("existing definition = %q, %v; want it untouched", got, err)
	}
	if !strings.Contains(stderr.String(), "cannot render") || stdout.Len() != 0 {
		t.Fatalf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}

	render = func(exe, stateDir string) (string, error) { return "new", nil }
	if code := installServiceDefinition(path, "next", "exe", "", render, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code = %d, want %d", code, exitOK)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "new" {
		t.Fatalf("definition = %q, %v; want the new content", got, err)
	}
}
