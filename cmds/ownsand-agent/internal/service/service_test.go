// SPDX-License-Identifier: Apache-2.0

package service

import (
	"encoding/xml"
	"errors"
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

// plistValue is a decoded plist value: a string, a bool, an array or a dict.
type plistValue struct {
	text  string
	flag  bool
	array []plistValue
	dict  map[string]plistValue
}

func decodePlistValue(t *testing.T, d *xml.Decoder, start xml.StartElement) plistValue {
	t.Helper()
	switch start.Name.Local {
	case "true":
		d.Skip()
		return plistValue{flag: true}
	case "false":
		d.Skip()
		return plistValue{}
	case "string":
		var s string
		if err := d.DecodeElement(&s, &start); err != nil {
			t.Fatal(err)
		}
		return plistValue{text: s}
	case "array":
		var v plistValue
		for {
			tok, err := d.Token()
			if err != nil {
				t.Fatal(err)
			}
			switch e := tok.(type) {
			case xml.StartElement:
				v.array = append(v.array, decodePlistValue(t, d, e))
			case xml.EndElement:
				return v
			}
		}
	case "dict":
		v := plistValue{dict: map[string]plistValue{}}
		key := ""
		for {
			tok, err := d.Token()
			if err != nil {
				t.Fatal(err)
			}
			switch e := tok.(type) {
			case xml.StartElement:
				if e.Name.Local == "key" {
					if err := d.DecodeElement(&key, &e); err != nil {
						t.Fatal(err)
					}
					continue
				}
				v.dict[key] = decodePlistValue(t, d, e)
			case xml.EndElement:
				return v
			}
		}
	}
	t.Fatalf("unsupported plist element %q", start.Name.Local)
	return plistValue{}
}

func decodePlist(t *testing.T, doc string) map[string]plistValue {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(doc))
	for {
		tok, err := d.Token()
		if err != nil {
			t.Fatalf("no plist dict: %v", err)
		}
		if e, ok := tok.(xml.StartElement); ok && e.Name.Local == "dict" {
			return decodePlistValue(t, d, e).dict
		}
	}
}

func TestLaunchdDaemonRunsWithLaunchdMarker(t *testing.T) {
	plist := decodePlist(t, LaunchdDaemon("/usr/local/bin/ownsand-agent", []string{"A=<b>"}))
	var args []string
	for _, a := range plist["ProgramArguments"].array {
		args = append(args, a.text)
	}
	want := []string{"/usr/local/bin/ownsand-agent", "run", "--service-manager", "launchd"}
	if strings.Join(args, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ProgramArguments = %v, want %v", args, want)
	}
	keepAlive := plist["KeepAlive"].dict
	if len(keepAlive) != 1 {
		t.Fatalf("KeepAlive = %v, want only SuccessfulExit", keepAlive)
	}
	if v, ok := keepAlive["SuccessfulExit"]; !ok || v.flag {
		t.Fatalf("KeepAlive SuccessfulExit = %v (present %v), want false", v.flag, ok)
	}
	if got := plist["EnvironmentVariables"].dict["A"].text; got != "<b>" {
		t.Fatalf("environment A = %q", got)
	}
	if !plist["RunAtLoad"].flag {
		t.Error("RunAtLoad is not true")
	}
}

func TestLaunchdExitCode(t *testing.T) {
	// With KeepAlive SuccessfulExit=false, launchd restarts the job after a
	// nonzero exit only. restarts is that predicate.
	for _, tc := range []struct {
		name     string
		code     int
		updated  bool
		want     int
		restarts bool
	}{
		{"installed update", 0, true, ExitLaunchdRestart, true},
		{"clean stop", 0, false, 0, false},
		{"not enrolled or removed", ExitNotEnrolled, false, 0, false},
		{"error", 1, false, 1, true},
		{"usage error", 2, false, 2, true},
	} {
		got := LaunchdExitCode(tc.code, tc.updated)
		if got != tc.want || (got != 0) != tc.restarts {
			t.Errorf("%s: LaunchdExitCode(%d, %v) = %d, want %d (restart %v)", tc.name, tc.code, tc.updated, got, tc.want, tc.restarts)
		}
	}
	if ExitLaunchdRestart == 0 || ExitLaunchdRestart == ExitNotEnrolled {
		t.Errorf("ExitLaunchdRestart = %d must be a nonzero code that differs from ExitNotEnrolled", ExitLaunchdRestart)
	}
}

