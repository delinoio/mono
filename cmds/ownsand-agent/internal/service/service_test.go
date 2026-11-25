// SPDX-License-Identifier: FSL-1.1-ALv2

package service

import (
	"encoding/xml"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDefinitions(t *testing.T) {
	unit := SystemdUnit("/usr/local/bin/ownsand-agent", []string{"OWNSAND_AGENT_RUNTIME=firecracker"})
	for _, want := range []string{`ExecStart="/usr/local/bin/ownsand-agent" run`, "KillMode=control-group", "RestartPreventExitStatus=3", `Environment="OWNSAND_AGENT_RUNTIME=firecracker"`} {
		if !strings.Contains(unit, want) {
			t.Errorf("systemd unit lacks %q", want)
		}
	}
	plist, err := LaunchdDaemon("/usr/local/bin/ownsand-agent", []string{"A=<b>"})
	if err != nil {
		t.Fatal(err)
	}
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
	doc, err := LaunchdDaemon("/usr/local/bin/ownsand-agent", []string{"A=<b>"})
	if err != nil {
		t.Fatal(err)
	}
	plist := decodePlist(t, doc)
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

// A carriage return must reach launchd as written: a literal CR or CRLF in
// the XML text is normalized to LF by a parser, so the plist writes &#13;.
func TestLaunchdDaemonRoundTripsLineBreaksAndSpecials(t *testing.T) {
	values := []string{
		"plain",
		"cr\rlf",
		"crlf\r\nend",
		"lf\nend",
		"tab\tend",
		"\r",
		"\r\n\r\n",
		`a&b<c>d"e'f&amp;g`,
		"unicode \u00e9\u4e16\U0001F600 \uFFFD \uD7FF\uE000",
	}
	for _, v := range values {
		exe := "/opt/dir " + v + "/ownsand-agent"
		doc, err := LaunchdDaemon(exe, []string{"K" + v + "=" + v})
		if err != nil {
			t.Fatalf("LaunchdDaemon(%q): %v", v, err)
		}
		if strings.Contains(doc, "\r") {
			t.Errorf("plist contains a literal CR for %q:\n%q", v, doc)
		}
		plist := decodePlist(t, doc)
		if got := plist["ProgramArguments"].array[0].text; got != exe {
			t.Errorf("executable round trip = %q, want %q", got, exe)
		}
		if got, ok := plist["EnvironmentVariables"].dict["K"+v]; !ok || got.text != v {
			t.Errorf("environment round trip = %q (present %v), want %q", got.text, ok, v)
		}
	}
}

func TestLaunchdDaemonRejectsTextXMLCannotRepresent(t *testing.T) {
	bad := map[string]string{
		"nul":             "a\x00b",
		"bell":            "a\x07b",
		"escape":          "a\x1bb",
		"unit separator":  "a\x1fb",
		"U+FFFE":          "a\ufffeb",
		"U+FFFF":          "a\uffffb",
		"invalid UTF-8":   "a\xffb",
		"truncated UTF-8": "a\xe4\xb8",
		"surrogate half":  "a\xed\xa0\x80b",
		"overlong NUL":    "a\xc0\x80b",
	}
	for name, v := range bad {
		if doc, err := LaunchdDaemon("/usr/bin/ownsand-agent"+v, nil); err == nil || doc != "" {
			t.Errorf("%s in the executable: doc %q, err %v, want an error", name, doc, err)
		}
		if doc, err := LaunchdDaemon("/usr/bin/ownsand-agent", []string{"K=" + v}); err == nil || doc != "" {
			t.Errorf("%s in an environment value: doc %q, err %v, want an error", name, doc, err)
		}
		if doc, err := LaunchdDaemon("/usr/bin/ownsand-agent", []string{"K" + v + "=x"}); err == nil || doc != "" {
			t.Errorf("%s in an environment name: doc %q, err %v, want an error", name, doc, err)
		}
	}
	_, err := LaunchdDaemon("/usr/bin/ownsand-agent", []string{"NAME=a\x01b"})
	if err == nil || !strings.Contains(err.Error(), "NAME") || !strings.Contains(err.Error(), "U+0001") {
		t.Errorf("error %v does not name the variable and the character", err)
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

func TestWindowsExitStatus(t *testing.T) {
	// With failure actions and the non-crash failure flag, the service
	// control manager restarts the service after a nonzero exit status only.
	for _, tc := range []struct {
		name          string
		code          int
		updated, stop bool
		specific      bool
		exit          uint32
	}{
		{"installed update", 0, true, false, true, ExitWindowsRestart},
		{"error", 1, false, false, true, 1},
		{"usage error", 2, false, false, true, 2},
		{"not enrolled or removed", ExitNotEnrolled, false, false, false, 0},
		{"clean stop", 0, false, false, false, 0},
		{"stop request after an error", 1, false, true, false, 0},
		{"stop request after an update", 0, true, true, false, 0},
	} {
		specific, exit := WindowsExitStatus(tc.code, tc.updated, tc.stop)
		if specific != tc.specific || exit != tc.exit {
			t.Errorf("%s: WindowsExitStatus(%d, %v, %v) = (%v, %d), want (%v, %d)", tc.name, tc.code, tc.updated, tc.stop, specific, exit, tc.specific, tc.exit)
		}
	}
	if ExitWindowsRestart == 0 || ExitWindowsRestart == ExitNotEnrolled {
		t.Errorf("ExitWindowsRestart = %d must be a nonzero code that differs from ExitNotEnrolled", ExitWindowsRestart)
	}
}

func TestWindowsDefinitionWithoutEnvironmentPrintsTheRecoveryCommands(t *testing.T) {
	source := `C:\Downloads\ownsand-agent.exe`
	exe := `C:\Program Files\OwnSand Agent\ownsand-service.exe`
	lines := strings.Split(strings.TrimSuffix(WindowsDefinition(source, exe, nil), "\n"), "\n")
	want := []string{
		`"C:\Downloads\ownsand-agent.exe" service prepare`,
		WindowsCommand(exe),
		"sc.exe failure ownsand-agent reset= 86400 actions= restart/5000",
		"sc.exe failureflag ownsand-agent 1",
	}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("WindowsDefinition() lines = %q, want %q", lines, want)
	}
	if strings.Contains(WindowsDefinition(source, exe, []string{}), "reg.exe") {
		t.Fatal("an empty environment must not print a reg.exe command")
	}
}

func TestWindowsDefinitionPrintsTheEnvironmentCommand(t *testing.T) {
	env := []string{"OWNSAND_AGENT_LOG_LEVEL=debug", `OWNSAND_AGENT_STATE_DIR=D:\ownsand test\`}
	lines := strings.Split(strings.TrimSuffix(WindowsDefinition(`C:\a.exe`, `C:\launcher.exe`, env), "\n"), "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[1], "sc.exe create ownsand-agent") ||
		!strings.HasPrefix(lines[2], "sc.exe failure ownsand-agent") || !strings.HasPrefix(lines[3], "sc.exe failureflag ownsand-agent") {
		t.Fatalf("definition lines = %q", lines)
	}
	// The trailing backslash in front of the closing quote is doubled, and
	// `#` is the first separator that no entry contains.
	want := `reg.exe add "HKLM\SYSTEM\CurrentControlSet\Services\ownsand-agent" /v Environment /t REG_MULTI_SZ /s "#" /d "OWNSAND_AGENT_LOG_LEVEL=debug#OWNSAND_AGENT_STATE_DIR=D:\ownsand test\\" /f`
	if lines[4] != want {
		t.Fatalf("reg command:\n got %s\nwant %s", lines[4], want)
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
	if out := WindowsDefinition(`C:\a.exe`, `C:\launcher.exe`, []string{all}); !strings.Contains(out, "REM no reg.exe separator") || strings.Contains(out, "reg.exe add") {
		t.Fatalf("definition must fall back to a REM line: %q", out)
	}
}

func TestInstallWindowsRecovery(t *testing.T) {
	boom := errors.New("boom")
	ok := func() error { return nil }
	fail := func() error { return boom }
	run := func(setActions, setFlag func() error, removeErr error) (error, int, int) {
		flagCalls, removed := 0, 0
		err := InstallWindowsRecovery(setActions,
			func() error { flagCalls++; return setFlag() },
			func() error { removed++; return removeErr })
		return err, flagCalls, removed
	}
	if err, flags, removed := run(ok, ok, nil); err != nil || flags != 1 || removed != 0 {
		t.Fatalf("success: err=%v flag calls=%d removed=%d", err, flags, removed)
	}
	err, flags, removed := run(fail, ok, nil)
	if !errors.Is(err, boom) || flags != 0 || removed != 1 || !strings.Contains(err.Error(), "the new service was removed") {
		t.Fatalf("actions failure: err=%v flag calls=%d removed=%d", err, flags, removed)
	}
	err, flags, removed = run(ok, fail, nil)
	if !errors.Is(err, boom) || flags != 1 || removed != 1 {
		t.Fatalf("flag failure: err=%v flag calls=%d removed=%d", err, flags, removed)
	}
	if err, _, _ := run(ok, fail, errors.New("rm")); err == nil || !strings.Contains(err.Error(), "removing the new service also failed") {
		t.Fatalf("remove failure: err=%v", err)
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

// decodeSystemdWord decodes one double-quoted systemd word that starts at the
// beginning of s and returns the text after the closing quote. It follows the
// rules of systemd.syntax(7) for the escapes that strconv.Quote writes, then
// expands specifiers like systemd does: %% is a literal percent sign and any
// other %X is an unresolved specifier, which makes systemd drop the
// assignment. A $ stays literal: systemd substitutes no variables in
// Environment= values or in the executable of ExecStart=.
func decodeSystemdWord(s string) (string, string, error) {
	if !strings.HasPrefix(s, `"`) {
		return "", "", fmt.Errorf("word %q does not start with a quote", s)
	}
	// Unquote the C-style escapes first; the quoted text ends at the first
	// unescaped quote.
	end := -1
	for i := 1; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == '"' {
			end = i
			break
		}
	}
	if end < 0 {
		return "", "", fmt.Errorf("word %q has no closing quote", s)
	}
	raw, err := strconv.Unquote(s[:end+1])
	if err != nil {
		return "", "", err
	}
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '%':
			if i+1 >= len(raw) || raw[i+1] != '%' {
				return "", "", fmt.Errorf("unresolved specifier in %q", raw)
			}
			b.WriteByte('%')
			i++
		default:
			b.WriteByte(raw[i])
		}
	}
	return b.String(), s[end+1:], nil
}

func TestSystemdUnitEnvironmentQuotesSpecifiers(t *testing.T) {
	values := []string{
		"OWNSAND_AGENT_API_URL=https://h/a%20b",
		"A=100%",
		"B=%%",
		"C=%h/%t/%n",
		"D=%",
		"E=a b",
		`F=say "hi"`,
		`G=C:\data\%USERPROFILE%`,
		"H=line1\nline2\ttab\x01",
		"I=héllo 世界 😀",
		"J=$FOO ${BAR} $$",
		"K=",
	}
	unit := SystemdUnit("/usr/bin/ownsand-agent", values)
	var got []string
	for _, line := range strings.Split(unit, "\n") {
		rest, ok := strings.CutPrefix(line, "Environment=")
		if !ok {
			continue
		}
		v, tail, err := decodeSystemdWord(rest)
		if err != nil || tail != "" {
			t.Fatalf("Environment line %q: decoded %q, tail %q, err %v", line, v, tail, err)
		}
		got = append(got, v)
	}
	if len(got) != len(values) {
		t.Fatalf("decoded %d Environment lines, want %d:\n%s", len(got), len(values), unit)
	}
	for i, want := range values {
		if got[i] != want {
			t.Errorf("Environment line %d decodes to %q, want %q", i, got[i], want)
		}
		if !utf8.ValidString(got[i]) {
			t.Errorf("Environment line %d decodes to invalid UTF-8", i)
		}
	}
}

func TestSystemdUnitEnvironmentWithoutPercentIsUnchanged(t *testing.T) {
	unit := SystemdUnit("/usr/bin/ownsand-agent", []string{"A=b c", `B=q"\`})
	for _, want := range []string{"Environment=\"A=b c\"\n", "Environment=" + strconv.Quote(`B=q"\`) + "\n"} {
		if !strings.Contains(unit, want) {
			t.Errorf("systemd unit lacks %q:\n%s", want, unit)
		}
	}
}

func TestSystemdUnitExecStartQuotesExecutable(t *testing.T) {
	for _, exe := range []string{
		"/usr/local/bin/ownsand-agent",
		"/opt/OwnSand Agent/ownsand-agent",
		"/opt/100%/own sand/agent",
		"/opt/%h/agent",
		"/opt/$HOME/$$x/agent",
		`/opt/back\slash "q"/agent`,
		"/opt/-dash/@at/agent",
		"/opt/世界 é/agent",
	} {
		unit := SystemdUnit(exe, nil)
		var line string
		for _, l := range strings.Split(unit, "\n") {
			if rest, ok := strings.CutPrefix(l, "ExecStart="); ok {
				line = rest
			}
		}
		word, tail, err := decodeSystemdWord(line)
		if err != nil {
			t.Errorf("exe %q: ExecStart=%s: %v", exe, line, err)
			continue
		}
		if word != exe {
			t.Errorf("exe %q: ExecStart executable decodes to %q", exe, word)
		}
		// The command line has exactly one more word, the argument run.
		if tail != " run" {
			t.Errorf("exe %q: ExecStart arguments are %q, want %q", exe, tail, " run")
		}
		for _, want := range []string{"Restart=always\n", "RestartSec=5\n", "KillMode=control-group\n", "StateDirectory=ownsand-agent\n", "StateDirectoryMode=0700\n"} {
			if !strings.Contains(unit, want) {
				t.Errorf("exe %q: systemd unit lacks %q", exe, want)
			}
		}
	}
}
