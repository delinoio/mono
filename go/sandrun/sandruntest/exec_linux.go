// SPDX-License-Identifier: Apache-2.0

package sandruntest

import "golang.org/x/sys/unix"

func execChildExited(pid int) bool {
	// WNOWAIT observes an exited child without consuming Cmd.Wait's result.
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
		if err != unix.EINTR {
			return err == unix.ECHILD || err == nil && info.Signo == int32(unix.SIGCHLD)
		}
	}
}
