// SPDX-License-Identifier: Apache-2.0

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

// imageBuild runs `ownsand-agent image build`. The running agent sees the
// new image at its next capability report.
func imageBuild(cfg config.Config, f imageBuildFlags, stdout, stderr io.Writer, c colors) int {
	guestOS, err := imagebuild.ParseGuestOS(f.os)
	if err != nil {
		fmt.Fprintln(stderr, "ownsand-agent:", err)
		return exitUsage
	}
	if f.diskGiB < 0 || f.cpus < 0 || f.memoryMiB < 0 {
		fmt.Fprintln(stderr, "ownsand-agent: --disk-gib, --cpus and --memory-mib must not be negative")
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
		CPUCount:    int32(f.cpus),
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
