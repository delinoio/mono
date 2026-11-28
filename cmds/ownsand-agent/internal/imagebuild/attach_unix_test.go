// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build unix

package imagebuild

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
)

// fakeHdiutilScript stands in for /usr/bin/hdiutil: it logs its arguments,
// prints the canned attach output and fails the first plain detach when
// FAKE_DETACH_FAIL_ONCE is set.
const fakeHdiutilScript = `#!/bin/sh
echo "hdiutil $*" >> "$FAKE_LOG"
case "$1" in
attach)
	cat "$FAKE_ATTACH_OUT"
	if [ -n "$FAKE_ATTACH_FAIL" ]; then
		echo "hdiutil: interrupted attach" >&2
		exit 23
	fi ;;
info) cat "$FAKE_INFO_OUT" ;;
detach)
	if [ -n "$FAKE_DETACH_FAIL_ALWAYS" ]; then
		echo "hdiutil: device still busy" >&2
		exit 1
	fi
	if [ -n "$FAKE_DETACH_FAIL_ONCE" ] && [ "$2" != "-force" ] && [ ! -e "$FAKE_LOG.failed" ]; then
		: > "$FAKE_LOG.failed"
		echo "hdiutil: couldn't unmount: resource busy" >&2
		exit 1
	fi ;;
esac
`

// fakeToolScript stands in for a host tool that the step after the attach
// runs, and always fails.
const fakeToolScript = `#!/bin/sh
echo "$(basename "$0") $*" >> "$FAKE_LOG"
echo "$(basename "$0"): conversion failed" >&2
exit 1
`

// TestWithAttachedDiskRealProcesses runs the production runner against shell
// scripts that replace the host tools, so that the attach, the failure of a
// later tool, the cancellation of the build context and the detach all go
// through real processes and real contexts.
func TestWithAttachedDiskRealProcesses(t *testing.T) {
	dir := t.TempDir()
	for name, script := range map[string]string{"hdiutil": fakeHdiutilScript, "plutil": fakeToolScript, "diskutil": fakeToolScript} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return runHostTool(ctx, filepath.Join(dir, filepath.Base(name)), args...)
	}
	for _, tc := range []struct {
		name         string
		attachOut    string
		failAttach   bool
		failDetach   bool
		use          func(ctx context.Context, cancel context.CancelFunc) error
		wantErr      string
		wantLogLines []string
	}{
		{
			name: "plutil fails after the attach", attachOut: attachPlist,
			use: func(ctx context.Context, _ context.CancelFunc) error {
				_, err := run(ctx, "/usr/bin/plutil", "-convert", "json", "-o", "-", "-")
				return err
			},
			wantErr:      "plutil: conversion failed",
			wantLogLines: []string{"plutil -convert json -o - -", "hdiutil detach /dev/disk4"},
		},
		{
			name: "build context is canceled after the attach", attachOut: attachPlist,
			use: func(ctx context.Context, cancel context.CancelFunc) error {
				cancel()
				_, err := run(ctx, "/usr/sbin/diskutil", "apfs", "list", "-plist")
				return err
			},
			wantErr:      "context canceled",
			wantLogLines: []string{"hdiutil detach /dev/disk4"},
		},
		{
			name: "plist without a GUID entity", attachOut: attachPlistNoGUID,
			use:          func(context.Context, context.CancelFunc) error { return nil },
			wantErr:      errNoGUIDPartitionTable.Error(),
			wantLogLines: []string{"hdiutil detach /dev/disk7"},
		},
		{
			name: "first detach fails and the forced detach succeeds", attachOut: attachPlist, failDetach: true,
			use:          func(context.Context, context.CancelFunc) error { return nil },
			wantLogLines: []string{"hdiutil detach /dev/disk4", "hdiutil detach -force /dev/disk4"},
		},
		{
			name: "attach reports a device and exits nonzero", attachOut: attachPlist, failAttach: true,
			wantErr:      "hdiutil: interrupted attach",
			wantLogLines: []string{"hdiutil info -plist", "hdiutil detach /dev/disk4"},
		},
		{
			name: "attach truncates stdout and exits nonzero", attachOut: attachPlist[:disk4EntryEnd], failAttach: true, failDetach: true,
			wantErr:      "hdiutil: interrupted attach",
			wantLogLines: []string{"hdiutil info -plist", "hdiutil detach /dev/disk4", "hdiutil info -plist", "hdiutil detach -force /dev/disk4"},
		},
		{
			name: "happy path", attachOut: attachPlist,
			use:          func(context.Context, context.CancelFunc) error { return nil },
			wantLogLines: []string{"hdiutil detach /dev/disk4"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			logPath := filepath.Join(work, "calls.log")
			attachPath := filepath.Join(work, "attach.plist")
			if err := os.WriteFile(attachPath, []byte(tc.attachOut), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FAKE_LOG", logPath)
			t.Setenv("FAKE_ATTACH_OUT", attachPath)
			infoPath := filepath.Join(work, "info.plist")
			if err := os.WriteFile(infoPath, []byte(diskInfoPlist("/build/disk.img", "/dev/disk4")), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FAKE_INFO_OUT", infoPath)
			attachFail := ""
			if tc.failAttach {
				attachFail = "1"
			}
			t.Setenv("FAKE_ATTACH_FAIL", attachFail)
			if tc.failDetach {
				t.Setenv("FAKE_DETACH_FAIL_ONCE", "1")
			} else {
				t.Setenv("FAKE_DETACH_FAIL_ONCE", "")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := withAttachedDisk(ctx, run, "/build/disk.img", discardLogger(), func(string) error {
				if tc.failAttach {
					t.Fatal("use called after a failed attach process")
				}
				return tc.use(ctx, cancel)
			})
			if tc.failAttach {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
					t.Fatalf("error = %v, want the original attach exit status 23", err)
				}
			}
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
			}
			b, rerr := os.ReadFile(logPath)
			if rerr != nil {
				t.Fatal(rerr)
			}
			lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
			const wantAttach = "hdiutil attach -imagekey diskimage-class=CRawDiskImage -nomount -plist /build/disk.img"
			if got := lines[0]; got != wantAttach {
				t.Fatalf("attach log line = %q, want %q", got, wantAttach)
			}
			if got, want := strings.Join(lines[1:], "|"), strings.Join(tc.wantLogLines, "|"); got != want {
				t.Fatalf("calls after the attach = %q, want %q", got, want)
			}
		})
	}
}

