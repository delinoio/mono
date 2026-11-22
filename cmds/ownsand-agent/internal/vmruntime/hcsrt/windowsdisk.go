// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"fmt"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
)

const gib = 1 << 30

// checkWindowsDiskSize rejects a requested Windows disk size that is below
// the virtual size of the copied source disk. A VHDX cannot shrink below
// its partitions, so a smaller request would boot a guest with more virtual
// capacity than the sandbox record and the control plane reservation
// account for. The request is compared after rounding up to vhdAlign, like
// the resize. The error wraps vmruntime.ErrUnsupported, which the sandbox
// manager reports as a failed precondition, and names both sizes in GiB
// (the required size rounded up).
func checkWindowsDiskSize(requested, actual int64) error {
	if (requested+vhdAlign-1)/vhdAlign*vhdAlign >= actual {
		return nil
	}
	return fmt.Errorf("%w: the Windows disk needs at least %d GiB, not the requested %d GiB",
		vmruntime.ErrUnsupported, (actual+gib-1)/gib, requested/gib)
}
