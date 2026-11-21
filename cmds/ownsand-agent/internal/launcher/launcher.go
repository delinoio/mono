// SPDX-License-Identifier: FSL-1.1-ALv2

// Package launcher starts VM processes so that they end with the agent
// process. On Linux every VM process gets a parent-death signal (SIGKILL).
// Linux sends that signal when the creating thread exits, not only when the
// process exits, so the processes are started from one dedicated OS thread
// that lives as long as the agent process.
package launcher

import "os/exec"

// Start starts cmd bound to the agent process.
func Start(cmd *exec.Cmd) error {
	return start(cmd)
}
