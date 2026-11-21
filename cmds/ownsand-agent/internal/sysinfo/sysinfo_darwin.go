// SPDX-License-Identifier: FSL-1.1-ALv2

package sysinfo

import "golang.org/x/sys/unix"

func memoryBytes() (uint64, error) {
	return unix.SysctlUint64("hw.memsize")
}
