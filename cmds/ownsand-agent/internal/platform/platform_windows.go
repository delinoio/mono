//go:build windows

package platform

import "github.com/delinoio/mono/cmds/ownsand-agent/internal/runtime"

// New returns the Windows runtime. Host Compute Service bindings are a new
// dependency that needs owner approval, so Linux and Windows guests are
// reported as unsupported on Windows hosts in this version.
func New(runtime.Config) (runtime.Runtime, error) {
	return runtime.NewUnsupported("hyper-v", map[string]string{
		"hyper_v": "not implemented in this agent version",
	}), nil
}
