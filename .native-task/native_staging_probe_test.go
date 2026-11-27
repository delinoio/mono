//go:build darwin && arm64

package imagebuild

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
)

func TestMacOSNativeStagingProbe(t *testing.T) {
	ctx := t.Context()
	work, err := os.MkdirTemp("", "ownsand-native-staging-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("disposable work directory: %s", work)
	build := filepath.Join(work, "macos.build")
	disk := filepath.Join(build, images.MacOSDisk)
	mnt := filepath.Join(build, "mnt")
	sentinel := filepath.Join(mnt, "guest-sentinel")
	const sentinelText = "ownsand guest disk recovery probe"
	var device string
	var unknownAttachment bool
	t.Cleanup(func() {
		if unknownAttachment {
			t.Errorf("keep %s: attach result named no verified device", work)
			return
		}
		if device != "" {
			if _, err := runHostTool(context.Background(), hdiutilPath, "detach", device); err != nil {
				t.Errorf("keep %s: could not detach %s: %v", work, device, err)
				return
			}
		}
		if err := removeMacOSBuild(context.Background(), runHostTool, macOSMountPoints, build); err != nil {
			t.Error(err)
			return
		}
		if err := os.RemoveAll(work); err != nil {
			t.Error(err)
		}
	})
	if err := os.MkdirAll(mnt, 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) []byte {
		t.Helper()
		out, err := runHostTool(ctx, hdiutilPath, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	create := func(path string) {
		t.Helper()
		run("create", "-size", "256m", "-fs", "HFS+", "-layout", "GPTSPUD", "-volname", "OwnSandProbe", "-type", "UDIF", path)
	}
	create(disk + ".dmg")
	if err := os.Rename(disk+".dmg", disk); err != nil {
		t.Fatal(err)
	}
	attach := func(path string, flags ...string) ([]byte, error) {
		args := append([]string{"attach", "-plist", "-nobrowse"}, flags...)
		unknownAttachment = true
		out, err := runHostTool(ctx, hdiutilPath, append(args, path)...)
		if err != nil {
			return nil, err
		}
		whole, guid := attachedWholeDisk(out)
		if whole == "" || !guid {
			return nil, errors.New("disposable disk attach did not report a GUID whole disk")
		}
		device = "/dev/" + whole
		unknownAttachment = false
		return out, nil
	}
	detach := func() {
		t.Helper()
		run("detach", device)
		device = ""
	}
	requireRefusal := func(text string) {
		t.Helper()
		err := removeMacOSBuild(ctx, runHostTool, macOSMountPoints, build)
		if err == nil || !strings.Contains(err.Error(), text) || !strings.Contains(err.Error(), build) {
			t.Fatalf("cleanup error = %v, want %q and the staging path", err, text)
		}
	}
	assertSentinel := func() {
		t.Helper()
		if text, err := os.ReadFile(sentinel); err != nil || string(text) != sentinelText {
			t.Fatalf("mounted sentinel = %q, %v", text, err)
		}
	}

	provisionErr := errors.New("injected provisioning failure")
	unmountErr := errors.New("injected unmount failure")
	detachErr := errors.New("injected detach failure")
	injected := func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch args[0] {
		case "attach":
			return attach(disk, "-mountpoint", mnt)
		case "mount":
			// hdiutil already mounted the disk at the requested path.
			return nil, nil
		case "unmount":
			return nil, unmountErr
		case "detach":
			return nil, detachErr
		default:
			t.Fatalf("unexpected tool command %s %v", name, args)
			return nil, nil
		}
	}
	logger := discardLogger()
	err = withAttachedDisk(ctx, injected, disk, logger, func(whole string) error {
		return withMountedDataVolume(ctx, injected, whole, mnt, func() error {
			if err := os.WriteFile(sentinel, []byte(sentinelText), 0o600); err != nil {
				t.Fatal(err)
			}
			return provisionErr
		})
	})
	err = cleanupFailedMacOSBuild(runHostTool, macOSMountPoints, build, logger, err)
	for _, cause := range []error{provisionErr, unmountErr, detachErr, errDiskDetachUnconfirmed} {
		if !errors.Is(err, cause) {
			t.Fatalf("cleanup error = %v, want %v", err, cause)
		}
	}
	if !strings.Contains(err.Error(), build) {
		t.Fatalf("cleanup error has no staging path: %v", err)
	}
	requireRefusal("is still active")
	assertSentinel()
	detach()
	t.Log("failed detach and startup with a mount preserved the mounted guest sentinel")

	if _, err := attach(disk, "-nomount"); err != nil {
		t.Fatal(err)
	}
	requireRefusal("is still attached")
	detach()
	t.Log("startup refused an attached but unmounted staged disk")

	outside := filepath.Join(work, "other.dmg")
	create(outside)
	if err := os.MkdirAll(mnt, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := attach(outside, "-mountpoint", mnt); err != nil {
		t.Fatal(err)
	}
	requireRefusal("is still active")
	detach()
	t.Log("startup refused a mount below staging from a different disk image")

	if err := removeMacOSBuild(ctx, runHostTool, macOSMountPoints, build); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(build); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging stat error = %v, want os.ErrNotExist", err)
	}
	t.Log("startup cleared staging after both native inventories became clear")
}
