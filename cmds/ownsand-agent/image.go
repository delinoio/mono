// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/imagebuild"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
)

type imageBuildFlags struct {
	os          string
	ipsw        string
	iso         string
	edition     string
	diskGiB     int64
	cpus        int
	memoryMiB   int64
	guestBinary string
}

// validateImageResources checks --disk-gib, --cpus and --memory-mib before the
// build creates any state, so that a value that would overflow the conversion
// to bytes or to a 32-bit vCPU count is a usage error and never wraps into a
// smaller image or VM. It returns the vCPU count for the build options.
func validateImageResources(f imageBuildFlags) (int32, error) {
	if err := imagebuild.ValidateResources(f.diskGiB, int64(f.cpus), f.memoryMiB); err != nil {
		return 0, err
	}
	return int32(f.cpus), nil
}

// imageBuild runs `ownsand-agent image build`. The running agent sees the
// new image at its next capability report.
func imageBuild(cfg config.Config, f imageBuildFlags, stdout, stderr io.Writer, c colors) int {
	guestOS, err := imagebuild.ParseGuestOS(f.os)
	if err != nil {
		fmt.Fprintln(stderr, "ownsand-agent:", err)
		return exitUsage
	}
	cpus, err := validateImageResources(f)
	if err != nil {
		fmt.Fprintln(stderr, "ownsand-agent:", err)
		return exitUsage
	}
	st, err := store.Open(cfg.StateDir)
	if err != nil {
		fmt.Fprintln(stderr, "ownsand-agent:", err)
		return exitError
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = imagebuild.Build(ctx, imagebuild.Options{
		ImagesDir:   st.Path(store.ImagesDir),
		GuestOS:     guestOS,
		IPSW:        f.ipsw,
		ISO:         f.iso,
		Edition:     f.edition,
		DiskGiB:     f.diskGiB,
		CPUCount:    cpus,
		MemoryMiB:   f.memoryMiB,
		GuestBinary: f.guestBinary,
		Logger:      newLogger(cfg),
	})
	if err != nil {
		fmt.Fprintln(stderr, c.bad("image build failed:"), err)
		if errors.Is(err, imagebuild.ErrUnsupported) {
			return exitUsage
		}
		return exitError
	}
	fmt.Fprintln(stdout, c.ok("Built"), "the", guestOS.String(), "base image")
	return exitOK
}
