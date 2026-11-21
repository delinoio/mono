// SPDX-License-Identifier: FSL-1.1-ALv2

// Package service produces the service definitions that run
// `ownsand-agent run` under the service manager of each host OS: a
// systemd unit on Linux, a launchd daemon on macOS and a Windows service.
package service

import (
	"fmt"
	"strings"
)

// Name is the service name on every host OS.
const Name = "ownsand-agent"

// LaunchdLabel is the launchd label on macOS.
const LaunchdLabel = "io.delino.ownsand-agent"

// ExitNotEnrolled is the exit code of `run` without an identity, also after
// a device removal. The service manager must not restart the agent for it.
const ExitNotEnrolled = 3

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
		fmt.Fprintf(&b, "Environment=%q\n", e)
	}
	b.WriteString(`
[Install]
WantedBy=multi-user.target
`)
	return b.String()
}

// LaunchdDaemon returns the launchd property list. KeepAlive restarts the
// agent unless it exited with ExitNotEnrolled.
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
`, LaunchdLabel, xmlEscape(exe), envXML.String())
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// WindowsCommand returns the sc.exe command that registers the Windows
// service, for operators who do not use `service install`.
func WindowsCommand(exe string) string {
	return fmt.Sprintf(`sc.exe create %s binPath= "\"%s\" run" start= auto DisplayName= "OwnSand device agent"`, Name, exe)
}
