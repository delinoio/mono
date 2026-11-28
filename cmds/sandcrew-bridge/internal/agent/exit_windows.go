// SPDX-License-Identifier: Apache-2.0

package agent

import "os"

// Windows processes end with an exit code only.
func exitOf(state *os.ProcessState) Exit {
	return Exit{Code: state.ExitCode()}
}
