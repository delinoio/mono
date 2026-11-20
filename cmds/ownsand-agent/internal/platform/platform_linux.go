//go:build linux

package platform

import (
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/runtime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/runtime/firecracker"
)

// New returns the Firecracker runtime.
func New(cfg runtime.Config) (runtime.Runtime, error) {
	return firecracker.New(firecracker.Config{
		StateDir:           cfg.StateDir,
		Logger:             cfg.Logger,
		AllowUnsignedImage: cfg.AllowUnsignedImage,
		SkipNetwork:        cfg.Passive,
	})
}
