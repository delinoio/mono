// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo && arm64

package imagebuild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Code-Hex/vz/v3"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/gpt"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestsync"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sysinfo"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/vzrt"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Defaults and limits of the macOS image build.
const (
	macOSDefaultDiskGiB   = 64
	macOSDefaultMemoryMiB = 8192
	macOSDefaultCPUs      = 4
	// macOSFirstBootTimeout bounds the first boot of the installed guest
	// until its guest helper answers; the first boot finishes the
	// installation and is much slower than later boots.
	macOSFirstBootTimeout = 30 * time.Minute
	macOSShutdownTimeout  = 5 * time.Minute
	// buildSandboxID names the build VM in guest helper requests; it is a
	// fixed UUID v7 that no real sandbox uses.
	buildSandboxID = "00000000-0000-7000-8000-000000000000"
)

// buildMacOS installs macOS from an Apple restore image into a new disk,
// installs the guest helper as a launchd daemon, boots the guest once until
// the helper answers and installs the image.
func buildMacOS(ctx context.Context, dir string, opts Options) (err error) {
	logger := opts.Logger.With("guest_os", "macos")
	if os.Geteuid() != 0 {
		// The guest helper files on the guest disk must be owned by root,
		// which only root can write.
		return errors.New("imagebuild: the macOS image build needs root; run it with sudo")
	}
	build := images.BuildDir(dir)
	if err := os.RemoveAll(build); err != nil {
		return err
	}
	if err := os.MkdirAll(build, 0o700); err != nil {
		return err
	}
	defer func() {
		if err == nil {
			return
		}
		if errors.Is(err, vzrt.ErrVMMayRun) {
			// The first-boot VM may still use the staged disk, so deleting
			// it could corrupt a running guest; keep it. The next build
			// removes the directory at its start.
			logger.Error("keeping the macOS build directory because the first-boot VM may still run", "build_dir", build)
			return
		}
		os.RemoveAll(build)
	}()

	ipsw := opts.IPSW
	if ipsw == "" {
		ipsw = filepath.Join(build, "restore.ipsw")
		if err := downloadRestoreImage(ctx, ipsw, logger); err != nil {
			return err
		}
		defer os.Remove(ipsw)
	}
	restore, err := vz.LoadMacOSRestoreImageFromPath(ipsw)
	if err != nil {
		return fmt.Errorf("imagebuild: load the restore image: %w", err)
	}
	req := restore.MostFeaturefulSupportedConfiguration()
	if req == nil {
		return fmt.Errorf("%w: this Mac cannot run the macOS version of the restore image", ErrUnsupported)
	}
	model := req.HardwareModel()
	if !model.Supported() {
		return fmt.Errorf("%w: this Mac does not support the hardware model of the restore image", ErrUnsupported)
	}
	minCPUs := int32(req.MinimumSupportedCPUCount())
	minMemMiB := int64(req.MinimumSupportedMemorySize() >> 20)
	cpus := opts.CPUCount
	if cpus == 0 {
		cpus = min(max(macOSDefaultCPUs, minCPUs), int32(runtime.NumCPU()))
	}
	memMiB := opts.MemoryMiB
	if memMiB == 0 {
		memMiB = min(max(macOSDefaultMemoryMiB, minMemMiB), sysinfo.MemoryMiB()/2)
	}
	if cpus < minCPUs || memMiB < minMemMiB {
		return fmt.Errorf("imagebuild: the restore image needs at least %d vCPUs and %d MiB of memory", minCPUs, minMemMiB)
	}
	diskGiB := opts.DiskGiB
	if diskGiB == 0 {
		diskGiB = macOSDefaultDiskGiB
	}
	source := fmt.Sprintf("macOS %s (%s)", restore.OperatingSystemVersion(), restore.BuildVersion())
	logger.Info("macOS image build starting", "source", source, "cpus", cpus, "memory_mib", memMiB, "disk_gib", diskGiB,
		"minimum_cpus", minCPUs, "minimum_memory_mib", minMemMiB)

	disk := filepath.Join(build, images.MacOSDisk)
	aux := filepath.Join(build, images.MacOSAuxStorage)
	f, err := os.OpenFile(disk, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Truncate(diskGiB << 30); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if _, err := vz.NewMacAuxiliaryStorage(aux, vz.WithCreatingMacAuxiliaryStorage(model)); err != nil {
		return fmt.Errorf("imagebuild: create the auxiliary storage: %w", err)
	}
	if err := os.WriteFile(filepath.Join(build, images.MacOSHardwareModel), model.DataRepresentation(), 0o600); err != nil {
		return err
	}
	machine := vzrt.MacOSMachine{Disk: disk, AuxStorage: aux, HardwareModel: model.DataRepresentation(), CPUs: cpus, MemoryMiB: memMiB}

	if err := installMacOS(ctx, machine, ipsw, logger); err != nil {
		return err
	}
	// The installer puts an APFS recovery container behind the system
	// container. Remove it, so the guest can grow the system container to
	// the end of a sandbox disk. Without it, recoveryOS and macOS updates in
	// the guest do not work; sandboxes use neither.
	removed, err := gpt.RemoveTrailing(disk, gpt.AppleAPFSRecovery)
	if err != nil {
		return fmt.Errorf("imagebuild: remove the recovery container: %w", err)
	}
	logger.Info("recovery container removed from the guest disk", "partitions", removed)
	if err := provisionMacOSDisk(ctx, build, disk, opts.GuestBinary, logger); err != nil {
		return err
	}
	var downloadedIPSW string
	if opts.IPSW == "" {
		downloadedIPSW = ipsw
	}
	metadata := images.Metadata{
		GuestOS:          ownsandv1.GuestOs_GUEST_OS_MACOS,
		Source:           source,
		DiskBytes:        diskGiB << 30,
		MinimumCPUCount:  minCPUs,
		MinimumMemoryMiB: minMemMiB,
	}
	if err := finishMacOSImage(dir, metadata, downloadedIPSW, func() (string, error) {
		return firstBootMacOS(ctx, machine, logger)
	}); err != nil {
		return err
	}
	logger.Info("macOS image installed", "source", source)
	return nil
}

func finishMacOSImage(dir string, metadata images.Metadata, downloadedIPSW string, firstBoot func() (string, error)) error {
	helper, err := firstBoot()
	if err != nil {
		return err
	}
	metadata.BuiltAt = time.Now().UTC()
	metadata.GuestVersion = helper
	if err := images.WriteMetadata(images.BuildDir(dir), metadata); err != nil {
		return err
	}
	if downloadedIPSW != "" {
		os.Remove(downloadedIPSW)
	}
	return images.Install(dir)
}

// downloadRestoreImage downloads the latest restore image that this Mac
// supports from Apple.
func downloadRestoreImage(ctx context.Context, path string, logger *slog.Logger) error {
	url, err := vz.GetLatestSupportedMacOSRestoreImageURL()
	if err != nil {
		return fmt.Errorf("imagebuild: look up the latest restore image: %w", err)
	}
	logger.Info("downloading the macOS restore image from Apple")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("imagebuild: download the restore image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("imagebuild: download the restore image: HTTP %d", resp.StatusCode)
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	pr := &progressReader{r: resp.Body, total: resp.ContentLength, logger: logger, what: "restore image download"}
	if _, err := io.Copy(out, pr); err != nil {
		out.Close()
		return fmt.Errorf("imagebuild: download the restore image: %w", err)
	}
	return out.Close()
}

// progressReader logs the progress of a long copy every 30 seconds.
type progressReader struct {
	r      io.Reader
	total  int64
	done   int64
	last   time.Time
	logger *slog.Logger
	what   string
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if time.Since(p.last) > 30*time.Second {
		p.last = time.Now()
		p.logger.Info(p.what, "bytes", p.done, "total_bytes", p.total)
	}
	return n, err
}

// installMacOS runs the Virtualization.framework installer.
func installMacOS(ctx context.Context, m vzrt.MacOSMachine, ipsw string, logger *slog.Logger) error {
	cfg, _, err := vzrt.NewMacOSConfiguration(m)
	if err != nil {
		return err
	}
	machine, err := vz.NewVirtualMachine(cfg)
	if err != nil {
		return fmt.Errorf("imagebuild: create the install VM: %w", err)
	}
	installer, err := vz.NewMacOSInstaller(machine, ipsw)
	if err != nil {
		return fmt.Errorf("imagebuild: create the installer: %w", err)
	}
	start := time.Now()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				logger.Info("macOS installation", "fraction_completed", installer.FractionCompleted())
			}
		}
	}()
	logger.Info("installing macOS")
	if err := installer.Install(ctx); err != nil {
		return fmt.Errorf("imagebuild: install macOS: %w", err)
	}
	if machine.CanStop() {
		_ = machine.Stop()
	}
	logger.Info("macOS installed", "install_ms", time.Since(start).Milliseconds())
	return nil
}

