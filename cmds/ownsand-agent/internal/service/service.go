// SPDX-License-Identifier: FSL-1.1-ALv2

// Package service produces the service definitions that run
// `ownsand-agent run` under the service manager of each host OS: a
// systemd unit on Linux, a launchd daemon on macOS and a Windows service.
package service

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Name is the service name on every host OS.
const Name = "ownsand-agent"

// LaunchdLabel is the launchd label on macOS.
const LaunchdLabel = "io.delino.ownsand-agent"

// ExitNotEnrolled is the exit code of `run` without an identity, also after
// a device removal. The service manager must not restart the agent for it.
const ExitNotEnrolled = 3

// ExitLaunchdRestart is the exit code of `run --service-manager launchd`
// after an installed update. launchd has only a success/failure predicate
// (KeepAlive SuccessfulExit=false restarts the job after a nonzero exit),
// so under launchd the agent reports an installed update as a failure code
// that makes launchd start the new executable.
const ExitLaunchdRestart = 75

// ExitWindowsRestart is the service-specific exit status that the Windows
// service reports after an installed update. The service control manager
// restarts a service whose failure actions are configured when it stops with
// a nonzero exit status, so this status makes it start the new executable.
// It has the same value as ExitLaunchdRestart.
const ExitWindowsRestart = 75

// Windows failure-action policy of the service. It mirrors the systemd unit
// (Restart=always, RestartSec=5): one restart action that the service
// control manager repeats for every failure. The reset period has no effect
// with a single repeating action and is a recorded choice.
const (
	WindowsRestartDelayMillis = 5000
	WindowsFailureResetSecs   = 86400
)

// Manager identifies the service manager that starts `run`. The empty
// value is a direct or systemd/Windows start and keeps the public exit codes.
type Manager string

// ManagerLaunchd marks a `run` started by the launchd daemon definition. The
// definition passes it as the hidden flag `--service-manager launchd`.
const ManagerLaunchd Manager = "launchd"

// ManagerFlag is the hidden `run` flag that carries the Manager.
const ManagerFlag = "--service-manager"

// LaunchdExitCode maps the public exit code of `run` to the exit code that
// the launchd KeepAlive predicate (SuccessfulExit=false) needs: an installed
// update (updated) becomes ExitLaunchdRestart so that launchd starts the new
// executable, ExitNotEnrolled becomes 0 so that launchd does not restart a
// device that is not enrolled or was removed, and every other code (errors,
// usage errors) stays unchanged so that launchd restarts the agent after an
// error like the systemd unit does.
func LaunchdExitCode(code int, updated bool) int {
	switch {
	case updated:
		return ExitLaunchdRestart
	case code == ExitNotEnrolled:
		return 0
	}
	return code
}

// WindowsExitStatus maps the public exit code of `run` to the exit status
// that the Windows service reports to the service control manager. With
// failure actions and the non-crash failure flag set, the manager restarts
// the service after a stop with a nonzero exit status. Restart is therefore
// reported as a service-specific error: ExitWindowsRestart after an installed
// update (updated) and the code itself for any other nonzero code, such as 1
// for an error. A requested stop or shutdown (stopRequested), a clean stop
// and ExitNotEnrolled (not enrolled or a finished removal) report success, so
// that the manager does not restart the agent. serviceSpecific tells the
// caller to report exit as a service-specific exit code.
func WindowsExitStatus(code int, updated, stopRequested bool) (serviceSpecific bool, exit uint32) {
	switch {
	case stopRequested:
		return false, 0
	case updated:
		return true, ExitWindowsRestart
	case code == ExitNotEnrolled || code == 0:
		return false, 0
	}
	return true, uint32(code)
}

// Paths of the service definitions.
const (
	SystemdUnitPath = "/etc/systemd/system/ownsand-agent.service"
	LaunchdPlist    = "/Library/LaunchDaemons/io.delino.ownsand-agent.plist"
)

// SystemdUnit returns the systemd unit. KillMode=control-group stops every
// VM process with the agent, so max_ttl holds when the agent stops. The
// agent runs as root because the Firecracker runtime needs /dev/kvm, tap
// devices and nftables (CAP_NET_ADMIN).
func SystemdUnit(exe string, env []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `[Unit]
Description=OwnSand device agent
Documentation=https://ownsand.delino.io
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s run
Restart=always
RestartSec=5
RestartPreventExitStatus=%d
KillMode=control-group
StateDirectory=ownsand-agent
StateDirectoryMode=0700
`, systemdQuote(exe), ExitNotEnrolled)
	for _, e := range env {
		fmt.Fprintf(&b, "Environment=%s\n", systemdQuote(e))
	}
	b.WriteString(`
[Install]
WantedBy=multi-user.target
`)
	return b.String()
}

