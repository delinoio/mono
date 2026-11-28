// SPDX-License-Identifier: Apache-2.0

package agent

import "os"

// groupAlive is false on Windows: the bridge cannot signal a process
// group there, and SIGKILL ends only the leader.
func groupAlive(*os.Process) bool { return false }

// Windows processes end with an exit code only.
func exitOf(state *os.ProcessState) Exit {
	return Exit{Code: state.ExitCode()}
}
