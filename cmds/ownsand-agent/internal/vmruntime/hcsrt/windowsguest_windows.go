// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestsync"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Timeouts of the Windows guest helper calls of the runtime.
const (
	flushTimeout = 30 * time.Second
	clockTimeout = 10 * time.Second
)

// flushVolumesCommand flushes the file system caches of a Windows guest
// (see cmds/ownsand-guest/service_windows.go); Windows has no `sync`.
var flushVolumesCommand = []string{WindowsGuestBinary, "flush-volumes"}

// readWindowsImage reads and checks the image.json of a Windows image or
// snapshot directory.
func readWindowsImage(dir string) (images.Metadata, error) {
	m, err := images.ReadMetadata(dir)
	if err != nil {
		return m, err
	}
	if m.GuestOS != ownsandv1.GuestOs_GUEST_OS_WINDOWS {
		return m, fmt.Errorf("%s describes a %v image", images.MetadataFile, m.GuestOS)
	}
	return m, nil
}

// recoverWindowsImage restores the previous Windows base image when an
// `ownsand-agent image build` ended between the two renames of its install
// (see images.Recover). It does nothing while a build holds the image lock.
// A failure is logged; the callers then see the image as it is.
func (r *Runtime) recoverWindowsImage() {
	if r.opts.WindowsImage == "" {
		return
	}
	if _, err := images.Recover(r.opts.WindowsImage); err != nil {
		slog.Warn("recover the Windows base image failed", "image_dir", r.opts.WindowsImage, "error", err)
	}
}

// windowsImageCheck reports whether the Windows base image is present.
func (r *Runtime) windowsImageCheck() (bool, string) {
	if r.opts.WindowsImage == "" {
		return false, "no Windows image directory is configured"
	}
	r.recoverWindowsImage()
	if !fileExists(filepath.Join(r.opts.WindowsImage, images.WindowsDisk)) {
		return false, "the Windows base image is not built; run `ownsand-agent image build --iso <Windows ISO>`"
	}
	m, err := readWindowsImage(r.opts.WindowsImage)
	if err != nil {
		return false, fmt.Sprintf("the Windows base image has no valid %s: %v", images.MetadataFile, err)
	}
	return true, fmt.Sprintf("the Windows base image (%s) is present", m.Source)
}

// checkWindowsResources rejects resources below the minimums of the image.
func checkWindowsResources(res *ownsandv1.Resources, m images.Metadata) error {
	if res.GetVcpuCount() < m.MinimumCPUCount {
		return fmt.Errorf("%w: the Windows image needs at least %d vCPUs, not %d", vmruntime.ErrUnsupported, m.MinimumCPUCount, res.GetVcpuCount())
	}
	if res.GetMemoryMib() < m.MinimumMemoryMiB {
		return fmt.Errorf("%w: the Windows image needs at least %d MiB of memory, not %d", vmruntime.ErrUnsupported, m.MinimumMemoryMiB, res.GetMemoryMib())
	}
	return nil
}

// prepareWindowsDisk validates and copies the selected source before growing
// the local disk. The copy is a full VHDX, so the sandbox and its snapshots
// can keep using it after a rebuild. The guest helper extends C: at boot.
func (r *Runtime) prepareWindowsDisk(ctx context.Context, spec vmruntime.Spec, ops vmCreateOps, prepareDir func() error) (vmSpec, int64, error) {
	disk := filepath.Join(spec.Dir, windowsDiskFile)
	var copyStarted time.Time
	copySource := func(srcDir, diskName string) error {
		src := filepath.Join(srcDir, diskName)
		st, err := os.Stat(src)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("hcsrt: check the Windows source disk: %w", err)
		}
		if err != nil || !st.Mode().IsRegular() {
			if spec.Snapshot != nil {
				return fmt.Errorf("%w: the snapshot has no Windows disk", vmruntime.ErrUnsupported)
			}
			return fmt.Errorf("%w: the Windows base image is not built", vmruntime.ErrUnsupported)
		}
		m, err := readWindowsImage(srcDir)
		if err != nil {
			return fmt.Errorf("%w: the Windows image metadata is unreadable: %w", vmruntime.ErrUnsupported, err)
		}
		if err := checkWindowsResources(spec.Resources, m); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := prepareDir(); err != nil {
			return err
		}
		copyStarted = time.Now()
		if err := ops.copyFile(ctx, src, disk); err != nil {
			return fmt.Errorf("hcsrt: prepare disk: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ops.copyFile(ctx, filepath.Join(srcDir, images.MetadataFile), filepath.Join(spec.Dir, images.MetadataFile)); err != nil {
			return fmt.Errorf("hcsrt: copy the image metadata: %w", err)
		}
		return ctx.Err()
	}

	var err error
	if spec.Snapshot != nil {
		err = copySource(spec.Snapshot.Dir, snapWindowsDisk)
	} else {
		baseDir := r.opts.WindowsImage
		if baseDir == "" {
			return vmSpec{}, 0, fmt.Errorf("%w: the Windows base image is not built", vmruntime.ErrUnsupported)
		}
		err = images.WithBaseImage(ctx, baseDir, func() error {
			return copySource(baseDir, images.WindowsDisk)
		})
	}
	if err != nil {
		return vmSpec{}, 0, err
	}
	copied := time.Now()
	if err := ctx.Err(); err != nil {
		return vmSpec{}, 0, err
	}
	size, err := ops.growVHDX(disk, spec.Resources.GetDiskGib()<<30)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return vmSpec{}, 0, ctxErr
	}
	if err != nil {
		return vmSpec{}, 0, fmt.Errorf("hcsrt: prepare disk: %w", err)
	}
	if err := grantVMGroup(disk, accessReadWrite, false); err != nil {
		return vmSpec{}, 0, err
	}
	var fileBytes int64
	if st, err := os.Stat(disk); err == nil {
		fileBytes = st.Size()
	}
	r.logger.Info("Windows disk prepared", "sandbox_id", spec.SandboxID, "virtual_bytes", size, "file_bytes", fileBytes,
		"copy_ms", copied.Sub(copyStarted).Milliseconds(), "resize_ms", time.Since(copied).Milliseconds())
	return vmSpec{Windows: true, AllowReset: true, Disk: disk}, size, nil
}

// flushGuest flushes the guest file system caches before a pause.
func (r *Runtime) flushGuest(ctx context.Context, v *vm) error {
	if v.guestOS != ownsandv1.GuestOs_GUEST_OS_WINDOWS {
		return guestsync.Sync(ctx, v.id, v.Dial)
	}
	ctx, cancel := context.WithTimeout(ctx, flushTimeout)
	defer cancel()
	code, err := guestsync.Run(ctx, v.id, v.Dial, flushVolumesCommand)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("flush-volumes exited with %d", code)
	}
	return nil
}

// setGuestClock sets the guest clock from the host clock; a failure only
// logs.
func (r *Runtime) setGuestClock(ctx context.Context, v *vm) {
	ctx, cancel := context.WithTimeout(ctx, clockTimeout)
	defer cancel()
	client := guestclient.New(v.Dial)
	defer client.Close()
	if err := client.SetClock(ctx, time.Now()); err != nil {
		r.logger.Warn("set the guest clock failed", "sandbox_id", v.id, "error", err)
	}
}
