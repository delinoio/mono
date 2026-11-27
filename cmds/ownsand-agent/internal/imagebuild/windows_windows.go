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
	"strings"
	"time"
	"unsafe"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestsync"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/hcsrt"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"golang.org/x/sys/windows"
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

// buildGuestFrameID labels provisioning requests on the VM-specific connection.
const buildGuestFrameID = "ownsand-image-build-windows"

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
	defer m.finish(logger)
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

	id, err := uuidv7.NewString()
	if err != nil {
		return fmt.Errorf("imagebuild: generate the Windows build VM ID: %w", err)
	}
	logger.Info("booting the image once; Windows Setup finishes the specialize and OOBE passes")
	vm, err := hcsrt.StartBuildVM(hcsrt.BuildVMOptions{ID: "ownsand-image-build-windows-" + id, Disk: disk, Dir: build, VCPUs: cpus, MemoryMiB: memory, Logger: logger})
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
	if err := images.RemoveWindowsVMAccess(build); err != nil {
		return fmt.Errorf("imagebuild: clear the build VM access entries: %w", err)
	}
	if err := images.Install(ctx, dir); err != nil {
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
	return installSetupFiles(logger, d.Windows+`\`, guestBinary, lang, randomPassword)
}

// installSetupFiles writes the answer file, SetupComplete.cmd and the guest
// helper below the applied Windows partition root. newPassword supplies the
// administrator password for the answer file; the build passes
// randomPassword. It never logs the password. It needs no Windows image,
// elevation or VM, so a test can run it against a temporary directory.
func installSetupFiles(logger *slog.Logger, root, guestBinary, lang string, newPassword func() (string, error)) error {
	password, err := newPassword()
	if err != nil {
		return err
	}
	answer, err := unattendXML(unattendSettings{Language: lang, Password: password})
	if err != nil {
		return err
	}
	image, err := openSetupRoot(root)
	if err != nil {
		return fmt.Errorf("imagebuild: open the applied image root: %w", err)
	}
	defer windows.CloseHandle(image)

	write := func(path string, data []byte) error {
		err := writeSetupFile(image, path, data)
		if err != nil {
			logger.Error("install a Windows setup file failed", "relative_path", path, "reason", err)
		}
		return err
	}
	if err := write(`Windows\Panther\unattend.xml`, answer); err != nil {
		return err
	}
	if err := write(`Windows\Setup\Scripts\SetupComplete.cmd`, []byte(setupCompleteCmd())); err != nil {
		return err
	}
	guest, err := os.ReadFile(guestBinary)
	if err != nil {
		return fmt.Errorf("imagebuild: read the guest helper: %w", err)
	}
	// hcsrt.WindowsGuestBinary below the Windows partition root.
	if err := write(hcsrt.WindowsGuestBinary[len(`C:\`):], guest); err != nil {
		return err
	}
	logger.Info("answer file, SetupComplete.cmd and guest helper installed", "guest_binary_bytes", len(guest))
	return nil
}

const setupDirectoryAccess = windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES

func openSetupRoot(path string) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	handle, err := windows.CreateFile(name, setupDirectoryAccess, windows.FILE_SHARE_READ,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, err
	}
	if err := checkSetupEntry(handle, true); err != nil {
		windows.CloseHandle(handle)
		return 0, err
	}
	return handle, nil
}

func writeSetupFile(root windows.Handle, path string, data []byte) (err error) {
	if !filepath.IsLocal(path) || filepath.Clean(path) == "." {
		return fmt.Errorf("imagebuild: invalid relative setup path %q", path)
	}
	parts := strings.Split(filepath.Clean(path), `\`)
	parent := root
	for i, name := range parts[:len(parts)-1] {
		dir, err := openSetupEntry(parent, name, true)
		if err != nil {
			return fmt.Errorf("imagebuild: prepare setup directory %q: %w", filepath.Join(parts[:i+1]...), err)
		}
		defer windows.CloseHandle(dir)
		parent = dir
	}
	handle, err := openSetupEntry(parent, parts[len(parts)-1], false)
	if err != nil {
		return fmt.Errorf("imagebuild: open setup file %q: %w", path, err)
	}
	f := os.NewFile(uintptr(handle), path)
	defer func() {
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	}()
	// Truncate only after checking the file handle for reparse attributes.
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

// Resolve each name against a checked directory handle. The caller holds
// the ancestor handles without write or delete sharing until writing is complete.
// FILE_OPEN_REPARSE_POINT applies to the single component opened here.
func openSetupEntry(parent windows.Handle, name string, directory bool) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	attrs := windows.OBJECT_ATTRIBUTES{
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	access := uint32(windows.FILE_GENERIC_WRITE | windows.FILE_READ_ATTRIBUTES)
	options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if directory {
		access = setupDirectoryAccess | windows.SYNCHRONIZE
		options |= windows.FILE_DIRECTORY_FILE
	}
	var handle windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	if err := windows.NtCreateFile(&handle, access, &attrs, &iosb, nil, windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ, windows.FILE_OPEN_IF, options, 0, 0); err != nil {
		return 0, err
	}
	if err := checkSetupEntry(handle, directory); err != nil {
		windows.CloseHandle(handle)
		return 0, err
	}
	return handle, nil
}

func checkSetupEntry(handle windows.Handle, directory bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("reparse point")
	}
	if directory && info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return errors.New("expected a directory")
	}
	if !directory && info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return errors.New("expected a regular file")
	}
	return nil
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
	code, err := guestsync.Run(ctx, buildGuestFrameID, vm.Dial, command)
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		return 0, fmt.Errorf("no result within %v", provisionTimeout)
	}
	return code, err
}
