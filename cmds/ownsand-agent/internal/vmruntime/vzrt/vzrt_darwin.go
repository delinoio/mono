// SPDX-License-Identifier: Apache-2.0

// Package vzrt is the macOS runtime (Virtualization.framework).
//
// Not implemented in this version: Virtualization.framework needs an
// Objective-C binding from Go (cgo, for example github.com/Code-Hex/vz,
// which is a new dependency that needs owner approval), a code-signed agent
// with the com.apple.security.virtualization entitlement, the userspace
// network stack of the network policy and an Apple silicon host for
// acceptance, none of which this change has. The runtime reports a failing
// `virtualization-framework` check and rejects creates.
package vzrt

import (
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/unavailable"
)

// New returns the macOS runtime stub.
func New() (vmruntime.Runtime, error) {
	return unavailable.New(config.RuntimeKindVirtualization, "virtualization-framework",
		"the Virtualization.framework runtime is not implemented in this agent version"), nil
}
