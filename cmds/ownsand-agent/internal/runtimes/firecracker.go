// SPDX-License-Identifier: FSL-1.1-ALv2

package runtimes

import (
	"log/slog"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/netpolicy"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/fcrt"
)

// LinuxImageDir is the directory of the Linux base image in the state
// directory.
const LinuxImageDir = images.LinuxDir

// linuxImage returns the kernel and the root file system of the Linux base
// image: the configured paths, or images/linux in the state directory.
func linuxImage(cfg config.Config, st *store.Store) (kernel, rootfs string) {
	kernel, rootfs = cfg.LinuxKernel, cfg.LinuxRootfs
	if kernel == "" {
		kernel = st.Path(store.ImagesDir, LinuxImageDir, "vmlinux")
	}
	if rootfs == "" {
		rootfs = st.Path(store.ImagesDir, LinuxImageDir, "rootfs.ext4")
	}
	return kernel, rootfs
}

func newFirecracker(cfg config.Config, st *store.Store, logger *slog.Logger) (vmruntime.Runtime, error) {
	kernel, rootfs := linuxImage(cfg, st)
	return fcrt.NewRuntime(fcrt.Options{
		Binary:  cfg.FirecrackerBinary,
		Kernel:  kernel,
		Rootfs:  rootfs,
		VMsDir:  st.Path(store.VMsDir),
		Network: netpolicy.NewManager(logger),
		Logger:  logger,
	})
}
