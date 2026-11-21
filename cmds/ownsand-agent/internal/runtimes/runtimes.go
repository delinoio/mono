// SPDX-License-Identifier: FSL-1.1-ALv2

// Package runtimes builds the sandbox runtime that the configuration
// selects.
package runtimes

import (
	"fmt"
	"log/slog"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/hcsrt"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/processrt"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/vzrt"
)

// New returns the runtime of cfg.
func New(cfg config.Config, st *store.Store, logger *slog.Logger) (vmruntime.Runtime, error) {
	switch cfg.Runtime {
	case config.RuntimeKindProcess:
		return processrt.New(cfg.GuestBinary, logger)
	case config.RuntimeKindFirecracker:
		return newFirecracker(cfg, st, logger)
	case config.RuntimeKindVirtualization:
		kernel, rootfs := linuxImage(cfg, st)
		return vzrt.New(vzrt.Options{Kernel: kernel, Rootfs: rootfs, MacOSImage: st.Path(store.ImagesDir, images.MacOSDir),
			VMsDir: st.Path(store.VMsDir), Logger: logger})
	case config.RuntimeKindHyperV:
		kernel, rootfs := linuxImage(cfg, st)
		return hcsrt.New(hcsrt.Options{Kernel: kernel, Rootfs: rootfs, WindowsImage: st.Path(store.ImagesDir, images.WindowsDir),
			VMsDir: st.Path(store.VMsDir), Logger: logger})
	}
	return nil, fmt.Errorf("%w: no sandbox runtime for this host; set %s", vmruntime.ErrUnsupported, config.EnvRuntime)
}
