// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

const nativeAttachProxy = `
stage="$1"
shift
/usr/bin/hdiutil "$@" > "$stage/attach.plist" || exit $?
/bin/cat "$stage/attach.plist" || exit $?
: > "$stage/ready"
exec /bin/sleep 60
`

func nativeAttachmentInventory() (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := runHostTool(ctx, hdiutilPath, "info", "-plist")
	if err != nil {
		return nil, err
	}
	plist, err := readDiskImagePlist(out)
	if err != nil {
		return nil, err
	}
	devices := make(map[string]string)
	for _, image := range plist.array("images") {
		for _, entity := range image.dict.array("system-entities") {
			dev := entity.dict.text("dev-entry")
			if wholeDiskEntryPattern.MatchString(dev) {
				devices[dev] = image.dict.text("image-path")
			}
		}
	}
	return devices, nil
}

func nativeDeviceForImage(devices map[string]string, disk string) string {
	for dev, image := range devices {
		if image == disk {
			return dev
		}
	}
	return ""
}

func TestNativeInterruptedAttach(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(dir, "staged disk.img")
	control := filepath.Join(dir, "control disk.img")
	for _, path := range []string{disk, control} {
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		err = f.Truncate(16 << 20)
		if closeErr := f.Close(); err != nil || closeErr != nil {
			t.Fatal(errors.Join(err, closeErr))
		}
		t.Cleanup(func() {
			inventory, err := nativeAttachmentInventory()
			if err != nil {
				t.Errorf("inspect disposable images during cleanup: %v", err)
				return
			}
			whole := nativeDeviceForImage(inventory, path)
			if whole == "" {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := runHostTool(ctx, hdiutilPath, "detach", whole); err != nil {
				t.Errorf("detach disposable test image %s: %v", filepath.Base(path), err)
			}
		})
	}

	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	_, err = runHostTool(ctx, hdiutilPath, "attach", "-imagekey", "diskimage-class=CRawDiskImage", "-nomount", "-plist", control)
	if err != nil {
		t.Fatalf("attach the unrelated control image: %v", err)
	}
	before, err := nativeAttachmentInventory()
	if err != nil {
		t.Fatal(err)
	}
	controlDevice := nativeDeviceForImage(before, control)
	if controlDevice == "" || nativeDeviceForImage(before, disk) != "" {
		t.Fatal("inventory before the test must include the control image only among the disposable images")
	}
	t.Logf("before attach: %d whole-disk attachments, control present, staged image absent", len(before))

	type observation struct {
		devices map[string]string
		err     error
	}
	observed := make(chan observation, 1)
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		defer cancel()
		for {
			if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
				inventory, err := nativeAttachmentInventory()
				observed <- observation{devices: inventory, err: err}
				return
			} else if !errors.Is(err, os.ErrNotExist) {
				observed <- observation{err: err}
				return
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				observed <- observation{err: fmt.Errorf("wait for the real attach process to report a device: %w", ctx.Err())}
				return
			}
		}
	}()

	var detaches [][]string
	run := func(toolCtx context.Context, name string, args ...string) ([]byte, error) {
		if args[0] == "attach" {
			proxy := []string{"-c", nativeAttachProxy, "attach-proxy", dir}
			return runHostTool(toolCtx, "/bin/sh", append(proxy, args...)...)
		}
		if args[0] == "detach" {
			detaches = append(detaches, args)
		}
		return runHostTool(toolCtx, name, args...)
	}
	err = withAttachedDisk(ctx, run, disk, discardLogger(), func(string) error {
		t.Error("provisioning ran after the interrupted attach")
		return nil
	})
	cancel()
	during := <-observed
	if during.err != nil {
		t.Fatalf("%v (attach result: %v)", during.err, err)
	}
	device := nativeDeviceForImage(during.devices, disk)
	if device == "" || during.devices[controlDevice] != control {
		t.Fatal("inventory at cancellation must include both disposable images")
	}
	t.Logf("at cancellation: %d whole-disk attachments, staged and control images present", len(during.devices))

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("expected the attach process's SIGKILL error, got %v", err)
	}
	if len(detaches) != 1 || len(detaches[0]) != 2 || detaches[0][1] != device {
		t.Fatalf("expected a single normal detach of the staged disk, got %v", detaches)
	}
	after, err := nativeAttachmentInventory()
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(before, after) {
		t.Fatal("inventory after recovery differs from the inventory before attach")
	}
	t.Logf("after recovery: %d whole-disk attachments, staged image absent and all other device associations preserved", len(after))
}