func TestWindowsDefinitionWithoutEnvironmentEqualsTheScCommand(t *testing.T) {
	exe := `C:\Program Files\ownsand-agent.exe`
	if got, want := WindowsDefinition(exe, nil), WindowsCommand(exe)+"\n"; got != want {
		t.Fatalf("WindowsDefinition() = %q, want %q", got, want)
	}
	if strings.Contains(WindowsDefinition(exe, []string{}), "reg.exe") {
		t.Fatal("an empty environment must not print a reg.exe command")
	}
}

func TestWindowsDefinitionPrintsTheEnvironmentCommand(t *testing.T) {
	env := []string{"OWNSAND_AGENT_LOG_LEVEL=debug", `OWNSAND_AGENT_STATE_DIR=D:\ownsand test\`}
	lines := strings.Split(strings.TrimSuffix(WindowsDefinition(`C:\a.exe`, env), "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "sc.exe create ownsand-agent") {
		t.Fatalf("definition lines = %q", lines)
	}
	// The trailing backslash in front of the closing quote is doubled, and
	// `#` is the first separator that no entry contains.
	want := `reg.exe add "HKLM\SYSTEM\CurrentControlSet\Services\ownsand-agent" /v Environment /t REG_MULTI_SZ /s "#" /d "OWNSAND_AGENT_LOG_LEVEL=debug#OWNSAND_AGENT_STATE_DIR=D:\ownsand test\\" /f`
	if lines[1] != want {
		t.Fatalf("reg command:\n got %s\nwant %s", lines[1], want)
	}
}

func TestWindowsEnvironmentCommandAvoidsSeparatorsInValues(t *testing.T) {
	// `\0` in a value must not be read as the default separator, and a
	// separator character inside a value must not be chosen.
	cmd, err := WindowsEnvironmentCommand([]string{`A=D:\0data#x`, `B="q"`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, `/s "~"`) {
		t.Errorf("separator must skip # (in a value): %s", cmd)
	}
	if !strings.Contains(cmd, `/d "A=D:\0data#x~B=\"q\""`) {
		t.Errorf("data string quoting: %s", cmd)
	}
	if got, err := WindowsEnvironmentCommand(nil); got != "" || err != nil {
		t.Errorf("empty environment: %q, %v", got, err)
	}
}

func TestWindowsEnvironmentCommandErrorsWithoutAFreeSeparator(t *testing.T) {
	all := "A=" + windowsSeparators
	if _, err := WindowsEnvironmentCommand([]string{all}); err == nil {
		t.Fatal("expected an error when every separator occurs in a value")
	}
	if out := WindowsDefinition(`C:\a.exe`, []string{all}); !strings.Contains(out, "REM no reg.exe separator") || strings.Contains(out, "reg.exe add") {
		t.Fatalf("definition must fall back to a REM line: %q", out)
	}
}

func TestInstallWindowsEnvironment(t *testing.T) {
	env := []string{"OWNSAND_AGENT_STATE_DIR=D:\\secret-value"}
	t.Run("no entries touch nothing", func(t *testing.T) {
		err := InstallWindowsEnvironment(nil,
			func([]string) error { t.Fatal("setEnv called"); return nil },
			func() error { t.Fatal("remove called"); return nil })
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("writes the entries and keeps the service", func(t *testing.T) {
		var got []string
		err := InstallWindowsEnvironment(env,
			func(e []string) error { got = e; return nil },
			func() error { t.Fatal("remove called"); return nil })
		if err != nil || strings.Join(got, "\n") != strings.Join(env, "\n") {
			t.Fatalf("err=%v got=%v", err, got)
		}
	})
	t.Run("a write failure removes the service and hides the values", func(t *testing.T) {
		removed := false
		err := InstallWindowsEnvironment(env,
			func([]string) error { return errors.New("access denied") },
			func() error { removed = true; return nil })
		if err == nil || !removed {
			t.Fatalf("err=%v removed=%v", err, removed)
		}
		if !strings.Contains(err.Error(), "access denied") || strings.Contains(err.Error(), "secret-value") {
			t.Fatalf("error text: %v", err)
		}
	})
	t.Run("a failed removal is reported too", func(t *testing.T) {
		err := InstallWindowsEnvironment(env,
			func([]string) error { return errors.New("access denied") },
			func() error { return errors.New("delete denied") })
		if err == nil || !strings.Contains(err.Error(), "access denied") || !strings.Contains(err.Error(), "delete denied") {
			t.Fatalf("error text: %v", err)
		}
	})
}
