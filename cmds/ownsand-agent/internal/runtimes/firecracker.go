// SPDX-License-Identifier: Apache-2.0

package runtimes

import (
	"fmt"
	"log/slog"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
)

// newFirecracker is replaced by the Linux Firecracker runtime.
func newFirecracker(config.Config, *store.Store, *slog.Logger) (vmruntime.Runtime, error) {
	return nil, fmt.Errorf("%w: the Firecracker runtime is not part of this build", vmruntime.ErrUnsupported)
}
