// SPDX-License-Identifier: FSL-1.1-ALv2

package service

import (
	"encoding/xml"
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
