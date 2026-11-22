// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestsync"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/hcsrt"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Defaults and limits of the Windows image build.
const (
	defaultWindowsDiskGiB   = 32
	minimumWindowsDiskGiB   = 16
	defaultWindowsCPUs      = 2
	defaultWindowsMemoryMiB = 4096
	// windowsSetupTimeout bounds the first boot: Windows Setup runs the
	// specialize and OOBE passes, reboots and starts the guest helper.
	windowsSetupTimeout = 60 * time.Minute
	setupProgress       = time.Minute
	provisionTimeout    = 5 * time.Minute
	shutdownTimeout     = 10 * time.Minute
	// buildVMID is the compute system ID of the build VM.
	buildVMID = "ownsand-image-build-windows"
)

// provisionCommands run through the guest helper once setup finished.
// Sandboxes have no network, so Windows Update only wastes the guest's
// CPU; a sandbox must not sleep or hibernate while it runs.
var provisionCommands = [][]string{
	{`C:\Windows\System32\reg.exe`, "add", `HKLM\SOFTWARE\Policies\Microsoft\Windows\WindowsUpdate\AU`, "/v", "NoAutoUpdate", "/t", "REG_DWORD", "/d", "1", "/f"},
	{`C:\Windows\System32\powercfg.exe`, "/hibernate", "off"},
	{`C:\Windows\System32\powercfg.exe`, "/change", "standby-timeout-ac", "0"},
	{`C:\Windows\System32\powercfg.exe`, "/change", "monitor-timeout-ac", "0"},
	// SetupComplete.cmd deleted the answer file; this repeats it in case
	// Setup kept a copy.
	{`C:\Windows\System32\cmd.exe`, "/c", `del /f /q C:\Windows\Panther\unattend.xml C:\Windows\Panther\Unattend\unattend.xml 2>nul & exit /b 0`},
}

// shutdownCommand shuts the guest down at once; the build then waits for
// the compute system to exit.
var shutdownCommand = []string{`C:\Windows\System32\shutdown.exe`, "/s", "/t", "0", "/f"}

// buildWindows builds the Windows base image from opts.ISO: it applies the
// selected image of the ISO to a new VHDX offline (no Windows Setup boot
// from the ISO, which waits for a key press on Hyper-V UEFI), adds the
// boot files, the answer file and the guest helper, boots the disk once
// until setup finished and the guest helper answers, provisions and shuts
// the guest down, and installs the image directory.
func buildWindows(ctx context.Context, dir string, opts Options) (err error) {
	logger := opts.Logger.With("guest_os", ownsandv1.GuestOs_GUEST_OS_WINDOWS.String())
	start := time.Now()
	if err := hcsrt.CheckHost(); err != nil {
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	iso, err := filepath.Abs(opts.ISO)
	if err != nil {
		return err
	}
	if st, err := os.Stat(iso); err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("imagebuild: the ISO file is not readable: %v", err)
	}
	diskGiB := opts.DiskGiB
	if diskGiB == 0 {
		diskGiB = defaultWindowsDiskGiB
	}
	if diskGiB < minimumWindowsDiskGiB {
		return fmt.Errorf("imagebuild: --disk-gib must be at least %d for a Windows image", minimumWindowsDiskGiB)
	}
	build := images.BuildDir(dir)
	if err := os.RemoveAll(build); err != nil {
		return fmt.Errorf("imagebuild: remove the earlier build directory: %w", err)
	}
	if err := os.MkdirAll(build, 0o700); err != nil {
		return err
	}
	// The build directory stays after a failure for inspection (its disk
	// holds the Windows Setup logs in Windows\Panther); the next build
	// removes it.
	defer func() {
		if err != nil {
			logger.Error("Windows image build failed; the build directory is kept until the next build", "dir", build, "error", err)
		}
	}()

	logger.Info("mounting the ISO")
	m, err := mountISO(ctx, iso)
	if err != nil {
		return err
	}
	// The deferred dismount is the retry of a failed explicit dismount below:
	// dismount keeps m.ours set until the detach succeeded.
	defer m.release(context.Background(), logger)
	wim, err := m.installImage()
	if err != nil {
		return err
	}
	list, err := listImages(ctx, wim)
	if err != nil {
		return err
	}
	chosen, err := selectEdition(list, opts.Edition)
	if err != nil {
		return err
	}
	details, err := imageDetails(ctx, wim, chosen.Index)
	if err != nil {
		return err
	}
	if details.Architecture != archAMD64 {
		return fmt.Errorf("%w: the image %q is not an x64 image (architecture %d)", ErrUnsupported, details.Name, details.Architecture)
	}
	minCPUs, minMemory := guestMinimums(details.InstallationType)
	cpus := opts.CPUCount
	if cpus == 0 {
		cpus = max(defaultWindowsCPUs, minCPUs)
		cpus = min(cpus, int32(runtime.NumCPU()))
	}
	memory := opts.MemoryMiB
	if memory == 0 {
		memory = max(defaultWindowsMemoryMiB, minMemory)
	}
	if cpus < minCPUs || memory < minMemory {
		return fmt.Errorf("imagebuild: %s needs at least %d vCPUs and %d MiB of memory", details.Name, minCPUs, minMemory)
	}
	lang := details.language()
	logger.Info("selected the Windows image", "index", chosen.Index, "name", details.Name, "installation_type", details.InstallationType,
		"version", details.Version, "language", lang, "disk_gib", diskGiB, "vcpus", cpus, "memory_mib", memory)

	disk := filepath.Join(build, images.WindowsDisk)
	if err := createVHDX(disk, diskGiB<<30); err != nil {
		return err
	}
	if err := applyImage(ctx, logger, disk, build, wim, chosen.Index, lang, opts.GuestBinary); err != nil {
		return err
	}
	m.release(ctx, logger)
	applied := time.Now()

	logger.Info("booting the image once; Windows Setup finishes the specialize and OOBE passes")
	vm, err := hcsrt.StartBuildVM(hcsrt.BuildVMOptions{ID: buildVMID, Disk: disk, Dir: build, VCPUs: cpus, MemoryMiB: memory, Logger: logger})
	if err != nil {
		return err
	}
	defer vm.Close()
	helperVersion, err := vm.WaitReady(ctx, windowsSetupTimeout, setupProgress)
	if err != nil {
		return err
	}
	ready := time.Now()
	logger.Info("Windows setup finished; the guest helper answers", "helper_version", helperVersion,
		"first_boot_ms", ready.Sub(applied).Milliseconds())
	for _, c := range provisionCommands {
		if err := runGuest(ctx, vm, c); err != nil {
			return err
		}
	}
	logger.Info("shutting the guest down")
	if _, err := runGuestCode(ctx, vm, shutdownCommand); err != nil {
		// The guest can end before the exec reports its exit.
		logger.Debug("shutdown exec ended without a result", "error", err)
	}
	if err := vm.WaitExit(ctx, shutdownTimeout); err != nil {
		return err
	}
	vm.Close()
	size, err := hcsrt.VHDXSize(disk)
	if err != nil {
		return err
	}
	var fileBytes int64
	if st, err := os.Stat(disk); err == nil {
		fileBytes = st.Size()
	}
	_ = os.Remove(filepath.Join(build, "console.log"))
	meta := images.Metadata{
		GuestOS:          ownsandv1.GuestOs_GUEST_OS_WINDOWS,
		Source:           fmt.Sprintf("%s %s (index %d)", details.Name, details.Version, chosen.Index),
		BuiltAt:          time.Now().UTC(),
		DiskBytes:        size,
		MinimumCPUCount:  minCPUs,
		MinimumMemoryMiB: minMemory,
		GuestVersion:     helperVersion,
	}
	if err := images.WriteMetadata(build, meta); err != nil {
		return err
	}
	if err := images.Install(dir); err != nil {
		return fmt.Errorf("imagebuild: install the image: %w", err)
	}
	logger.Info("Windows image installed", "source", meta.Source, "disk_bytes", size, "file_bytes", fileBytes,
		"apply_ms", applied.Sub(start).Milliseconds(), "first_boot_ms", ready.Sub(applied).Milliseconds(),
		"total_ms", time.Since(start).Milliseconds())
	return nil
}