// Run the cleanup path against real subprocess failures and disposable files.
// The host-tool scripts simulate attachment state. They do not mount a disk.
func TestMacOSBuildCleanupRealProcesses(t *testing.T) {
	for _, tc := range []struct {
		name           string
		provisionFails bool
		detachFails    bool
	}{
		{name: "provision and detach fail", provisionFails: true, detachFails: true},
		{name: "provision succeeds and detach fails", detachFails: true},
		{name: "provision fails and forced detach succeeds", provisionFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			build := filepath.Join(work, "macos.build")
			mnt := filepath.Join(build, "mnt")
			disk := filepath.Join(build, images.MacOSDisk)
			hdiutil := filepath.Join(work, "hdiutil")
			diskutil := filepath.Join(work, "diskutil")
			logPath := filepath.Join(work, "calls.log")
			attachPath := filepath.Join(work, "attach.plist")
			infoPath := filepath.Join(work, "info.plist")
			if err := os.Mkdir(build, 0o700); err != nil {
				t.Fatal(err)
			}
			for path, contents := range map[string]string{
				hdiutil: fakeHdiutilScript,
				diskutil: `#!/bin/sh
case "$1" in
mount) exit 0 ;;
unmount) echo "diskutil: mount still busy" >&2; exit 1 ;;
esac
`,
				attachPath: attachPlist,
				infoPath:   emptyDiskInventory,
				disk:       "staged disk sentinel",
			} {
				if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("FAKE_LOG", logPath)
			t.Setenv("FAKE_ATTACH_OUT", attachPath)
			t.Setenv("FAKE_INFO_OUT", infoPath)
			t.Setenv("FAKE_ATTACH_FAIL", "")
			t.Setenv("FAKE_DETACH_FAIL_ONCE", "1")
			t.Setenv("FAKE_DETACH_FAIL_ALWAYS", "")
			if tc.detachFails {
				t.Setenv("FAKE_DETACH_FAIL_ALWAYS", "1")
			}
			run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return runHostTool(ctx, filepath.Join(work, filepath.Base(name)), args...)
			}
			ctx := t.Context()
			logger := discardLogger()
			provisionErr := errors.New("provisioning failed")
			err := withAttachedDisk(ctx, run, disk, logger, func(string) error {
				return withMountedDataVolume(ctx, run, "disk5s1", mnt, func() error {
					if err := os.WriteFile(filepath.Join(mnt, "sentinel"), []byte("guest sentinel"), 0o600); err != nil {
						t.Fatal(err)
					}
					if tc.provisionFails {
						return provisionErr
					}
					return nil
				})
			})
			mounts := func() ([]string, error) {
				if tc.detachFails {
					t.Fatal("a failed detach must retain staging before it checks host state")
				}
				return nil, nil
			}
			err = cleanupFailedMacOSBuild(run, mounts, build, logger, err)
			if errors.Is(err, provisionErr) != tc.provisionFails || !strings.Contains(err.Error(), "diskutil: mount still busy") {
				t.Fatalf("build error = %v, want unmount failure and provisioning failure = %v", err, tc.provisionFails)
			}
			if !tc.detachFails {
				if errors.Is(err, errDiskDetachUnconfirmed) {
					t.Fatalf("forced detach succeeded: %v", err)
				}
				if _, err := os.Stat(build); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("staging stat error = %v, want os.ErrNotExist", err)
				}
				return
			}
			if !errors.Is(err, errDiskDetachUnconfirmed) || !strings.Contains(err.Error(), "hdiutil: device still busy") || !strings.Contains(err.Error(), build) {
				t.Fatalf("build error = %v, want the staging path and failed detach", err)
			}
			for path, want := range map[string]string{disk: "staged disk sentinel", filepath.Join(mnt, "sentinel"): "guest sentinel"} {
				if got, err := os.ReadFile(path); err != nil || string(got) != want {
					t.Fatalf("sentinel %q = %q, %v", path, got, err)
				}
			}
			calls, err := os.ReadFile(logPath)
			if err != nil || !strings.Contains(string(calls), "hdiutil detach /dev/disk4\nhdiutil detach -force /dev/disk4\n") {
				t.Fatalf("host calls = %s, %v, want both detach attempts", calls, err)
			}
		})
	}
}
