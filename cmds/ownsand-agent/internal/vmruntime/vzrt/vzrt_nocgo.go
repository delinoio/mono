// SPDX-License-Identifier: Apache-2.0

//go:build darwin && !cgo

package vzrt

import (
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/unavailable"
)

// New returns a runtime that rejects creates: the Virtualization.framework
// binding needs cgo, and this agent binary was built without it (for
// example, cross-compiled in CI).
func New(Options) (vmruntime.Runtime, error) {
	return unavailable.New(config.RuntimeKindVirtualization, checkFramework,
		"this agent binary was built without cgo, which the Virtualization.framework runtime needs"), nil
}