// firstBootMacOS boots the provisioned guest until its guest helper answers,
// provisions it through the helper, shuts it down and returns the helper
// version. When the first boot fails, it stops the VM within a bounded time
// and reports the stop failure together with the original error; the error
// wraps vzrt.ErrVMMayRun when the VM may still run.
func firstBootMacOS(ctx context.Context, m vzrt.MacOSMachine, logger *slog.Logger) (version string, err error) {
	// The guest creates the network service of its interface during this
	// boot; the interface has the MAC address of every macOS sandbox, so the
	// sandboxes find the service. The link carries no traffic.
	nic, closeNIC, err := vzrt.NewIdleNetworkDevice(vzrt.MacOSGuestMAC)
	if err != nil {
		return "", err
	}
	defer closeNIC.Close()
	m.Network = nic
	cfg, _, err := vzrt.NewMacOSConfiguration(m)
	if err != nil {
		return "", err
	}
	machine, err := vz.NewVirtualMachine(cfg)
	if err != nil {
		return "", fmt.Errorf("imagebuild: create the first-boot VM: %w", err)
	}
	exit := vzrt.WatchVMExit(machine.StateChangedNotify())
	start := time.Now()
	if err := machine.Start(); err != nil {
		return "", fmt.Errorf("imagebuild: start the first boot: %w", err)
	}
	defer func() {
		if err != nil {
			err = vzrt.StopVMAfterFailure(err, machine, exit.Done())
		}
	}()
	dial := func(ctx context.Context) (net.Conn, error) {
		return vzrt.DialGuest(ctx, machine, vzrt.GuestPort, exit.Done())
	}
	return provisionAndShutdownMacOS(ctx, dial, exit, logger, start, macOSShutdownTimeout)
}

