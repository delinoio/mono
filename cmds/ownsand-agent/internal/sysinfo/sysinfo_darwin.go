// SPDX-License-Identifier: Apache-2.0

package sysinfo

import "golang.org/x/sys/unix"

func memoryBytes() (uint64, error) {
	return unix.SysctlUint64("hw.memsize")
}
