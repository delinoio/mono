// SPDX-License-Identifier: Apache-2.0

package sandruntest

import (
	"errors"

	"golang.org/x/sys/unix"
)

func execChildExited(pid int) bool {
	// XNU's sys/proc.h defines SZOMB as the state of an exited child
	// awaiting collection. SysctlKinfoProc returns EIO for an absent PID.
	const zombieState = 5
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	return errors.Is(err, unix.EIO) || err == nil && info.Proc.P_stat == zombieState
}
