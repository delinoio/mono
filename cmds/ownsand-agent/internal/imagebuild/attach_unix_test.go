// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build unix

package imagebuild

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHdiutilScript stands in for /usr/bin/hdiutil: it logs its arguments,
// prints the canned attach output and fails the first plain detach when
// FAKE_DETACH_FAIL_ONCE is set.
const fakeHdiutilScript = `#!/bin/sh
echo "hdiutil $*" >> "$FAKE_LOG"
case "$1" in
attach) cat "$FAKE_ATTACH_OUT" ;;
detach)
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
			if tc.failDetach {
				t.Setenv("FAKE_DETACH_FAIL_ONCE", "1")
			} else {
				t.Setenv("FAKE_DETACH_FAIL_ONCE", "")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := withAttachedDisk(ctx, run, "/build/disk.img", discardLogger(), func(string) error {
				return tc.use(ctx, cancel)
			})
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
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(lines) < 1 || !strings.HasPrefix(lines[0], "hdiutil attach ") {
				t.Fatalf("call log = %q", lines)
			}
			if got, want := strings.Join(lines[1:], "|"), strings.Join(tc.wantLogLines, "|"); got != want {
				t.Fatalf("calls after the attach = %q, want %q", got, want)
			}
		})
	}
}