// systemdQuote quotes s as one double-quoted systemd.syntax(7) word. systemd
// reads the C-style escapes that strconv.Quote writes (\\, \", \n, \xNN, \uNNNN),
// and it expands specifiers (%h, %t, ...) in Environment= values and command
// lines even inside quotes, so every % is doubled to keep the literal text.
// A $ stays as it is: systemd does not expand it in Environment= values, nor
// in the executable of ExecStart= (the first word is used as the path to
// execute without variable substitution), and a doubled $$ there would stay
// literal and break the path. A quoted executable also cannot be read as one
// of the ExecStart= prefixes (@, -, :, +, !) or split at spaces.
func systemdQuote(s string) string {
	return strings.ReplaceAll(strconv.Quote(s), "%", "%%")
}

// LaunchdDaemon returns the launchd property list. KeepAlive restarts the
// agent after a nonzero exit only (SuccessfulExit=false). The definition
// starts `run --service-manager launchd`, under which the agent maps its exit
// codes (see LaunchdExitCode): an installed update restarts the agent and
// ExitNotEnrolled does not.
//
// The property list is XML 1.0, so a value that XML cannot represent (a
// control character other than tab, LF and CR, U+FFFE, U+FFFF or invalid
// UTF-8) makes LaunchdDaemon return an error and no definition, instead of a
// file that launchd cannot read or reads back as another value.
func LaunchdDaemon(exe string, env []string) (string, error) {
	var envXML strings.Builder
	if len(env) > 0 {
		envXML.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, e := range env {
			k, v, _ := strings.Cut(e, "=")
			ek, err := xmlEscape(k)
			if err != nil {
				return "", fmt.Errorf("launchd environment variable name: %w", err)
			}
			ev, err := xmlEscape(v)
			if err != nil {
				return "", fmt.Errorf("launchd environment variable %s: %w", k, err)
			}
			fmt.Fprintf(&envXML, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", ek, ev)
		}
		envXML.WriteString("\t</dict>\n")
	}
	escExe, err := xmlEscape(exe)
	if err != nil {
		return "", fmt.Errorf("launchd executable path: %w", err)
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>run</string>
		<string>%s</string>
		<string>%s</string>
	</array>
%s	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>StandardErrorPath</key>
	<string>/Library/Logs/ownsand-agent.log</string>
</dict>
</plist>
`, LaunchdLabel, escExe, ManagerFlag, ManagerLaunchd, envXML.String()), nil
}

// xmlEscape escapes s as XML 1.0 character data. A carriage return is written
// as the character reference &#13;, because an XML parser normalizes a literal
// CR or CRLF to LF, which would change the value that launchd reads. It
// returns an error for text that XML 1.0 cannot represent: invalid UTF-8 and
// runes outside the Char production (tab, LF, CR, U+0020-U+D7FF,
// U+E000-U+FFFD and U+10000-U+10FFFF).
func xmlEscape(s string) (string, error) {
	var b strings.Builder
	for i, r := range s {
		if r == utf8.RuneError {
			if _, size := utf8.DecodeRuneInString(s[i:]); size == 1 {
				return "", fmt.Errorf("invalid UTF-8 at byte %d cannot be represented in XML 1.0", i)
			}
		}
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case r == '"':
			b.WriteString("&quot;")
		case r == '\r':
			b.WriteString("&#13;")
		case isXMLChar(r):
			b.WriteRune(r)
		default:
			return "", fmt.Errorf("character U+%04X at byte %d cannot be represented in XML 1.0", r, i)
		}
	}
	return b.String(), nil
}

// isXMLChar reports whether r is in the XML 1.0 Char production.
func isXMLChar(r rune) bool {
	return r == '\t' || r == '\n' || r == '\r' ||
		(r >= 0x20 && r <= 0xD7FF) ||
		(r >= 0xE000 && r <= 0xFFFD) ||
		(r >= 0x10000 && r <= 0x10FFFF)
}

// Windows registry location of the per-service environment. The service
// control manager gives a service the REG_MULTI_SZ value Environment of its
// key (NAME=value entries) as its process environment; a service never
// inherits the environment of the shell that installed it.
const (
	WindowsServiceRegistryKey = `SYSTEM\CurrentControlSet\Services\` + Name
	WindowsEnvironmentValue   = "Environment"
)

// windowsSeparators lists the candidate separators of the printed reg.exe
// command. reg.exe splits a REG_MULTI_SZ /d string at the separator, which
// is \0 by default; the printed command uses an explicit one-character
// separator that no entry contains, so that a value such as `D:\0data`
// is not split at its `\0`. The characters are quoted in the printed
// command, so command-shell metacharacters are safe.
const windowsSeparators = "#~|@$*?;:!^&<>{}[]()'`"

// WindowsCommand returns the sc.exe command that registers the Windows
// service, for operators who do not use `service install`.
func WindowsCommand(exe string) string {
	return fmt.Sprintf(`sc.exe create %s binPath= "\"%s\" service launch run" start= auto DisplayName= "OwnSand device agent"`, Name, exe)
}

// WindowsRecoveryCommands returns the two sc.exe commands that set the
// failure actions of the Windows service: restart after
// WindowsRestartDelayMillis, and the flag that also applies the actions when
// the service stops with a nonzero exit status. The commands are for
// operators who do not use `service install` and for existing installations.
func WindowsRecoveryCommands() []string {
	return []string{
		fmt.Sprintf("sc.exe failure %s reset= %d actions= restart/%d", Name, WindowsFailureResetSecs, WindowsRestartDelayMillis),
		fmt.Sprintf("sc.exe failureflag %s 1", Name),
	}
}

// WindowsEnvironmentCommand returns the reg.exe command that sets the
// Environment value of the service to the NAME=value entries, or an empty
// string when env is empty. It errors when every candidate separator occurs
// in an entry.
func WindowsEnvironmentCommand(env []string) (string, error) {
	if len(env) == 0 {
		return "", nil
	}
	joined := strings.Join(env, "")
	sep := ""
	for _, c := range windowsSeparators {
		if !strings.ContainsRune(joined, c) {
			sep = string(c)
			break
		}
	}
	if sep == "" {
		return "", errors.New("no reg.exe separator character is free of the environment values")
	}
	return fmt.Sprintf(`reg.exe add "HKLM\%s" /v %s /t REG_MULTI_SZ /s "%s" /d %s /f`,
		WindowsServiceRegistryKey, WindowsEnvironmentValue, sep, windowsQuote(strings.Join(env, sep))), nil
}

// windowsQuote quotes s as one command-line argument for the C runtime
// parser that reg.exe uses: it doubles backslashes before a quote or the
// closing quote and escapes quotes. The command shell still expands %NAME%
// inside quotes, so a value that contains a percent sign is not safe to paste
// (`service install` writes the registry directly and is not affected).
func windowsQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	backslashes := 0
	for _, r := range s {
		switch r {
		case '\\':
			backslashes++
			b.WriteRune(r)
			continue
		case '"':
			b.WriteString(strings.Repeat(`\`, backslashes+1))
		}
		backslashes = 0
		b.WriteRune(r)
	}
	b.WriteString(strings.Repeat(`\`, backslashes))
	b.WriteByte('"')
	return b.String()
}

// WindowsDefinition returns the printed Windows service definition: the
// sc.exe create command, the two sc.exe failure-action commands and, when env
// is not empty, the reg.exe command that sets the service environment.
func WindowsDefinition(source, launcher string, env []string) string {
	out := fmt.Sprintf(`"%s" service prepare`+"\n", source) + WindowsCommand(launcher) + "\n"
	for _, c := range WindowsRecoveryCommands() {
		out += c + "\n"
	}
	if len(env) == 0 {
		return out
	}
	reg, err := WindowsEnvironmentCommand(env)
	if err != nil {
		return out + "REM " + err.Error() + "; use `ownsand-agent service install`, which writes the Environment value directly\n"
	}
	return out + reg + "\n"
}

// InstallWindowsEnvironment writes the service environment after the
// service is created and before it starts. With no entries it does nothing.
// When setEnv fails it calls remove, so that a failed install does not leave
// a service that starts without its environment, and returns the error;
// the entries are never part of an error message.
func InstallWindowsEnvironment(env []string, setEnv func([]string) error, remove func() error) error {
	if len(env) == 0 {
		return nil
	}
	if err := setEnv(env); err != nil {
		if rerr := remove(); rerr != nil {
			return fmt.Errorf("write the service Environment value: %w (removing the new service also failed: %v)", err, rerr)
		}
		return fmt.Errorf("write the service Environment value: %w (the new service was removed)", err)
	}
	return nil
}

// InstallWindowsRecovery sets the failure actions of a new service and then
// the non-crash failure flag. When either fails it calls remove, so that a
// failed install does not leave a service that is never restarted, and
// returns the error.
func InstallWindowsRecovery(setActions, setFlag, remove func() error) error {
	err := setActions()
	step := "set the service failure actions"
	if err == nil {
		err = setFlag()
		step = "set the service failure actions flag"
	}
	if err == nil {
		return nil
	}
	if rerr := remove(); rerr != nil {
		return fmt.Errorf("%s: %w (removing the new service also failed: %v)", step, err, rerr)
	}
	return fmt.Errorf("%s: %w (the new service was removed)", step, err)
}
