// SPDX-License-Identifier: Apache-2.0

package sysinfo

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
)

func memoryBytes() (uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kib, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			return kib << 10, nil
		}
	}
	return 0, errors.New("sysinfo: MemTotal missing from /proc/meminfo")
}
