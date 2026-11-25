// SPDX-License-Identifier: FSL-1.1-ALv2

package vzrt

import (
	"fmt"
	"math"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
)

func memoryMiBToBytes(memoryMiB int64) (uint64, error) {
	const maxMemoryMiB int64 = math.MaxUint64 >> 20
	if memoryMiB < 1 || memoryMiB > maxMemoryMiB {
		return 0, fmt.Errorf("%w: VM memory must be between 1 and %d MiB to fit in a uint64 byte count, got %d MiB",
			vmruntime.ErrUnsupported, maxMemoryMiB, memoryMiB)
	}
	return uint64(memoryMiB) << 20, nil
}
