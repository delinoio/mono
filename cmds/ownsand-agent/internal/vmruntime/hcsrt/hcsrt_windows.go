// SPDX-License-Identifier: Apache-2.0

// Package hcsrt is the Windows runtime (Hyper-V through the Host Compute
// Service).
//
// Not implemented in this version: it needs a Host Compute Service binding
// (for example github.com/Microsoft/hcsshim, a new dependency that needs
// owner approval), the recorded Windows network enforcement (HNS ACLs or
// WFP, still an open decision), Hyper-V sockets for the guest helper and a
// Windows host for acceptance. The runtime reports a failing `hyper-v`
// check and rejects creates.
package hcsrt

import (
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/unavailable"
)

// New returns the Windows runtime stub.
func New() (vmruntime.Runtime, error) {
	return unavailable.New(config.RuntimeKindHyperV, "hyper-v",
		"the Hyper-V runtime is not implemented in this agent version"), nil
}
