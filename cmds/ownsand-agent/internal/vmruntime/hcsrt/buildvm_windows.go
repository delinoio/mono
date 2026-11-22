// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
)

// BuildOwner is the HCS owner of the image build VM. It differs from Owner,
// so a running agent never takes the build VM for a sandbox VM of an
// earlier agent process and stops it.
const BuildOwner = "ownsand-agent-image-build"

// BuildVMOptions configure the VM that boots a new Windows image once.
type BuildVMOptions struct {
	// ID is the compute system ID.
	ID string
	// Disk is the VHDX of the new image.
	Disk string
	// Dir receives console.log (the COM1 output: the UEFI console).
	Dir       string
	VCPUs     int32
	MemoryMiB int64
	Logger    *slog.Logger
}

// BuildVM is a running image build VM. It boots the disk with UEFI and
// keeps running across guest reboots (Windows Setup reboots between its
// passes). The VM ends when its handle closes, also when the build process
// ends.
type BuildVM struct {
	sys       *system
	runtimeID guid.GUID
	logger    *slog.Logger
	stop      chan struct{}
	console   chan struct{}
	closeOnce sync.Once
}

// CheckHost reports whether this process can create Hyper-V VMs.
func CheckHost() error {
	if err := hcsLoad(); err != nil {
		return fmt.Errorf("the Host Compute Service API (computecore.dll) is not available; enable Hyper-V: %w", err)
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("the process is not elevated; the Host Compute Service and the disk tools need an Administrator")
	}
	return nil
}

// StartBuildVM creates and starts the image build VM.
func StartBuildVM(opts BuildVMOptions) (*BuildVM, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if err := CheckHost(); err != nil {
		return nil, err
	}
	// A compute system left by an interrupted build holds the disk.
	if old, err := openSystem(opts.ID); err == nil {
		opts.Logger.Warn("stopping a compute system of an earlier image build", "id", opts.ID)
		_ = old.terminate()
		select {
		case <-old.exited:
		case <-time.After(stopTimeout):
		}
		old.close()
	}
	if err := grantVMGroup(opts.Dir, accessReadWrite, true); err != nil {
		return nil, err
	}
	if err := grantVMGroup(opts.Disk, accessReadWrite, false); err != nil {
		return nil, err
	}
	pipe := `\\.\pipe\` + opts.ID + `-com1`
	doc := newComputeSystemDoc(vmSpec{Owner: BuildOwner, Windows: true, AllowReset: true, Disk: opts.Disk,
		ConsolePipe: pipe, VCPUs: opts.VCPUs, MemoryMiB: opts.MemoryMiB})
	sys, err := createSystem(opts.ID, doc)
	if err != nil {
		return nil, err
	}
	b := &BuildVM{sys: sys, logger: opts.Logger, stop: make(chan struct{}), console: make(chan struct{})}
	props, err := sys.properties()
	if err == nil {
		b.runtimeID, err = guid.FromString(props.RuntimeID)
	}
	if err != nil {
		sys.close()
		return nil, fmt.Errorf("hcsrt: read the VM runtime ID: %w", err)
	}
	if err := sys.start(); err != nil {
		sys.close()
		return nil, err
	}
	go b.copyConsole(pipe, filepath.Join(opts.Dir, consoleFile))
	return b, nil
}

// Exited is closed when the VM ends.
func (b *BuildVM) Exited() <-chan struct{} { return b.sys.exited }

// Dial connects to the guest helper.
func (b *BuildVM) Dial(ctx context.Context) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	go func() {
		select {
		case <-b.sys.exited:
			cancel()
		case <-ctx.Done():
		}
	}()
	return winio.Dial(ctx, &winio.HvsockAddr{VMID: b.runtimeID, ServiceID: winio.VsockServiceID(GuestPort)})
}

// WaitReady waits until the guest helper answers CheckReady, at most
// timeout, and logs progress every progress interval.
func (b *BuildVM) WaitReady(ctx context.Context, timeout, progress time.Duration) (string, error) {
	start := time.Now()
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := guestclient.New(b.Dial)
	defer client.Close()
	type result struct {
		version string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := client.WaitReady(rctx, time.Second)
		done <- result{resp.GetHelperVersion(), err}
	}()
	tick := time.NewTicker(progress)
	defer tick.Stop()
	for {
		select {
		case r := <-done:
			if r.err != nil {
				return "", fmt.Errorf("hcsrt: the guest helper did not answer within %v: %w", timeout, r.err)
			}
			return r.version, nil
		case <-b.sys.exited:
			return "", errors.New("hcsrt: the build VM stopped before its guest helper answered")
		case <-tick.C:
			b.logger.Info("waiting for Windows setup to finish", "elapsed", time.Since(start).Round(time.Second).String())
		}
	}
}

// WaitExit waits until the guest shut down, at most timeout.
func (b *BuildVM) WaitExit(ctx context.Context, timeout time.Duration) error {
	select {
	case <-b.sys.exited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(timeout):
		return fmt.Errorf("hcsrt: the build VM did not shut down within %v", timeout)
	}
}

// Close terminates the VM, if it still runs, and releases it. It is
// idempotent.
func (b *BuildVM) Close() { b.closeOnce.Do(b.close) }

func (b *BuildVM) close() {
	select {
	case <-b.sys.exited:
	default:
		_ = b.sys.terminate()
		select {
		case <-b.sys.exited:
		case <-time.After(stopTimeout):
		}
	}
	close(b.stop)
	b.sys.close()
	b.sys.markExited()
	select {
	case <-b.console:
	case <-time.After(stopTimeout):
	}
}

// copyConsole copies the COM1 output of the build VM to path.
func (b *BuildVM) copyConsole(pipe, path string) {
	defer close(b.console)
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		b.logger.Warn("open console log failed", "error", err)
		return
	}
	defer out.Close()
	dial := func() (net.Conn, error) {
		timeout := time.Second
		return winio.DialPipe(pipe, &timeout)
	}
	// The shared loop ends each closer goroutine with its copy, so a console
	// pipe that ends while the VM runs leaves no goroutine behind.
	copyConsoleLoop(dial, out, b.stop, b.sys.exited)
}
