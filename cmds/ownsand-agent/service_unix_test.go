// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
	clearAgentEnv(t)
	const (
		exe      = "/usr/bin/ownsand-agent"
		flagDir  = "/var/lib/from-flag"
		envDir   = "/var/lib/from-env"
		guestBin = envDir + "/ownsand-guest"
	)
	t.Setenv(config.EnvStateDir, envDir)
	t.Setenv(config.EnvGuestBinary, guestBin)
	render := func(t *testing.T, flag string) string {
		t.Helper()
		def, err := serviceDefinition(exe, flag)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkServiceEnvironment(def, config.EnvGuestBinary, guestBin); err != nil {
			t.Fatal(err)
		}
		return def
	}

	for _, tc := range []struct{ name, flag, want string }{
		{"explicit", flagDir, flagDir},
		{"fallback", "", envDir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkServiceEnvironment(render(t, tc.flag), config.EnvStateDir, tc.want); err != nil {
				t.Fatal(err)
			}
		})
	}

	for _, tc := range []struct{ name, stateDir string }{
		{"missing-state", ""},
		{"wrong-state", "/var/lib/wrong"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EnvStateDir, tc.stateDir)
			def := render(t, "")
			for _, want := range []string{flagDir, envDir} {
				if err := checkServiceEnvironment(def, config.EnvStateDir, want); err == nil {
					t.Errorf("accepted %s = %q, want rejection when expecting %q", config.EnvStateDir, tc.stateDir, want)
				}
			}
		})
	}
}

func checkServiceEnvironment(definition, name, want string) error {
	if runtime.GOOS == "darwin" {
		return checkLaunchdEnvironment(definition, name, want)
	}

	prefix := `Environment="` + name + "="
	var assignment string
	for line := range strings.SplitSeq(definition, "\n") {
		if strings.HasPrefix(line, prefix) {
			if assignment != "" {
				return fmt.Errorf("systemd unit contains multiple assignments for %s", name)
			}
			assignment = line
		}
	}
	expected := "Environment=" + strings.ReplaceAll(strconv.Quote(name+"="+want), "%", "%%")
	if assignment != expected {
		return fmt.Errorf("systemd assignment = %q, want %q", assignment, expected)
	}
	return nil
}

type servicePlistElement struct {
	XMLName  xml.Name
	Text     string                `xml:",chardata"`
	Children []servicePlistElement `xml:",any"`
}

func plistDictionaryValue(dict servicePlistElement, key string) servicePlistElement {
	if dict.XMLName.Local == "dict" {
		for i := 0; i+1 < len(dict.Children); i += 2 {
			if entry := dict.Children[i]; entry.XMLName.Local == "key" && entry.Text == key {
				return dict.Children[i+1]
			}
		}
	}
	return servicePlistElement{}
}

func checkLaunchdEnvironment(definition, name, want string) error {
	var plist struct {
		XMLName xml.Name            `xml:"plist"`
		Dict    servicePlistElement `xml:"dict"`
	}
	if err := xml.Unmarshal([]byte(definition), &plist); err != nil {
		return fmt.Errorf("decode launchd plist: %w", err)
	}
	env := plistDictionaryValue(plist.Dict, "EnvironmentVariables")
	got := plistDictionaryValue(env, name)
	if got.XMLName.Local != "string" || got.Text != want {
		return fmt.Errorf("launchd EnvironmentVariables[%s] = <%s>%q, want string %q", name, got.XMLName.Local, got.Text, want)
	}
	return nil
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
