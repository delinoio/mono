//go:build linux

package platform

import "github.com/delinoio/mono/cmds/ownsand-agent/internal/runtime"

// New returns the Linux runtime. The Firecracker runtime is not wired yet,
// so every guest is reported as unsupported.
func New(runtime.Config) (runtime.Runtime, error) {
	return runtime.NewUnsupported("firecracker", map[string]string{
		"firecracker": "not implemented in this agent version",
	}), nil
}
