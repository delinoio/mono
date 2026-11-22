// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"context"
	"fmt"
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

// windowsImageCheck reports whether the Windows base image is present.
func (r *Runtime) windowsImageCheck() (bool, string) {
	if r.opts.WindowsImage == "" {
		return false, "no Windows image directory is configured"
	}
	if !fileExists(filepath.Join(r.opts.WindowsImage, images.WindowsDisk)) {
		return false, "the Windows base image is not built; run `ownsand-agent image build --iso <Windows ISO>`"
	}
	m, err := readWindowsImage(r.opts.WindowsImage)
	if err != nil {
		return false, fmt.Sprintf("the Windows base image has no valid %s: %v", images.MetadataFile, err)
	}
	return true, fmt.Sprintf("the Windows base image (%s) is present", m.Source)
}

// windowsSource returns the disk and the image directory (with image.json)
// that a Windows sandbox starts from.
func (r *Runtime) windowsSource(spec vmruntime.Spec) (disk, metaDir string) {
	if spec.Snapshot != nil {
		return filepath.Join(spec.Snapshot.Dir, snapWindowsDisk), spec.Snapshot.Dir
	}
	return filepath.Join(r.opts.WindowsImage, images.WindowsDisk), r.opts.WindowsImage
}

// checkWindowsSpec rejects a Windows sandbox that this runtime cannot run.
func (r *Runtime) checkWindowsSpec(spec vmruntime.Spec) error {
	if spec.NetworkMode == ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND {
		return fmt.Errorf("%w: Windows sandboxes run only with network mode NONE in this version", vmruntime.ErrUnsupported)
	}
	disk, metaDir := r.windowsSource(spec)
	if !fileExists(disk) {
		if spec.Snapshot != nil {
			return fmt.Errorf("%w: the snapshot has no Windows disk", vmruntime.ErrUnsupported)
		}
		return fmt.Errorf("%w: the Windows base image is not built", vmruntime.ErrUnsupported)
	}
	m, err := readWindowsImage(metaDir)
	if err != nil {
		return fmt.Errorf("%w: the Windows image metadata is unreadable: %v", vmruntime.ErrUnsupported, err)
	}
	return checkWindowsResources(spec.Resources, m)
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

// prepareWindowsDisk copies the Windows disk into the VM directory and
// grows it to the requested size; the guest helper extends C: at boot. A
// request below the virtual size of the copy (the source disk, also of a
// snapshot) fails as unsupported before any compute system exists, and the
// create removes the VM directory. The copy is a full, self-contained VHDX (no differencing parent), so a
// snapshot never depends on the base image, which a rebuild replaces.
func (r *Runtime) prepareWindowsDisk(spec vmruntime.Spec) (vmSpec, int64, error) {
	src, metaDir := r.windowsSource(spec)
	disk := filepath.Join(spec.Dir, windowsDiskFile)
	start := time.Now()
	if err := copyFile(src, disk); err != nil {
		return vmSpec{}, 0, fmt.Errorf("hcsrt: prepare disk: %w", err)
	}
	copied := time.Now()
	size, err := growVHDX(disk, spec.Resources.GetDiskGib()<<30)
	if err != nil {
		return vmSpec{}, 0, fmt.Errorf("hcsrt: prepare disk: %w", err)
	}
	if err := copyFile(filepath.Join(metaDir, images.MetadataFile), filepath.Join(spec.Dir, images.MetadataFile)); err != nil {
		return vmSpec{}, 0, fmt.Errorf("hcsrt: copy the image metadata: %w", err)
	}
	if err := grantVMGroup(disk, accessReadWrite, false); err != nil {
		return vmSpec{}, 0, err
	}
	var fileBytes int64
	if st, err := os.Stat(disk); err == nil {
		fileBytes = st.Size()
	}
	r.logger.Info("Windows disk prepared", "sandbox_id", spec.SandboxID, "virtual_bytes", size, "file_bytes", fileBytes,
		"copy_ms", copied.Sub(start).Milliseconds(), "resize_ms", time.Since(copied).Milliseconds())
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
