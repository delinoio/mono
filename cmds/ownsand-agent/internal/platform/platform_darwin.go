//go:build darwin

package platform

import "github.com/delinoio/mono/cmds/ownsand-agent/internal/runtime"

// New returns the macOS runtime. Virtualization.framework bindings are a new
// dependency that needs owner approval, so Linux and macOS guests are
// reported as unsupported on macOS hosts in this version.
func New(runtime.Config) (runtime.Runtime, error) {
	return runtime.NewUnsupported("virtualization-framework", map[string]string{
		"virtualization_framework": "not implemented in this agent version",
	}), nil
}