// applyImage attaches the new disk, partitions it, applies the image,
// writes the boot files, the answer file, SetupComplete.cmd and the guest
// helper, and detaches the disk again.
func applyImage(ctx context.Context, logger *slog.Logger, disk, scratch, wim string, index int, lang, guestBinary string) (err error) {
	d, err := attachVHDX(disk)
	if err != nil {
		return err
	}
	defer func() {
		if derr := d.detach(); derr != nil {
			logger.Warn("detach the image disk failed", "error", derr)
			if err == nil {
				err = derr
			}
		}
	}()
	logger.Info("partitioning the image disk", "disk_number", d.number)
	if err := d.partition(ctx, scratch); err != nil {
		return err
	}
	logger.Info("applying the Windows image; this takes several minutes", "index", index)
	t := time.Now()
	if out, err := runTool(ctx, "dism.exe", "/Apply-Image", "/ImageFile:"+wim, "/Index:"+strconv.Itoa(index), `/ApplyDir:`+d.Windows+`\`); err != nil {
		return fmt.Errorf("imagebuild: dism /Apply-Image failed: %w: %s", err, out)
	}
	logger.Info("Windows image applied", "duration_ms", time.Since(t).Milliseconds())
	win := d.Windows + `\Windows`
	if out, err := runTool(ctx, "bcdboot.exe", win, "/s", d.System, "/f", "UEFI"); err != nil {
		return fmt.Errorf("imagebuild: bcdboot failed: %w: %s", err, out)
	}
	logger.Info("boot files written")
	password, err := randomPassword()
	if err != nil {
		return err
	}
	answer, err := unattendXML(unattendSettings{Language: lang, Password: password})
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(win, "Panther", "unattend.xml"), answer); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(win, "Setup", "Scripts", "SetupComplete.cmd"), []byte(setupCompleteCmd())); err != nil {
		return err
	}
	guest, err := os.ReadFile(guestBinary)
	if err != nil {
		return fmt.Errorf("imagebuild: read the guest helper: %w", err)
	}
	// hcsrt.WindowsGuestBinary below the Windows partition root.
	if err := writeFile(filepath.Join(d.Windows+`\`, hcsrt.WindowsGuestBinary[len(`C:\`):]), guest); err != nil {
		return err
	}
	logger.Info("answer file, SetupComplete.cmd and guest helper installed", "guest_binary_bytes", len(guest))
	return nil
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// runGuest runs a provisioning command and requires exit code 0.
func runGuest(ctx context.Context, vm *hcsrt.BuildVM, command []string) error {
	code, err := runGuestCode(ctx, vm, command)
	if err != nil {
		return fmt.Errorf("imagebuild: guest command %s: %w", filepath.Base(command[0]), err)
	}
	if code != 0 {
		return fmt.Errorf("imagebuild: guest command %s exited with %d", filepath.Base(command[0]), code)
	}
	return nil
}

func runGuestCode(ctx context.Context, vm *hcsrt.BuildVM, command []string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()
	code, err := guestsync.Run(ctx, buildVMID, vm.Dial, command)
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		return 0, fmt.Errorf("no result within %v", provisionTimeout)
	}
	return code, err
}