func provisionAndShutdownMacOS(ctx context.Context, dial guestclient.Dialer, exit *vzrt.VMExit, logger *slog.Logger, start time.Time, shutdownTimeout time.Duration) (string, error) {
	bootCtx, cancel := context.WithTimeout(ctx, macOSFirstBootTimeout)
	defer cancel()
	client := guestclient.New(dial)
	defer client.Close()
	ready := make(chan error, 1)
	var helper string
	go func() {
		resp, err := client.WaitReady(bootCtx, 2*time.Second)
		if err == nil {
			helper = resp.GetHelperVersion()
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			return "", fmt.Errorf("imagebuild: the guest helper did not answer after the first boot: %w", err)
		}
	case <-exit.Done():
		return "", errors.New("imagebuild: the guest stopped during its first boot")
	}
	logger.Info("macOS guest helper answered", "first_boot_ms", time.Since(start).Milliseconds(), "helper_version", helper)
	for _, cmd := range macOSProvisionCommands {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		code, err := guestsync.Run(cctx, buildSandboxID, dial, cmd)
		cancel()
		if err != nil || code != 0 {
			return "", fmt.Errorf("imagebuild: provisioning command %q failed (exit code %d): %v", cmd[0], code, err)
		}
	}
	logger.Info("macOS guest provisioned; shutting down")
	// The guest can disconnect before the helper replies. Use the VM state
	// to confirm that shutdown succeeded.
	sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
	_, _ = guestsync.Run(sctx, buildSandboxID, dial, []string{"/sbin/shutdown", "-h", "now"})
	scancel()
	select {
	case <-exit.Done():
		if state := exit.State(); state != vz.VirtualMachineStateStopped {
			return "", fmt.Errorf("imagebuild: the first-boot VM failed during shutdown (%s)", state)
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
	case <-time.After(shutdownTimeout):
		return "", errors.New("imagebuild: the guest did not shut down")
	case <-ctx.Done():
		return "", ctx.Err()
	}
	logger.Info("macOS first boot finished", "total_ms", time.Since(start).Milliseconds())
	return helper, nil
}
