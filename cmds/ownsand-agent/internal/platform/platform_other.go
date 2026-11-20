//go:build !linux && !darwin && !windows

package platform

import "github.com/delinoio/mono/cmds/ownsand-agent/internal/runtime"

// New returns a runtime that supports no guest on other host OSes.
func New(runtime.Config) (runtime.Runtime, error) {
	return runtime.NewUnsupported("none", map[string]string{"hypervisor": "unsupported host OS"}), nil
}
