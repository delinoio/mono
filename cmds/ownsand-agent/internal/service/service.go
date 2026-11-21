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
`, exe, ExitNotEnrolled)
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
func systemdQuote(s string) string {
	return strings.ReplaceAll(strconv.Quote(s), "%", "%%")
}

// LaunchdDaemon returns the launchd property list. KeepAlive restarts the
// agent after a nonzero exit only (SuccessfulExit=false). The definition
// starts `run --service-manager launchd`, under which the agent maps its exit
// codes (see LaunchdExitCode): an installed update restarts the agent and
// ExitNotEnrolled does not.
func LaunchdDaemon(exe string, env []string) string {
	var envXML strings.Builder
	if len(env) > 0 {
		envXML.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, e := range env {
			k, v, _ := strings.Cut(e, "=")
			fmt.Fprintf(&envXML, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", xmlEscape(k), xmlEscape(v))
		}
		envXML.WriteString("\t</dict>\n")
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
`, LaunchdLabel, xmlEscape(exe), ManagerFlag, ManagerLaunchd, envXML.String())
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
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
	return fmt.Sprintf(`sc.exe create %s binPath= "\"%s\" run" start= auto DisplayName= "OwnSand device agent"`, Name, exe)
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
// sc.exe create command and, when env is not empty, the reg.exe command that
// sets the service environment. Without environment variables the output is
// the sc.exe command alone.
func WindowsDefinition(exe string, env []string) string {
	out := WindowsCommand(exe) + "\n"
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
